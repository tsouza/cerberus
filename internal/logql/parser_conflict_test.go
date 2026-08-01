package logql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestParserConflictRename_EveryParserFamily is the anti-divergence gate
// for Loki's label-collision contract: when a parser-extracted key would
// shadow a stream-selector label, the stream label wins and the extracted
// value stays reachable under the `_extracted`-suffixed name.
//
// The policy used to live only on the `| logfmt` lowering; `| json`
// (bare and typed) and `| regexp` did a raw `mapConcat` that let the
// extracted key silently overwrite the stream label. Every parser family
// that lowers to a labels merge is enumerated here so a family cannot be
// added — or an existing one rewritten — without the rename, which is the
// exact way the three stages drifted apart in the first place.
//
// Each case is a stream selector on `level` plus a parser stage that
// extracts a `level` of its own, so the collision is forced; the query
// then filters on `level_extracted`, which only resolves if the rename
// fired. The assertion is on the emitted SQL shape rather than on rows so
// it runs on the default build tag alongside the seeded TXTAR fixtures
// (test/spec/logql/parser_{json,json_typed,regexp}_conflict.txtar) that
// pin the end-to-end row behaviour.
func TestParserConflictRename_EveryParserFamily(t *testing.T) {
	t.Parallel()

	// mapApplyRename is the dynamic-key shape: the extracted key set is
	// unknown at SQL-emit time, so the rename runs per-key in a lambda.
	const mapApplyRename = "mapApply((k, v) -> tuple(if(mapContains(`ResourceAttributes`, k), concat(k, ?), k), v)"
	// perKeyRename is the static-identifier shape: each destination name
	// is known at emit time, so the rename is a per-key if(...).
	const perKeyRename = "map(if(mapContains(`ResourceAttributes`, ?), ?, ?)"

	cases := []struct {
		name  string
		query string
		want  string
		// reject is the pre-fix unmediated shape — a raw merge of the
		// extracted keys onto the stream labels with no rename in
		// between. Its presence means the collision policy was skipped.
		reject string
	}{
		{
			name:   "logfmt bare",
			query:  `{level="error"} | logfmt | level_extracted="warn"`,
			want:   mapApplyRename,
			reject: "mapConcat(`ResourceAttributes`, extractKeyValuePairs(",
		},
		{
			name:   "logfmt typed",
			query:  `{level="error"} | logfmt level="severity" | level_extracted="warn"`,
			want:   perKeyRename,
			reject: "map(?, extractKeyValuePairs(",
		},
		{
			name:   "json bare",
			query:  `{level="error"} | json | level_extracted="warn"`,
			want:   mapApplyRename,
			reject: "mapConcat(`ResourceAttributes`, CAST(JSONExtractKeysAndValues(",
		},
		{
			name:   "json typed",
			query:  `{level="error"} | json level="severity" | level_extracted="warn"`,
			want:   perKeyRename,
			reject: "map(?, JSONExtractString(",
		},
		{
			name:   "regexp named capture",
			query:  `{level="error"} | regexp "level=(?P<level>\\w+)" | level_extracted="warn"`,
			want:   perKeyRename,
			reject: "map(?, extractAllGroupsHorizontal(",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schema.DefaultOTelLogs()
			sql := emitSQL(t, tc.query, s)

			if !strings.Contains(sql, tc.want) {
				t.Errorf("SQL missing collision rename %q\nfull SQL: %s", tc.want, sql)
			}
			if strings.Contains(sql, tc.reject) {
				t.Errorf("SQL carries unmediated merge %q — collision policy skipped\nfull SQL: %s",
					tc.reject, sql)
			}
		})
	}
}

// TestParserConflictRename_BindsSuffixedDestination pins that every
// static-identifier parser family binds BOTH `level` and
// `level_extracted` as arguments — the
// `if(mapContains(ResourceAttributes, 'level'), 'level_extracted',
// 'level')` shape needs both at runtime. Asserting on the bound args
// rather than the SQL text catches a rename that emits the right call
// shape with the wrong name in it (e.g. suffixing the source key instead
// of the destination identifier).
func TestParserConflictRename_BindsSuffixedDestination(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{"json typed", `{job="api"} | json level="severity" | level="error"`},
		{"regexp named capture", `{job="api"} | regexp "level=(?P<level>\\w+)" | level="error"`},
		{"logfmt typed", `{job="api"} | logfmt level="severity" | level="error"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schema.DefaultOTelLogs()
			args := emitArgs(t, tc.query, s)

			var sawBare, sawSuffixed bool
			for _, a := range args {
				v, ok := a.(string)
				if !ok {
					continue
				}
				switch v {
				case "level":
					sawBare = true
				case "level_extracted":
					sawSuffixed = true
				}
			}
			if !sawBare {
				t.Errorf("missing `level` (destination identifier) in args=%v", args)
			}
			if !sawSuffixed {
				t.Errorf("missing `level_extracted` (suffixed destination) in args=%v", args)
			}
		})
	}
}
