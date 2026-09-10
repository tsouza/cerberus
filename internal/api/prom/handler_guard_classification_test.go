package prom_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestGuardClassification_ExceptionMessageBoundary(t *testing.T) {
	t.Parallel()
	const (
		unknownIdentifierCode = 47
		throwIfCode           = 395
		duplicateSeriesCode   = 768
		trailer               = ": while executing 'FUNCTION throwIf(...)'"
	)
	guard := chsql.RateWindowFanoutBudgetMessage
	duplicate := chplan.DuplicateSeriesTagsMessagePrefix + " {job=api}, duplicate series in the same result set are not allowed"
	typed := func(code int32, msg string) error {
		return &clickhouse.Exception{Code: code, Message: msg}
	}
	cases := []struct {
		name        string
		err         error
		wantMessage string
	}{
		{"native_guard", typed(throwIfCode, guard+trailer), guard},
		{"wrapped_guard", &chclient.ThrowIfError{Message: guard + trailer, Cause: typed(throwIfCode, guard+trailer)}, guard},
		{"standalone_guard_wrapper", &chclient.ThrowIfError{Message: guard + trailer}, guard},
		{"columnar_guard", typed(throwIfCode, "Code: 395. DB::Exception: "+guard+trailer), guard},
		{"text_guard", errors.New("Code: 395. DB::Exception: " + guard + trailer), guard},
		{"stream_chunk_guard", errors.New("error in chunk: Code: 395. DB::Exception: " + guard + trailer), guard},
		{"native_duplicate", typed(duplicateSeriesCode, duplicate+trailer), duplicate},
		{"columnar_duplicate", typed(duplicateSeriesCode, "Code: 768. DB::Exception: "+duplicate+trailer), duplicate},
		{"text_duplicate", errors.New("Code: 768. DB::Exception: " + duplicate + trailer), duplicate},
		{"typed_unknown_identifier", typed(unknownIdentifierCode, "Unknown identifier in SELECT throwIf(1, '"+guard+"')"), ""},
		{"text_unknown_identifier", errors.New("Code: 47. DB::Exception: Unknown identifier in SELECT throwIf(1, '" + guard + "')"), ""},
		{"stream_chunk_unknown_identifier", errors.New("error in chunk: Code: 47. DB::Exception: SELECT '" + guard + "'"), ""},
		{"typed_non_guard_overrides_wrapper", &chclient.ThrowIfError{Message: guard, Cause: typed(unknownIdentifierCode, "Code: 395. DB::Exception: "+guard)}, ""},
		{"typed_unknown_duplicate", typed(unknownIdentifierCode, "Unknown identifier in SELECT '"+duplicate+"'"), ""},
		{"text_unknown_duplicate", errors.New("Code: 47. DB::Exception: Unknown identifier in SELECT '" + duplicate + "'"), ""},
		{"embedded_fake_envelope", errors.New("Code: 47. DB::Exception: SELECT 'Code: 395. DB::Exception: " + guard + "'"), ""},
		{"ambient_guard", errors.New("failed SELECT throwIf(1, '" + guard + "')"), ""},
		{"other_guard_with_sql_literal", typed(throwIfCode, "unrelated guard"+trailer+guard), ""},
		{"other_duplicate_with_sql_literal", typed(duplicateSeriesCode, "unrelated rejection"+trailer+duplicate), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, cursor := range []bool{false, true} {
				t.Run(fmt.Sprintf("cursor=%t", cursor), func(t *testing.T) {
					wrapped := fmt.Errorf("chclienttest: query: %w", tc.err)
					querier := &gridNativeGuardQuerier{guardErr: wrapped}
					srv := newServer(querier)
					path := "/api/v1/query_range?query=up&start=1767225600&end=1767225660&step=60"
					if !cursor {
						querier.stubQuerier.err = wrapped
						path = "/api/v1/query?query=up&time=1767225600"
					}
					t.Cleanup(srv.Close)
					resp, err := http.Get(srv.URL + path)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					var body queryResponse
					if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					wantStatus, wantKind := http.StatusBadGateway, "internal"
					if tc.wantMessage != "" {
						wantStatus, wantKind = http.StatusUnprocessableEntity, "execution"
					}
					if resp.StatusCode != wantStatus || body.ErrorType != wantKind {
						t.Fatalf("status=%d kind=%q message=%q; want %d %s", resp.StatusCode, body.ErrorType, body.Error, wantStatus, wantKind)
					}
					if body.Status != "error" {
						t.Fatalf("response status=%q, want error", body.Status)
					}
					if tc.wantMessage != "" && body.Error != tc.wantMessage {
						t.Fatalf("message=%q want%q", body.Error, tc.wantMessage)
					}
				})
			}
		})
	}
}
