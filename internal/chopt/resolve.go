package chopt

import (
	"fmt"
	"sort"
	"strings"
)

// Mode governs how an explicitly-requested feature that the connected server
// is too old to support is handled. It is consulted ONLY for an explicit list;
// under auto an unsupported feature is silently skipped (auto is "best
// available") and under off nothing is selected at all.
type Mode int

const (
	// Enforcing (the default) turns an unsupported explicit feature into a
	// FATAL startup error. It is the default because `auto` and `off` already
	// cover the graceful paths -- `auto` is "best available" and silently
	// skips unsupported features -- so an operator who provides an EXPLICIT
	// feature list is asserting "I require these", which should fail loudly
	// when the connected ClickHouse version cannot honour the request.
	Enforcing Mode = iota
	// Permissive skips an unsupported explicit feature with a WARN and
	// continues startup. Opt in via CERBERUS_CH_OPTIMIZATIONS_MODE=permissive.
	Permissive
)

// ParseMode parses the CERBERUS_CH_OPTIMIZATIONS_MODE value. It accepts
// "permissive" and "enforcing" (case-insensitive, surrounding whitespace
// trimmed) and rejects anything else with an error naming the offending value,
// preserving cerberus's fail-fast-on-misconfiguration contract. An empty
// string resolves to the default Enforcing so an unset env var is not an
// error (internal/config seeds the default, but ParseMode is defensive).
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "enforcing":
		return Enforcing, nil
	case "permissive":
		return Permissive, nil
	default:
		return Enforcing, fmt.Errorf("invalid optimizations mode %q: want \"permissive\" or \"enforcing\"", s)
	}
}

// String renders the mode for boot logging.
func (m Mode) String() string {
	if m == Enforcing {
		return "enforcing"
	}
	return "permissive"
}

// LegacyFlag models the tri-state legacy CERBERUS_EXPERIMENTAL_TS_GRID_RANGE
// bool: unset (Set=false, no effect) vs explicitly set (Set=true) with the
// parsed boolean in Value. The resolver maps an explicit true onto a forced
// enable of ts_grid_range and an explicit false onto a forced disable.
type LegacyFlag struct {
	Set   bool
	Value bool
}

// Config is the resolver input, parsed from the environment by
// internal/config. It carries the raw optimization selection, the parsed mode,
// and the tri-state legacy alias; the resolver combines them with the probed
// server version.
type Config struct {
	// Optimizations is the raw CERBERUS_CH_OPTIMIZATIONS value: a
	// comma-separated list of tokens, each "auto", "off", or a feature id.
	// "auto" composes with explicit ids (e.g. "auto,columnar_result_decode").
	Optimizations string
	// Mode is the parsed CERBERUS_CH_OPTIMIZATIONS_MODE (enforcing/permissive).
	Mode Mode
	// LegacyTSGrid carries the deprecated CERBERUS_EXPERIMENTAL_TS_GRID_RANGE:
	// Set=false means unset (no effect); Set=true means Value applies.
	LegacyTSGrid LegacyFlag
}

// EnabledSet is the immutable resolved result: the set of feature ids the
// auto-picker decided to enable against the probed server version. It is the
// single source of truth every consumer reads; nothing downstream re-reads the
// raw env.
type EnabledSet struct {
	ids map[string]struct{}
}

// Has reports whether feature id is in the resolved set.
func (s EnabledSet) Has(id string) bool {
	_, ok := s.ids[id]
	return ok
}

// IDs returns the enabled feature ids sorted, for deterministic boot logging.
func (s EnabledSet) IDs() []string {
	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

const (
	selectionAuto = "auto"
	selectionOff  = "off"
)

// Resolve runs ONCE at startup, after the runtime version probe, and produces
// the immutable EnabledSet plus the human-readable warnings to log at boot
// (permissive skips and the legacy-alias deprecation / override notices).
//
// Selection is a comma-separated list of tokens; each is "auto", "off", or a
// feature id, and the tokens compose:
//
//   - "off"  -> the empty set. "off" is absolute and may NOT be combined with
//     any other token (off + anything -> FATAL).
//   - "auto" -> unions in every STABLE feature whose MinVersion <= server.
//     Experimental / opt-in features are NEVER pulled in by "auto" (they
//     require explicit listing), preserving the historical experimental-off
//     default. "auto" may sit alongside explicit ids, so
//     "auto,columnar_result_decode" means the auto-set PLUS columnar_result_decode
//     -- the way to add an opt-in feature without giving up version-aware
//     auto-selection of the rest.
//   - a feature id -> an explicit request: supported -> enable; unsupported
//     under Enforcing -> err (FATAL); unsupported under Permissive -> WARN +
//     skip. An explicit id keeps its "I require this" semantics even next to
//     "auto". An UNKNOWN id is ALWAYS err (typo guard), regardless of mode.
//
// The legacy CERBERUS_EXPERIMENTAL_TS_GRID_RANGE alias is layered on top:
//
//   - the legacy flag AND any explicit CERBERUS_CH_OPTIMIZATIONS choice (a
//     feature list OR "off") -> the new CERBERUS_CH_OPTIMIZATIONS wins; the
//     legacy flag is ignored with a WARN (Permissive) or a FATAL err
//     (Enforcing). In particular "off" is absolute: a stale legacy env var can
//     never resurrect ts_grid_range under "off".
//   - under the default "auto" an explicit legacy true force-enables
//     ts_grid_range (subject to version + mode), an explicit legacy false
//     force-disables it.
//   - whenever the legacy flag is set at all, a one-time deprecation warning is
//     appended pointing at CERBERUS_CH_OPTIMIZATIONS.
//
// err != nil means the caller must exit non-zero (it is the FATAL path).
func Resolve(cfg Config, server Version) (EnabledSet, []string, error) {
	selection := strings.ToLower(strings.TrimSpace(cfg.Optimizations))
	if selection == "" {
		selection = selectionAuto
	}

	enabled := make(map[string]struct{})
	var warnings []string

	tokens := splitSelection(selection)
	if hasToken(tokens, selectionOff) {
		// "off" is the absolute kill-switch and may not be combined with
		// anything else: it leaves the empty set.
		if len(tokens) != 1 {
			return EnabledSet{}, nil, fmt.Errorf("ch_opt %q cannot be combined with other selections (got %q)", selectionOff, selection)
		}
	} else {
		// "auto" tokens union in the auto-set; every other token is an explicit
		// feature request. They compose, so "auto,columnar_result_decode" is the
		// auto-set plus that one opt-in feature.
		warns, err := resolveTokens(tokens, cfg.Mode, server, enabled)
		if err != nil {
			return EnabledSet{}, nil, err
		}
		warnings = append(warnings, warns...)
	}

	// The legacy alias is overridden whenever the operator made an explicit
	// non-default choice via CERBERUS_CH_OPTIMIZATIONS -- that includes both an
	// explicit feature list AND the "off" kill-switch. "off" must mean off
	// absolutely: a stale legacy env var may not resurrect ts_grid_range. Only
	// the default "auto" lets the legacy alias take effect.
	legacyOverridden := selection != selectionAuto
	legacyWarns, err := applyLegacyTSGrid(cfg, server, legacyOverridden, enabled)
	if err != nil {
		return EnabledSet{}, nil, err
	}
	warnings = append(warnings, legacyWarns...)

	return EnabledSet{ids: enabled}, warnings, nil
}

// resolveTokens walks the parsed selection tokens. An "auto" token unions in
// the auto-set (every STABLE feature the server supports); every other token is
// an explicit feature request, enabled if supported and otherwise handled per
// mode (Enforcing -> fatal, Permissive -> WARN + skip). An unknown id is always
// fatal. Tokens compose, so "auto,columnar_result_decode" yields the auto-set
// plus that one opt-in feature. Returns the permissive WARN strings.
func resolveTokens(tokens []string, mode Mode, server Version, enabled map[string]struct{}) ([]string, error) {
	var warnings []string
	for _, id := range tokens {
		if id == selectionAuto {
			for _, f := range registry {
				if f.Stability == Stable && server.AtLeast(f.MinVersion) {
					enabled[f.ID] = struct{}{}
				}
			}
			continue
		}
		f, ok := featureByID(id)
		if !ok {
			// Typo guard: unknown id is fatal in BOTH modes.
			return nil, fmt.Errorf("unknown ch_opt feature %q (valid: %s, or %q/%q)", id, strings.Join(allFeatureIDs(), ", "), selectionAuto, selectionOff)
		}
		if server.AtLeast(f.MinVersion) {
			enabled[f.ID] = struct{}{}
			continue
		}
		// Explicitly requested but unsupported by the connected server. The
		// "I require this" contract holds even alongside "auto".
		if mode == Enforcing {
			return nil, fmt.Errorf("ch_opt %q requires ClickHouse >=%s, server is %s", f.ID, f.MinVersion, server)
		}
		warnings = append(warnings, fmt.Sprintf("ch_opt %q disabled: needs ClickHouse >=%s, server is %s", f.ID, f.MinVersion, server))
	}
	return warnings, nil
}

// splitSelection comma-splits a selection string into trimmed, non-empty tokens.
func splitSelection(selection string) []string {
	parts := strings.Split(selection, ",")
	tokens := make([]string, 0, len(parts))
	for _, raw := range parts {
		if t := strings.TrimSpace(raw); t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

// hasToken reports whether want appears among tokens.
func hasToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}

// applyLegacyTSGrid layers the deprecated CERBERUS_EXPERIMENTAL_TS_GRID_RANGE
// alias onto the resolved set. It returns the deprecation / override warnings,
// or a fatal error when the alias forces an enable that the server is too old
// for under Enforcing.
//
// overridden is true when the operator made an explicit non-default
// CERBERUS_CH_OPTIMIZATIONS choice -- an explicit feature list OR the "off"
// kill-switch. In both cases the new knob wins and the legacy flag is ignored
// (WARN under Permissive, FATAL under Enforcing); the legacy alias only takes
// effect under the default "auto".
func applyLegacyTSGrid(cfg Config, server Version, overridden bool, enabled map[string]struct{}) ([]string, error) {
	if !cfg.LegacyTSGrid.Set {
		return nil, nil
	}

	// Always emit the one-time deprecation notice when the flag is set at all.
	warnings := []string{
		"CERBERUS_EXPERIMENTAL_TS_GRID_RANGE is deprecated; use CERBERUS_CH_OPTIMIZATIONS (list ts_grid_range to enable the native rate path)",
	}

	// When the operator made an explicit CERBERUS_CH_OPTIMIZATIONS choice (a
	// feature list or the "off" kill-switch), the new knob wins and the legacy
	// flag is ignored.
	if overridden {
		msg := "CERBERUS_EXPERIMENTAL_TS_GRID_RANGE ignored: CERBERUS_CH_OPTIMIZATIONS is set and takes precedence"
		if cfg.Mode == Enforcing {
			return nil, fmt.Errorf("%s", msg)
		}
		return append(warnings, msg), nil
	}

	f, _ := featureByID(FeatureTSGridRange)
	if cfg.LegacyTSGrid.Value {
		// Force-enable, subject to version + mode.
		if server.AtLeast(f.MinVersion) {
			enabled[f.ID] = struct{}{}
			return warnings, nil
		}
		if cfg.Mode == Enforcing {
			return nil, fmt.Errorf("ch_opt %q (via CERBERUS_EXPERIMENTAL_TS_GRID_RANGE) requires ClickHouse >=%s, server is %s", f.ID, f.MinVersion, server)
		}
		return append(warnings, fmt.Sprintf("ch_opt %q disabled: needs ClickHouse >=%s, server is %s", f.ID, f.MinVersion, server)), nil
	}

	// Explicit legacy false force-disables ts_grid_range even if otherwise
	// selected (it cannot be auto-selected since it is experimental, but a
	// belt-and-braces delete keeps the contract exact).
	delete(enabled, f.ID)
	return warnings, nil
}

// allFeatureIDs returns every registered feature id, for the unknown-id error
// message.
func allFeatureIDs() []string {
	ids := make([]string, 0, len(registry))
	for _, f := range registry {
		ids = append(ids, f.ID)
	}
	sort.Strings(ids)
	return ids
}
