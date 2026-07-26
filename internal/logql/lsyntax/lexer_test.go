package lsyntax

import (
	"strings"
	"testing"
	"time"
)

// See parser_test.go's file comment: internal/logql/lsyntax has no
// default-tag unit tests of its own lexer, so these boundary-exact tests
// close the mutation-coverage gap gremlins' phase4-logql-lsyntax leg
// surfaced (65.4% efficacy, below the repo's 95% bar).

// --- byte/rune classification boundaries -----------------------------------

func TestIsDigit(t *testing.T) {
	for _, b := range []byte{'0', '9'} {
		if !isDigit(b) {
			t.Errorf("isDigit(%q) = false, want true", b)
		}
	}
	for _, b := range []byte{'/', ':'} { // one below '0', one above '9'
		if isDigit(b) {
			t.Errorf("isDigit(%q) = true, want false", b)
		}
	}
}

func TestIsHexDigit(t *testing.T) {
	for _, b := range []byte{'0', '9', 'a', 'f', 'A', 'F'} {
		if !isHexDigit(b) {
			t.Errorf("isHexDigit(%q) = false, want true", b)
		}
	}
	// One below/above each range: '`'/'g' bracket 'a'-'f' extended to
	// 'a'-'z' isn't accepted; '@'/'G' bracket 'A'-'F'.
	for _, b := range []byte{'`', 'g', '@', 'G'} {
		if isHexDigit(b) {
			t.Errorf("isHexDigit(%q) = true, want false", b)
		}
	}
}

func TestIsLetter(t *testing.T) {
	for _, b := range []byte{'a', 'z', 'A', 'Z', 'm', 'M'} {
		if !isLetter(b) {
			t.Errorf("isLetter(%q) = false, want true", b)
		}
	}
	// One below 'a', one above 'z', one below 'A', one above 'Z'.
	for _, b := range []byte{'`', '{', '@', '['} {
		if isLetter(b) {
			t.Errorf("isLetter(%q) = true, want false", b)
		}
	}
}

func TestIsIdentStart(t *testing.T) {
	if !isIdentStart('_') {
		t.Error("isIdentStart('_') = false, want true")
	}
	if !isIdentStart('a') {
		t.Error("isIdentStart('a') = false, want true")
	}
	if isIdentStart('0') {
		t.Error("isIdentStart('0') = true, want false (digits can't start an identifier)")
	}
	if isIdentStart('-') {
		t.Error("isIdentStart('-') = true, want false")
	}
}

// --- isFunction: whitespace-skipping + by/without keyword detection -------

func TestLexerIsFunction(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"direct paren", "(", true},
		{"whitespace then paren", "  (", true},
		{"bare identifier", "x", false},
		{"all whitespace, no paren", "   ", false},
		{"by immediately followed by paren", "by(x)", true},
		{"by, single space, paren", "by (x)", true},
		{"by, multiple spaces, paren", "by  (x)", true},
		{"by is a prefix of a longer identifier", "bytes", false},
		{"by is a prefix, followed directly by paren", "byte(", false},
		{"by at the exact end of input", "by", false},
		{"without immediately followed by paren", "without(x)", true},
		{"without, multiple spaces, paren", "without  (x)", true},
		{"without followed by another identifier", "without foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &lexer{src: tc.src, pos: 0}
			if got := l.isFunction(); got != tc.want {
				t.Errorf("isFunction(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// --- scanRange / scanString: unterminated-input error paths ---------------

func TestLexScanRangeUnterminated(t *testing.T) {
	_, err := lex("[5m")
	if err == nil {
		t.Fatal("expected an error for an unterminated range")
	}
	if !strings.Contains(err.Error(), "missing closing ']'") {
		t.Errorf("expected a missing-closing-bracket error, got: %v", err)
	}
}

func TestLexScanRangeTerminated(t *testing.T) {
	toks, err := lex("[5m]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toks) != 2 || toks[0].kind != tkRange {
		t.Fatalf("lex(\"[5m]\") = %+v, want a single tkRange token + EOF", toks)
	}
}

func TestLexScanStringUnterminated(t *testing.T) {
	cases := []string{`"abc`, "`abc"}
	for _, src := range cases {
		_, err := lex(src)
		if err == nil {
			t.Fatalf("lex(%q): expected an error for an unterminated string literal", src)
		}
		if !strings.Contains(err.Error(), "literal not terminated") {
			t.Errorf("lex(%q): expected a not-terminated error, got: %v", src, err)
		}
	}
}

func TestLexScanStringTerminated(t *testing.T) {
	cases := []string{`"abc"`, "`abc`"}
	for _, src := range cases {
		toks, err := lex(src)
		if err != nil {
			t.Fatalf("lex(%q): unexpected error: %v", src, err)
		}
		if len(toks) != 2 || toks[0].kind != tkString {
			t.Fatalf("lex(%q) = %+v, want a single tkString token + EOF", src, toks)
		}
	}
}

// --- scanFrom1: two-character operators with a one-character fallback -----

func TestLexScanFrom1(t *testing.T) {
	cases := []struct {
		src      string
		wantKind tokenKind
	}{
		{">=", tkGte},
		{">", tkGt},
		{"<=", tkLte},
		{"<", tkLt},
	}
	for _, tc := range cases {
		toks, err := lex(tc.src)
		if err != nil {
			t.Fatalf("lex(%q): unexpected error: %v", tc.src, err)
		}
		if len(toks) != 2 || toks[0].kind != tc.wantKind {
			t.Errorf("lex(%q) = %+v, want a single token of kind %v + EOF", tc.src, toks, tc.wantKind)
		}
	}
}

// --- scanMinus: `--flag`, negative durations, and plain subtraction -------

func TestLexScanMinus(t *testing.T) {
	t.Run("recognized function flags", func(t *testing.T) {
		for _, src := range []string{"--strict", "--keep-empty"} {
			toks, err := lex(src)
			if err != nil {
				t.Fatalf("lex(%q): unexpected error: %v", src, err)
			}
			if len(toks) != 2 || toks[0].kind != tkFunctionFlag || toks[0].str != src {
				t.Errorf("lex(%q) = %+v, want a single tkFunctionFlag(%q) + EOF", src, toks, src)
			}
		}
	})

	t.Run("unrecognized double-dash falls back to two minus tokens", func(t *testing.T) {
		toks, err := lex("--bogus")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(toks) < 2 || toks[0].kind != tkSub || toks[1].kind != tkSub {
			t.Fatalf("lex(\"--bogus\") = %+v, want two leading tkSub tokens", toks)
		}
	})

	t.Run("negative duration, single unit", func(t *testing.T) {
		toks, err := lex("-5m")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(toks) != 2 || toks[0].kind != tkDuration || toks[0].dur != -5*time.Minute {
			t.Errorf("lex(\"-5m\") = %+v, want a single tkDuration(-5m) + EOF", toks)
		}
	})

	t.Run("negative duration, fractional unit", func(t *testing.T) {
		toks, err := lex("-5.5h")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := -time.Duration(5.5 * float64(time.Hour))
		if len(toks) != 2 || toks[0].kind != tkDuration || toks[0].dur != want {
			t.Errorf("lex(\"-5.5h\") = %+v, want a single tkDuration(%v) + EOF", toks, want)
		}
	})

	t.Run("minus followed by a digit but no valid unit falls back to tkSub", func(t *testing.T) {
		toks, err := lex("-5x")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(toks) < 3 || toks[0].kind != tkSub || toks[1].kind != tkNumber || toks[1].str != "5" {
			t.Fatalf("lex(\"-5x\") = %+v, want tkSub, tkNumber(5), ...", toks)
		}
	})
}

// --- scanNumber: hex literals, decimals, and exponents ---------------------

func TestLexScanNumber(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"0x1A", "0x1A"},
		{"0X1A", "0X1A"},
		{"0", "0"},
		{"01", "01"},
		{"1e5", "1e5"},
		{"1E-3", "1E-3"},
		{"5.25", "5.25"},
	}
	for _, tc := range cases {
		toks, err := lex(tc.src)
		if err != nil {
			t.Fatalf("lex(%q): unexpected error: %v", tc.src, err)
		}
		if len(toks) != 2 || toks[0].kind != tkNumber || toks[0].str != tc.want {
			t.Errorf("lex(%q) = %+v, want a single tkNumber(%q) + EOF", tc.src, toks, tc.want)
		}
	}
}

// --- scanHexRun: hex-digit run starting at an arbitrary offset -------------

func TestLexerScanHexRun(t *testing.T) {
	// The starting offset is NOT zero, so a mutation that discards the
	// input `j` and hard-codes the post-prefix offset (e.g. `j = 2`
	// instead of `j += 2`) is only detectable when the run doesn't start
	// at position 0. "0x" sits at [2:4); the hex digits "1A" follow.
	l := &lexer{src: ".." + "0x1A" + "z"}
	got := l.scanHexRun(2)
	const want = 6 // just past "0x1A", before 'z'
	if got != want {
		t.Errorf("scanHexRun(2) = %d, want %d", got, want)
	}
}

// --- scanExponent: `e`/`E` exponent boundary handling ----------------------

func TestLexScanExponentBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantKinds  []tokenKind
		wantFirst  string
		commentary string
	}{
		{
			name:      "bare 'e' at end of input is not an exponent",
			src:       "1e",
			wantKinds: []tokenKind{tkNumber, tkIdentifier, tkEOF},
			wantFirst: "1",
		},
		{
			name:      "'e' with a sign but no digits, at end of input",
			src:       "1e+",
			wantKinds: []tokenKind{tkNumber, tkIdentifier, tkAdd, tkEOF},
			wantFirst: "1",
		},
		{
			name:      "'e' with a sign followed by a non-digit",
			src:       "1e+x",
			wantKinds: []tokenKind{tkNumber, tkIdentifier, tkAdd, tkIdentifier, tkEOF},
			wantFirst: "1",
		},
		{
			name:      "a genuine exponent is consumed whole",
			src:       "1e5",
			wantKinds: []tokenKind{tkNumber, tkEOF},
			wantFirst: "1e5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := lex(tc.src)
			if err != nil {
				t.Fatalf("lex(%q): unexpected error: %v", tc.src, err)
			}
			if len(toks) != len(tc.wantKinds) {
				t.Fatalf("lex(%q) = %+v, want %d tokens of kind %v", tc.src, toks, len(tc.wantKinds), tc.wantKinds)
			}
			for i, k := range tc.wantKinds {
				if toks[i].kind != k {
					t.Errorf("lex(%q): token[%d].kind = %v, want %v", tc.src, i, toks[i].kind, k)
				}
			}
			if toks[0].str != tc.wantFirst {
				t.Errorf("lex(%q): token[0].str = %q, want %q", tc.src, toks[0].str, tc.wantFirst)
			}
		})
	}
}

// --- trySuffix: multi-component durations can embed digits/dots in the ----
// --- middle of the unit run (e.g. "1h30m", "1h30.5m") ----------------------

func TestLexDurationSuffixMultiComponent(t *testing.T) {
	cases := []struct {
		src  string
		want time.Duration
	}{
		{"1h30m", time.Hour + 30*time.Minute},
		{"1h30.5m", time.Hour + 30*time.Minute + 30*time.Second},
	}
	for _, tc := range cases {
		toks, err := lex(tc.src)
		if err != nil {
			t.Fatalf("lex(%q): unexpected error: %v", tc.src, err)
		}
		if len(toks) != 2 || toks[0].kind != tkDuration || toks[0].dur != tc.want {
			t.Errorf("lex(%q) = %+v, want a single tkDuration(%v) + EOF", tc.src, toks, tc.want)
		}
	}
}

// --- tryBytesSuffix: an embedded '.' inside the suffix scan (white-box) ---

func TestLexerTryBytesSuffixEmbeddedDot(t *testing.T) {
	l := &lexer{src: "1.5MB"}
	v, consumed, ok := l.tryBytesSuffix("0")
	if !ok {
		t.Fatal("tryBytesSuffix: expected ok=true")
	}
	if consumed != len(l.src) {
		t.Errorf("consumed = %d, want %d (the whole suffix run)", consumed, len(l.src))
	}
	const want = 1500000 // "01.5MB" parsed as decimal megabytes
	if v != want {
		t.Errorf("value = %d, want %d", v, want)
	}
}

// --- trySuffix / tryBytesSuffix: duration and byte-size unit scanning -----

func TestLexDurationSuffix(t *testing.T) {
	cases := []struct {
		src  string
		want time.Duration
	}{
		// ASCII duration unit.
		{"5m", 5 * time.Minute},
		// Multi-byte (UTF-8) duration unit: exercises the >= utf8.RuneSelf
		// branch of trySuffix's scan loop.
		{"5µs", 5 * time.Microsecond},
	}
	for _, tc := range cases {
		toks, err := lex(tc.src)
		if err != nil {
			t.Fatalf("lex(%q): unexpected error: %v", tc.src, err)
		}
		if len(toks) != 2 || toks[0].kind != tkDuration || toks[0].dur != tc.want {
			t.Errorf("lex(%q) = %+v, want a single tkDuration(%v) + EOF", tc.src, toks, tc.want)
		}
	}
}

func TestLexBytesSuffix(t *testing.T) {
	cases := []struct {
		src  string
		want uint64
	}{
		{"5KB", 5000},
		{"1.5GiB", 1610612736},
		{"10Mi", 10485760},
	}
	for _, tc := range cases {
		toks, err := lex(tc.src)
		if err != nil {
			t.Fatalf("lex(%q): unexpected error: %v", tc.src, err)
		}
		if len(toks) != 2 || toks[0].kind != tkBytes || toks[0].bytes != tc.want {
			t.Errorf("lex(%q) = %+v, want a single tkBytes(%d) + EOF", tc.src, toks, tc.want)
		}
	}
}

// --- lineCol: byte-offset-to-line/col conversion for error reporting ------

func TestLexerLineCol(t *testing.T) {
	const src = "ab\ncd"
	cases := []struct {
		name     string
		pos      int
		wantLine int
		wantCol  int
	}{
		// Exactly at the newline: the newline itself is NOT yet counted.
		{"before the newline", 2, 1, 3},
		// One past: the newline IS counted, resetting the column.
		{"just after the newline", 3, 2, 1},
		// Far past the end of the source: must clamp to scanning the
		// whole string exactly once, not read out of bounds.
		{"past end of input", 100, 2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &lexer{src: src, pos: tc.pos}
			line, col := l.lineCol()
			if line != tc.wantLine || col != tc.wantCol {
				t.Errorf("lineCol() at pos=%d = (%d,%d), want (%d,%d)", tc.pos, line, col, tc.wantLine, tc.wantCol)
			}
		})
	}
}

// --- peekByte / peekByteAt: bounds-safe lookahead --------------------------

func TestLexerPeekByte(t *testing.T) {
	l := &lexer{src: "a", pos: 0}
	if got := l.peekByte(); got != 'a' {
		t.Errorf("peekByte() at pos=0 = %q, want 'a'", got)
	}
	l.pos = 1 // exactly at len(src)
	if got := l.peekByte(); got != 0 {
		t.Errorf("peekByte() at pos=len(src) = %q, want 0", got)
	}
}

func TestLexerPeekByteAt(t *testing.T) {
	l := &lexer{src: "ab", pos: 1}
	if got := l.peekByteAt(0); got != 'b' {
		t.Errorf("peekByteAt(0) at pos=1 = %q, want 'b'", got)
	}
	// pos+off == len(src) exactly: must clamp to 0, not read (and not
	// read l.src[pos-off] either, which an ARITHMETIC_BASE mutant on
	// `pos+off` would wrongly do).
	if got := l.peekByteAt(1); got != 0 {
		t.Errorf("peekByteAt(1) at pos=1 (== len(src)) = %q, want 0", got)
	}
}

// --- scanIdent: identifier running to end of input -------------------------

func TestLexScanIdentToEOF(t *testing.T) {
	toks, err := lex("abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toks) != 2 || toks[0].kind != tkIdentifier || toks[0].str != "abc" {
		t.Fatalf("lex(\"abc\") = %+v, want a single tkIdentifier(\"abc\") + EOF", toks)
	}
	if toks[1].kind != tkEOF {
		t.Errorf("final token kind = %v, want tkEOF", toks[1].kind)
	}
}
