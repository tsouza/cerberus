// Package logqlwrap reconstructs the production LogQL wire projection for
// spec fixtures whose recorded SQL is captured before Lang.ProjectSamples.
package logqlwrap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// ReconstructLogStreamWrap reconstructs the post-ProjectSamples SQL that the
// production Loki handler sends to ClickHouse for a log-stream fixture.
//
// Layer 2a records LogQL SQL before the engine applies ProjectSamples, so a
// log-stream fixture normally contains a bare SELECT * and never exercises
// the production cursor's (Line, Attributes, TimeUnix[, Metadata]) scan. This
// helper runs the same Lang.Parse + Lang.ProjectSamples pair as production and
// emits the resulting wire projection. Metric queries return ok=false because
// their recorded SQL already carries the matrix shape strict-scan exercises.
func ReconstructLogStreamWrap(c *spec.Case) (sqlStr string, args []any, ok bool, err error) {
	plan, ok, err := ReconstructLogStreamWrapPlan(c)
	if err != nil || !ok {
		return "", nil, ok, err
	}
	sqlStr, args, err = chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", nil, false, fmt.Errorf("chsql.Emit(wrapped): %w", err)
	}
	return sqlStr, args, true, nil
}

// ReconstructLogStreamWrapPlan is ReconstructLogStreamWrap's plan-level twin.
// It returns ok=false for non-LogQL fixtures and LogQL metric queries.
func ReconstructLogStreamWrapPlan(c *spec.Case) (plan chplan.Node, ok bool, err error) {
	query, hasQuery := c.Section("query.logql")
	if !hasQuery {
		return nil, false, nil
	}
	start, end, step, err := readWindowSections(c)
	if err != nil {
		return nil, false, err
	}

	lang := &logql.Lang{
		Schema: schema.DefaultOTelLogs(),
		Start:  start,
		End:    end,
		Step:   step,
	}
	rawPlan, meta, err := lang.Parse(context.Background(), strings.TrimSpace(query))
	if err != nil {
		return nil, false, fmt.Errorf("logql lower (wrap reconstruction): %w", err)
	}
	if meta.IsMetric {
		return nil, false, nil
	}
	return lang.ProjectSamples(rawPlan, meta), true, nil
}

func readWindowSections(c *spec.Case) (start, end time.Time, step time.Duration, err error) {
	if v, ok := c.Section("start"); ok && strings.TrimSpace(v) != "" {
		start, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("bad start %q: %w", v, err)
		}
	}
	if v, ok := c.Section("end"); ok && strings.TrimSpace(v) != "" {
		end, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("bad end %q: %w", v, err)
		}
	}
	if v, ok := c.Section("step"); ok && strings.TrimSpace(v) != "" {
		step, err = time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("bad step %q: %w", v, err)
		}
	}
	return start, end, step, nil
}
