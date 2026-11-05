package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// memStore is a fake of Store for unit tests.
type memStore struct {
	mu     sync.Mutex
	creds  map[string]*domain.Credential
	rotate int
}

func newMemStore() *memStore { return &memStore{creds: map[string]*domain.Credential{}} }

func (m *memStore) InsertCredential(_ context.Context, c *domain.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.creds[c.ID]; dup {
		return domain.NewError(domain.CodeConflict, "duplicate")
	}
	cp := *c
	m.creds[c.ID] = &cp
	return nil
}

func (m *memStore) GetCredential(_ context.Context, id string) (*domain.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "not found")
	}
	cp := *c
	return &cp, nil
}

func (m *memStore) ListCredentials(_ context.Context, providerID string, limit int) ([]domain.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Credential{}
	for _, c := range m.creds {
		if providerID == "" || c.ProviderID == providerID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (m *memStore) RotateCredentialSecret(_ context.Context, id string, ciphertext []byte, keyVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "not found")
	}
	c.Ciphertext = ciphertext
	c.KeyVersion = keyVersion
	c.Generation++
	m.rotate++
	return nil
}

func (m *memStore) SetCredentialStatus(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "not found")
	}
	c.Status = status
	return nil
}

func (m *memStore) DisableUpstreamAccountsByCredential(_ context.Context, credentialID string) (int64, error) {
	return 1, nil
}

// memRecorder collects audit events.
type memRecorder struct {
	mu     sync.Mutex
	events []management.AuditEvent
	fail   bool
}

func (r *memRecorder) Record(_ context.Context, ev management.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("audit store down")
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *memRecorder) find(action string) *management.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.events {
		if r.events[i].Action == action {
			return &r.events[i]
		}
	}
	return nil
}

func testOp() Operator {
	return Operator{UserID: "user-1", AppID: "yunhou-website", Roles: []string{"admin"}}
}

func TestServiceLifecycleAndAudit(t *testing.T) {
	v, err := NewVault(testKeys(t, 1, 2), 2)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	rec := &memRecorder{}
	svc := NewService(v, store, rec)

	view, err := svc.Create(context.Background(), testOp(), "prov-1", "prod key", "api_key", "sk-super-secret", "initial onboarding", nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.KeyVersion != 2 || view.Generation != 1 || view.Status != "active" {
		t.Fatalf("unexpected view: %+v", view)
	}
	// Stored row: ciphertext present but the audit event must not carry it.
	stored, _ := store.GetCredential(context.Background(), view.ID)
	if len(stored.Ciphertext) == 0 {
		t.Fatal("ciphertext must be stored")
	}
	ev := rec.find("credential.create")
	if ev == nil {
		t.Fatal("credential.create audit missing")
	}
	if ev.ActorUser != "user-1" || ev.ActorApp != "yunhou-website" {
		t.Fatalf("dual attribution: %+v", ev)
	}
	if ev.ObjectID != view.ID || ev.Reason == "" {
		t.Fatalf("object/reason: %+v", ev)
	}

	// Rotate bumps generation and re-encrypts under the current version.
	rotated, err := svc.Rotate(context.Background(), testOp(), view.ID, "sk-rotated", "scheduled rotation")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Generation != 2 || rotated.KeyVersion != 2 {
		t.Fatalf("rotate: %+v", rotated)
	}
	if rec.find("credential.rotate") == nil {
		t.Fatal("credential.rotate audit missing")
	}

	// Test (decrypt check).
	if _, err := svc.Test(context.Background(), testOp(), view.ID, "post-rotate check"); err != nil {
		t.Fatal(err)
	}

	// Disable: resolve must refuse afterwards.
	if _, err := svc.SetStatus(context.Background(), testOp(), view.ID, "revoked", "compromised"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSecret(context.Background(), view.ID, nil); err == nil {
		t.Fatal("resolve of disabled credential must fail")
	}
	if rec.find("credential.disable") == nil {
		t.Fatal("credential.disable audit missing")
	}
}

func TestServiceResponsesNeverLeakSecrets(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	rec := &memRecorder{}
	svc := NewService(v, newMemStore(), rec)
	secret := "sk-must-never-appear"
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", secret, "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("view leaks plaintext secret")
	}
	for _, ev := range rec.events {
		d, _ := json.Marshal(ev.Detail)
		if strings.Contains(string(d), secret) {
			t.Fatalf("audit detail leaks secret: %s", d)
		}
	}
}

func TestServiceValidation(t *testing.T) {
	svc := NewService(nil, newMemStore(), &memRecorder{})
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "s", "r", nil); err == nil {
		t.Fatal("nil vault must fail closed")
	}
	v, _ := NewVault(testKeys(t, 1), 1)
	svc = NewService(v, newMemStore(), &memRecorder{})
	if _, err := svc.Create(context.Background(), testOp(), "", "l", "api_key", "s", "r", nil); err == nil {
		t.Fatal("empty provider must fail")
	}
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "badtype", "s", "r", nil); err == nil {
		t.Fatal("bad auth_type must fail")
	}
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "", "r", nil); err == nil {
		t.Fatal("empty secret must fail")
	}
}

func TestServiceResolvePinnedGeneration(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	store := newMemStore()
	svc := NewService(v, store, &memRecorder{})
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "gen-1-secret", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Admission pins generation 1.
	pin := int64(1)
	plain, _, err := svc.ResolveSecret(context.Background(), view.ID, &pin)
	if err != nil || string(plain) != "gen-1-secret" {
		t.Fatalf("pinned resolve: %v", err)
	}
	// Rotate → new generation; a NEW resolve pinned at the old generation is
	// refused (no silent cross-generation secret use)...
	svc.Rotate(context.Background(), testOp(), view.ID, "gen-2-secret", "r")
	if _, _, err := svc.ResolveSecret(context.Background(), view.ID, &pin); err == nil {
		t.Fatal("stale pinned generation must fail")
	}
	// ...while an unpinned resolve (new dispatch) gets the current secret.
	plain, cred, err := svc.ResolveSecret(context.Background(), view.ID, nil)
	if err != nil || string(plain) != "gen-2-secret" || cred.Generation != 2 {
		t.Fatalf("current resolve: %v", err)
	}
}

func TestServiceAuditFailureFailsClosed(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	rec := &memRecorder{fail: true}
	svc := NewService(v, newMemStore(), rec)
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "s", "r", nil); err == nil {
		t.Fatal("audit failure must surface as an error")
	}
}

func TestServiceRotateDisabledRejected(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	view, _ := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "s", "r", nil)
	svc.SetStatus(context.Background(), testOp(), view.ID, "revoked", "r")
	if _, err := svc.Rotate(context.Background(), testOp(), view.ID, "new", "r"); err == nil {
		t.Fatal("rotate of disabled credential must fail")
	}
	if _, err := svc.Test(context.Background(), testOp(), view.ID, "r"); err == nil {
		t.Fatal("test of disabled credential must fail")
	}
}

// ---------------------------------------------------------------------------
// T1 — 出站解析：oauth 凭据只交出 access token
// ---------------------------------------------------------------------------

// oauthBundleFor seals the same bundle shape credentials.HandleCallback
// stores (access + refresh in one JSON blob).
func oauthBundleFor(t *testing.T, access, refresh string) string {
	t.Helper()
	raw, err := MarshalBundle(&OAuthBundle{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer"})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return string(raw)
}

func TestResolveOutboundAuth_OAuthYieldsAccessTokenOnly(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	const access, refresh = "acc-token-unit", "refresh-token-unit"
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "oauth", oauthBundleFor(t, access, refresh), "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	auth, cred, err := svc.ResolveOutboundAuth(context.Background(), view.ID, nil)
	if err != nil {
		t.Fatalf("resolve outbound auth: %v", err)
	}
	if auth.AuthType != "oauth" {
		t.Errorf("auth type = %q, want oauth", auth.AuthType)
	}
	if auth.Token != access {
		t.Errorf("token is not the bare access token (bytes=%d)", len(auth.Token))
	}
	for _, forbidden := range []string{refresh, "access_token", "refresh_token", "{"} {
		if strings.Contains(auth.Token, forbidden) {
			t.Errorf("outbound token carries bundle material (%q)", forbidden)
		}
	}
	if cred == nil || cred.ID != view.ID {
		t.Error("the credential row must come back with the resolution")
	}
}

func TestResolveOutboundAuth_StaticCredentialsUnchanged(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	for _, authType := range []string{"api_key", "service"} {
		t.Run(authType, func(t *testing.T) {
			const secret = "sk-static-unit-secret"
			view, err := svc.Create(context.Background(), testOp(), "p", "l", authType, secret, "r", nil)
			if err != nil {
				t.Fatal(err)
			}
			auth, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, nil)
			if err != nil {
				t.Fatalf("resolve outbound auth: %v", err)
			}
			if auth.AuthType != authType || auth.Token != secret {
				t.Errorf("static credential must resolve to its plaintext unchanged (type=%q)", auth.AuthType)
			}
		})
	}
}

func TestResolveOutboundAuth_UnusableOAuthFailsClosed(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	const refresh = "refresh-token-unit"

	empty, err := svc.Create(context.Background(), testOp(), "p", "l", "oauth", oauthBundleFor(t, "", refresh), "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveOutboundAuth(context.Background(), empty.ID, nil); err == nil {
		t.Fatal("an oauth credential without an access token must not resolve")
	} else if domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("empty access token error code = %v, want conflict", domain.CodeOf(err))
	}

	const broken = "not-a-bundle-" + refresh
	bad, err := svc.Create(context.Background(), testOp(), "p", "l", "oauth", broken, "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.ResolveOutboundAuth(context.Background(), bad.ID, nil)
	if err == nil {
		t.Fatal("an unparsable bundle must not resolve")
	}
	if domain.CodeOf(err) != domain.CodeInternal {
		t.Errorf("unparsable bundle error code = %v, want internal", domain.CodeOf(err))
	}
	for _, forbidden := range []string{refresh, broken, "not-a-bundle"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("the failure surfaced credential material (%q)", forbidden)
		}
	}
}

func TestResolveOutboundAuth_StatusAndGenerationPreserved(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	store := newMemStore()
	svc := NewService(v, store, &memRecorder{})
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "oauth", oauthBundleFor(t, "acc-1", "ref-1"), "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 代次钉扎：轮换后旧代次不可再解析（旧凭据不可重放派发）。
	pin := int64(1)
	if _, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, &pin); err != nil {
		t.Fatalf("pinned resolve: %v", err)
	}
	if _, err := svc.Rotate(context.Background(), testOp(), view.ID, oauthBundleFor(t, "acc-2", "ref-2"), "rotate"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, &pin); err == nil {
		t.Fatal("a stale pinned generation must not resolve")
	} else if domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("stale generation error code = %v, want conflict", domain.CodeOf(err))
	}
	auth, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, nil)
	if err != nil || auth.Token != "acc-2" {
		t.Fatalf("current resolve must yield the rotated access token: %v", err)
	}
	// 状态：rotating 仍可派发，revoked 不可。
	if err := store.SetCredentialStatus(context.Background(), view.ID, "rotating"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, nil); err != nil {
		t.Errorf("a rotating credential stays dispatchable: %v", err)
	}
	if err := store.SetCredentialStatus(context.Background(), view.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, nil); err == nil {
		t.Error("a revoked credential must not resolve")
	} else if domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("revoked credential error code = %v, want conflict", domain.CodeOf(err))
	}
}

// TestResolveOAuthBundle_SharedDecodePath: 两个公开出口必须共用同一套
// bundle 解码口径 —— 坏 bundle / 空 access token 一律拒绝，且拒绝完全一致
// （同 code、同 message）、不挂原始 JSON 解析原因、不带任何凭据材料。
func TestResolveOAuthBundle_SharedDecodePath(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	const refresh = "shared-refresh-token"

	cases := []struct {
		name   string
		bundle string
		code   domain.Code
	}{
		{"empty access token", oauthBundleFor(t, "", refresh), domain.CodeConflict},
		{"corrupt bundle", "not-a-json-bundle-" + refresh, domain.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, err := svc.Create(context.Background(), testOp(), "p", "l", "oauth", tc.bundle, "r", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, bearerErr := svc.ResolveBearerToken(context.Background(), view.ID, nil)
			_, _, outboundErr := svc.ResolveOutboundAuth(context.Background(), view.ID, nil)
			if bearerErr == nil || outboundErr == nil {
				t.Fatalf("both paths must refuse an unusable oauth credential")
			}
			if domain.CodeOf(bearerErr) != tc.code || domain.CodeOf(outboundErr) != tc.code {
				t.Errorf("error codes = %v / %v, want %v",
					domain.CodeOf(bearerErr), domain.CodeOf(outboundErr), tc.code)
			}
			// 只比对 domain.Error 的 Message（不含 cause 链），失败信息里
			// 不会出现任何凭据材料。
			bMsg, oMsg := domainMessage(bearerErr), domainMessage(outboundErr)
			if bMsg != oMsg {
				t.Errorf("the two public paths must reject identically (bearer message=%q, outbound message=%q)", bMsg, oMsg)
			}
			// 原始 JSON 解析原因不得挂进错误（它会回显明文的一个字符）。
			for _, err := range []error{bearerErr, outboundErr} {
				for _, parser := range []string{"invalid character", "unexpected end of JSON", "cannot unmarshal"} {
					if strings.Contains(err.Error(), parser) {
						t.Errorf("the failure attached a raw json parse cause")
					}
				}
			}
			for _, forbidden := range []string{refresh, tc.bundle, "not-a-json-bundle"} {
				for _, err := range []error{bearerErr, outboundErr} {
					if strings.Contains(err.Error(), forbidden) {
						t.Errorf("the failure surfaced credential material")
					}
				}
			}
		})
	}
}

// TestResolveStaticCredentialBehaviourPreserved: 共享解码只作用于 oauth 分支
// —— 两个公开出口各自的静态 api_key/service 行为保持原样（探测出口继续
// trim，热路径出口保持逐字节明文）。
func TestResolveStaticCredentialBehaviourPreserved(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	const secret = "  sk-static-padded  "
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", secret, "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := svc.ResolveBearerToken(context.Background(), view.ID, nil)
	if err != nil {
		t.Fatalf("bearer path: %v", err)
	}
	if bearer != "sk-static-padded" {
		t.Errorf("the probe path must keep trimming static credentials")
	}
	outbound, _, err := svc.ResolveOutboundAuth(context.Background(), view.ID, nil)
	if err != nil {
		t.Fatalf("outbound path: %v", err)
	}
	if outbound.Token != secret {
		t.Errorf("the hot path must keep the static plaintext byte-identical")
	}
}

// domainMessage extracts the domain error message (never the cause chain, so
// a failure message can be printed without leaking credential material).
func domainMessage(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return ""
}
