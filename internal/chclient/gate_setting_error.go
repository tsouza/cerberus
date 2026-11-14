package chclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// chCodeUnknownSetting is ClickHouse's UNKNOWN_SETTING: the server does not
// know a setting the query carries, and rejects the whole query.
const chCodeUnknownSetting = 115

// ErrGateSettingUnsupported is the sentinel a *GateSettingUnsupportedError
// unwraps to.
var ErrGateSettingUnsupported = errors.New("chclient: a ClickHouse node is too old for a setting cerberus needs")

// GateSettingUnsupportedError is a query a ClickHouse node rejected with
// UNKNOWN_SETTING for one of cerberus's defect-gate settings
// (chopt.GateSettings), sent at the gate's own value: the node predates the
// setting, which cerberus sends because another node of the same fleet runs a
// build that answers the query wrong without it. The node that rejected it
// predates every such defect, so the query never returns wrong rows; it fails
// on that node until the node is upgraded past Gate.Since.
type GateSettingUnsupportedError struct {
	Gate chopt.GateSetting
	// Value is the value the query carried the setting at, nil when the
	// settings the query was sent with do not carry it.
	Value any
	Cause error
}

func (e *GateSettingUnsupportedError) Error() string {
	sent := e.Gate.Name
	if e.Value != nil {
		sent = fmt.Sprintf("%s=%v", e.Gate.Name, e.Value)
	}
	return fmt.Sprintf(
		"ClickHouse node is too old for setting %s, which cerberus sends as %s to avoid wrong results from %s on another node of this fleet; upgrade this node to ClickHouse %s or later",
		e.Gate.Name, sent, strings.Join(e.Gate.Defects, ", "), e.Gate.Since,
	)
}

// Unwrap exposes the sentinels and the underlying ClickHouse exception.
func (e *GateSettingUnsupportedError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrGateSettingUnsupported, ErrNodeTooOld}
	}
	return []error{ErrGateSettingUnsupported, ErrNodeTooOld, e.Cause}
}

// wrapGateSettingUnsupported converts an UNKNOWN_SETTING exception naming a
// defect-gate setting into a *GateSettingUnsupportedError, given sent, the
// settings the query was sent with. A gate setting sent at a value its
// chopt.DroppableSettings entry may be dropped at is the stamp of a feature,
// not of the gate, so its rejection is left to wrapUnknownName like any other
// setting the set in force sends. An UNKNOWN_SETTING for any other setting,
// and every other error, passes through untouched. The code is matched typed;
// the setting is read off the exception's message (parseUnknownName),
// because ClickHouse reports it nowhere else.
func wrapGateSettingUnsupported(err error, sent clickhouse.Settings) error {
	u, ok := parseUnknownName(err)
	if !ok || u.kind != unknownNameSetting {
		return err
	}
	g, ok := chopt.GateSettingByName(u.name)
	if !ok {
		return err
	}
	value, carried := sent[u.name]
	if d, droppable := chopt.LookupDroppableSetting(u.name); carried && droppable && d.DroppableAt(value) {
		return err
	}
	return &GateSettingUnsupportedError{Gate: g, Value: value, Cause: err}
}
