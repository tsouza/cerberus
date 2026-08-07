package loki

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/buger/jsonparser"
)

// Loki's label-key and label-value sanitisation, reimplemented clean-room
// so the binary stays Apache-only. It reproduces upstream
// (pkg/logql/log's sanitizeLabelKey / readValue) byte-for-byte: every
// byte outside `[A-Za-z0-9_]` becomes `_`, a digit-leading key gains a
// leading `_`, and a value loses any UTF-8 replacement runes.
//
// The consumer is the `| unpack` post-processing path, which unpacks a
// JSON envelope in Go because no ClickHouse expression can hoist an
// arbitrary object into the label set. Everything else that needs a
// parsed label set — the query path's `| json` / `| logfmt` stages and
// the /detected_fields inventory built from them — is evaluated by
// ClickHouse from the SAME chplan expressions, so no second Go-side
// re-derivation of a parsed field exists to drift from the query path.

// unescapeStackBufSize matches upstream's allocation-free unescape buffer.
const unescapeStackBufSize = 64

// unescapeJSONString renders a JSON string scalar the way Loki's
// readValue does: unescape, then strip invalid UTF-8.
func unescapeJSONString(b []byte) string {
	var stackbuf [unescapeStackBufSize]byte
	bU, err := jsonparser.Unescape(b, stackbuf[:])
	if err != nil {
		return ""
	}
	res := string(bU)
	if strings.ContainsRune(res, utf8.RuneError) {
		res = string(removeInvalidUTF([]byte(res)))
	}
	return res
}

// sanitizeLabelKey replaces every non-`[A-Za-z0-9_]` byte with `_`,
// trims surrounding space, and prepends `_` when the key starts with a
// digit. Matches upstream sanitizeLabelKey.
func sanitizeLabelKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) == 0 {
		return key
	}
	if key[0] >= '0' && key[0] <= '9' {
		key = "_" + key
	}
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, key)
}

// removeInvalidUTF maps the UTF-8 replacement rune out of a value, as
// Loki does before storing an extracted value.
func removeInvalidUTF(b []byte) []byte {
	if !bytes.ContainsRune(b, utf8.RuneError) {
		return b
	}
	return bytes.Map(func(r rune) rune {
		if r == utf8.RuneError {
			return -1
		}
		return r
	}, b)
}
