package analytics

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// Emitter failure persistence (FailureStore wiring)
// ============================================================================

// recordingFailureStore captures SaveFailed calls.
type recordingFailureStore struct {
	mu   sync.Mutex
	evts []Event
}

func (r *recordingFailureStore) SaveFailed(_ context.Context, evt Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evts = append(r.evts, evt)
	return nil
}

func (r *recordingFailureStore) saved() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.evts...)
}

func waitSaved(t *testing.T, r *recordingFailureStore, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.saved(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d SaveFailed calls, got %d", n, len(r.saved()))
	return nil
}

func TestEmitter_ServerError_PersistsFailure(t *testing.T) {
	t.Parallel()
	srv, _, _ := batchServer(t, http.StatusInternalServerError)
	e := NewEmitter("phc_testtoken", srv.URL, "staging")
	store := &recordingFailureStore{}
	e.SetFailureStore(store)

	evt := Event{Name: "purchase_completed", DistinctID: "u-1", UUID: EventUUID("purchase:txn-1")}
	e.Capture(evt)

	got := waitSaved(t, store, 1)
	if got[0].UUID != evt.UUID {
		t.Errorf("persisted UUID = %q, want %q", got[0].UUID, evt.UUID)
	}
	if got[0].Name != evt.Name || got[0].DistinctID != evt.DistinctID {
		t.Errorf("persisted event = %+v, want %+v", got[0], evt)
	}
}

func TestEmitter_TransportError_PersistsFailure(t *testing.T) {
	t.Parallel()
	e := NewEmitter("phc_testtoken", "http://unused", "staging")
	e.SetHTTPClient(&http.Client{Transport: errRoundTripper{}})
	store := &recordingFailureStore{}
	e.SetFailureStore(store)

	e.Capture(Event{Name: "signup_completed", DistinctID: "u-1", UUID: EventUUID("signup:u-1")})

	got := waitSaved(t, store, 1)
	if want := EventUUID("signup:u-1"); got[0].UUID != want {
		t.Errorf("persisted UUID = %q, want %q", got[0].UUID, want)
	}
}

func TestEmitter_Success_DoesNotPersist(t *testing.T) {
	t.Parallel()
	srv, got, _ := batchServer(t, http.StatusOK)
	e := NewEmitter("phc_testtoken", srv.URL, "staging")
	store := &recordingFailureStore{}
	e.SetFailureStore(store)

	e.Capture(Event{Name: "trial_started", DistinctID: "u-1", UUID: EventUUID("trial:s-1")})
	waitRequest(t, got)

	if n := len(store.saved()); n != 0 {
		t.Errorf("SaveFailed called %d times on success, want 0", n)
	}
}

func TestEmitter_Unmarshalable_DoesNotPersist(t *testing.T) {
	t.Parallel()
	// Marshal failure is a call-site code bug, not a delivery failure —
	// it must be logged and dropped without touching the store.
	srv, _, calls := batchServer(t, http.StatusOK)
	e := NewEmitter("phc_testtoken", srv.URL, "staging")
	store := &recordingFailureStore{}
	e.SetFailureStore(store)

	e.Capture(Event{Name: "signup_completed", DistinctID: "u-1",
		Properties: map[string]any{"bad": make(chan int)}})

	select {
	case r := <-waitChan(srv, calls):
		t.Fatalf("unmarshalable event reached the server: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	if n := len(store.saved()); n != 0 {
		t.Errorf("SaveFailed called %d times for a marshal error, want 0", n)
	}
}

func TestEmitter_NilStore_NoPanic(t *testing.T) {
	t.Parallel()
	e := NewEmitter("phc_testtoken", "http://unused", "staging")
	e.SetHTTPClient(&http.Client{Transport: errRoundTripper{}})
	// No SetFailureStore call: failures are logged and dropped, the M2
	// behavior. Must not panic (race detector watches the goroutine).
	e.Capture(Event{Name: "signup_completed", DistinctID: "u-1", UUID: EventUUID("signup:u-1")})
	// Give the goroutine real time to run: a panic inside it fails the test
	// binary, and the race detector watches it for the whole sleep. A
	// plain sleep (not waitSaved with n=0, which returns immediately)
	// is what actually exercises the nil-store branch.
	time.Sleep(100 * time.Millisecond)
}

func TestEmitter_SetFailureStore_NilReceiver(t *testing.T) {
	t.Parallel()
	var e *Emitter
	e.SetFailureStore(&recordingFailureStore{}) // must not panic
}

// ============================================================================
// RetryWorker
// ============================================================================

// fakeOutboxStore is the in-memory OutboxStore double.
type fakeOutboxStore struct {
	mu      sync.Mutex
	due     []OutboxEvent
	sent    []string
	resched map[string]struct {
		attempts int
		next     time.Time
	}
	listErr error
}

func (f *fakeOutboxStore) SaveFailed(_ context.Context, evt Event) error { return nil }

func (f *fakeOutboxStore) ListDue(_ context.Context, limit int) ([]OutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []OutboxEvent
	var rest []OutboxEvent
	for i, e := range f.due {
		if i < limit {
			out = append(out, e)
		} else {
			rest = append(rest, e)
		}
	}
	f.due = rest
	return out, nil
}

func (f *fakeOutboxStore) MarkSent(_ context.Context, eventUUID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, eventUUID)
	return nil
}

func (f *fakeOutboxStore) Reschedule(_ context.Context, eventUUID string, attempts int, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resched[eventUUID] = struct {
		attempts int
		next     time.Time
	}{attempts, next}
	return nil
}

func (f *fakeOutboxStore) sentList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeOutboxStore) reschedEntry(uuid string) (int, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.resched[uuid]
	return r.attempts, r.next, ok
}

func TestRetryWorker_SuccessMarksSent(t *testing.T) {
	t.Parallel()
	evt := Event{Name: "purchase_completed", DistinctID: "u-1", UUID: EventUUID("purchase:txn-ok")}
	store := &fakeOutboxStore{due: []OutboxEvent{{Event: evt}}, resched: map[string]struct {
		attempts int
		next     time.Time
	}{}}
	w := NewRetryWorker(store, func(context.Context, Event) error { return nil }, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	// The first drain runs before the first tick.
	deadline := time.Now().Add(2 * time.Second)
	for len(store.sentList()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	if got := store.sentList(); len(got) != 1 || got[0] != evt.UUID {
		t.Errorf("sent = %v, want [%s]", got, evt.UUID)
	}
	if _, _, ok := store.reschedEntry(evt.UUID); ok {
		t.Error("successful redelivery must not be rescheduled")
	}
}

func TestRetryWorker_FailureReschedulesWithBackoff(t *testing.T) {
	t.Parallel()
	evt := Event{Name: "purchase_completed", DistinctID: "u-1", UUID: EventUUID("purchase:txn-fail")}
	store := &fakeOutboxStore{due: []OutboxEvent{{Event: evt}}, resched: map[string]struct {
		attempts int
		next     time.Time
	}{}}
	boom := errors.New("posthog down")
	w := NewRetryWorker(store, func(context.Context, Event) error { return boom }, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, ok := store.reschedEntry(evt.UUID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no reschedule recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	attempts, next, _ := store.reschedEntry(evt.UUID)
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	// Backoff base is 1 minute: next ≈ now + 1m.
	if d := time.Until(next); d < 30*time.Second || d > 70*time.Minute {
		t.Errorf("next attempt in %v, want ~1m", d)
	}
	if got := store.sentList(); len(got) != 0 {
		t.Errorf("sent = %v, want empty on failure", got)
	}
}

func TestRetryWorker_DeadLetterAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	evt := Event{Name: "purchase_completed", DistinctID: "u-1", UUID: EventUUID("purchase:txn-dead")}
	// 9 prior failures: the next one is the 10th and final attempt.
	store := &fakeOutboxStore{due: []OutboxEvent{{Event: evt, Attempts: retryMaxAttempts - 1}},
		resched: map[string]struct {
			attempts int
			next     time.Time
		}{}}
	w := NewRetryWorker(store, func(context.Context, Event) error { return errors.New("down") }, time.Hour)
	w.drain(context.Background())

	attempts, next, ok := store.reschedEntry(evt.UUID)
	if !ok {
		t.Fatal("dead-lettered event must still be rescheduled (parked at infinity)")
	}
	if attempts != retryMaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, retryMaxAttempts)
	}
	if !next.Equal(retryInfinity) {
		t.Errorf("next = %v, want parked %v", next, retryInfinity)
	}
}

func TestRetryWorker_ListErrorSurvives(t *testing.T) {
	t.Parallel()
	store := &fakeOutboxStore{listErr: errors.New("db down"), resched: map[string]struct {
		attempts int
		next     time.Time
	}{}}
	w := NewRetryWorker(store, func(context.Context, Event) error { return nil }, time.Hour)
	// A list failure must be logged, not crash the loop.
	w.drain(context.Background())
}

func TestRetryBackoff(t *testing.T) {
	t.Parallel()
	cases := map[int]time.Duration{
		1:  time.Minute,
		2:  2 * time.Minute,
		3:  4 * time.Minute,
		10: 6 * time.Hour, // capped
		20: 6 * time.Hour, // capped
	}
	for attempts, want := range cases {
		if got := retryBackoff(attempts); got != want {
			t.Errorf("retryBackoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}
