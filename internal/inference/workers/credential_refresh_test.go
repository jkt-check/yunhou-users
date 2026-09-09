// credential_refresh_test.go — Task 12 刷新 worker 真库验收：到期扫描轮换、
// 双实例并发各跑一次（一个赢家一个收敛）、invalid_grant 传播、连接器不
// 可用只重试。

package workers

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

// refreshEnv bundles the wired worker + mock vendor over a real DB.
type refreshEnv struct {
	db       *sqlx.DB
	store    *postgres.Store
	vault    *credentials.Vault
	vendor   *refreshVendor
	registry connector.Registry
	provider string
	modelID  string
}

type refreshVendor struct {
	srv *httptest.Server
	mu  sync.Mutex
	// refresh behavior
	responses []stubResponse
	calls     int
	onCall    func()
	health    int // health endpoint status (default 200)
	quotaBody string
}

type stubResponse struct {
	block  <-chan struct{}
	status int
	body   string
}

func newRefreshVendor(t *testing.T) *refreshVendor {
	t.Helper()
	v := &refreshVendor{health: 200, quotaBody: `{"limit_micros":1000000,"remaining_micros":400000,"reset_at":"2026-09-10T00:00:00Z"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		v.mu.Lock()
		v.calls++
		onCall := v.onCall
		var stub *stubResponse
		if len(v.responses) > 0 {
			stub = &v.responses[0]
			v.responses = v.responses[1:]
		}
		v.mu.Unlock()
		if onCall != nil {
			onCall()
		}
		w.Header().Set("Content-Type", "application/json")
		if stub != nil {
			if stub.block != nil {
				<-stub.block
			}
			w.WriteHeader(stub.status)
			fmt.Fprint(w, stub.body)
			return
		}
		fmt.Fprint(w, `{"access_token":"at-refreshed","refresh_token":"rt-new","token_type":"bearer","expires_in":3600}`)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		status := v.health
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/quota", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		body := v.quotaBody
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	// 根路径：自托管部署健康探测目标（ServiceAuth 探测 base_url + "/"）。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		status := v.health
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{}`)
	})
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func (v *refreshVendor) registry() connector.Registry {
	return connector.Registry{"testvendor": connector.Spec{
		Key:          "testvendor",
		AuthorizeURL: v.srv.URL + "/authorize",
		TokenURL:     v.srv.URL + "/token",
		RevokeURL:    v.srv.URL + "/revoke",
		ModelsURL:    v.srv.URL + "/models",
		QuotaURL:     v.srv.URL + "/quota",
		HealthURL:    v.srv.URL + "/health",
		ClientID:     "cid-test",
		RedirectURL:  "https://ops.example/admin/oauth/callback",
	}}
}

func (v *refreshVendor) push(st stubResponse) {
	v.mu.Lock()
	v.responses = append(v.responses, st)
	v.mu.Unlock()
}

func (v *refreshVendor) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func newRefreshEnv(t *testing.T) refreshEnv {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	provCode := "task12refresh-" + suffix
	modelID := "task12-refresh-model-" + suffix
	seed := []string{
		fmt.Sprintf(`INSERT INTO users (id, status) VALUES ('12121212-1212-4212-8212-121212121212','active') ON CONFLICT (id) DO NOTHING`),
		fmt.Sprintf(`INSERT INTO inference_providers (code, display_name, access_type) VALUES ('%s','Refresh Provider','oauth_connector')`, provCode),
		fmt.Sprintf(`INSERT INTO inference_models (id, display_name, context_tokens, max_output_tokens) VALUES ('%s','M',128000,8192)`, modelID),
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	var providerID string
	if err := db.GetContext(ctx, &providerID, `SELECT id FROM inference_providers WHERE code = $1`, provCode); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: key}, 1)
	if err != nil {
		t.Fatal(err)
	}
	vendor := newRefreshVendor(t)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM inference_session_bindings WHERE model_id = $1`, modelID)
		db.Exec(`DELETE FROM inference_oauth_grants WHERE connector = 'testvendor'`)
		db.Exec(`DELETE FROM inference_upstream_accounts WHERE provider_id = $1`, providerID)
		db.Exec(`DELETE FROM inference_credentials WHERE provider_id = $1`, providerID)
		db.Exec(`DELETE FROM inference_audit_log WHERE object_type IN ('credential','oauth_authorization','upstream_account')`)
		db.Exec(`DELETE FROM inference_models WHERE id = $1`, modelID)
		db.Exec(`DELETE FROM inference_providers WHERE code = $1`, provCode)
		db.Close()
	})
	return refreshEnv{
		db: db, store: postgres.NewStore(db), vault: vault, vendor: vendor,
		registry: vendor.registry(), provider: providerID, modelID: modelID,
	}
}

// seedExpiringCredential stores one oauth credential expiring in 2min + one
// active account.
func (e refreshEnv) seedExpiringCredential(t *testing.T) (credID, accountID string) {
	t.Helper()
	ctx := context.Background()
	expiry := time.Now().Add(2 * time.Minute)
	bundle, err := credentials.MarshalBundle(&credentials.OAuthBundle{
		AccessToken: "at-old", RefreshToken: "rt-old", TokenType: "bearer", ObtainedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	credID = uuid.NewString()
	ct, kv, err := e.vault.Encrypt(credID, e.provider, bundle)
	if err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: credID, ProviderID: e.provider, Label: "c", AuthType: "oauth",
		Ciphertext: ct, KeyVersion: kv, Generation: 1, ExpiresAt: &expiry,
		Status: "active", Connector: "testvendor",
	}
	if err := e.store.InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{
		ProviderID: e.provider, CredentialID: credID, DisplayName: "acct",
		Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := e.store.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	return credID, acct.ID
}

func (e refreshEnv) refresher() *credentials.Refresher {
	return credentials.NewRefresher(e.vault, e.store, e.store,
		&connector.Client{HTTP: e.vendor.srv.Client()}, e.registry, nil)
}

func TestCredentialRefreshWorkerRotatesExpiring(t *testing.T) {
	env := newRefreshEnv(t)
	credID, accountID := env.seedExpiringCredential(t)

	w := NewCredentialRefresh(env.store, env.refresher(), CredentialRefreshConfig{}, nil)
	m, err := w.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Scanned != 1 || m.Rotated != 1 {
		t.Fatalf("expected 1 scanned 1 rotated: %+v", m)
	}
	var gen int64
	var acctStatus string
	env.db.QueryRow(`SELECT generation FROM inference_credentials WHERE id = $1`, credID).Scan(&gen)
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	if gen != 2 || acctStatus != "active" {
		t.Fatalf("gen=%d acct=%s", gen, acctStatus)
	}
	// 第二轮：新 expiry 在 1h 后，超出 skew → 不再扫到（幂等收敛）。
	m2, err := w.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m2.Scanned != 0 || m2.Rotated != 0 {
		t.Fatalf("second pass must be a no-op: %+v", m2)
	}
}

// TestCredentialRefreshWorkerTwoInstances — 两个 worker 实例（独立连接池）
// 并发跑同一轮：恰一个 rotated、一个 converged，无 lost update。
func TestCredentialRefreshWorkerTwoInstances(t *testing.T) {
	env := newRefreshEnv(t)
	credID, _ := env.seedExpiringCredential(t)

	// 厂商编排：两个 refresh 调用都在途后才放行。
	arrived := make(chan struct{}, 2)
	both := make(chan struct{})
	go func() { <-arrived; <-arrived; close(both) }()
	env.vendor.push(stubResponse{block: both, status: 200, body: `{"access_token":"at-w1","refresh_token":"rt-w1","expires_in":3600}`})
	env.vendor.push(stubResponse{block: both, status: 200, body: `{"access_token":"at-w2","refresh_token":"rt-w2","expires_in":3600}`})
	// 每个 stub 到达时各报一次。
	env.vendor.mu.Lock()
	env.vendor.onCall = func() { arrived <- struct{}{} }
	env.vendor.mu.Unlock()

	db2, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	store2 := postgres.NewStore(db2)
	refresher2 := credentials.NewRefresher(env.vault, store2, store2,
		&connector.Client{HTTP: env.vendor.srv.Client()}, env.registry, nil)

	w1 := NewCredentialRefresh(env.store, env.refresher(), CredentialRefreshConfig{}, nil)
	w2 := NewCredentialRefresh(store2, refresher2, CredentialRefreshConfig{}, nil)

	var m1, m2 CredentialRefreshMetrics
	var e1, e2 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m1, e1 = w1.RunPass(context.Background()) }()
	go func() { defer wg.Done(); m2, e2 = w2.RunPass(context.Background()) }()
	wg.Wait()
	if e1 != nil || e2 != nil {
		t.Fatalf("pass errors: %v / %v", e1, e2)
	}
	if m1.Scanned != 1 || m2.Scanned != 1 {
		t.Fatalf("both instances must scan the credential: %+v / %+v", m1, m2)
	}
	totalRotated := m1.Rotated + m2.Rotated
	totalConverged := m1.Converged + m2.Converged
	if totalRotated != 1 || totalConverged != 1 {
		t.Fatalf("exactly one rotation expected: %+v / %+v", m1, m2)
	}
	var gen int64
	env.db.Get(&gen, `SELECT generation FROM inference_credentials WHERE id = $1`, credID)
	if gen != 2 {
		t.Fatalf("lost update across instances: generation=%d", gen)
	}
}

// mergeGate was removed; arrivals are reported via vendor.onCall.

func TestCredentialRefreshWorkerInvalidGrantStopsAssignment(t *testing.T) {
	env := newRefreshEnv(t)
	_, accountID := env.seedExpiringCredential(t)
	ctx := context.Background()

	// 粘性绑定指向该账号（失效传播消费点）。
	binding := &domain.SessionBinding{
		SessionKey: "sess-refresh", ModelID: env.modelID, AccountID: accountID,
		Status: domain.BindingActive, ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := env.store.InsertSessionBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}

	env.vendor.push(stubResponse{status: 400, body: `{"error":"invalid_grant"}`})
	w := NewCredentialRefresh(env.store, env.refresher(), CredentialRefreshConfig{}, nil)
	m, err := w.RunPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReauthRequired != 1 || m.Rotated != 0 {
		t.Fatalf("expected reauth outcome: %+v", m)
	}
	var acctStatus, bindStatus string
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	env.db.QueryRow(`SELECT status FROM inference_session_bindings WHERE id = $1`, binding.ID).Scan(&bindStatus)
	if acctStatus != "reauth_required" || bindStatus != "ended" {
		t.Fatalf("propagation failed: acct=%s binding=%s", acctStatus, bindStatus)
	}
	acts, err := env.store.ListActiveUpstreamAccounts(ctx, env.provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("reauth account must stop receiving new dispatches: %d", len(acts))
	}
}

// TestCredentialRefreshWorkerConnectorDown — 连接器进程级不可达（连接拒
// 绝）：只记可重试失败，账号保持 active。
func TestCredentialRefreshWorkerConnectorDown(t *testing.T) {
	env := newRefreshEnv(t)
	_, accountID := env.seedExpiringCredential(t)

	// 关掉 vendor 再刷新 = 传输层失败。
	env.vendor.srv.Close()
	w := NewCredentialRefresh(env.store, env.refresher(), CredentialRefreshConfig{}, nil)
	m, err := w.RunPass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Retryable != 1 || m.Rotated != 0 || m.ReauthRequired != 0 {
		t.Fatalf("connector-down must be retryable only: %+v", m)
	}
	var acctStatus string
	var gen int64
	env.db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	env.db.QueryRow(`SELECT generation FROM inference_credentials c JOIN inference_upstream_accounts a ON a.credential_id = c.id WHERE a.id = $1`, accountID).Scan(&gen)
	if acctStatus != "active" || gen != 1 {
		t.Fatalf("connector down must not touch state: acct=%s gen=%d", acctStatus, gen)
	}
}
