// Package analytics is the server-side PostHog emitter. Events are
// captured asynchronously against the public capture API ({host}/batch/)
// with plain net/http — no SDK dependency. The emitter is fail-silent on
// the request path by design: analytics must never block or break it, so
// transport errors and non-2xx responses are logged and handed to the
// optional FailureStore — a RetryWorker redelivers them later (PostHog
// dedupes on the stable event UUID).
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultHost is the PostHog US cloud ingest endpoint.
const DefaultHost = "https://us.i.posthog.com"

// eventNamespace is the fixed UUIDv5 namespace for EventUUID. The whole
// service shares it, so the same business key (e.g. "signup:<userID>")
// always derives the same event UUID — that is the idempotency key PostHog
// dedupes on. Never change it: doing so would re-mint every historical
// key.
var eventNamespace = uuid.MustParse("0303b271-99eb-407d-8e4d-266229354fa8")

// Event is one capture payload. Timestamp is rendered ISO 8601 UTC on the
// wire. Properties must never carry PII ($ip, email, nickname) — the
// event call sites simply don't set those keys.
type Event struct {
	Name       string
	DistinctID string
	UUID       string
	Timestamp  time.Time
	Properties map[string]any
}

// EventUUID derives a deterministic UUID v5 from a business key.
func EventUUID(businessKey string) string {
	return uuid.NewSHA1(eventNamespace, []byte(businessKey)).String()
}

// Emitter POSTs events to PostHog. An empty token disables the emitter:
// every method no-ops and zero HTTP requests are made. All methods are
// nil-receiver safe so unwired services need no nil checks at call sites.
type Emitter struct {
	token       string
	host        string
	environment string
	httpClient  *http.Client
	store       FailureStore
}

// NewEmitter builds an emitter. Empty host falls back to DefaultHost.
func NewEmitter(token, host, environment string) *Emitter {
	if host == "" {
		host = DefaultHost
	}
	return &Emitter{
		token:       token,
		host:        strings.TrimRight(host, "/"),
		environment: environment,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// SetFailureStore wires the durable-retry backing store. Delivery
// failures are handed to it after logging; nil disables persistence
// (failures are logged and dropped, the old behavior). Nil-receiver safe.
func (e *Emitter) SetFailureStore(s FailureStore) {
	if e == nil {
		return
	}
	e.store = s
}

// FailureStore persists events whose delivery failed so a RetryWorker can
// redeliver them later. Implementations must key on evt.UUID (INSERT ...
// ON CONFLICT (uuid) DO NOTHING) — the event UUID is the idempotency key,
// so a repeated failure of the same event never produces a second row.
type FailureStore interface {
	SaveFailed(ctx context.Context, evt Event) error
}

// SetHTTPClient overrides the HTTP client (test hook, cf.
// GitHubOAuthService.SetHTTPClient).
func (e *Emitter) SetHTTPClient(c *http.Client) {
	if e == nil {
		return
	}
	e.httpClient = c
}

// Environment returns the environment label ("production"/"staging") the
// emitter was configured with; event call sites put it in properties.
func (e *Emitter) Environment() string {
	if e == nil {
		return ""
	}
	return e.environment
}

// Capture enqueues one event. Fire-and-forget: the POST runs in a
// goroutine with its own 10s timeout (Capture has no caller ctx to
// detach, so the timeout parents off Background). Delivery failures are
// logged and handed to the FailureStore when one is wired.
func (e *Emitter) Capture(evt Event) {
	if e == nil || e.token == "" {
		return
	}
	go e.send(evt)
}

// batchEvent is the wire shape of one entry in the PostHog /batch/ body.
type batchEvent struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	UUID       string         `json:"uuid,omitempty"`
	Timestamp  string         `json:"timestamp"`
	Properties map[string]any `json:"properties,omitempty"`
}

// errUnmarshalableEvent marks events whose payload cannot be marshaled.
// That is a code bug at the call site, not a delivery failure, so it is
// logged but never persisted to the outbox.
var errUnmarshalableEvent = errors.New("analytics: unmarshalable event")

func (e *Emitter) send(evt Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.sendOne(ctx, evt); err != nil {
		log.Printf("analytics: capture %q: %v", evt.Name, err)
		if errors.Is(err, errUnmarshalableEvent) {
			return
		}
		e.persistFailure(evt)
	}
}

// persistFailure hands a failed delivery to the FailureStore so the
// RetryWorker can redeliver it. The store call gets its own short timeout
// — the outbox write must never extend the request path (Capture already
// ran in a goroutine, but a wedged DB must not pile up goroutines).
func (e *Emitter) persistFailure(evt Event) {
	if e == nil || e.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.SaveFailed(ctx, evt); err != nil {
		log.Printf("analytics: persist failed event %q (%s): %v", evt.Name, evt.UUID, err)
	}
}

// sendOne POSTs one event to /batch/ in the canonical wire shape — the
// single place the wire format is defined, so a RetryWorker redelivery
// carries the same event (same UUID → PostHog dedupes on uuid).
func (e *Emitter) sendOne(ctx context.Context, evt Event) error {
	ts := evt.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	body, err := json.Marshal(struct {
		APIKey string       `json:"api_key"`
		Batch  []batchEvent `json:"batch"`
	}{
		APIKey: e.token,
		Batch: []batchEvent{{
			Event:      evt.Name,
			DistinctID: evt.DistinctID,
			UUID:       evt.UUID,
			Timestamp:  ts.UTC().Format(time.RFC3339),
			Properties: evt.Properties,
		}},
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errUnmarshalableEvent, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.host+"/batch/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("posthog returned %s", resp.Status)
	}
	return nil
}

// Redeliver re-POSTs a stored event. Wired into RetryWorker as its send
// func so retries share sendOne's wire format with first attempts.
func (e *Emitter) Redeliver(ctx context.Context, evt Event) error {
	return e.sendOne(ctx, evt)
}
