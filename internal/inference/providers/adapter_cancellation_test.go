package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestDispatch_CancellationExecutionBoundary(t *testing.T) {
	for _, sent := range []bool{false, true} {
		name := "before_send"
		if sent {
			name = "after_send"
		}
		t.Run(name, func(t *testing.T) {
			received := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				received <- struct{}{}
				<-r.Context().Done()
			}))
			defer srv.Close()
			client := NewHTTPClient(nil)
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !sent {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := Dispatch(ctx, client, NewOpenAIChat(),
					chatCall(testDeployment(domain.ProtocolOpenAIChat, srv.URL), false))
				done <- err
			}()
			if sent {
				select {
				case <-received:
					cancel()
				case <-time.After(2 * time.Second):
					t.Fatal("upstream did not receive the request")
				}
			}
			select {
			case err := <-done:
				de, ok := err.(*DispatchError)
				if !ok || de.ExecutedUnknown != sent || de.Retryable() {
					t.Fatalf("dispatch = %#v, want unknown=%t, no retry", err, sent)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not stop dispatch")
			}
			if !sent {
				select {
				case <-received:
					t.Fatal("pre-canceled request reached upstream")
				default:
				}
			}
		})
	}
}
