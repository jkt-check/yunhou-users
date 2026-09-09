// connector_test.go — 连接器 client 纯单元验收（无 DB）：授权 URL 形状、
// 错误分类（invalid_grant/401→reauth，5xx/传输→retryable）、限额未知口径、
// 自托管服务认证适配器。

package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildAuthorizeURL(t *testing.T) {
	spec := Spec{
		Key:          "k",
		AuthorizeURL: "https://vendor.example/authorize?prompt=consent",
		TokenURL:     "https://vendor.example/token",
		ClientID:     "cid",
		Scopes:       []string{"a", "b"},
		RedirectURL:  "https://ops.example/cb",
	}
	c := &Client{}
	u, err := c.BuildAuthorizeURL(spec, "state-1", "challenge-1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "cid" ||
		q.Get("redirect_uri") != "https://ops.example/cb" || q.Get("state") != "state-1" ||
		q.Get("scope") != "a b" || q.Get("code_challenge") != "challenge-1" ||
		q.Get("code_challenge_method") != "S256" || q.Get("prompt") != "consent" {
		t.Fatalf("authorize url shape: %s", u)
	}
	if strings.Contains(u, "verifier") {
		t.Fatal("verifier must never appear in the authorize url")
	}

	// PKCE 显式关闭的厂商不携带 challenge。
	off := false
	spec.PKCE = &off
	u, err = c.BuildAuthorizeURL(spec, "s", "c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u, "code_challenge") {
		t.Fatalf("pkce=off vendor must not send a challenge: %s", u)
	}
}

func TestTokenErrorClassification(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("code") {
		case "expired":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		case "bad":
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
		case "boom":
			w.WriteHeader(503)
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
		}
	}))
	defer srv.Close()
	spec := Spec{Key: "k", AuthorizeURL: srv.URL + "/a", TokenURL: srv.URL + "/token",
		ClientID: "cid", RedirectURL: "https://ops.example/cb"}
	c := &Client{HTTP: srv.Client()}
	ctx := context.Background()

	if _, err := c.ExchangeCode(ctx, spec, "expired", "v"); KindOf(err) != KindReauthRequired {
		t.Fatalf("invalid_grant must classify reauth_required: %v", err)
	}
	if _, err := c.ExchangeCode(ctx, spec, "bad", "v"); KindOf(err) != KindReauthRequired {
		t.Fatalf("401 must classify reauth_required: %v", err)
	}
	if _, err := c.ExchangeCode(ctx, spec, "boom", "v"); KindOf(err) != KindRetryable {
		t.Fatalf("503 must classify retryable: %v", err)
	}
	ts, err := c.ExchangeCode(ctx, spec, "ok", "v")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "a" || ts.RefreshToken != "r" || ts.Expiry.IsZero() {
		t.Fatalf("token set mismatch: %+v", ts)
	}

	// 传输错误（连接拒绝）→ retryable，绝不误判 reauth。
	srv.Close()
	if _, err := c.ExchangeCode(ctx, spec, "ok", "v"); KindOf(err) != KindRetryable {
		t.Fatalf("transport failure must classify retryable: %v", err)
	}
	if !errors.Is(err, err) { // sanity
		t.Fatal("unreachable")
	}
}

func TestQuotaUnknownStaysUnknown(t *testing.T) {
	c := &Client{}
	// 无 quota 端点 → nil（未知），不是错误。
	snap, err := c.Quota(context.Background(), Spec{Key: "k"}, "tok", time.Now())
	if err != nil || snap != nil {
		t.Fatalf("no quota endpoint must be unknown, got %+v %v", snap, err)
	}
	// 不可解析的 quota 响应 → nil（未知），不是错误，更不是编造的数值。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html>not json</html>`)
	}))
	defer srv.Close()
	snap, err = c.Quota(context.Background(), Spec{Key: "k", QuotaURL: srv.URL}, "tok", time.Now())
	if err != nil || snap != nil {
		t.Fatalf("unparsable quota must stay unknown, got %+v %v", snap, err)
	}
}

func TestServiceAuthClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()
	sa := &ServiceAuth{HTTP: srv.Client()}
	if err := sa.Check(context.Background(), srv.URL+"/", []byte("good")); err != nil {
		t.Fatalf("good static credential must pass: %v", err)
	}
	if err := sa.Check(context.Background(), srv.URL+"/", []byte("bad")); KindOf(err) != KindReauthRequired {
		t.Fatalf("rejected static credential must classify reauth: %v", err)
	}
	srv.Close()
	if err := sa.Check(context.Background(), srv.URL+"/", []byte("good")); KindOf(err) != KindRetryable {
		t.Fatalf("unreachable service must classify retryable: %v", err)
	}
}
