package surfaceparity

import (
	"strings"
	"testing"
)

// TestClassify pins every cell of the parity matrix. The live inventory can
// hold no symbol in a given cell (today every PromQL symbol is a parity
// accept), so the table is pinned here rather than through it.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		cerberus, reference Verdict
		want                Classification
	}{
		{VerdictAccept, VerdictAccept, ClassParityAccept},
		{VerdictReject, VerdictReject, ClassParityReject},
		{VerdictReject, VerdictAccept, ClassWrongReject},
		{VerdictAccept, VerdictReject, ClassWrongAccept},
	} {
		if got := classify(tc.cerberus, tc.reference); got != tc.want {
			t.Errorf("classify(%s, %s) = %s, want %s", tc.cerberus, tc.reference, got, tc.want)
		}
	}
}

func TestPromQLReferenceVerdicts_Verdict(t *testing.T) {
	rv := &referenceVerdicts{Verdicts: map[string]string{
		"fn:accepted": "accept",
		"fn:rejected": "reject",
		"fn:garbled":  "maybe",
	}}
	for _, tc := range []struct {
		symbol  string
		want    Verdict
		wantErr string
	}{
		{"fn:accepted", VerdictAccept, ""},
		{"fn:rejected", VerdictReject, ""},
		{"fn:garbled", "", `invalid verdict "maybe"`},
		{"fn:missing", "", `no reference verdict for "fn:missing"`},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			got, err := rv.verdict(tc.symbol)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("verdict(%q) error = %v, want one containing %q", tc.symbol, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("verdict(%q): %v", tc.symbol, err)
			}
			if got != tc.want {
				t.Errorf("verdict(%q) = %q, want %q", tc.symbol, got, tc.want)
			}
		})
	}
}

// TestCerberusVerdictPromQL_Rejections pins the two ways a probe is
// rejected: a probe the parser refuses is tagged as a probe-synthesis bug,
// and one the lowering refuses carries the lowering's own message.
func TestCerberusVerdictPromQL_Rejections(t *testing.T) {
	v, msg := cerberusVerdictPromQL("rate(")
	if v != VerdictReject || !strings.HasPrefix(msg, "probe-parse: ") {
		t.Errorf("unparsable probe: verdict %q, message %q; want reject tagged probe-parse", v, msg)
	}

	// Two capture groups sharing a name, one of them nullable: a regex the
	// lowering cannot express in ClickHouse (the rejection-parity catalogue's
	// label_replace divergence).
	v, msg = cerberusVerdictPromQL(`label_replace(up, "d", "$dup", "src", "(?:(?P<dup>a?)|y)*(?P<dup>b)")`)
	if v != VerdictReject || !strings.HasPrefix(msg, "promql: ") {
		t.Errorf("lowering rejection: verdict %q, message %q; want reject with the lowering's promql: message", v, msg)
	}

	if v, msg := cerberusVerdictPromQL("up"); v != VerdictAccept || msg != "" {
		t.Errorf("accepted probe: verdict %q, message %q; want accept with no message", v, msg)
	}
}
