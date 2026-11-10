package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestFencing_CancelsBlockedUpstream(t *testing.T) {
	for _, phase := range []string{"headers", "body", "silent_sse"} {
		t.Run(phase, func(t *testing.T) {
			arrived := make(chan struct{})
			stopped := make(chan struct{})
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if phase != "headers" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					if phase == "body" {
						_, _ = io.WriteString(w, "{")
					}
					w.(http.Flusher).Flush()
				}
				close(arrived)
				<-r.Context().Done()
				close(stopped)
			})
			f := newFixture(t, up, withKeyConcurrencyLimit(1))
			f.routing.LeaseTTL = 150 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			p, key := f.principal()
			go func() {
				out, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat,
					chatReq(phase == "silent_sse", "hi"))
				if err == nil && out.Stream != nil {
					_, err = out.Stream.Read(make([]byte, 4096))
					_ = out.Stream.Finish(EndCompleted)
					_ = out.Stream.Close()
				}
				done <- err
			}()
			select {
			case <-arrived:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream not reached")
			}
			res, err := f.db.Exec(`UPDATE inference_concurrency_leases SET state='expired'
				WHERE scope='upstream_account' AND state='held'`)
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("expired %d leases, want 1", n)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("fenced upstream completed successfully")
				}
				if phase == "silent_sse" && !strings.Contains(err.Error(), "fenced") {
					t.Fatalf("read error lost fencing cause: %v", err)
				}
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("fencing did not unblock upstream I/O")
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("upstream connection remains live after fencing")
			}
			var held, requests, charges int
			if err := f.db.Get(&held, `SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Get(&requests, `SELECT COUNT(*) FROM inference_requests WHERE status='reconciliation_required'`); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Get(&charges, `SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`); err != nil {
				t.Fatal(err)
			}
			// Dispatch-level unknown retains the Key lease until TTL; only an
			// already handed-off stream releases it through Finish.
			wantHeld := 1
			if phase == "silent_sse" {
				wantHeld = 0
			}
			heldKey := keyLeaseStates(t, f)["held"]
			if held != wantHeld || heldKey != wantHeld || requests != 1 || charges != 0 || up.calls.Load() != 1 {
				t.Fatalf("held=%d held Key=%d unknown requests=%d charges=%d dispatches=%d; want %d/%d/1/0/1",
					held, heldKey, requests, charges, up.calls.Load(), wantHeld, wantHeld)
			}
		})
	}
}
