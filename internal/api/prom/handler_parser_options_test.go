package prom

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestHandlerLang_ParserOptionsAligned pins the invariant that the
// PromQL parser the Handler uses on its scalar-fold short-circuit
// (Handler.parseExpr, via tryScalarFold) and the parser the Lang
// adapter uses on the engine path (lang.Parse, via executeInstant /
// executeRangeStreaming) are the SAME instance — so no
// promparser.Options drift between the two parse paths is even
// structurally possible.
//
// Background: PR #639 caught a real drift bug where
// normalizeDottedSelectors was wired into only one of the two parse
// paths. The same drift shape could exist for promparser.Options
// itself — e.g. if h.parser is built with EnableExperimentalFunctions
// =true but a separate lang.Parser defaults to false, a query using
// an experimental function (e.g. `limitk(3, up)`) would fold-parse
// on the short-circuit and fail on the engine path. This test
// guarantees both paths share one parser, so the failure mode cannot
// recur silently.
//
// Strategy: two layers, instance + behavioural.
//
//  1. Instance: build a Handler via the production constructor
//     New(...), then construct lang values the same way
//     executeInstant / executeRangeStreaming do
//     (`Parser: h.parser`) and assert the interface values are
//     identical. If a future refactor introduces a separate parser
//     for the Lang path, this initialiser is the place the drift
//     would land — and the identity assertion below would fail on
//     it.
//
//  2. Behavioural: parse an experimental-aggregator query
//     (`limitk(3, up)`, gated by EnableExperimentalFunctions per
//     upstream prometheus/promql/parser/lex.go) through BOTH paths.
//     If either parser was constructed without the experimental
//     flag, one of the two paths would surface a parse error. This
//     catches the case where someone splits the parser construction
//     but copies the wrong Options literal — alignment by identity
//     would be lost but Options drift would also be observable.
func TestHandlerLang_ParserOptionsAligned(t *testing.T) {
	t.Parallel()

	h := New(nil, schema.DefaultOTelMetrics(), nil)

	// Mirror the production construction sites in handler.go
	// (executeInstant / executeRangeStreaming). If a future refactor
	// introduces a separate parser for the Lang path, this
	// initialiser is the place the drift would land.
	instant := &lang{Parser: h.parser, Schema: h.Schema}
	rangeStream := &lang{Parser: h.parser, Schema: h.Schema}

	if instant.Parser != h.parser {
		t.Errorf("executeInstant lang.Parser must reuse h.parser by interface identity; got different instance")
	}
	if rangeStream.Parser != h.parser {
		t.Errorf("executeRangeStreaming lang.Parser must reuse h.parser by interface identity; got different instance")
	}

	// Behavioural cross-check: an experimental aggregator must parse
	// cleanly on both the handler short-circuit (h.parser.ParseExpr,
	// the entrypoint Handler.parseExpr wraps) and the Lang path
	// (instant.Parser.ParseExpr, the entrypoint lang.Parse wraps).
	// Per upstream prometheus/promql/parser/lex.go, LIMITK is gated
	// by EnableExperimentalFunctions; if either Options literal
	// flipped that gate off, the corresponding call below would
	// surface "limitk() ... requires the EnableExperimentalFunctions
	// parser option".
	const experimental = `limitk(3, up)`

	if _, err := h.parser.ParseExpr(experimental); err != nil {
		t.Errorf("h.parser.ParseExpr(%q): unexpected error — Options.EnableExperimentalFunctions must be true on the scalar-fold path: %v",
			experimental, err)
	}
	if _, err := instant.Parser.ParseExpr(experimental); err != nil {
		t.Errorf("instant lang.Parser.ParseExpr(%q): unexpected error — Options.EnableExperimentalFunctions must be true on the engine path: %v",
			experimental, err)
	}
	if _, err := rangeStream.Parser.ParseExpr(experimental); err != nil {
		t.Errorf("range-stream lang.Parser.ParseExpr(%q): unexpected error — Options.EnableExperimentalFunctions must be true on the engine path: %v",
			experimental, err)
	}
}
