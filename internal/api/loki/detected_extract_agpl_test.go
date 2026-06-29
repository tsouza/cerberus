//go:build agpl_oracle

// A/B oracle: the in-house detected-fields line extractor (parseLine in
// detected_extract.go) must agree with upstream Loki's JSONParser /
// LogfmtParser + LabelsBuilder on the extracted key→value map, the
// parser that produced it, and the per-key JSON paths.
//
// This is the ONLY detected-fields test that imports the AGPL
// pkg/logql/log package, gated behind the `agpl_oracle` build tag. Run:
//
//	CGO_ENABLED=1 go test -tags agpl_oracle ./internal/api/loki/
package loki

import (
	"reflect"
	"testing"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/prometheus/prometheus/model/labels"
)

// lokiParseLine reproduces the upstream cascade cerberus used to call:
// JSON first (with path capture), logfmt fallback, error labels dropped.
func lokiParseLine(line string, stream map[string]string) (map[string]string, string, map[string][]string) {
	streamLbls := labels.FromMap(stream)
	lbs := loglib.NewBaseLabelsBuilder().ForLabels(streamLbls, labels.StableHash(streamLbls))

	parser := "json"
	jp := loglib.NewJSONParser(true)
	_, jsonOK := jp.Process(0, []byte(line), lbs)
	if !jsonOK || lbs.HasErr() {
		lbs.Reset()
		parser = "logfmt"
		lp := loglib.NewLogfmtParser(false, false)
		_, lfOK := lp.Process(0, []byte(line), lbs)
		if !lfOK || lbs.HasErr() {
			return nil, "", nil
		}
	}

	out := map[string]string{}
	paths := map[string][]string{}
	lbs.LabelsResult().Parsed().Range(func(l labels.Label) {
		switch l.Name {
		case logqlmodel.ErrorLabel, logqlmodel.ErrorDetailsLabel, logqlmodel.PreserveErrorLabel:
			return
		}
		out[l.Name] = l.Value
		if p := lbs.GetJSONPath(l.Name); len(p) > 0 {
			paths[l.Name] = p
		}
	})
	if len(out) == 0 {
		return nil, "", nil
	}
	return out, parser, paths
}

func TestDetectedExtract_MatchesLoki(t *testing.T) {
	stream := map[string]string{"job": "api", "status": "stream-wins"}
	corpus := []string{
		// JSON — flat, nested, arrays, numbers/bools/null, collisions.
		`{"status":"ok","path":"/x","code":200}`,
		`{"a":{"b":{"c":"deep"}},"flat":"v"}`,
		`{"level":"info","ok":true,"missing":null,"n":3.14}`,
		`{"arr":[1,2,3],"keep":"yes"}`,
		`{"status":"shadow","other":"v"}`, // status collides with stream label
		`{"weird key!":"v","ok":"y"}`,     // key sanitisation
		`{"a":{"status":"nested"}}`,       // nested collision (a_status)
		`{}`,                              // empty object -> no fields
		`{"123num":"v"}`,                  // digit-leading key
		// logfmt fallback.
		`level=info msg="hello world" code=200`,
		`status=lf-shadow other=v`, // status collides
		`a=1 b= c=3`,               // empty value dropped
		`key="quoted \"escaped\"" x=1`,
		`garbage without equals here`,
		`malformed="unterminated key2=val2`,
		// neither parser yields anything.
		``,
		`   `,
		`just plain prose text`,
	}

	for _, line := range corpus {
		line := line
		t.Run(line, func(t *testing.T) {
			wantFields, wantParser, wantPaths := lokiParseLine(line, stream)
			gotFields, gotParsers, gotPaths := parseLine(line, stream)

			var gotParser string
			if len(gotParsers) > 0 {
				gotParser = gotParsers[0]
			}
			if gotParser != wantParser {
				t.Fatalf("parser mismatch for %q: loki=%q in-house=%q", line, wantParser, gotParser)
			}
			if !reflect.DeepEqual(normMap(wantFields), normMap(gotFields)) {
				t.Fatalf("fields mismatch for %q:\n loki=%#v\n in-house=%#v", line, wantFields, gotFields)
			}
			// Compare json paths only for the keys that survived (loki and
			// in-house both omit top-level/no-path keys identically).
			if wantParser == "json" {
				if !reflect.DeepEqual(normPaths(wantPaths), normPaths(gotPaths)) {
					t.Fatalf("json-path mismatch for %q:\n loki=%#v\n in-house=%#v", line, wantPaths, gotPaths)
				}
			}
		})
	}
}

func normMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func normPaths(m map[string][]string) map[string][]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
