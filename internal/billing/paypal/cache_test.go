package paypal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenCache_GetOrFetch_HitsCache(t *testing.T) {
	calls := 0
	fetcher := func() (*Token, error) {
		calls++
		return &Token{AccessToken: "AT", ExpiresIn: 3600}, nil
	}
	cache := NewTokenCache(60 * time.Second)

	tok1, err := cache.GetOrFetch("cid1", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := cache.GetOrFetch("cid1", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if tok1.AccessToken != "AT" || tok2.AccessToken != "AT" {
		t.Errorf("tokens = %+v, %+v", tok1, tok2)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (second lookup should hit cache)", calls)
	}
}

func TestTokenCache_GetOrFetch_DifferentKeys(t *testing.T) {
	calls := map[string]int{}
	makeFetcher := func(k string) func() (*Token, error) {
		return func() (*Token, error) {
			calls[k]++
			return &Token{AccessToken: "AT-" + k, ExpiresIn: 3600}, nil
		}
	}
	cache := NewTokenCache(60 * time.Second)

	if _, err := cache.GetOrFetch("cid1", makeFetcher("cid1")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetOrFetch("cid2", makeFetcher("cid2")); err != nil {
		t.Fatal(err)
	}
	if calls["cid1"] != 1 || calls["cid2"] != 1 {
		t.Errorf("calls = %v, want each once", calls)
	}
}

func TestTokenCache_GetOrFetch_Expired(t *testing.T) {
	calls := 0
	fetcher := func() (*Token, error) {
		calls++
		return &Token{AccessToken: "AT", ExpiresIn: 1}, nil
	}
	cache := NewTokenCache(500 * time.Millisecond)

	if _, err := cache.GetOrFetch("cid1", fetcher); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := cache.GetOrFetch("cid1", fetcher); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (cache should expire)", calls)
	}
}

func TestCachedClient_FetchToken_ComposestOAuthAndCache(t *testing.T) {
	var upstreamHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT","expires_in":3600}`))
	}))
	defer srv.Close()

	oauth := NewOAuthClient(srv.Client(), srv.URL)
	cache := NewTokenCache(60 * time.Second)
	cc := NewCachedClient(oauth, cache)

	for i := 0; i < 3; i++ {
		if _, err := cc.FetchToken(context.Background(), "cid", "cs"); err != nil {
			t.Fatal(err)
		}
	}
	if upstreamHits != 1 {
		t.Errorf("upstream hits = %d, want 1 (cache should absorb repeats)", upstreamHits)
	}
}