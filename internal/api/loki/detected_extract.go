package loki

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/buger/jsonparser"
)

// This file is the clean-room reimplementation of Loki's detected-fields
// line extraction (pkg/logql/log JSONParser + LogfmtParser + the
// LabelsBuilder collision handling), so the binary stays Apache-only. It
// reproduces upstream byte-for-byte: the JSON parser flattens nested
// objects with `_`, sanitises keys, suffixes `_extracted` on collisions
// with the stream labels, and records the original JSON path for nested
// keys; the logfmt parser extracts key/value pairs with the same key
// sanitisation and collision rules. The buger/jsonparser library used
// for JSON walking is the very one upstream uses (MIT).

// jsonSpacer joins nested JSON key segments, matching upstream.
const jsonSpacer = '_'

// parseLine runs the json-then-logfmt cascade over a log body, seeded
// with the row's stream labels (so extracted keys that shadow a stream
// label rename to `<key>_extracted`). It returns the extracted
// key→value map (error labels are never produced here), the
// single-parser list that produced it, and — for json — the original
// JSON path per key. Returns (nil, nil, nil) when neither parser yields
// a field.
//
// Cascade semantics match upstream parseEntry: JSON is tried first; if
// the line is a valid JSON object the JSON result is used (even if it
// yields no fields, in which case the line contributes nothing — logfmt
// is NOT tried); only a JSON parse error falls through to logfmt.
func parseLine(line string, streamLabels map[string]string) (map[string]string, []string, map[string][]string) {
	je := &jsonExtractor{
		out:    map[string]string{},
		paths:  map[string][]string{},
		stream: streamLabels,
	}
	if err := jsonparser.ObjectEach([]byte(line), je.object); err == nil {
		if len(je.out) == 0 {
			return nil, nil, nil
		}
		return je.out, []string{"json"}, je.paths
	}

	lf := extractLogfmt([]byte(line), streamLabels)
	if len(lf) == 0 {
		return nil, nil, nil
	}
	return lf, []string{"logfmt"}, nil
}

// jsonExtractor walks a JSON object accumulating flattened key/value
// pairs (and their paths) the way Loki's JSONParser does.
type jsonExtractor struct {
	prefix [][]byte
	out    map[string]string
	paths  map[string][]string
	stream map[string]string
}

func (j *jsonExtractor) object(key, value []byte, dataType jsonparser.ValueType, _ int) error {
	switch dataType {
	case jsonparser.String, jsonparser.Number, jsonparser.Boolean:
		j.labelValue(key, value, dataType)
	case jsonparser.Object:
		prefixLen := len(j.prefix)
		j.prefix = append(j.prefix, key)
		_ = jsonparser.ObjectEach(value, j.object)
		j.prefix = j.prefix[:prefixLen]
	case jsonparser.Array, jsonparser.Null, jsonparser.NotExist, jsonparser.Unknown:
		// Arrays, null, and unknown values are not extracted (upstream's
		// parseObject only handles String / Number / Boolean / Object).
	}
	return nil
}

func (j *jsonExtractor) labelValue(key, value []byte, dataType jsonparser.ValueType) {
	if len(j.prefix) == 0 {
		// Top-level scalar: sanitise the bare key. The JSON path is the
		// single RAW key (matching upstream, which records the unsanitised
		// key under the sanitised label name).
		sk := sanitizeLabelKey(string(key), true)
		if sk == "" {
			return
		}
		if _, collides := j.stream[sk]; collides {
			sk += duplicateSuffix
		}
		j.out[sk] = readJSONValue(value, dataType)
		j.paths[sk] = []string{string(key)}
		return
	}

	// Nested scalar: build the `parent_child` key from the prefix buffer.
	prefixLen := len(j.prefix)
	j.prefix = append(j.prefix, key)
	keyString := string(buildSanitizedPrefix(j.prefix))
	if _, collides := j.stream[keyString]; collides {
		leaf := make([]byte, 0, len(key)+len(duplicateSuffix))
		leaf = append(leaf, key...)
		leaf = append(leaf, duplicateSuffix...)
		j.prefix[prefixLen] = leaf
		keyString = string(buildSanitizedPrefix(j.prefix))
	}
	if path := buildJSONPath(j.prefix); len(path) > 0 {
		j.paths[keyString] = path
	}
	j.prefix = j.prefix[:prefixLen]
	j.out[keyString] = readJSONValue(value, dataType)
}

// extractLogfmt extracts key/value pairs from a logfmt line with Loki's
// non-strict semantics: malformed pairs are skipped, keys are sanitised,
// keys colliding with a stream label get `_extracted`, and empty values
// are dropped.
func extractLogfmt(line []byte, stream map[string]string) map[string]string {
	out := map[string]string{}
	dec := newLogfmtDecoder(line)
	for !dec.eol() {
		if !dec.scanKeyval() {
			continue
		}
		key := sanitizeLabelKey(string(dec.Key()), true)
		if key == "" {
			continue
		}
		if _, collides := stream[key]; collides {
			key += duplicateSuffix
		}
		val := removeInvalidUTF(dec.Value())
		if len(val) == 0 {
			continue
		}
		out[key] = string(val)
	}
	return out
}

// readJSONValue renders a JSON scalar the way Loki's readValue does.
func readJSONValue(v []byte, dataType jsonparser.ValueType) string {
	switch dataType {
	case jsonparser.String:
		return unescapeJSONString(v)
	case jsonparser.Null:
		return ""
	case jsonparser.Number:
		return string(v)
	case jsonparser.Boolean:
		if bytes.Equal(v, []byte("true")) {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// unescapeStackBufSize matches upstream's allocation-free unescape buffer.
const unescapeStackBufSize = 64

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
// trims surrounding space, and (for the prefix form) prepends `_` when
// the key starts with a digit. Matches upstream sanitizeLabelKey.
func sanitizeLabelKey(key string, isPrefix bool) string {
	key = strings.TrimSpace(key)
	if len(key) == 0 {
		return key
	}
	if isPrefix && key[0] >= '0' && key[0] <= '9' {
		key = "_" + key
	}
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, key)
}

// buildSanitizedPrefix joins the prefix buffer parts with `_`, sanitising
// each part and skipping empty ones, matching upstream
// buildSanitizedPrefixFromBuffer.
func buildSanitizedPrefix(prefix [][]byte) []byte {
	out := make([]byte, 0, 64)
	for i, part := range prefix {
		if len(bytes.TrimSpace(part)) == 0 {
			continue
		}
		if i > 0 && len(out) > 0 {
			out = append(out, byte(jsonSpacer))
		}
		out = appendSanitized(out, part)
	}
	return out
}

// appendSanitized appends key to to, replacing invalid bytes with `_`
// and prepending `_` when the result would otherwise start with a digit.
func appendSanitized(to, key []byte) []byte {
	key = bytes.TrimSpace(key)
	if len(key) == 0 {
		return to
	}
	if len(to) == 0 && key[0] >= '0' && key[0] <= '9' {
		to = append(to, '_')
	}
	for _, r := range string(key) {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '_' && (r < '0' || r > '9') {
			to = append(to, '_')
			continue
		}
		// r is provably ASCII (a-z, A-Z, 0-9, _) on this branch.
		to = append(to, byte(r)) //nolint:gosec // r is ASCII per the guard above
	}
	return to
}

// buildJSONPath returns the raw key segments of the prefix buffer, with
// any `_extracted` collision suffix stripped, matching upstream.
func buildJSONPath(prefix [][]byte) []string {
	if len(prefix) == 0 {
		return nil
	}
	path := make([]string, 0, len(prefix))
	for _, part := range prefix {
		path = append(path, strings.TrimSuffix(string(part), duplicateSuffix))
	}
	return path
}
