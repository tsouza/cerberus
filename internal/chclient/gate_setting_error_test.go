package chclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestWrapGateSettingUnsupported pins the classification: UNKNOWN_SETTING
// naming a gate setting sent at the gate's value becomes a
// *GateSettingUnsupportedError whose message names the setting, the value
// sent, the defects and the release to upgrade to, and keeps the exception
// reachable; UNKNOWN_SETTING for any other setting and every other code pass
// through untouched.
func TestWrapGateSettingUnsupported(t *testing.T) {
	t.Parallel()

	for _, g := range chopt.GateSettings() {
		ex := &clickhouse.Exception{Code: chCodeUnknownSetting, Name: "UNKNOWN_SETTING", Message: "Unknown setting '" + g.Name + "'"}
		err := wrapGateSettingUnsupported(fmt.Errorf("query: %w", ex), clickhouse.Settings{g.Name: gateOff})
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
		for _, want := range append([]string{g.Name + "=0", g.Since.String()}, g.Defects...) {
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
		if got := wrapGateSettingUnsupported(err, clickhouse.Settings{chopt.SettingUseQueryConditionCache: gateOff}); got != err {
			t.Errorf("%s: err = %v; want it unchanged", name, got)
		}
	}
	if wrapGateSettingUnsupported(nil, nil) != nil {
		t.Error("nil error wrapped")
	}
}

// gateOff is the value every gate setting is stamped at to switch its
// defective mechanism off.
const gateOff = 0

// TestWrapGateSettingUnsupported_FeatureStampIsNotTheGate: a gate setting
// that is also a droppable feature stamp, rejected when the query carried it
// at the feature's value, is not a gate rejection — its message would claim a
// wrong-result defect the query never guarded against — and passes through to
// wrapUnknownName. Sent at the gate's value it is the gate's. Not carried at
// all, it is still classified as the gate, and the message claims no value.
func TestWrapGateSettingUnsupported_FeatureStampIsNotTheGate(t *testing.T) {
	t.Parallel()
	name := chopt.SettingUseQueryConditionCache
	d, ok := chopt.LookupDroppableSetting(name)
	if !ok || d.OnlyAt == nil {
		t.Fatalf("%s is not a droppable feature stamp with an OnlyAt value", name)
	}
	ex := &clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Unknown setting '" + name + "'"}

	if got := wrapGateSettingUnsupported(ex, clickhouse.Settings{name: d.OnlyAt}); got != error(ex) {
		t.Errorf("sent at the feature's value %v: err = %v; want it unchanged", d.OnlyAt, got)
	}
	var unknown *UnknownNameError
	c := newWithConn(&staleConn{})
	ctx := WithQuerySetting(context.Background(), name, d.OnlyAt)
	if err := c.classifyDriverErr(ctx, ex); !errors.As(err, &unknown) || unknown.Name != name || strings.Contains(err.Error(), "wrong results") {
		t.Errorf("classifyDriverErr at the feature's value = %v; want an *UnknownNameError that claims no wrong-result defect", err)
	}

	var gate *GateSettingUnsupportedError
	if err := c.classifyDriverErr(WithQuerySetting(context.Background(), name, gateOff), ex); !errors.As(err, &gate) || gate.Value != gateOff {
		t.Errorf("classifyDriverErr at the gate's value = %v; want a *GateSettingUnsupportedError carrying %d", err, gateOff)
	}
	err := wrapGateSettingUnsupported(ex, nil)
	if !errors.As(err, &gate) || gate.Value != nil || strings.Contains(err.Error(), name+"=") {
		t.Errorf("not carried: err = %v; want a *GateSettingUnsupportedError naming no value", err)
	}
}
