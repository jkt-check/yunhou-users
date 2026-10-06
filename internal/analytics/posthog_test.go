package analytics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// capturedRequest records what the httptest server received.
type capturedRequest struct {
	contentType string
	body        []byte
}

// batchServer returns an httptest server that records each request and
// replies with the given status code.
func batchServer(t *testing.T, status int) (*httptest.Server, chan capturedRequest, *atomic.Int32) {
	t.Helper()
	got := make(chan capturedRequest, 8)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		got <- capturedRequest{contentType: r.Header.Get("Content-Type"), body: body}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, got, &calls
}

func waitRequest(t *testing.T, got chan capturedRequest) capturedRequest {
	t.Helper()
	select {
	case r := <-got:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the capture HTTP request")
		return capturedRequest{}
	}
}

func TestEmitter_Disabled_MakesZeroHTTPCalls(t *testing.T) {
	t.Parallel()
	srv, _, calls := batchServer(t, http.StatusOK)
	e := NewEmitter("", srv.URL, "staging") // empty token = disabled
	e.Capture(Event{Name: "signup_completed", DistinctID: "u-1", UUID: EventUUID("signup:u-1")})

	select {
	case r := <-waitChan(srv, calls):
		t.Fatalf("disabled emitter made an HTTP request: %+v", r)
	case <-time.After(200 * time.Millisecond):
		// zero requests, as required
	}
}

// waitChan polls the call counter briefly; returns true once a call landed.
func waitChan(_ *httptest.Server, calls *atomic.Int32) chan bool {
	ch := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			if calls.Load() > 0 {
				ch <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return ch
}

func TestEmitter_Capture_PostsBatchPayload(t *testing.T) {
	t.Parallel()
	srv, got, _ := batchServer(t, http.StatusOK)
	e := NewEmitter("phc_testtoken", srv.URL, "production")

	ts := time.Date(2026, 10, 6, 8, 30, 0, 0, time.FixedZone("CST", 8*3600))
	e.Capture(Event{
		Name:       "signup_completed",
		DistinctID: "user-uuid-1",
		UUID:       EventUUID("signup:user-uuid-1"),
		Timestamp:  ts,
		Properties: map[string]any{"region": "intl", "auth_provider": "github"},
	})

	req := waitRequest(t, got)
	if ct := req.contentType; ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var payload struct {
		APIKey string `json:"api_key"`
		Batch  []struct {
			Event      string         `json:"event"`
			DistinctID string         `json:"distinct_id"`
			UUID       string         `json:"uuid"`
			Timestamp  string         `json:"timestamp"`
			Properties map[string]any `json:"properties"`
		} `json:"batch"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("body is not the PostHog batch shape: %v (%s)", err, req.body)
	}
	if payload.APIKey != "phc_testtoken" {
		t.Errorf("api_key = %q, want phc_testtoken", payload.APIKey)
	}
	if len(payload.Batch) != 1 {
		t.Fatalf("batch len = %d, want 1", len(payload.Batch))
	}
	evt := payload.Batch[0]
	if evt.Event != "signup_completed" {
		t.Errorf("event = %q, want signup_completed", evt.Event)
	}
	if evt.DistinctID != "user-uuid-1" {
		t.Errorf("distinct_id = %q, want user-uuid-1", evt.DistinctID)
	}
	if evt.UUID != EventUUID("signup:user-uuid-1") {
		t.Errorf("uuid = %q, want EventUUID output", evt.UUID)
	}
	// ISO 8601 UTC rendering regardless of the input zone.
	parsed, err := time.Parse(time.RFC3339, evt.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q not ISO 8601: %v", evt.Timestamp, err)
	}
	if !parsed.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", parsed, ts)
	}
	if parsed.Location() != time.UTC && evt.Timestamp[len(evt.Timestamp)-1] != 'Z' {
		t.Errorf("timestamp %q must be rendered in UTC", evt.Timestamp)
	}
	if evt.Properties["region"] != "intl" || evt.Properties["auth_provider"] != "github" {
		t.Errorf("properties = %v", evt.Properties)
	}
	// PII guard: these events must never carry ip/email/nickname.
	for _, banned := range []string{"$ip", "email", "nickname"} {
		if _, ok := evt.Properties[banned]; ok {
			t.Errorf("properties must never include %q", banned)
		}
	}
}

func TestEventUUID_Deterministic(t *testing.T) {
	t.Parallel()
	a1 := EventUUID("signup:user-1")
	a2 := EventUUID("signup:user-1")
	if a1 != a2 {
		t.Errorf("same key produced different UUIDs: %q vs %q", a1, a2)
	}
	b := EventUUID("signup:user-2")
	if a1 == b {
		t.Error("different keys produced the same UUID")
	}
	c := EventUUID("trial:sub-1")
	if a1 == c {
		t.Error("different prefixes produced the same UUID")
	}
	if _, err := uuid.Parse(a1); err != nil {
		t.Errorf("EventUUID output %q is not a valid UUID: %v", a1, err)
	}
}

func TestEmitter_Capture_ServerError_DroppedNotBlocking(t *testing.T) {
	t.Parallel()
	srv, got, _ := batchServer(t, http.StatusInternalServerError)
	e := NewEmitter("phc_testtoken", srv.URL, "staging")

	done := make(chan struct{})
	go func() {
		e.Capture(Event{Name: "trial_started", DistinctID: "u-1", UUID: EventUUID("trial:s-1")})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Capture blocked the caller")
	}
	// The request still lands server-side; the 500 is logged and dropped.
	waitRequest(t, got)
}

func TestEmitter_NilReceiver_Safe(t *testing.T) {
	t.Parallel()
	var e *Emitter
	e.Capture(Event{Name: "signup_completed", DistinctID: "u-1"}) // must not panic
	if got := e.Environment(); got != "" {
		t.Errorf("nil emitter Environment = %q, want empty", got)
	}
}

func TestEmitter_Environment(t *testing.T) {
	t.Parallel()
	if got := NewEmitter("tok", "", "production").Environment(); got != "production" {
		t.Errorf("Environment = %q, want production", got)
	}
}
