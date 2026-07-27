package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tsouza/cerberus/internal/schema"
)

// A cerberus.yaml is written in the same shape as the Helm chart's values.yaml
// — `clickhouse.addr`, `query.maxSamples`, `migrate.verify.ref` — so an
// operator who has configured cerberus once has configured it everywhere. The
// flat `CERBERUS_CH_ADDR: …` form stays valid in the same file: it is the long
// tail's escape hatch, reaching the settings the nested shape has no name for.
//
// The contract is deliberately narrow: a config file is EXACTLY equivalent to
// exporting the corresponding environment variables. No setting is parsed
// differently, none resolves to a different value, and precedence is untouched
// (environment > file > built-in default). That equivalence is what lets the
// chart stay the reference for the shape, and what keeps this file from
// becoming a second, subtly different interpretation of any setting.
//
// The nested names are not derivable from the variable names, which is why the
// table below is explicit rather than a naming rule: the chart groups
// `maxOpenConns` under `clickhouse.pool` though the variable has no POOL
// segment, `query.chMaxMemory` lowers to CERBERUS_CH_QUERY_MAX_MEMORY under a
// different prefix entirely, and `schema.replicated.enabled` lowers to
// CERBERUS_SCHEMA_DATABASE_REPLICATED. A rule producing those from the variable
// names would be a rule with more exceptions than cases.

// bindingKind says how a nested value becomes the scalar its environment
// variable would have carried.
type bindingKind int

const (
	// bindScalar renders the value as written: the common case.
	bindScalar bindingKind = iota
	// bindCommaList renders a sequence comma-joined, mirroring the chart's
	// `join ","`, for the settings whose parser splits on commas.
	bindCommaList
	// bindLineList renders a sequence newline-joined, for the settings whose
	// entries may themselves contain commas — LogQL stream selectors — and
	// which are therefore split on newlines, never commas.
	bindLineList
	// bindPairMap renders a mapping as the sorted `k=v,k2=v2` scalar the chart
	// builds for ClickHouse MergeTree SETTINGS.
	bindPairMap
)

// binding maps one nested path to the setting it fills.
type binding struct {
	path    string
	setting string
	kind    bindingKind
}

// Settings the `cerberus migrate` CLI reads. They have no entry in the server's
// typed registry — nothing in the running gateway looks at them — but a
// migration is configured from the same file as the gateway it migrates to, so
// one cerberus.yaml carries both surfaces.
const (
	envInventorySource        = "CERBERUS_INVENTORY_SOURCE"
	envInventoryWindow        = "CERBERUS_INVENTORY_WINDOW"
	envInventoryLokiSource    = "CERBERUS_INVENTORY_LOKI_SOURCE"
	envInventoryLokiSelectors = "CERBERUS_INVENTORY_LOKI_SELECTORS"
	envInventoryTempoSource   = "CERBERUS_INVENTORY_TEMPO_SOURCE"

	envVerifyCorpus             = "CERBERUS_VERIFY_CORPUS"
	envVerifyStart              = "CERBERUS_VERIFY_START"
	envVerifyEnd                = "CERBERUS_VERIFY_END"
	envVerifyStep               = "CERBERUS_VERIFY_STEP"
	envVerifyTolerance          = "CERBERUS_VERIFY_TOLERANCE"
	envVerifyReport             = "CERBERUS_VERIFY_REPORT"
	envVerifyRef                = "CERBERUS_VERIFY_REF"
	envVerifyRefToken           = "CERBERUS_VERIFY_REF_TOKEN" //nolint:gosec // env-var name, not a credential
	envVerifyRefLoki            = "CERBERUS_VERIFY_REF_LOKI"
	envVerifyRefLokiToken       = "CERBERUS_VERIFY_REF_LOKI_TOKEN" //nolint:gosec // env-var name, not a credential
	envVerifyRefLokiOrgID       = "CERBERUS_VERIFY_REF_LOKI_ORG_ID"
	envVerifyRefTempo           = "CERBERUS_VERIFY_REF_TEMPO"
	envVerifyRefTempoToken      = "CERBERUS_VERIFY_REF_TEMPO_TOKEN" //nolint:gosec // env-var name, not a credential
	envVerifyRefTempoOrgID      = "CERBERUS_VERIFY_REF_TEMPO_ORG_ID"
	envVerifyCerberus           = "CERBERUS_VERIFY_CERBERUS"
	envVerifyCerberusToken      = "CERBERUS_VERIFY_CERBERUS_TOKEN" //nolint:gosec // env-var name, not a credential
	envVerifyCerberusLoki       = "CERBERUS_VERIFY_CERBERUS_LOKI"
	envVerifyCerberusLokiToken  = "CERBERUS_VERIFY_CERBERUS_LOKI_TOKEN" //nolint:gosec // env-var name, not a credential
	envVerifyCerberusTempo      = "CERBERUS_VERIFY_CERBERUS_TEMPO"
	envVerifyCerberusTempoToken = "CERBERUS_VERIFY_CERBERUS_TEMPO_TOKEN" //nolint:gosec // env-var name, not a credential
)

// bindings mirrors the chart's typed value blocks one for one, plus the
// `migrate.*` block the chart has no analogue for (it deploys the gateway; it
// does not run a migration). Adding a setting here is what gives it a nested
// name — until then it is reachable by its flat CERBERUS_* key, which never
// stops working.
var bindings = []binding{
	// ClickHouse connection.
	{"clickhouse.addr", envCHAddr, bindCommaList},
	{"clickhouse.database", envCHDatabase, bindScalar},
	{"clickhouse.username", envCHUsername, bindScalar},
	{"clickhouse.password", envCHPassword, bindScalar},
	{"clickhouse.protocol", envCHProtocol, bindScalar},
	{"clickhouse.dialTimeout", envCHDialTimeout, bindScalar},

	// Connection pool. The chart groups these under `pool`; the variables have
	// no matching segment.
	{"clickhouse.pool.maxOpenConns", envCHMaxOpenConns, bindScalar},
	{"clickhouse.pool.maxIdleConns", envCHMaxIdleConns, bindScalar},
	{"clickhouse.pool.connMaxLifetime", envCHConnMaxLifetime, bindScalar},

	// TLS. The chart derives the three file paths from mounted Secret keys; a
	// config file names the paths directly, since it is not deploying anything.
	{"clickhouse.tls.enabled", envCHTLSEnabled, bindScalar},
	{"clickhouse.tls.insecureSkipVerify", envCHTLSSkipVerify, bindScalar},
	{"clickhouse.tls.serverName", envCHTLSServerName, bindScalar},
	{"clickhouse.tls.caFile", envCHTLSCAFile, bindScalar},
	{"clickhouse.tls.certFile", envCHTLSCertFile, bindScalar},
	{"clickhouse.tls.keyFile", envCHTLSKeyFile, bindScalar},

	// Query safety limits. `chMaxMemory` bounds ClickHouse rather than
	// cerberus, so it lowers to a CERBERUS_CH_* variable even though the chart
	// groups it with the other two.
	{"query.maxSamples", envQueryMaxSamples, bindScalar},
	{"query.timeout", envQueryTimeout, bindScalar},
	{"query.chMaxMemory", envCHQueryMaxMemory, bindScalar},

	// Self-telemetry export.
	{"otlp.endpoint", envOTLPEndpoint, bindScalar},
	{"otlp.insecure", envOTLPInsecure, bindScalar},
	{"otlp.headers", envOTLPHeaders, bindScalar},
	{"otlp.exportInterval", envOTLPExportInterval, bindScalar},
	{"otlp.timeout", envOTLPTimeout, bindScalar},

	// Schema provisioning.
	{"autoCreate.schema", envAutoCreateSchema, bindScalar},
	{"autoCreate.database", envAutoCreateDatabase, bindScalar},

	// Per-head admission.
	{"admit.prom", envAdmitProm, bindScalar},
	{"admit.loki", envAdmitLoki, bindScalar},
	{"admit.tempo", envAdmitTempo, bindScalar},
	{"admit.disabled", envAdmitDisabled, bindScalar},

	// Generated DDL. The chart reaches its long tail through a generic
	// `schema.<KEY>` passthrough; here every knob is named, so a mistyped one
	// is caught rather than forwarded to a variable nothing reads.
	{"schema.cluster", envSchemaCluster, bindScalar},
	{"schema.tableEngine", envSchemaTableEngine, bindScalar},
	{"schema.ttl", envSchemaTTL, bindScalar},
	{"schema.ttlMetrics", envSchemaTTLMetrics, bindScalar},
	{"schema.ttlLogs", envSchemaTTLLogs, bindScalar},
	{"schema.ttlTraces", envSchemaTTLTraces, bindScalar},
	{"schema.storagePolicy", envSchemaStoragePolicy, bindScalar},
	{"schema.settings", envSchemaSettings, bindPairMap},
	{"schema.replicated.enabled", envSchemaDBReplicated, bindScalar},
	{"schema.replicated.zookeeperPath", envSchemaDBReplPath, bindScalar},
	{"schema.replicated.shard", envSchemaDBReplShard, bindScalar},
	{"schema.replicated.replica", envSchemaDBReplReplica, bindScalar},

	// Read-side schema shape: the tables cerberus queries, and the
	// resource-attribute allowlist it projects as Prometheus labels.
	{"schema.metrics.gaugeTable", schema.EnvMetricsGaugeTable, bindScalar},
	{"schema.metrics.sumTable", schema.EnvMetricsSumTable, bindScalar},
	{"schema.metrics.histogramTable", schema.EnvMetricsHistogramTable, bindScalar},
	{"schema.metrics.expHistogramTable", schema.EnvMetricsExpHistogramTable, bindScalar},
	{"schema.metrics.summaryTable", schema.EnvMetricsSummaryTable, bindScalar},
	{"schema.logs.table", schema.EnvLogsTable, bindScalar},
	{"schema.traces.table", schema.EnvTracesTable, bindScalar},
	{"schema.traces.tsLookup", schema.EnvTracesTsLookup, bindScalar},
	{"prom.resourceLabels", schema.EnvPromResourceLabels, bindCommaList},

	// Server surface.
	{"http.addr", envHTTPAddr, bindScalar},
	{"debug.pprof", envDebugPProf, bindScalar},
	{"chOptimizations", envCHOptimizations, bindCommaList},
	{"enabledHeads", envEnabledHeads, bindCommaList},
	{"logLevel", envLogLevel, bindScalar},
	{"logFormat", envLogFormat, bindScalar},
	{"requirementsCheck", envRequirementsCheck, bindScalar},

	// `cerberus migrate`. No chart analogue: the chart deploys the gateway, it
	// does not run a migration.
	{"migrate.inventory.source", envInventorySource, bindScalar},
	{"migrate.inventory.window", envInventoryWindow, bindScalar},
	{"migrate.inventory.loki.source", envInventoryLokiSource, bindScalar},
	{"migrate.inventory.loki.selectors", envInventoryLokiSelectors, bindLineList},
	{"migrate.inventory.tempo.source", envInventoryTempoSource, bindScalar},

	{"migrate.verify.corpus", envVerifyCorpus, bindScalar},
	{"migrate.verify.start", envVerifyStart, bindScalar},
	{"migrate.verify.end", envVerifyEnd, bindScalar},
	{"migrate.verify.step", envVerifyStep, bindScalar},
	{"migrate.verify.tolerance", envVerifyTolerance, bindScalar},
	{"migrate.verify.report", envVerifyReport, bindScalar},
	{"migrate.verify.ref", envVerifyRef, bindScalar},
	{"migrate.verify.refToken", envVerifyRefToken, bindScalar},
	{"migrate.verify.cerberus", envVerifyCerberus, bindScalar},
	{"migrate.verify.cerberusToken", envVerifyCerberusToken, bindScalar},

	{"migrate.verify.loki.ref", envVerifyRefLoki, bindScalar},
	{"migrate.verify.loki.refToken", envVerifyRefLokiToken, bindScalar},
	{"migrate.verify.loki.refOrgID", envVerifyRefLokiOrgID, bindScalar},
	{"migrate.verify.loki.cerberus", envVerifyCerberusLoki, bindScalar},
	{"migrate.verify.loki.cerberusToken", envVerifyCerberusLokiToken, bindScalar},

	{"migrate.verify.tempo.ref", envVerifyRefTempo, bindScalar},
	{"migrate.verify.tempo.refToken", envVerifyRefTempoToken, bindScalar},
	{"migrate.verify.tempo.refOrgID", envVerifyRefTempoOrgID, bindScalar},
	{"migrate.verify.tempo.cerberus", envVerifyCerberusTempo, bindScalar},
	{"migrate.verify.tempo.cerberusToken", envVerifyCerberusTempoToken, bindScalar},
}

// settingPrefix is the prefix of every flat key. A key carrying it is taken
// literally rather than looked up as a nested path.
const settingPrefix = "CERBERUS_"

// bindingByPath and bindingBySetting index the table, and building them doubles
// as the duplicate guard: two entries claiming one path, or two paths claiming
// one setting, would silently make one of them dead.
var bindingByPath, bindingBySetting = func() (map[string]binding, map[string]binding) {
	byPath := make(map[string]binding, len(bindings))
	bySetting := make(map[string]binding, len(bindings))
	for _, b := range bindings {
		if prev, dup := byPath[b.path]; dup {
			panic(fmt.Sprintf("config: nested path %q is bound twice, to %s and %s",
				b.path, prev.setting, b.setting))
		}
		if prev, dup := bySetting[b.setting]; dup {
			panic(fmt.Sprintf("config: setting %s is bound twice, to %s and %s",
				b.setting, prev.path, b.path))
		}
		byPath[b.path] = b
		bySetting[b.setting] = b
	}
	return byPath, bySetting
}()

// ConfigFileSettings returns every setting the nested config-file shape can
// express, sorted. The chart-parity gate consumes it to prove the two surfaces
// have not drifted apart: a knob the chart lowers but a config file cannot
// express is a knob whose config-file users have no way to reach.
func ConfigFileSettings() []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.setting)
	}
	sort.Strings(out)
	return out
}

// ConfigFilePath returns the nested path a setting is written as in a
// cerberus.yaml, and whether it has one at all — a setting with no nested name
// is still reachable by its flat CERBERUS_* key. Writers of config files (the
// docs generator, the migration harness) resolve the path here rather than
// restating it, so one table decides the shape everything produces and
// everything reads.
func ConfigFilePath(setting string) (string, bool) {
	b, ok := bindingBySetting[setting]
	return b.path, ok
}

// translateDoc turns a parsed cerberus.yaml into the flat setting-to-value map
// the loader merges, accepting the nested shape and the literal CERBERUS_* form
// in the same document.
//
// An unrecognised key is an error rather than a value quietly left on its
// default. That is the whole point of the nested shape: a mistyped or
// wrongly-nested key looks exactly like a correct one until something
// downstream behaves oddly under load, which is the worst possible moment to
// discover the file was never read the way it was written.
func translateDoc(file string, doc map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(doc))
	var unknown []string
	for key, value := range flattenDoc(doc) {
		switch {
		case strings.HasPrefix(key, settingPrefix):
			// The flat escape hatch. A flat key is a leaf by construction —
			// flattenDoc never descends below one — so it renders with the kind
			// the setting uses nested, or as a newline-joined list when it has
			// no nested name (the shape Lookup.Lines splits back apart).
			kind := bindLineList
			if b, ok := bindingBySetting[key]; ok {
				kind = b.kind
			}
			rendered, err := renderValue(file, key, value, kind)
			if err != nil {
				return nil, err
			}
			out[key] = rendered
		case bindingByPath[key].setting != "":
			b := bindingByPath[key]
			rendered, err := renderValue(file, key, value, b.kind)
			if err != nil {
				return nil, err
			}
			out[b.setting] = rendered
		default:
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown %s: %s%s", file,
			plural(len(unknown), "setting", "settings"),
			strings.Join(unknown, ", "), suggestions(unknown))
	}
	return out, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// suggestions names the closest known path for each unknown one, because the
// mistakes this rejection catches are overwhelmingly near-misses — a typo, or
// the right leaf under the wrong block — and an error that only says "unknown"
// leaves the reader diffing their file against the reference by eye.
func suggestions(unknown []string) string {
	var b strings.Builder
	for _, u := range unknown {
		if best := nearestPath(u); best != "" {
			fmt.Fprintf(&b, "\n  %s — did you mean %s?", u, best)
		}
	}
	return b.String()
}

// nearestPath returns the shortest known path whose last segment equals
// unknown's, or "" when nothing matches. Comparing leaf names rather than edit
// distance is what catches the common shape: the right setting nested under the
// wrong parent.
func nearestPath(unknown string) string {
	leaf := lastSegment(unknown)
	var best string
	for _, b := range bindings {
		if strings.EqualFold(lastSegment(b.path), leaf) && (best == "" || len(b.path) < len(best)) {
			best = b.path
		}
	}
	return best
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// renderValue turns one YAML value into the string its environment variable
// would have carried. Scalars render as YAML wrote them: an unquoted `false`
// and a quoted `"false"` are indistinguishable by the time the typed registry
// parses them, which is the equivalence this file rests on.
func renderValue(file, key string, value any, kind bindingKind) (string, error) {
	switch kind {
	case bindCommaList:
		return joinSequence(file, key, value, ",")
	case bindLineList:
		return joinSequence(file, key, value, "\n")
	case bindPairMap:
		return joinPairs(file, key, value)
	default:
		if isContainer(value) {
			return "", fmt.Errorf("%s: %s takes a single value, not a %s", file, key, kindName(value))
		}
		return scalarString(value), nil
	}
}

// joinSequence renders a sequence the way the chart's `join` does, and accepts
// a bare scalar too: an operator naming one ClickHouse host should not have to
// write a one-item list.
func joinSequence(file, key string, value any, sep string) (string, error) {
	switch v := value.(type) {
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if isContainer(item) {
				return "", fmt.Errorf("%s: %s has a %s among its entries, which cannot be a value",
					file, key, kindName(item))
			}
			parts = append(parts, scalarString(item))
		}
		return strings.Join(parts, sep), nil
	case map[string]any:
		return "", fmt.Errorf("%s: %s takes a list, not a mapping", file, key)
	default:
		return scalarString(value), nil
	}
}

// joinPairs renders a mapping as the sorted `k=v,k2=v2` scalar the chart builds
// for ClickHouse MergeTree SETTINGS. Sorted so one file always yields one
// value, whatever order the keys were written in.
func joinPairs(file, key string, value any) (string, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s: %s takes a mapping of setting names to values, not a %s",
			file, key, kindName(value))
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, n := range names {
		if isContainer(m[n]) {
			return "", fmt.Errorf("%s: %s.%s must be a single value, not a %s",
				file, key, n, kindName(m[n]))
		}
		pairs = append(pairs, n+"="+scalarString(m[n]))
	}
	return strings.Join(pairs, ","), nil
}

func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func kindName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "mapping"
	case []any:
		return "list"
	}
	return "value"
}

// scalarString renders a YAML scalar as the environment variable would have
// carried it. A whole float renders without a decimal point or an exponent:
// YAML loads `5000000` as an int, but a value arriving as a float must not
// reach an integer parser as "5e+06".
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// flattenDoc walks a parsed document into dotted leaf keys. A mapping bound to
// a whole setting — `schema.settings` — is a leaf rather than a branch, so it
// reaches renderValue intact.
func flattenDoc(doc map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, node map[string]any)
	walk = func(prefix string, node map[string]any) {
		for k, v := range node {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			child, isMap := v.(map[string]any)
			descend := isMap && len(child) > 0 &&
				bindingByPath[key].kind != bindPairMap &&
				!strings.HasPrefix(key, settingPrefix)
			if descend {
				walk(key, child)
				continue
			}
			out[key] = v
		}
	}
	walk("", doc)
	return out
}

// configFileNames are the names probed in each search directory, in order.
// Both spellings are accepted because both are what people type.
var configFileNames = []string{configFileBaseName + ".yaml", configFileBaseName + ".yml"}

// configFileDirs are the directories probed, in order: the working directory
// first — where `cerberus migrate` runs, and where a developer expects it —
// then the packaged location a container image mounts.
var configFileDirs = []string{".", "/etc/cerberus"}

// findConfigFile returns the path of the first cerberus.yaml in the search
// order, or "" when there is none.
func findConfigFile() string {
	for _, dir := range configFileDirs {
		for _, name := range configFileNames {
			path := filepath.Join(dir, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return ""
}

// loadConfigFile reads and translates cerberus.yaml into the flat settings it
// carries. No file is not an error — the environment alone configures cerberus
// completely — but a file that exists and cannot be understood is: it was
// written to configure something, and booting with it silently ignored is how a
// gateway ends up serving on defaults nobody chose.
func loadConfigFile() (map[string]string, string, error) {
	path := findConfigFile()
	if path == "" {
		return nil, "", nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-named config path is the point
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil
		}
		return nil, path, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	if doc == nil {
		return map[string]string{}, path, nil
	}
	out, err := translateDoc(path, doc)
	if err != nil {
		return nil, path, err
	}
	return out, path, nil
}
