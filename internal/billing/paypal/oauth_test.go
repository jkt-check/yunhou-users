package paypal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthClient_FetchToken_Success(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:cs"))
		if got := r.Header.Get("Authorization"); got != expected {
			t.Errorf("auth header = %q, want %q", got, expected)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type = %q", ct)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT-1",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	defer srv.Close()

	c := NewOAuthClient(srv.Client(), srv.URL)
	tok, err := c.FetchToken(context.Background(), "cid", "cs")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if tok.AccessToken != "AT-1" || tok.ExpiresIn != 3600 {
		t.Errorf("unexpected token: %+v", tok)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

func TestOAuthClient_FetchToken_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewOAuthClient(srv.Client(), srv.URL)
	if _, err := c.FetchToken(context.Background(), "cid", "cs"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestOAuthClient_FetchToken_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	c := NewOAuthClient(&http.Client{Timeout: 50 * time.Millisecond}, srv.URL)
	if _, err := c.FetchToken(context.Background(), "cid", "cs"); err == nil {
		t.Fatal("expected timeout error")
	}
}