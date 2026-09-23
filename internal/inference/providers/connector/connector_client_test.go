package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// connector_client_test.go — Task 16 覆盖率补强：token 交换/刷新成功路
// 径、撤销（含无端点本地语义）、模型发现两种形状、quota 全解析、健康探
// 测分类、Registry 解析与 Spec 校验。

func vendorSpec(base string) Spec {
	return Spec{
		Key: "vendor-x", AuthorizeURL: base + "/authorize", TokenURL: base + "/token",
		RevokeURL: base + "/revoke", ModelsURL: base + "/models", QuotaURL: base + "/quota",
		HealthURL: base + "/health",
		ClientID:  "cid", ClientSecret: "csec", RedirectURL: "https://app.example.com/cb",
	}
}

func TestSpecValidate_AndParseRegistry(t *testing.T) {
	base := vendorSpec("https://v.example.com")
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	if !base.SupportsPKCE() {
		t.Error("PKCE default must be true")
	}
	off := false
	base.PKCE = &off
	if base.SupportsPKCE() {
		t.Error("explicit pkce=false")
	}
	for _, bad := range []Spec{
		{AuthorizeURL: "https://a", TokenURL: "https://t", ClientID: "c", RedirectURL: "r"}, // 无 key
		{Key: "k", TokenURL: "https://t", ClientID: "c", RedirectURL: "r"},                  // 无 authorize
		{Key: "k", AuthorizeURL: "ftp://a", TokenURL: "https://t", ClientID: "c", RedirectURL: "r"},
		{Key: "k", AuthorizeURL: "https://a", TokenURL: "https://t", RedirectURL: "r"}, // 无 client_id
		{Key: "k", AuthorizeURL: "https://a", TokenURL: "https://t", ClientID: "c"},    // 无 redirect
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("invalid spec accepted: %+v", bad)
		}
	}
	// Registry：空串 → 空注册表；重复 key → 致命；坏 JSON → 致命。
	reg, err := ParseRegistry("")
	if err != nil || len(reg) != 0 {
		t.Fatalf("empty registry = %v/%v", reg, err)
	}
	doc := `[{"key":"k1","authorize_url":"https://a","token_url":"https://t","client_id":"c","redirect_url":"r"}]`
	reg, err = ParseRegistry(doc)
	if err != nil || len(reg) != 1 {
		t.Fatalf("registry = %v/%v", reg, err)
	}
	dup := `[{"key":"k1","authorize_url":"https://a","token_url":"https://t","client_id":"c","redirect_url":"r"},
	         {"key":"k1","authorize_url":"https://a2","token_url":"https://t2","client_id":"c","redirect_url":"r"}]`
	if _, err := ParseRegistry(dup); err == nil {
		t.Error("duplicate key must be fatal")
	}
	if _, err := ParseRegistry(`not json`); err == nil {
		t.Error("bad JSON must be fatal")
	}
}

// tokenVendor serves the token/revoke/models/quota/health endpoints.
func tokenVendor(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("token form: %v", err)
			}
			if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csec" {
				t.Errorf("client auth missing in form: %v", r.Form)
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				if r.Form.Get("code_verifier") == "" {
					t.Errorf("PKCE verifier missing on exchange")
				}
			case "refresh_token":
				if r.Form.Get("refresh_token") != "rt-1" {
					t.Errorf("refresh token = %q", r.Form.Get("refresh_token"))
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-2","token_type":"Bearer","expires_in":3600,"account_id":"ext-9"}`)
		case "/revoke":
			w.WriteHeader(http.StatusOK)
		case "/models":
			if r.Header.Get("Authorization") != "Bearer at-1" {
				t.Errorf("models auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"},{"id":""}],"models":[{"name":"models/gm-1"},{"name":""}]}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"limit_micros":1000,"remaining_micros":400,"reset_at":"2026-10-01T00:00:00Z"}`)
		case "/health":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestExchangeRefreshRevokeModelsQuotaHealth(t *testing.T) {
	srv := tokenVendor(t)
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	spec := vendorSpec(srv.URL)
	ctx := context.Background()

	ts, err := c.ExchangeCode(ctx, spec, "code-1", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if ts.AccessToken != "at-1" || ts.RefreshToken != "rt-2" || ts.ExternalAccountID != "ext-9" || ts.Expiry.IsZero() {
		t.Fatalf("token set = %+v", ts)
	}
	ts, err = c.RefreshToken(ctx, spec, "rt-1")
	if err != nil || ts.AccessToken != "at-1" {
		t.Fatalf("refresh = %+v/%v", ts, err)
	}
	if err := c.Revoke(ctx, spec, "at-1"); err != nil {
		t.Fatal(err)
	}
	// 无端点/空 token → 本地语义，无调用无错误。
	if err := c.Revoke(ctx, Spec{}, "at-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(ctx, spec, ""); err != nil {
		t.Fatal(err)
	}

	models, err := c.Models(ctx, spec, "at-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "m1" || models[1] != "gm-1" {
		t.Fatalf("models = %v (空 id/name 过滤 + models/ 前缀剥离)", models)
	}
	// 无发现端点 → 显式不支持。
	if _, err := c.Models(ctx, Spec{}, "at-1"); err == nil {
		t.Error("no models endpoint must be an explicit error")
	}

	snap, err := c.Quota(ctx, spec, "at-1", time.Now())
	if err != nil || snap == nil {
		t.Fatalf("quota = %+v/%v", snap, err)
	}
	if snap.LimitMicros == nil || *snap.LimitMicros != 1000 ||
		snap.RemainingMicros == nil || *snap.RemainingMicros != 400 || snap.ResetAt == nil {
		t.Fatalf("quota snapshot = %+v", snap)
	}
	// 无端点 → nil（未知保持未知）。
	snap, err = c.Quota(ctx, Spec{}, "at-1", time.Now())
	if err != nil || snap != nil {
		t.Fatalf("quota without endpoint = %+v/%v, want nil/nil", snap, err)
	}

	if err := c.Health(ctx, spec, "at-1"); err != nil {
		t.Fatal(err)
	}
	// HealthURL 为空时回退 ModelsURL。
	spec2 := spec
	spec2.HealthURL = ""
	if err := c.Health(ctx, spec2, "at-1"); err != nil {
		t.Fatal(err)
	}
}

func TestHealth_401ClassifiesReauth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	spec := vendorSpec(srv.URL)
	err := c.Health(context.Background(), spec, "dead-token")
	if KindOf(err) != KindReauthRequired {
		t.Fatalf("health 401 = %v, want reauth_required", err)
	}
	// 5xx → retryable。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	spec.TokenURL = srv2.URL
	spec.HealthURL = srv2.URL
	if err := c.Health(context.Background(), spec, "t"); KindOf(err) != KindRetryable {
		t.Fatalf("health 503 = %v, want retryable", err)
	}
}

func TestTokenErrorAndTruncate(t *testing.T) {
	e := &Error{Kind: KindRetryable, StatusCode: 500, Msg: "boom"}
	if !strings.Contains(e.Error(), "500") || e.Unwrap() != nil {
		t.Errorf("error shape = %v", e.Error())
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ab", 3); got != "ab" {
		t.Errorf("truncate short = %q", got)
	}
	// KindOf 未知错误 → retryable（传输失败绝不翻 reauth）。
	if KindOf(io.ErrUnexpectedEOF) != KindRetryable {
		t.Error("unknown error must be retryable")
	}
	// 解析失败的 token 响应 → invalid_response。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"no_access_token":true}`)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	_, err := c.RefreshToken(context.Background(), vendorSpec(srv.URL), "rt")
	if KindOf(err) != KindInvalid {
		t.Fatalf("missing access_token = %v, want invalid_response", err)
	}
	// 400 invalid_grant → reauth。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	defer srv2.Close()
	c2 := &Client{HTTP: srv2.Client()}
	_, err = c2.RefreshToken(context.Background(), vendorSpec(srv2.URL), "rt")
	if KindOf(err) != KindReauthRequired {
		t.Fatalf("invalid_grant = %v, want reauth_required", err)
	}
}
