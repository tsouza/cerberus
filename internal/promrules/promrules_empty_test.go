package promrules

import "testing"

// This pins #3182. Parse was a plain yaml.Unmarshal into RuleGroups, so any
// well-formed YAML decoded cleanly — a `--rules` glob that matched a
// prometheus.yml, an empty file or a comments-only file yielded zero groups and
// no error. That is neither a zero-match glob nor a parse failure, so the
// harvester recorded no skip, and the cutover gate's rulegraph stage returned
// PASS: "nothing must stay materialized after cutover" certified for a
// Prometheus whose recording rules were never read.
func TestParseRejectsADocumentThatIsNotARuleFile(t *testing.T) {
	for name, input := range map[string]string{
		"empty file":          "",
		"prometheus.yml":      "global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: a\n",
		"comments only":       "# no rules here\n",
		"empty mapping":       "{}\n",
		"groups key, no rows": "groups: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			rg, err := Parse([]byte(input))
			if err == nil {
				t.Fatalf("Parse(%q) returned no error and %d group(s); a document with no rule "+
					"groups must not read as an empty-but-valid rule set", input, len(rg.Groups))
			}
		})
	}
}

// The other direction, so the rejection above cannot be satisfied by rejecting
// everything: a real rule file still parses, with its rules intact.
func TestParseStillAcceptsARealRuleFile(t *testing.T) {
	const input = "groups:\n" +
		"  - name: cpu\n" +
		"    rules:\n" +
		"      - record: job:cpu:rate5m\n" +
		"        expr: sum(rate(cpu[5m])) by (job)\n" +
		"      - alert: HighCPU\n" +
		"        expr: job:cpu:rate5m > 0.9\n"
	rg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("a real rule file must parse: %v", err)
	}
	if len(rg.Groups) != 1 || len(rg.Groups[0].Rules) != 2 {
		t.Fatalf("Groups = %+v, want one group of two rules", rg.Groups)
	}
	if rg.Groups[0].Rules[0].Record != "job:cpu:rate5m" {
		t.Errorf("Record = %q, want job:cpu:rate5m", rg.Groups[0].Rules[0].Record)
	}
	if rg.Groups[0].Rules[1].Alert != "HighCPU" {
		t.Errorf("Alert = %q, want HighCPU", rg.Groups[0].Rules[1].Alert)
	}
}
