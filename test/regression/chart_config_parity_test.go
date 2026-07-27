package regression

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
)

// TestChartEnvKeysAreReachableFromAConfigFile pins the promise a nested
// cerberus.yaml makes: it is written in the same shape as the chart's
// values.yaml, so an operator who has configured cerberus in Kubernetes has
// configured it on a VM too.
//
// The chart's `cerberus.nonSecretEnv` helper is the only place the typed values
// blocks are lowered to CERBERUS_* variables, which makes it the authority on
// what the shape covers — internal/config's binding table was transcribed from
// it. The two drift apart silently: adding a typed block to the chart emits a
// new variable that the config-file table has no nested name for, and the
// operator's key is then rejected as unknown by the very shape it was copied
// from. Nothing else notices, because the chart is rendered by helm and the
// table is consumed by Go.
//
// The check runs one way only. A setting may have a nested name with no chart
// block — `migrate.*` is the whole `cerberus migrate` surface, which the chart
// has no analogue for, since it deploys the gateway rather than running a
// migration.
func TestChartEnvKeysAreReachableFromAConfigFile(t *testing.T) {
	t.Parallel()

	const helperPath = "../../deploy/helm/cerberus/templates/_helpers.tpl"
	helper, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read chart helper %s: %v", helperPath, err)
	}

	bindable := make(map[string]bool)
	for _, s := range config.ConfigFileSettings() {
		bindable[s] = true
	}

	var missing []string
	seen := make(map[string]bool)
	for _, key := range chartEnvKeys(string(helper)) {
		if seen[key] || bindable[key] {
			continue
		}
		seen[key] = true
		missing = append(missing, key)
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("%s lowers %d env key(s) that a nested cerberus.yaml cannot express: %v\n"+
			"Every typed chart value must have a matching nested path in internal/config's binding table, "+
			"or an operator who copies their values.yaml shape into cerberus.yaml gets it rejected as an unknown key. "+
			"Add the binding (and its chart-mirroring path) to internal/config/nested.go.",
			helperPath, len(missing), missing)
	}
}

// chartEnvKeyRE matches a CERBERUS_* token in the chart helper. Tokens ending
// in `_` are not keys: they are either a templated prefix
// (`CERBERUS_SCHEMA_{{ $k }}`, the generic long-tail passthrough, whose suffix
// is whatever the operator wrote) or a glob in a comment (`CERBERUS_CH_*`).
// Both are excluded by the trailing-underscore rule rather than by name, so a
// new one of either does not need this test edited.
var chartEnvKeyRE = regexp.MustCompile(`CERBERUS_[A-Z0-9_]+`)

func chartEnvKeys(helper string) []string {
	var keys []string
	for _, tok := range chartEnvKeyRE.FindAllString(helper, -1) {
		if strings.HasSuffix(tok, "_") {
			continue
		}
		keys = append(keys, tok)
	}
	return keys
}

// TestChartSecretKeyIsBindableFromAConfigFile covers the one variable the chart
// deliberately keeps out of its ConfigMap. CERBERUS_CH_PASSWORD reaches the
// container through a secretKeyRef, so it is emitted by a different helper and
// in a different form — and a config file is exactly where an operator running
// outside Kubernetes puts it instead.
func TestChartSecretKeyIsBindableFromAConfigFile(t *testing.T) {
	t.Parallel()

	const key = "CERBERUS_CH_PASSWORD"
	for _, s := range config.ConfigFileSettings() {
		if s == key {
			return
		}
	}
	t.Errorf("%s has no nested path in internal/config's binding table; the chart supplies it from a Secret, "+
		"so a config file is the only way to set it outside Kubernetes", key)
}
