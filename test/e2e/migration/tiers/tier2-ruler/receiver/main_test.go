package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// post drives one notification through the receiver's real routes.
func post(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /webhook = %d, want 200 — the contact point treats anything else as a delivery failure", rec.Code)
	}
	return rec
}

// The substrate's self-check asserts delivery by reading /notifications
// rather than parsing the log, so the count and the log must not disagree:
// a notification that reached the log but not the count (or the reverse)
// makes the whole tier-2 delivery assertion read the wrong number.
func TestWebhookCapturesToBothTheCountAndTheLog(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	mux := (&sink{log: &logged}).routes()

	post(t, mux, `{"alerts":[{"status":"firing"}]}`)
	post(t, mux, `{"alerts":[{"status":"resolved"}]}`)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/notifications", nil))
	var counted struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &counted); err != nil {
		t.Fatalf("/notifications is not JSON: %v (%s)", err, rec.Body)
	}
	if counted.Count != 2 {
		t.Fatalf("/notifications count = %d, want 2", counted.Count)
	}

	// One line per notification, so a consumer tailing the file sees the
	// same two deliveries the count reports.
	if got := strings.Count(strings.TrimRight(logged.String(), "\n"), "\n") + 1; got != 2 {
		t.Fatalf("the log holds %d lines, want 2:\n%s", got, logged.String())
	}
	if !strings.Contains(logged.String(), `"firing"`) || !strings.Contains(logged.String(), `"resolved"`) {
		t.Fatalf("the log lost a notification body:\n%s", logged.String())
	}
}

// /notifications/list is what MIG-18's fire/resolve diffing parses, so the
// bodies must come back byte-for-byte (this receiver is deliberately
// notifier-schema-agnostic) and each must carry the receipt time the
// payload's own timestamps cannot supply.
func TestNotificationsListReturnsRawBodiesWithReceiptTimes(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	mux := (&sink{log: &logged}).routes()
	const body = `{"alerts":[{"status":"firing","labels":{"alertname":"HighLatency"}}]}`
	before := time.Now().UTC()
	post(t, mux, body)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/notifications/list", nil))
	var listed struct {
		Notifications []capturedNotification `json:"notifications"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("/notifications/list is not JSON: %v (%s)", err, rec.Body)
	}
	if len(listed.Notifications) != 1 {
		t.Fatalf("listed %d notifications, want 1", len(listed.Notifications))
	}
	if got := string(listed.Notifications[0].Body); got != body {
		t.Fatalf("body round-tripped as %s, want it unchanged: %s", got, body)
	}
	if listed.Notifications[0].ReceivedAt.Before(before) {
		t.Fatalf("received_at %v predates the request at %v", listed.Notifications[0].ReceivedAt, before)
	}
}

// The buffer is bounded by dropping the OLDEST entry, so a runaway rule
// group cannot turn this test double into an unbounded memory sink — and,
// just as importantly, the newest notification always survives. Dropping
// the newest instead would keep memory bounded while silently discarding
// exactly the delivery a scenario just asserted.
func TestCapturedBufferDropsTheOldestAtCapacity(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	s := &sink{log: &logged}
	s.captured = make([]capturedNotification, maxCaptured)
	s.captured[0].Body = json.RawMessage(`{"seq":"oldest"}`)
	mux := s.routes()

	post(t, mux, `{"seq":"newest"}`)

	if len(s.captured) != maxCaptured {
		t.Fatalf("the buffer grew to %d, want it held at %d", len(s.captured), maxCaptured)
	}
	if string(s.captured[0].Body) == `{"seq":"oldest"}` {
		t.Fatal("the oldest notification survived at capacity, so the buffer is not bounded")
	}
	if got := string(s.captured[len(s.captured)-1].Body); got != `{"seq":"newest"}` {
		t.Fatalf("the newest notification is %s, want it kept", got)
	}
	// The count is a delivery total, not a buffer length: it must keep
	// climbing after the buffer stops growing.
	if s.count != 1 {
		t.Fatalf("count = %d, want 1 delivery counted even at capacity", s.count)
	}
}

func TestHealthzAnswersOK(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer
	rec := httptest.NewRecorder()
	(&sink{log: &logged}).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 — the container's HEALTHCHECK reads this", rec.Code)
	}
}
