// refresh_db_test.go — Task 12 刷新协议真库验收：分布式刷新锁 +
// generation CAS（双实例同时刷新、旧 token 晚返回不覆盖新 token）、
// invalid_grant → reauth_required 传播（停止分配 + 终止绑定）。

package credentials_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

// seedOAuthCredential stores one oauth credential (bundle via vault) + one
// active account; returns credential/account ids.
func seedOAuthCredential(t *testing.T, db *sqlx.DB, vault *credentials.Vault, providerID, refreshToken string, expiry time.Time) (credID, accountID string) {
	t.Helper()
	ctx := context.Background()
	id := newTestUUID(t)
	bundle, err := credentials.MarshalBundle(&credentials.OAuthBundle{
		AccessToken:  "at-old",
		RefreshToken: refreshToken,
		TokenType:    "bearer",
		ObtainedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ct, kv, err := vault.Encrypt(id, providerID, bundle)
	if err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: id, ProviderID: providerID, Label: "oauth-cred", AuthType: "oauth",
		Ciphertext: ct, KeyVersion: kv, Generation: 1, ExpiresAt: &expiry,
		Status: "active", Connector: "testvendor",
	}
	if err := postgres.NewStore(db).InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{
		ProviderID: providerID, CredentialID: id, ExternalAccountID: "vendor-acct-1",
		DisplayName: "acct", Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := postgres.NewStore(db).InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	return id, acct.ID
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func readBundle(t *testing.T, db *sqlx.DB, vault *credentials.Vault, credID string) *credentials.OAuthBundle {
	t.Helper()
	cred, err := postgres.NewStore(db).GetCredential(context.Background(), credID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vault.Decrypt(cred.ID, cred.ProviderID, cred.KeyVersion, cred.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	b, err := credentials.UnmarshalBundle(plain)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRefreshRotatesWithGenerationCAS(t *testing.T) {
	db, _, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute) // 即将到期
	credID, _ := seedOAuthCredential(t, db, vault, providerID, "rt-old", expiry)

	r := credentials.NewRefresher(vault, postgres.NewStore(db), postgres.NewStore(db),
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	outcome, err := r.RefreshCredential(ctx, credID, "test rotation")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Rotated || outcome.Generation != 2 {
		t.Fatalf("expected rotation to generation 2: %+v", outcome)
	}
	b := readBundle(t, db, vault, credID)
	if b.AccessToken != "at-refreshed" || b.RefreshToken != "rt-new" {
		t.Fatalf("bundle not rotated: %+v", b)
	}
	var gen int64
	var newExpiry time.Time
	if err := db.QueryRow(`SELECT generation, expires_at FROM inference_credentials WHERE id = $1`, credID).Scan(&gen, &newExpiry); err != nil {
		t.Fatal(err)
	}
	if gen != 2 || !newExpiry.After(time.Now().Add(30*time.Minute)) {
		t.Fatalf("generation/expiry not updated: gen=%d exp=%v", gen, newExpiry)
	}
	var n int
	db.Get(&n, `SELECT count(*) FROM inference_audit_log WHERE action = 'oauth.refresh.rotated' AND object_id = $1`, credID)
	if n != 1 {
		t.Fatalf("missing rotation audit: %d", n)
	}
}

// TestRefreshOldTokenLateMustNotOverwrite — 验收场景"旧 token 返回晚于新
// token"：实例 B 先发起刷新（厂商慢），实例 A 后发起但先完成
// （generation 1→2）；B 的旧响应晚返回，CAS 必须拒绝写入并收敛。
func TestRefreshOldTokenLateMustNotOverwrite(t *testing.T) {
	db, _, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute)
	credID, _ := seedOAuthCredential(t, db, vault, providerID, "rt-old", expiry)

	// 厂商行为编排：第 1 个 refresh 调用（B 的）阻塞到 release；之后的调用
	// 立即返回。每个调用返回唯一 token，便于判定最终落库的是谁的。
	release := make(chan struct{})
	var callMu sync.Mutex
	callN := 0
	vendor.mu.Lock()
	vendor.tokenHandler = func(form url.Values) (int, string) {
		callMu.Lock()
		callN++
		n := callN
		callMu.Unlock()
		if n == 1 {
			<-release // B 的旧刷新挂起
			return 200, `{"access_token":"at-B-old-late","refresh_token":"rt-B","token_type":"bearer","expires_in":3600}`
		}
		return 200, `{"access_token":"at-A-new","refresh_token":"rt-A","token_type":"bearer","expires_in":3600}`
	}
	vendor.mu.Unlock()

	// 实例 B：独立连接池（模拟另一台机器/进程）。
	db2, err := sqlx.Connect("postgres", atomicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	refresherB := credentials.NewRefresher(vault, postgres.NewStore(db2), postgres.NewStore(db2),
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	refresherA := credentials.NewRefresher(vault, postgres.NewStore(db), postgres.NewStore(db),
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)

	type result struct {
		outcome *credentials.RefreshOutcome
		err     error
	}
	bDone := make(chan result, 1)
	go func() {
		o, err := refresherB.RefreshCredential(ctx, credID, "B refresh")
		bDone <- result{o, err}
	}()

	// 等 B 的厂商调用确实在途（handler 已计数到 1）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		callMu.Lock()
		n := callN
		callMu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("B never reached the vendor")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A 全程跑完：厂商立即返回 → CAS 1→2 提交。
	outA, err := refresherA.RefreshCredential(ctx, credID, "A refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !outA.Rotated || outA.Generation != 2 {
		t.Fatalf("A must rotate to generation 2: %+v", outA)
	}

	// 放行 B 的旧响应：它晚于 A 的新 token 返回。
	close(release)
	resB := <-bDone
	if resB.err != nil {
		t.Fatalf("B must converge, not error: %v", resB.err)
	}
	if !resB.outcome.Converged || resB.outcome.Rotated {
		t.Fatalf("B must converge without rotating: %+v", resB.outcome)
	}

	// 终态：generation=2，密文是 A 的新 token；B 的旧 token 已被丢弃。
	var gen int64
	if err := db.Get(&gen, `SELECT generation FROM inference_credentials WHERE id = $1`, credID); err != nil {
		t.Fatal(err)
	}
	if gen != 2 {
		t.Fatalf("stale refresh overwrote newer token: generation=%d", gen)
	}
	b := readBundle(t, db, vault, credID)
	if b.AccessToken != "at-A-new" {
		t.Fatalf("old token won the race: %+v", b)
	}
}

// TestRefreshTwoInstancesSimultaneous — 验收场景"两个实例同时刷新"：
// 双方都从 generation=1 发起；advisory 锁 + CAS 保证恰有一个赢家。
func TestRefreshTwoInstancesSimultaneous(t *testing.T) {
	db, _, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute)
	credID, _ := seedOAuthCredential(t, db, vault, providerID, "rt-old", expiry)

	// 两个厂商调用都在途后才放行：双方都以 g0=1 进入写阶段。
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	vendor.mu.Lock()
	vendor.tokenHandler = func(form url.Values) (int, string) {
		arrived <- struct{}{}
		<-release
		return 200, `{"access_token":"at-race","refresh_token":"rt-race","token_type":"bearer","expires_in":3600}`
	}
	vendor.mu.Unlock()

	db2, err := sqlx.Connect("postgres", atomicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	mk := func(d *sqlx.DB) *credentials.Refresher {
		return credentials.NewRefresher(vault, postgres.NewStore(d), postgres.NewStore(d),
			&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	}
	var wg sync.WaitGroup
	outcomes := make([]*credentials.RefreshOutcome, 2)
	errs := make([]error, 2)
	for i, r := range []*credentials.Refresher{mk(db), mk(db2)} {
		wg.Add(1)
		go func(i int, r *credentials.Refresher) {
			defer wg.Done()
			outcomes[i], errs[i] = r.RefreshCredential(ctx, credID, "concurrent refresh")
		}(i, r)
	}
	<-arrived
	<-arrived
	close(release)
	wg.Wait()

	rotated, converged := 0, 0
	for i := range outcomes {
		if errs[i] != nil {
			t.Fatalf("instance %d errored: %v", i, errs[i])
		}
		switch {
		case outcomes[i].Rotated:
			rotated++
		case outcomes[i].Converged:
			converged++
		}
	}
	if rotated != 1 || converged != 1 {
		t.Fatalf("exactly one winner expected: rotated=%d converged=%d", rotated, converged)
	}
	var gen int64
	db.Get(&gen, `SELECT generation FROM inference_credentials WHERE id = $1`, credID)
	if gen != 2 {
		t.Fatalf("double rotation lost update: generation=%d", gen)
	}
}

// TestRefreshInvalidGrantMovesAccountsToReauth — 验收场景"授权撤销/过期"
// 与"绑定账号失效"：invalid_grant → 账号 reauth_required（停止分配新请
// 求）+ 会话绑定终止，同事务 + 审计。
func TestRefreshInvalidGrantMovesAccountsToReauth(t *testing.T) {
	db, store, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute)
	credID, accountID := seedOAuthCredential(t, db, vault, providerID, "rt-revoked", expiry)

	// 先建一条粘性会话绑定指向该账号（绑定失效传播的消费点）。
	if _, err := db.Exec(`INSERT INTO inference_models
		(id, display_name, context_tokens, max_output_tokens)
		VALUES ('task12-model','M',128000,8192) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DELETE FROM inference_models WHERE id = 'task12-model'`)
	binding := &domain.SessionBinding{
		SessionKey: "sess-1", ModelID: "task12-model", AccountID: accountID,
		Status: domain.BindingActive, ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.InsertSessionBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}

	vendor.mu.Lock()
	vendor.tokenHandler = func(form url.Values) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"refresh token revoked"}`
	}
	vendor.mu.Unlock()

	r := credentials.NewRefresher(vault, store, store,
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	outcome, err := r.RefreshCredential(ctx, credID, "scheduled refresh")
	if err != nil {
		t.Fatalf("definitive rejection must be an outcome, not an error: %v", err)
	}
	if !outcome.ReauthRequired {
		t.Fatalf("expected reauth outcome: %+v", outcome)
	}

	var acctStatus string
	db.Get(&acctStatus, `SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID)
	if acctStatus != "reauth_required" {
		t.Fatalf("account not reauth_required: %s", acctStatus)
	}
	// 停止分配新请求：active 池为空。
	acts, err := store.ListActiveUpstreamAccounts(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("reauth account still schedulable: %d", len(acts))
	}
	// 会话绑定已终止（可迁移但先终止）。
	var bindStatus, reason string
	db.QueryRow(`SELECT status, ended_reason FROM inference_session_bindings WHERE id = $1`, binding.ID).Scan(&bindStatus, &reason)
	if bindStatus != "ended" || reason != domain.BindingEndedAccountInvalid {
		t.Fatalf("binding not terminated: %s %s", bindStatus, reason)
	}
	// 凭据本身不被改写（revoke 是运营动作，不是刷新器动作）。
	var credStatus string
	var gen int64
	db.QueryRow(`SELECT status, generation FROM inference_credentials WHERE id = $1`, credID).Scan(&credStatus, &gen)
	if credStatus != "active" || gen != 1 {
		t.Fatalf("credential must be untouched by reauth path: %s gen=%d", credStatus, gen)
	}
	var n int
	db.Get(&n, `SELECT count(*) FROM inference_audit_log WHERE action = 'oauth.refresh.reauth_required' AND object_id = $1`, credID)
	if n != 1 {
		t.Fatalf("missing reauth audit: %d", n)
	}
}

// TestRefreshConnectorUnavailableIsRetryable — 验收场景"连接器不可用"：
// 传输错误/5xx 只重试，不动账号状态（连接器暂时不可用 ≠ 授权失效）。
func TestRefreshConnectorUnavailableIsRetryable(t *testing.T) {
	db, store, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute)
	credID, accountID := seedOAuthCredential(t, db, vault, providerID, "rt-old", expiry)

	vendor.mu.Lock()
	vendor.tokenHandler = func(form url.Values) (int, string) {
		return 503, `{"error":"temporarily_unavailable"}`
	}
	vendor.mu.Unlock()

	r := credentials.NewRefresher(vault, store, store,
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	_, err := r.RefreshCredential(ctx, credID, "retry me")
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("retryable failure must surface as upstream_unavailable, got %v", err)
	}
	var acctStatus string
	var gen int64
	db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	db.QueryRow(`SELECT generation FROM inference_credentials WHERE id = $1`, credID).Scan(&gen)
	if acctStatus != "active" || gen != 1 {
		t.Fatalf("retryable failure must not touch state: acct=%s gen=%d", acctStatus, gen)
	}
}

// TestRefreshAccessTokenOnlyGrant — 无 refresh token 的授权不轮换、不报错。
func TestRefreshAccessTokenOnlyGrant(t *testing.T) {
	db, store, _, _, vault, _, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute)
	credID, _ := seedOAuthCredential(t, db, vault, providerID, "", expiry)

	r := credentials.NewRefresher(vault, store, store, &connector.Client{}, connector.Registry{}, nil)
	outcome, err := r.RefreshCredential(ctx, credID, "no-op")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Rotated || outcome.Converged || outcome.ReauthRequired {
		t.Fatalf("access-token-only grant must no-op: %+v", outcome)
	}
}

// TestRefreshStaleInvalidGrantMustNotFlip — 审查修复 C-1：RT 轮换型厂商
// （一次成功即作废旧 RT）。实例 A、B 同以 g0=1/rt-old 调厂商；A 先成功
// （g 1→2，rt-old 作废），B 后收到针对旧 RT 的 invalid_grant——锁内重读
// 代次必须识别"失败信号针对的是已被替换的凭据"，按 Converged 返回，
// 绝不翻转账号/终止绑定。
func TestRefreshStaleInvalidGrantMustNotFlip(t *testing.T) {
	db, store, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expiry := time.Now().Add(2 * time.Minute)
	credID, accountID := seedOAuthCredential(t, db, vault, providerID, "rt-old", expiry)

	// RT 轮换语义厂商：rt-old 只有一次成功机会，成功即作废。编排上 B 的
	// 请求先到（第 1 个 rt-old 调用）并挂起；A 的请求（第 2 个）立即成功
	// 并作废 rt-old；B 放行时读到的是 invalid_grant。
	var vendorMu sync.Mutex
	rtOldCalls := 0
	bRelease := make(chan struct{})
	bArrived := make(chan struct{}, 1)
	vendor.mu.Lock()
	vendor.tokenHandler = func(form url.Values) (int, string) {
		rt := form.Get("refresh_token")
		if rt != "rt-old" {
			return 200, `{"access_token":"at-later","refresh_token":"rt-newer","token_type":"bearer","expires_in":3600}`
		}
		vendorMu.Lock()
		rtOldCalls++
		n := rtOldCalls
		vendorMu.Unlock()
		if n == 1 {
			// B：挂起直到 A 提交；放行时 rt-old 已被 A 作废。
			bArrived <- struct{}{}
			<-bRelease
			return 400, `{"error":"invalid_grant","error_description":"refresh token already rotated"}`
		}
		// A：成功轮换并作废 rt-old。
		return 200, `{"access_token":"at-A","refresh_token":"rt-new","token_type":"bearer","expires_in":3600}`
	}
	vendor.mu.Unlock()

	// 编排：B 先到厂商（挂起），A 后跑完整流程。
	db2, err := sqlx.Connect("postgres", atomicDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	refresherA := credentials.NewRefresher(vault, postgres.NewStore(db), postgres.NewStore(db),
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	refresherB := credentials.NewRefresher(vault, postgres.NewStore(db2), postgres.NewStore(db2),
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)

	type result struct {
		outcome *credentials.RefreshOutcome
		err     error
	}
	bDone := make(chan result, 1)
	go func() {
		o, err := refresherB.RefreshCredential(ctx, credID, "B refresh")
		bDone <- result{o, err}
	}()
	select {
	case <-bArrived: // B 的 rt-old 请求已在厂商在途
	case <-time.After(5 * time.Second):
		t.Fatal("B never reached the vendor")
	}

	// A：rt-old 仍有效 → 成功轮换 g 1→2，rt-old 作废。
	outA, err := refresherA.RefreshCredential(ctx, credID, "A refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !outA.Rotated || outA.Generation != 2 {
		t.Fatalf("A must rotate to generation 2: %+v", outA)
	}

	// 放行 B：rt-old 已作废 → invalid_grant。修复前 B 会把账号翻
	// reauth_required；修复后必须 Converged。
	close(bRelease)
	resB := <-bDone
	if resB.err != nil {
		t.Fatalf("B must converge, not error: %v", resB.err)
	}
	if resB.outcome.ReauthRequired {
		t.Fatal("C-1 regression: stale invalid_grant flipped accounts to reauth_required")
	}
	if !resB.outcome.Converged {
		t.Fatalf("B must converge on the moved generation: %+v", resB.outcome)
	}

	// 终态：账号 active、generation=2、无 B 产生的 reauth 审计。
	var acctStatus string
	var gen int64
	db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	db.QueryRow(`SELECT generation FROM inference_credentials WHERE id = $1`, credID).Scan(&gen)
	if acctStatus != "active" || gen != 2 {
		t.Fatalf("stale invalid_grant must not touch state: acct=%s gen=%d", acctStatus, gen)
	}
	var n int
	db.Get(&n, `SELECT count(*) FROM inference_audit_log WHERE action = 'oauth.refresh.reauth_required' AND object_id = $1`, credID)
	if n != 0 {
		t.Fatalf("stale rejection must not audit reauth: %d", n)
	}
	// 账号仍在可调度池。
	acts, err := store.ListActiveUpstreamAccounts(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 {
		t.Fatalf("account must stay schedulable: %d", len(acts))
	}
}

// TestRefreshAccessTokenOnlyExpiredGoesReauth — 审查修复 I-1：无 RT 且
// access token 已过期 = 授权失效，走 reauth 传播（停止分配 + 终止绑定）。
func TestRefreshAccessTokenOnlyExpiredGoesReauth(t *testing.T) {
	db, store, _, _, vault, vendor, providerID := oauthSetup(t)
	ctx := context.Background()

	expired := time.Now().Add(-time.Minute)
	credID, accountID := seedOAuthCredential(t, db, vault, providerID, "", expired)

	r := credentials.NewRefresher(vault, store, store,
		&connector.Client{HTTP: vendor.srv.Client()}, vendor.registry(), nil)
	outcome, err := r.RefreshCredential(ctx, credID, "expired access-token-only")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.ReauthRequired {
		t.Fatalf("expired access-token-only grant must reauth: %+v", outcome)
	}
	var acctStatus string
	db.QueryRow(`SELECT status FROM inference_upstream_accounts WHERE id = $1`, accountID).Scan(&acctStatus)
	if acctStatus != "reauth_required" {
		t.Fatalf("account must leave the pool: %s", acctStatus)
	}
	acts, err := store.ListActiveUpstreamAccounts(ctx, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("dead-token account must not be schedulable: %d", len(acts))
	}
}
