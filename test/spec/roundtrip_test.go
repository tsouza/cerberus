package spec

import (
	"testing"

	"golang.org/x/tools/txtar"
)

func TestLoadRoundTrip_RawStringsSectionSetsFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "absent_section_keeps_false",
			body: `-- seed --
CREATE TABLE t (a String) ENGINE = Memory;
INSERT INTO t VALUES ('hi');
-- sql --
SELECT a FROM t
-- expected_rows --
[["hi"]]
`,
			want: false,
		},
		{
			name: "present_section_sets_true",
			body: `-- seed --
CREATE TABLE t (a String) ENGINE = Memory;
INSERT INTO t VALUES ('hi');
-- sql --
SELECT a FROM t
-- expected_rows --
[["hi"]]
-- raw_strings --
`,
			want: true,
		},
		{
			name: "present_section_with_body_still_true",
			body: `-- seed --
CREATE TABLE t (a String) ENGINE = Memory;
INSERT INTO t VALUES ('hi');
-- sql --
SELECT a FROM t
-- expected_rows --
[["hi"]]
-- raw_strings --
preserve literal { and [ payloads
`,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Case{
				Name:    "inline",
				archive: txtar.Parse([]byte(tc.body)),
			}
			rt, err := LoadRoundTrip(c)
			if err != nil {
				t.Fatalf("LoadRoundTrip: %v", err)
			}
			if rt.RawStrings != tc.want {
				t.Fatalf("RawStrings = %v, want %v", rt.RawStrings, tc.want)
			}
		})
	}
}
