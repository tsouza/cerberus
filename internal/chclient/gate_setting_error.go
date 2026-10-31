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
// (chopt.GateSettings): the node predates the setting, which cerberus sends
// because another node of the same fleet runs a build that answers the query
// wrong without it. The node that rejected it predates every such defect, so
// the query never returns wrong rows; it fails on that node until the node is
// upgraded past Gate.Since.
type GateSettingUnsupportedError struct {
	Gate  chopt.GateSetting
	Cause error
}

func (e *GateSettingUnsupportedError) Error() string {
	return fmt.Sprintf(
		"ClickHouse node is too old for setting %s, which cerberus sends to avoid wrong results from %s on another node of this fleet; upgrade this node to ClickHouse %s or later",
		e.Gate.Name, strings.Join(e.Gate.Defects, ", "), e.Gate.Since,
	)
}

// Unwrap exposes the sentinel and the underlying ClickHouse exception.
func (e *GateSettingUnsupportedError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrGateSettingUnsupported}
	}
	return []error{ErrGateSettingUnsupported, e.Cause}
}

// wrapGateSettingUnsupported converts an UNKNOWN_SETTING exception naming a
// defect-gate setting into a *GateSettingUnsupportedError. An UNKNOWN_SETTING
// for any other setting, and every other error, passes through untouched.
// The code is matched typed; the setting is read off the exception's message,
// which quotes it, because ClickHouse reports it nowhere else.
func wrapGateSettingUnsupported(err error) error {
	var ex *clickhouse.Exception
	if err == nil || !errors.As(err, &ex) || ex.Code != chCodeUnknownSetting {
		return err
	}
	for _, g := range chopt.GateSettings() {
		if strings.Contains(ex.Message, "'"+g.Name+"'") {
			return &GateSettingUnsupportedError{Gate: g, Cause: err}
		}
	}
	return err
}
