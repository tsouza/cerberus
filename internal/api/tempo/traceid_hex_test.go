package tempo

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestNormaliseTraceID covers the input-side parser that lets cerberus
// accept both the stripped (e.g. `ab`) and the padded 32-char form on
// /api/traces/{id}/. Tempo's wire format strips leading zeros from
// trace IDs in responses; Grafana echoes that stripped form back on
// follow-up lookups. Cerberus's storage column carries the 32-char
// padded shape (the OTel-CH exporter writes `hex.EncodeToString`), so
// the handler must restore the padding before comparing.
func TestNormaliseTraceID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "already_32_hex",
			in:   "0123456789abcdef0123456789abcdef",
			want: "0123456789abcdef0123456789abcdef",
		},
		{
			name: "stripped_short_form",
			in:   "ab",
			want: "000000000000000000000000000000ab",
		},
		{
			name: "tempo_real_world_stripped",
			in:   "af843259b0a78f5cbe59e11cbaf66b",
			want: "00af843259b0a78f5cbe59e11cbaf66b",
		},
		{
			name: "all_zero_single_digit",
			in:   "0",
			want: "00000000000000000000000000000000",
		},
		{
			name: "empty",
			in:   "",
			want: "00000000000000000000000000000000",
		},
		{
			name: "uppercase_padded_lowercased",
			in:   "AB",
			want: "000000000000000000000000000000ab",
		},
		{
			name: "longer_than_32_returned_unchanged",
			in:   "00000123456789abcdef0123456789abcdef",
			want: "00000123456789abcdef0123456789abcdef",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := normaliseTraceID(c.in)
			if got != c.want {
				t.Errorf("normaliseTraceID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestStripLeadingHexZeros_RendersExpectedSQL pins the SQL shape emitted
// by the leading-zero-stripping projection. The actual byte-level
// stripping is exercised end-to-end by the Tempo compatibility harness
// (which runs CH); this test guards the chsql contract — the function
// name + argument shape that CH consumes.
func TestStripLeadingHexZeros_RendersExpectedSQL(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_traces"},
		Projections: []chplan.Projection{
			{Expr: stripLeadingHexZeros("TraceId"), Alias: "tid"},
		},
	}

	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sql, "replaceRegexpOne(`TraceId`,") {
		t.Errorf("SQL missing replaceRegexpOne(`TraceId`, ...) call; got:\n%s", sql)
	}
	// The two regex literals ride positional args; we don't pin their
	// indices because the emitter may re-order, but they must be present.
	wantLit := []string{"^0+([0-9a-f])", `\1`}
	for _, want := range wantLit {
		found := false
		for _, a := range args {
			if s, ok := a.(string); ok && s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected arg %q not in %v", want, args)
		}
	}
}

// TestCanonicalSampleProjections_StripsTraceIDLeadingZeros verifies the
// /api/search projection routes TraceId through stripLeadingHexZeros so
// the wire format matches Tempo's leading-zero-stripped shape. Without
// this strip the canonical-key differ pairs the two backends'
// summaries by mismatched keys (cerberus `00af…66b` vs Tempo
// `af…66b`), generating spurious missing_in_a / missing_in_b reasons
// per case.
func TestCanonicalSampleProjections_StripsTraceIDLeadingZeros(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	projs := canonicalSampleProjections(s)

	plan := &chplan.Project{
		Input:       &chplan.Scan{Table: s.SpansTable},
		Projections: projs,
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sql, "replaceRegexpOne(`"+s.TraceIDColumn+"`,") {
		t.Errorf("canonical search projection must wrap TraceId in replaceRegexpOne; got:\n%s", sql)
	}
}

// TestTraceByIDProjections_StripsAllIDColumns covers the
// /api/traces/{id} response path. TraceId, SpanId, and ParentSpanId
// all need the leading-zero strip to match Tempo's response shape.
func TestTraceByIDProjections_StripsAllIDColumns(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	projs := traceByIDProjections(s)

	plan := &chplan.Project{
		Input:       &chplan.Scan{Table: s.SpansTable},
		Projections: projs,
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	for _, col := range []string{s.TraceIDColumn, s.SpanIDColumn, s.ParentSpanIDColumn} {
		needle := "replaceRegexpOne(`" + col + "`,"
		if !strings.Contains(sql, needle) {
			t.Errorf("trace-by-id projection must wrap %s in replaceRegexpOne; SQL:\n%s", col, sql)
		}
	}
}

// TestLowerTraceByID_AcceptsBothPaddedAndStrippedForms pins the
// /api/traces/{id} input-parse contract: cerberus must accept both
// the leading-zero-stripped form Tempo emits on the wire AND the
// already-padded 32-char form. Both produce an identical WHERE
// predicate so the row lookup hits the column value the OTel-CH
// exporter wrote.
func TestLowerTraceByID_AcceptsBothPaddedAndStrippedForms(t *testing.T) {
	t.Parallel()

	const padded = "00af843259b0a78f5cbe59e11cbaf66b"
	const stripped = "af843259b0a78f5cbe59e11cbaf66b"

	h := &Handler{Schema: schema.DefaultOTelTraces()}

	planA, err := h.lowerTraceByID(padded)
	if err != nil {
		t.Fatalf("padded form: %v", err)
	}
	planB, err := h.lowerTraceByID(stripped)
	if err != nil {
		t.Fatalf("stripped form: %v", err)
	}

	sqlA, argsA, err := chsql.Emit(context.Background(), planA)
	if err != nil {
		t.Fatalf("emit padded: %v", err)
	}
	sqlB, argsB, err := chsql.Emit(context.Background(), planB)
	if err != nil {
		t.Fatalf("emit stripped: %v", err)
	}
	if sqlA != sqlB {
		t.Errorf("SQL must be identical for stripped vs padded inputs;\npadded:  %s\nstripped: %s", sqlA, sqlB)
	}
	if len(argsA) != len(argsB) {
		t.Fatalf("arg count differs: padded=%d stripped=%d", len(argsA), len(argsB))
	}
	for i, a := range argsA {
		if argsB[i] != a {
			t.Errorf("arg %d differs: padded=%v stripped=%v", i, a, argsB[i])
		}
	}
	// The argument the WHERE predicate binds against MUST be the 32-char
	// canonical form (so the comparison hits the column value the
	// OTel-CH exporter wrote via `hex.EncodeToString`).
	foundCanonical := false
	for _, a := range argsA {
		if s, ok := a.(string); ok && s == padded {
			foundCanonical = true
			break
		}
	}
	if !foundCanonical {
		t.Errorf("expected canonical 32-char trace id %q in args; got %v", padded, argsA)
	}
}

// TestLowerTraceByID_AllZeroTraceID pins the corner case: an all-zero
// TraceID round-trips through normaliseTraceID to the 32-char zero
// string, matching the column value.
func TestLowerTraceByID_AllZeroTraceID(t *testing.T) {
	t.Parallel()

	h := &Handler{Schema: schema.DefaultOTelTraces()}
	plan, err := h.lowerTraceByID("0")
	if err != nil {
		t.Fatalf("lowerTraceByID: %v", err)
	}
	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	want := strings.Repeat("0", 32)
	found := false
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected all-zero trace id %q in args; got %v", want, args)
	}
}
