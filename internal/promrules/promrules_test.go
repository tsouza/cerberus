package promrules

import "testing"

// TestParse pins that a rule file with both a recording rule and an alerting
// rule decodes into the right group/name/expr shape.
func TestParse(t *testing.T) {
	const rules = `
groups:
  - name: cpu
    rules:
      - record: job:cpu:rate5m
        expr: sum(rate(cpu_seconds_total[5m])) by (job)
      - alert: HighErrorRate
        expr: rate(errors_total[5m]) > 0.5
        for: 10m
`
	rg, err := Parse([]byte(rules))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rg.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(rg.Groups))
	}
	g := rg.Groups[0]
	if g.Name != "cpu" {
		t.Errorf("group name = %q, want cpu", g.Name)
	}
	if len(g.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(g.Rules))
	}

	rec := g.Rules[0]
	if rec.Record != "job:cpu:rate5m" {
		t.Errorf("record name = %q", rec.Record)
	}
	if rec.Alert != "" {
		t.Errorf("recording rule should have no alert name, got %q", rec.Alert)
	}
	if rec.Expr != "sum(rate(cpu_seconds_total[5m])) by (job)" {
		t.Errorf("record expr = %q", rec.Expr)
	}

	alert := g.Rules[1]
	if alert.Alert != "HighErrorRate" {
		t.Errorf("alert name = %q", alert.Alert)
	}
	if alert.Record != "" {
		t.Errorf("alerting rule should have no record name, got %q", alert.Record)
	}
	if alert.Expr != "rate(errors_total[5m]) > 0.5" {
		t.Errorf("alert expr = %q", alert.Expr)
	}
}

// TestParseInvalidYAML pins that a malformed rule file surfaces a wrapped error
// rather than a zero-value success.
func TestParseInvalidYAML(t *testing.T) {
	if _, err := Parse([]byte("groups: [::: not yaml")); err == nil {
		t.Fatal("expected a parse error for malformed YAML")
	}
}
