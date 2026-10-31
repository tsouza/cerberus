package chclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestWrapGateSettingUnsupported pins the classification: UNKNOWN_SETTING
// naming a gate setting becomes a *GateSettingUnsupportedError whose message
// names the setting, the defects and the release to upgrade to, and keeps
// the exception reachable; UNKNOWN_SETTING for any other setting and every
// other code pass through untouched.
func TestWrapGateSettingUnsupported(t *testing.T) {
	t.Parallel()

	for _, g := range chopt.GateSettings() {
		ex := &clickhouse.Exception{Code: chCodeUnknownSetting, Name: "UNKNOWN_SETTING", Message: "Unknown setting '" + g.Name + "'"}
		err := wrapGateSettingUnsupported(fmt.Errorf("query: %w", ex))
		var gate *GateSettingUnsupportedError
		if !errors.As(err, &gate) || gate.Gate.Name != g.Name {
			t.Fatalf("%s: err = %v; want a *GateSettingUnsupportedError for it", g.Name, err)
		}
		if !errors.Is(err, ErrGateSettingUnsupported) {
			t.Errorf("%s: err does not unwrap to ErrGateSettingUnsupported", g.Name)
		}
		var cause *clickhouse.Exception
		if !errors.As(err, &cause) || cause.Code != chCodeUnknownSetting {
			t.Errorf("%s: the exception is not reachable through the wrapper", g.Name)
		}
		for _, want := range append([]string{g.Name, g.Since.String()}, g.Defects...) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: message %q does not name %q", g.Name, err, want)
			}
		}
	}

	for name, err := range map[string]error{
		"other unknown setting": &clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Unknown setting 'use_query_cache_x'"},
		"other code naming a gate setting": &clickhouse.Exception{
			Code: chCodeMemoryLimitExceeded, Message: "'" + chopt.SettingUseQueryConditionCache + "'",
		},
		"no exception": errors.New("dial: refused"),
	} {
		if got := wrapGateSettingUnsupported(err); got != err {
			t.Errorf("%s: err = %v; want it unchanged", name, got)
		}
	}
	if wrapGateSettingUnsupported(nil) != nil {
		t.Error("nil error wrapped")
	}
}
