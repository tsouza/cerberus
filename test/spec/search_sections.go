package spec

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SectionSearchWindow and SectionSearchLimit name the two optional TraceQL
// fixture sections that thread /api/search's request bounds into lowering:
// the request window as two whitespace-separated Unix-second bounds, and the
// response trace limit as one integer.
const (
	SectionSearchWindow = "search_window"
	SectionSearchLimit  = "search_limit"
)

// SearchWindow returns the [start, end] request window the fixture's
// `search_window:` section declares, and whether it declares one.
func (c *Case) SearchWindow() (start, end time.Time, ok bool, err error) {
	v, ok := c.Section(SectionSearchWindow)
	if !ok {
		return time.Time{}, time.Time{}, false, nil
	}
	fields := strings.Fields(v)
	if len(fields) != 2 {
		return time.Time{}, time.Time{}, true, fmt.Errorf("fixture %s: %s wants two Unix-second bounds, got %q", c.Name, SectionSearchWindow, v)
	}
	startSec, err1 := strconv.ParseInt(fields[0], 10, 64)
	endSec, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil {
		return time.Time{}, time.Time{}, true, fmt.Errorf("fixture %s: bad %s %q", c.Name, SectionSearchWindow, v)
	}
	return time.Unix(startSec, 0).UTC(), time.Unix(endSec, 0).UTC(), true, nil
}

// SearchLimit returns the response trace limit the fixture's `search_limit:`
// section declares, and whether it declares one.
func (c *Case) SearchLimit() (n int, ok bool, err error) {
	v, ok := c.Section(SectionSearchLimit)
	if !ok {
		return 0, false, nil
	}
	n, err = strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, true, fmt.Errorf("fixture %s: bad %s %q: %w", c.Name, SectionSearchLimit, v, err)
	}
	return n, true, nil
}
