package paypal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// TestTokenCache_GetOrFetch_ConcurrentMissesDeduped verifies the singleflight
// behavior: a burst of concurrent misses for the same key results in exactly
// one fetch call. Without this, a cold cache under burst load would issue
// N parallel upstream POSTs (regression guard for the thundering-herd bug).
func TestTokenCache_GetOrFetch_ConcurrentMissesDeduped(t *testing.T) {
	var calls int32
	gate := make(chan struct{}) // releases the sleeping fetcher
	fetcher := func() (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-gate
		return &Token{AccessToken: "AT", ExpiresIn: 3600}, nil
	}
	cache := NewTokenCache(60 * time.Second)

	const n = 32
	var wg sync.WaitGroup
	results := make([]*Token, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i], errs[i] = cache.GetOrFetch("cid-shared", fetcher)
		}()
	}
	// Let all goroutines pile up before letting the fetcher proceed.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch calls = %d, want 1 (singleflight should collapse %d concurrent misses)", got, n)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: err = %v", i, err)
		}
		if results[i] == nil || results[i].AccessToken != "AT" {
			t.Errorf("goroutine %d: token = %+v", i, results[i])
		}
	}
}

// TestTokenCache_GetOrFetch_DistinctKeysStayIndependent verifies singleflight
// doesn't incorrectly collapse misses for distinct keys.
func TestTokenCache_GetOrFetch_DistinctKeysStayIndependent(t *testing.T) {
	var calls int32
	mkFetcher := func(_ string) func() (*Token, error) {
		return func() (*Token, error) {
			atomic.AddInt32(&calls, 1)
			time.Sleep(10 * time.Millisecond) // window for races
			return &Token{AccessToken: "AT", ExpiresIn: 3600}, nil
		}
	}
	cache := NewTokenCache(60 * time.Second)

	var wg sync.WaitGroup
	keys := []string{"a", "b", "c"}
	wg.Add(len(keys))
	for _, k := range keys {
		k := k
		go func() {
			defer wg.Done()
			if _, err := cache.GetOrFetch(k, mkFetcher(k)); err != nil {
				t.Errorf("%s: %v", k, err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != int32(len(keys)) {
		t.Fatalf("fetch calls = %d, want %d (one per key)", got, len(keys))
	}
}

// sentinelErr is the distinct error the leader fetch returns in the
// failure-propagation test. Using a package-level sentinel lets us assert
// errors.Is walks the chain on followers.
var sentinelErr = errors.New("upstream paypal 503")

// TestTokenCache_GetOrFetch_LeaderFailurePropagatesToFollowers verifies the
// singleflight's failure semantics: when the leader's fetch fails, every
// concurrent follower receives the same error (wrapped with ErrUpstreamFailed)
// and does NOT retry. Without this, a PayPal 5xx incident under burst load
// would re-create the thundering-herd the singleflight is designed to
// prevent — N followers each fanning out their own retry.
func TestTokenCache_GetOrFetch_LeaderFailurePropagatesToFollowers(t *testing.T) {
	var calls int32
	gate := make(chan struct{}) // releases the failing fetcher
	fetcher := func() (*Token, error) {
		atomic.AddInt32(&calls, 1)
		<-gate
		return nil, sentinelErr
	}
	cache := NewTokenCache(60 * time.Second)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = cache.GetOrFetch("cid-fail", fetcher)
		}()
	}
	// Let all goroutines pile up on the inflight wait before letting the
	// fetcher return its error.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch calls = %d, want 1 (followers must NOT each retry on leader error)", got)
	}
	for i, err := range errs {
		if err == nil {
			t.Errorf("goroutine %d: nil error, want wrapped sentinel", i)
			continue
		}
		if !errors.Is(err, ErrUpstreamFailed) {
			t.Errorf("goroutine %d: err = %v, want errors.Is(_, ErrUpstreamFailed)", i, err)
		}
		if !errors.Is(err, sentinelErr) {
			t.Errorf("goroutine %d: err = %v, want errors.Is(_, sentinelErr) (inner must stay in the chain)", i, err)
		}
	}
}

// TestTokenCache_GetOrFetch_NilTokenNoPanic guards the (nil, nil) fetch
// callback contract: a future fetch implementation that returns nil token
// alongside nil error must NOT panic when followers dereference
// tok.AccessToken. Followers should observe ErrUpstreamFailed instead.
func TestTokenCache_GetOrFetch_NilTokenNoPanic(t *testing.T) {
	t.Parallel()
	cache := NewTokenCache(60 * time.Second)
	fetcher := func() (*Token, error) { return nil, nil }
	_, err := cache.GetOrFetch("cid-nil", fetcher)
	if err == nil {
		t.Fatal("expected error from nil-token fetch, got nil")
	}
	if !errors.Is(err, ErrUpstreamFailed) {
		t.Errorf("err = %v, want errors.Is(_, ErrUpstreamFailed)", err)
	}
}
func TestMode_BaseURL(t *testing.T) {
	t.Parallel()

	t.Run("sandbox returns sandbox URL", func(t *testing.T) {
		t.Parallel()
		got := ModeSandbox.BaseURL()
		if got != "https://api-m.sandbox.paypal.com" {
			t.Errorf("ModeSandbox.BaseURL() = %q, want https://api-m.sandbox.paypal.com", got)
		}
	})

	t.Run("live returns live URL", func(t *testing.T) {
		t.Parallel()
		got := ModeLive.BaseURL()
		if got != "https://api-m.paypal.com" {
			t.Errorf("ModeLive.BaseURL() = %q, want https://api-m.paypal.com", got)
		}
	})

	t.Run("unknown mode falls through to live URL", func(t *testing.T) {
		t.Parallel()
		got := Mode("weird").BaseURL()
		if got != "https://api-m.paypal.com" {
			t.Errorf("unexpected base URL for unknown mode: %q", got)
		}
	})

	t.Run("empty mode falls through to live URL", func(t *testing.T) {
		t.Parallel()
		got := Mode("").BaseURL()
		if got != "https://api-m.paypal.com" {
			t.Errorf("unexpected base URL for empty mode: %q", got)
		}
	})
}

// TestCachedClient_FetchToken_OAuthError — exercise the err != nil branch
// in FetchToken (so far only the happy path was covered).
func TestCachedClient_FetchToken_OAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad creds", http.StatusUnauthorized)
	}))
	defer srv.Close()

	oauth := NewOAuthClient(srv.Client(), srv.URL)
	cache := NewTokenCache(60 * time.Second)
	cc := NewCachedClient(oauth, cache)

	if _, err := cc.FetchToken(context.Background(), "cid", "cs"); err == nil {
		t.Error("expected error on OAuth 401, got nil")
	}
}
