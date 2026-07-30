package routerrules

import (
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/optcorpus"
)

// This file is the enforcement half of the deliberate no-import contract
// documented on CorpusTableName / routeColumn / routeUnclassified in columns.go.
// Production code in this package must not import internal/optcorpus (arch-lint
// forbids it, so the corpus writer and the corpus reader stay decoupled), which
// leaves those three constants re-declared verbatim. A re-declaration nobody
// compares is a comment, not a contract — and this one has already drifted once:
// the writer folded an unclassified route into 'A' while the reader kept reading
// the empty token, so the same corpus produced different findings per backend.
//
// A test file may import optcorpus (.go-arch-lint.yml excludes *_test.go), so
// the comparison lives here and a drift is a compile-or-test failure rather than
// a silently divergent wire format.

func TestCorpusTableNameMatchesWriter(t *testing.T) {
	t.Parallel()
	if CorpusTableName != optcorpus.CorpusTableName {
		t.Errorf("routerrules reads %q but optcorpus writes %q", CorpusTableName, optcorpus.CorpusTableName)
	}
}

func TestRouteUnclassifiedTokenMatchesWriter(t *testing.T) {
	t.Parallel()
	// The token itself, not just its emptiness: the reader's whole
	// "was this row classified?" predicate is an equality against it, so a writer
	// that switched to a word like "none" would make every row look classified
	// and hand every geometry percentile a population of zeroes to fit.
	if routeUnclassified != optcorpus.RouteUnclassified {
		t.Errorf("routerrules reads the unclassified route as %q but optcorpus writes %q",
			routeUnclassified, optcorpus.RouteUnclassified)
	}
}

// TestCorpusColumnsMatchWriterTable pins the reader's column contract against the
// writer's table definition, name for name AND position for position.
// corpusColumns is what the catalog validator checks every ColRef against, so a
// column the writer renames but the reader still lists lets a rule reference a
// column that does not exist on the table (a query-time error, not a load-time
// one), and a column the writer adds but the reader omits is silently unreachable
// from any rule.
//
// The comparison target is deliberately the TABLE, not optcorpus.Row: Row is the
// JSONL wire form and the two shapes differ on purpose (Row carries opts +
// profile_events, which the table does not store; the table carries event_time,
// which the sink stamps per batch). Anchoring to Row would demand a correspondence
// neither side promises.
func TestCorpusColumnsMatchWriterTable(t *testing.T) {
	t.Parallel()

	writer := corpusWriterColumnNames()
	reader := make([]string, 0, len(corpusColumns))
	for _, c := range corpusColumns {
		reader = append(reader, c.name)
	}

	// Position too, not just membership: the ordering is what the JSONL sources
	// project a row into, so a swap between two same-typed columns (say read_rows
	// and read_bytes) reads every value into the wrong column silently.
	if len(writer) != len(reader) {
		t.Fatalf("corpus column count differs: writer has %d %v, routerrules lists %d %v",
			len(writer), writer, len(reader), reader)
	}
	for i := range writer {
		if writer[i] != reader[i] {
			t.Errorf("corpus column %d: writer declares %q, routerrules lists %q", i, writer[i], reader[i])
		}
	}
}

// TestRouteColumnIsAWriterColumn pins the one column name this package spells out
// as its own constant (the classification predicate reads it directly) against the
// writer's table, so a rename cannot leave the predicate pointing at a column that
// no longer exists.
func TestRouteColumnIsAWriterColumn(t *testing.T) {
	t.Parallel()
	if slices.Contains(corpusWriterColumnNames(), routeColumn) {
		return
	}
	t.Errorf("routerrules reads the classification from column %q, which the corpus table does not have", routeColumn)
}

// corpusWriterColumnNames projects the writer's column defs down to names, in
// wire order.
func corpusWriterColumnNames() []string {
	defs := optcorpus.CorpusColumns()
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}
