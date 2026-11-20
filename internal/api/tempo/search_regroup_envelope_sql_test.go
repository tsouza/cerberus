package tempo_test

import (
	"net/url"
	"strings"
	"testing"
)

// TestSearch_RegroupEnvelope_SQLShape pins the statement a search ending in
// by() or coalesce() sends: one aggregate per trace over the stage's own
// input, recording whether a root span matched. Over a scan it also reads
// the matched root's name and service; over a structural relation it reads
// neither, and the root lookup names the root. Each query below would
// otherwise group by trace and key (or span) first, holding a representative
// span's name, ResourceAttributes and ParentSpanId per group, and fold the
// groups into traces in a second aggregate.
func TestSearch_RegroupEnvelope_SQLShape(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		`{ kind = server } >> { } | by(kind)`,
		`{ kind = server } >> { } | coalesce()`,
		`{ } | by(kind)`,
		`{ } | by(kind) | by(name)`,
		`{ } | coalesce()`,
		`{ span.http.status_code = 500 } | by(resource.service.name)`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			sql := searchSQL(t, "/api/search?q="+url.QueryEscape(q)+"&limit=20")
			for _, absent := range []string{
				"GROUP BY `TraceId`, `GroupKey`",
				"GROUP BY `TraceId`, `SpanId`",
				"any(`SpanName`)",
				"any(`ResourceAttributes`",
				"any(`ParentSpanId`)",
			} {
				if strings.Contains(sql, absent) {
					t.Errorf("SQL contains %q:\n%s", absent, sql)
				}
			}
			rootName := "argMax(`SpanName`, (`ParentSpanId` IN (?, ?, ?))) AS `RootName`"
			if strings.Contains(q, ">>") {
				if strings.Contains(sql, rootName) {
					t.Errorf("SQL over a structural relation reads the root's name:\n%s", sql)
				}
			} else if !strings.Contains(sql, rootName) {
				t.Errorf("SQL lacks %q:\n%s", rootName, sql)
			}
			for _, want := range []string{
				"GROUP BY `TraceId`",
				"max((`ParentSpanId` IN (?, ?, ?))) AS `RootMatched`",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("SQL lacks %q:\n%s", want, sql)
				}
			}
		})
	}
}

// TestSearch_RegroupEnvelope_ScalarFilterKeepsGroups pins that a scalar
// filter after by() still reads each group's own value: the groups are
// computed first, filtered, and only then folded into traces.
func TestSearch_RegroupEnvelope_ScalarFilterKeepsGroups(t *testing.T) {
	t.Parallel()
	sql := searchSQL(t, "/api/search?q="+url.QueryEscape(`{ } | by(kind) | count() > 1`)+"&limit=20")
	if !strings.Contains(sql, "GROUP BY `TraceId`, `GroupKey`") {
		t.Errorf("SQL lacks the per-group aggregate:\n%s", sql)
	}
}
