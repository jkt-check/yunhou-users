// Package analytics is the server-side PostHog emitter. Events are
// captured asynchronously against the public capture API ({host}/batch/)
// with plain net/http — no SDK dependency. The emitter is fail-silent by
// design: analytics must never block or break the request path, so
// transport errors and non-2xx responses are logged and dropped (no
// retry in M2).
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
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
// detach, so the timeout parents off Background), and any failure is
// logged and dropped.
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

func (e *Emitter) send(evt Event) {
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
		log.Printf("analytics: marshal event %q: %v", evt.Name, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.host+"/batch/", bytes.NewReader(body))
	if err != nil {
		log.Printf("analytics: build request for %q: %v", evt.Name, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		log.Printf("analytics: capture %q: %v", evt.Name, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("analytics: capture %q: posthog returned %s", evt.Name, resp.Status)
	}
}
