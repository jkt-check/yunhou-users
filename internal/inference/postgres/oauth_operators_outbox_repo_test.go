package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// oauth_operators_outbox_repo_test.go — oauth_repo / operators_repo /
// outbox_repo 的真实库行为（Task 16 覆盖率补强）。行为此前由
// credentials/workers 包间接覆盖（不计入本包覆盖率）；这里钉的是持久面
// 本身的语义：一次性授权消费、CAS 轮换、条件状态翻转、quota 单调守卫、
// 会话绑定生命周期、outbox 幂等/退避、角色与审计读取面。

// seedOAuthChain seeds provider + credential（可带 connector/过期）+ 上游账号。
func seedOAuthChain(t *testing.T, s *Store, connector string, expiresAt *time.Time) (provID, credID, acctID string) {
	t.Helper()
	ctx := context.Background()
	prov := &domain.Provider{Code: "p-" + uuid.NewString()[:8], DisplayName: "P",
		AccessType: domain.AccessOAuthConnector, Status: "active"}
	if err := s.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: uuid.NewString(), ProviderID: prov.ID, Label: "main", AuthType: "oauth",
		Ciphertext: []byte("ct"), KeyVersion: 1, Generation: 1, Connector: connector, ExpiresAt: expiresAt,
	}
	if err := s.InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{
		ProviderID: prov.ID, CredentialID: cred.ID, Status: domain.AccountActive, ConcurrencyLimit: 4,
	}
	if err := s.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	return prov.ID, cred.ID, acct.ID
}

// 评审轮1 M2：部分观测快照不得抹掉已知字段——只带 remaining 的探测落库
// 后，已知的 limit/耗尽视图必须保留（COALESCE($n, 旧值) 口径）。
func TestUpdateUpstreamAccountQuota_PartialSnapshotKeepsKnownFields(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	_, _, acctID := seedOAuthChain(t, s, "vendor-x", nil)

	t1 := time.Now().UTC()
	lim := domain.Microcredit(1000)
	rem := domain.Microcredit(800)
	src := "reported"
	ok, err := s.UpdateUpstreamAccountQuota(ctx, acctID, domain.UpstreamQuota{
		LimitMicros: &lim, RemainingMicros: &rem, ObservedAt: &t1, Source: &src,
	})
	if err != nil || !ok {
		t.Fatalf("full snapshot = %v/%v", ok, err)
	}

	// 部分快照：只带 remaining=0（耗尽观测），不带 limit/source。
	t2 := t1.Add(time.Minute)
	zero := domain.Microcredit(0)
	ok, err = s.UpdateUpstreamAccountQuota(ctx, acctID, domain.UpstreamQuota{
		RemainingMicros: &zero, ObservedAt: &t2,
	})
	if err != nil || !ok {
		t.Fatalf("partial snapshot = %v/%v", ok, err)
	}
	var gotLim, gotRem *int64
	var gotSrc *string
	if err := s.db.QueryRow(
		`SELECT quota_limit_micros, quota_remaining_micros, quota_source
		 FROM inference_upstream_accounts WHERE id = $1`, acctID).
		Scan(&gotLim, &gotRem, &gotSrc); err != nil {
		t.Fatal(err)
	}
	if gotLim == nil || *gotLim != 1000 {
		t.Errorf("limit = %v, want 1000 preserved (部分快照不得清空已知字段)", gotLim)
	}
	if gotRem == nil || *gotRem != 0 {
		t.Errorf("remaining = %v, want 0 (partial observation lands its own fields)", gotRem)
	}
	if gotSrc == nil || *gotSrc != "reported" {
		t.Errorf("source = %v, want reported preserved", gotSrc)
	}

	// 对称：只带 limit 的快照不清空已知 remaining=0 的耗尽状态。
	t3 := t2.Add(time.Minute)
	lim2 := domain.Microcredit(2000)
	ok, err = s.UpdateUpstreamAccountQuota(ctx, acctID, domain.UpstreamQuota{
		LimitMicros: &lim2, ObservedAt: &t3,
	})
	if err != nil || !ok {
		t.Fatalf("limit-only snapshot = %v/%v", ok, err)
	}
	if err := s.db.QueryRow(
		`SELECT quota_limit_micros, quota_remaining_micros
		 FROM inference_upstream_accounts WHERE id = $1`, acctID).
		Scan(&gotLim, &gotRem); err != nil {
		t.Fatal(err)
	}
	if gotLim == nil || *gotLim != 2000 || gotRem == nil || *gotRem != 0 {
		t.Errorf("limit-only snapshot: limit=%v remaining=%v, want 2000/0 (耗尽状态不被抹掉)", gotLim, gotRem)
	}
}

func TestOAuthGrant_ConsumeOnceAndExpiry(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	provID, _, _ := seedOAuthChain(t, s, "vendor-x", nil)

	now := time.Now().UTC()
	opUser := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, opUser); err != nil {
		t.Fatal(err)
	}
	g := &domain.OAuthGrant{
		State: "st-" + uuid.NewString(), Connector: "vendor-x", ProviderID: provID,
		AccountLabel: "main", CodeVerifier: "verifier-secret",
		OperatorUserID: opUser, OperatorAppID: "ops",
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := s.InsertOAuthGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	// Tx 变体覆盖。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g2 := &domain.OAuthGrant{
		State: "st2-" + uuid.NewString(), Connector: "vendor-x", ProviderID: provID,
		CodeVerifier: "v2", OperatorUserID: g.OperatorUserID, OperatorAppID: "ops",
		ExpiresAt: now.Add(-time.Minute), // 已过期
	}
	if err := s.InsertOAuthGrantTx(ctx, uow, g2); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.ConsumeOAuthGrant(ctx, g.State, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != g.ID || got.CodeVerifier != "verifier-secret" {
		t.Fatalf("consumed grant = %+v", got)
	}
	var consumed bool
	if err := s.db.Get(&consumed,
		`SELECT consumed_at IS NOT NULL FROM inference_oauth_grants WHERE id = $1`, g.ID); err != nil {
		t.Fatal(err)
	}
	if !consumed {
		t.Fatal("grant not marked consumed in DB")
	}
	// 一次性：第二次消费冲突（先消费后验身份——消费动作已发生才报错）。
	if _, err := s.ConsumeOAuthGrant(ctx, g.State, now); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second consume = %v, want conflict (一次性)", err)
	}
	// 过期授权不得消费。
	if _, err := s.ConsumeOAuthGrant(ctx, g2.State, now); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expired consume = %v, want conflict", err)
	}
}

func TestCredentialRotateCAS_GenerationGuard(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	_, credID, _ := seedOAuthChain(t, s, "vendor-x", nil)

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireCredentialRefreshLockTx(ctx, uow, credID); err != nil {
		t.Fatal(err)
	}
	// 锁内重读 + CAS 写：generation 0 → 1。
	got, err := s.GetCredentialTx(ctx, uow, credID)
	if err != nil {
		t.Fatal(err)
	}
	newGen, err := s.RotateCredentialSecretCAS(ctx, uow, credID, []byte("ct2"), 1, got.Generation, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newGen != got.Generation+1 {
		t.Fatalf("generation = %d, want %d", newGen, got.Generation+1)
	}
	// 陈旧代次 → 冲突（旧 token 晚返回不得覆盖）。
	if _, err := s.RotateCredentialSecretCAS(ctx, uow, credID, []byte("ct3"), 1, got.Generation, nil); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale CAS = %v, want conflict", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRepo_ExpiringScanAndStatusAndQuota(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	soon := time.Now().UTC().Add(time.Hour)
	provID, credID, acctID := seedOAuthChain(t, s, "vendor-x", &soon)
	// 非 oauth 凭据不进刷新扫描集。
	_, otherCredID, _ := seedOAuthChain(t, s, "", &soon)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inference_credentials SET auth_type = 'api_key' WHERE id = $1`, otherCredID); err != nil {
		t.Fatal(err)
	}

	creds, err := s.ListOAuthCredentialsExpiring(ctx, time.Now().UTC().Add(2*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].ID != credID {
		t.Fatalf("expiring scan = %d rows, want exactly the connected credential", len(creds))
	}
	// 账号不可调度（disabled）→ 凭据移出扫描集（Task 13 M-4）。
	ok, err := s.SetUpstreamAccountStatusConditional(ctx, acctID,
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountDisabled)
	if err != nil || !ok {
		t.Fatalf("conditional status = %v/%v", ok, err)
	}
	// 条件不匹配 → false。
	ok, err = s.SetUpstreamAccountStatusConditional(ctx, acctID,
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountDisabled)
	if err != nil || ok {
		t.Fatalf("conditional from-mismatch = %v/%v, want false", ok, err)
	}
	creds, err = s.ListOAuthCredentialsExpiring(ctx, time.Now().UTC().Add(2*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 0 {
		t.Fatalf("scan after account disabled = %d, want 0 (M-4 传播)", len(creds))
	}

	// ByCredentialTx 批量翻转 + 账号恢复后回到扫描集。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.SetUpstreamAccountsStatusByCredentialTx(ctx, uow, credID,
		[]domain.UpstreamAccountStatus{domain.AccountDisabled}, domain.AccountActive)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != acctID {
		t.Fatalf("by-credential flip = %v", ids)
	}
	// Tx 单账号变体。
	ok, err = s.SetUpstreamAccountStatusConditionalTx(ctx, uow, acctID,
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountActive)
	if err != nil || !ok {
		t.Fatalf("tx conditional = %v/%v", ok, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	creds, err = s.ListOAuthCredentialsExpiring(ctx, time.Now().UTC().Add(2*time.Hour), 100)
	if err != nil || len(creds) != 1 {
		t.Fatalf("scan after account restored = %d/%v, want 1", len(creds), err)
	}

	// quota 单调守卫：旧 observed_at 不得覆盖新观测。
	now := time.Now().UTC()
	rem := domain.Microcredit(500)
	src := "reported"
	lim := domain.Microcredit(1000)
	ok, err = s.UpdateUpstreamAccountQuota(ctx, acctID, domain.UpstreamQuota{
		LimitMicros: &lim, RemainingMicros: &rem, ObservedAt: &now, Source: &src,
	})
	if err != nil || !ok {
		t.Fatalf("quota update = %v/%v", ok, err)
	}
	stale := now.Add(-time.Minute)
	zero := domain.Microcredit(0)
	ok, err = s.UpdateUpstreamAccountQuota(ctx, acctID, domain.UpstreamQuota{
		RemainingMicros: &zero, ObservedAt: &stale, Source: &src,
	})
	if err != nil || ok {
		t.Fatalf("stale quota update = %v/%v, want false (单调守卫)", ok, err)
	}

	// 健康扫描与账号列表（limit 钳制路径）。
	if _, err := s.ListUpstreamAccountsForHealth(ctx, time.Now().UTC().Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUpstreamAccounts(ctx, provID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list accounts = %d/%v", len(list), err)
	}

	// InsertUpstreamAccountTx 变体（(provider,credential) 唯一 → 裸凭据对）。
	bare := &domain.Credential{
		ID: uuid.NewString(), ProviderID: provID, Label: "bare", AuthType: "api_key",
		Ciphertext: []byte("ct"), KeyVersion: 1, Generation: 1,
	}
	if err := s.InsertCredential(ctx, bare); err != nil {
		t.Fatal(err)
	}
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acct2 := &domain.UpstreamAccount{ProviderID: provID, CredentialID: bare.ID, Status: domain.AccountActive}
	if err := s.InsertUpstreamAccountTx(ctx, uow, acct2); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil || acct2.ID == "" {
		t.Fatalf("tx account insert: %v id=%q", err, acct2.ID)
	}
}

func TestSessionBinding_Lifecycle(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	_, credID, acctID := seedOAuthChain(t, s, "vendor-x", nil)
	_ = credID

	if err := s.InsertModel(ctx, &domain.Model{
		ID: "m1", DisplayName: "M1", ContextTokens: 1000, MaxOutputTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	b := &domain.SessionBinding{
		SessionKey: "sess-1", ModelID: "m1", AccountID: acctID,
		BoundAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := s.InsertSessionBinding(ctx, b); err != nil {
		t.Fatal(err)
	}
	// Tx 变体（第二个 session）。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b2 := &domain.SessionBinding{
		SessionKey: "sess-2", ModelID: "m1", AccountID: acctID,
		BoundAt: now, LastUsedAt: now, ExpiresAt: now.Add(-time.Minute), // 已过期
	}
	if err := s.InsertSessionBindingTx(ctx, uow, b2); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetActiveSessionBinding(ctx, "sess-1", "m1", now)
	if err != nil || got.ID != b.ID {
		t.Fatalf("active binding = %v/%v", got, err)
	}
	if err := s.TouchSessionBinding(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	// 过期绑定不可见。
	if _, err := s.GetActiveSessionBinding(ctx, "sess-2", "m1", now); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired binding = %v, want NotFound", err)
	}
	// End 幂等：第二次 false。
	ok, err := s.EndSessionBinding(ctx, b.ID, "test")
	if err != nil || !ok {
		t.Fatalf("end = %v/%v", ok, err)
	}
	ok, err = s.EndSessionBinding(ctx, b.ID, "test")
	if err != nil || ok {
		t.Fatalf("re-end = %v/%v, want false", ok, err)
	}
	// Tx 变体。
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ok, err = s.EndSessionBindingTx(ctx, uow, b2.ID, "test")
	if err != nil || !ok {
		t.Fatalf("tx end = %v/%v", ok, err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 按账号/凭据批量终止 + 过期清扫。
	for _, sk := range []string{"sess-a", "sess-b"} {
		bb := &domain.SessionBinding{
			SessionKey: sk, ModelID: "m1", AccountID: acctID,
			BoundAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour),
		}
		if err := s.InsertSessionBinding(ctx, bb); err != nil {
			t.Fatal(err)
		}
	}
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.EndSessionBindingsForAccountTx(ctx, uow, acctID, "reauth")
	if err != nil || len(ids) != 2 {
		t.Fatalf("end-for-account = %v/%v", ids, err)
	}
	n, err := s.EndSessionBindingsForCredentialTx(ctx, uow, credID, "revoke")
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("end-for-credential after account-end = %d, want 0 (幂等)", n)
	}
	if _, err := s.EndSessionBindingsForCredential(ctx, credID, "revoke"); err != nil {
		t.Fatal(err)
	}
	swept, err := s.EndExpiredSessionBindings(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	_ = swept // b2 已显式终止；清扫幂等即可
}

func TestOperatorsRepo_RolesAndAudit(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	uid := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatal(err)
	}
	// FindUserIDByEmail（social_identities 解析）。
	email := "op-" + uuid.NewString()[:8] + "@example.com"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO social_identities (user_id, provider, provider_uid, email) VALUES ($1, 'github', $2, $3)`,
		uid, uuid.NewString(), email); err != nil {
		t.Fatal(err)
	}
	found, err := s.FindUserIDByEmail(ctx, email)
	if err != nil || found != uid {
		t.Fatalf("find by email = %q/%v, want %q", found, err, uid)
	}
	if _, err := s.FindUserIDByEmail(ctx, "nobody@example.com"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unknown email = %v, want NotFound", err)
	}

	by := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1) ON CONFLICT DO NOTHING`, by); err != nil {
		t.Fatal(err)
	}
	ok, err := s.GrantRole(ctx, uid, management.RoleOperator, &by, "seed")
	if err != nil || !ok {
		t.Fatalf("grant = %v/%v", ok, err)
	}
	ok, err = s.GrantRole(ctx, uid, management.RoleOperator, &by, "seed")
	if err != nil || ok {
		t.Fatalf("re-grant = %v/%v, want false (幂等)", ok, err)
	}
	roles, err := s.RolesForUser(ctx, uid)
	if err != nil || len(roles) != 1 || roles[0] != management.RoleOperator {
		t.Fatalf("roles = %v/%v", roles, err)
	}
	ops, err := s.ListOperators(ctx)
	if err != nil || len(ops) != 1 || ops[0].UserID != uid || ops[0].GrantedBy == nil || *ops[0].GrantedBy != by {
		t.Fatalf("operators = %+v/%v", ops, err)
	}
	ok, err = s.RevokeRole(ctx, uid, management.RoleOperator)
	if err != nil || !ok {
		t.Fatalf("revoke = %v/%v", ok, err)
	}
	ok, err = s.RevokeRole(ctx, uid, management.RoleOperator)
	if err != nil || ok {
		t.Fatalf("re-revoke = %v/%v, want false", ok, err)
	}

	// 审计：Record + RecordTx + ListAudit 过滤。
	ev := management.AuditEvent{
		Action: "credential.rotate", ObjectType: "credential", ObjectID: "c-1",
		Reason: "rotation", ActorUser: uid, ActorApp: "ops",
		Detail: map[string]any{"note": "n1"},
	}
	if err := s.Record(ctx, ev); err != nil {
		t.Fatal(err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev2 := ev
	ev2.Action = "credential.create"
	if err := s.RecordTx(ctx, uow, ev2); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListAudit(ctx, "credential.", "", "", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit prefix filter = %d/%v", len(entries), err)
	}
	entries, err = s.ListAudit(ctx, "", "credential", "c-1", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit object filter = %d/%v", len(entries), err)
	}
	if entries[0].Action != "credential.create" || entries[0].ActorUserID == nil || *entries[0].ActorUserID != uid {
		t.Fatalf("audit newest-first = %+v", entries[0])
	}
	entries, err = s.ListAudit(ctx, "wallet.", "", "", 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("audit miss filter = %d/%v", len(entries), err)
	}
}

func TestOutboxRepo_EnqueueFetchDeliverRetry(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()

	dedup := "dk-" + uuid.NewString()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id1, err := s.EnqueueOutbox(ctx, uow, "entitlement.sync", json.RawMessage(`{"n":1}`), &dedup)
	if err != nil || id1 == 0 {
		t.Fatalf("enqueue = %d/%v", id1, err)
	}
	// 同 dedup 键 → 幂等 no-op（id=0）。
	id2, err := s.EnqueueOutbox(ctx, uow, "entitlement.sync", json.RawMessage(`{"n":1}`), &dedup)
	if err != nil || id2 != 0 {
		t.Fatalf("dedup enqueue = %d/%v, want 0 (幂等)", id2, err)
	}
	if _, err := s.EnqueueOutbox(ctx, uow, "wallet.sync", json.RawMessage(`{"n":2}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 裸 *sqlx.Tx 变体 + nil 守卫。
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueOutboxSQLTx(ctx, tx, "entitlement.sync", json.RawMessage(`{"n":3}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueOutboxSQLTx(ctx, nil, "x", nil, nil); err == nil {
		t.Fatal("nil tx must error")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 按 topic 拉取 oldest-first。
	msgs, err := s.FetchPendingOutboxByTopic(ctx, "entitlement.sync", 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("fetch by topic = %d/%v, want 2 (dedup 吸收一条)", len(msgs), err)
	}
	if msgs[0].ID != id1 {
		t.Fatalf("oldest-first: first = %d, want %d", msgs[0].ID, id1)
	}
	all, err := s.FetchPendingOutbox(ctx, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("fetch all = %d/%v", len(all), err)
	}

	// 失败退避：next_retry 在未来 → 不可拉取；时间到 → 再现。
	if err := s.MarkOutboxFailed(ctx, id1, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	msgs, err = s.FetchPendingOutboxByTopic(ctx, "entitlement.sync", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch after backoff = %d/%v, want 1 (退避中不可见)", len(msgs), err)
	}
	if err := s.MarkOutboxFailed(ctx, id1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	msgs, err = s.FetchPendingOutboxByTopic(ctx, "entitlement.sync", 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("fetch after retry due = %d/%v, want 2", len(msgs), err)
	}

	// 投递完成（含 Tx 变体）→ 不再出现。
	if err := s.MarkOutboxDelivered(ctx, id1); err != nil {
		t.Fatal(err)
	}
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkOutboxDeliveredTx(ctx, uow, all[2].ID); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, err = s.FetchPendingOutboxByTopic(ctx, "entitlement.sync", 10)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("fetch after delivered = %d/%v, want 0", len(msgs), err)
	}
}

func TestOutboxRepo_PaymentReadFaces(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()

	uid := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, product_code, currency)
		VALUES ('cp_read', 'CP Read', 29.9, 30, 'coding-plan', 'CNY')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	// 无订阅 → NotFound。
	if _, err := s.GetSyncSubscription(ctx, uid, "coding-plan"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("sync sub empty = %v, want NotFound", err)
	}
	exp := time.Now().UTC().Add(30 * 24 * time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, product_code, expires_at)
		VALUES ($1, 'cp_read', 'active', 'coding-plan', $2)`, uid, exp); err != nil {
		t.Fatal(err)
	}
	sub, err := s.GetSyncSubscription(ctx, uid, "coding-plan")
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanID != "cp_read" || sub.Status != "active" || sub.ExpiresAt == nil {
		t.Fatalf("sync sub = %+v", sub)
	}
	// 产品隔离：查 kaya-membership 不得返回 coding-plan 订阅。
	if _, err := s.GetSyncSubscription(ctx, uid, "kaya-membership"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("sync sub wrong product = %v, want NotFound", err)
	}

	// 最近已支付权益订单快照。
	if _, err := s.GetLatestPaidBenefitOrder(ctx, uid, "cp_read"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("benefit order empty = %v, want NotFound", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders (user_id, plan_id, amount, status, benefit_policy_version_id, benefit_model_ids, benefit_grant_mode)
		VALUES ($1, 'cp_read', 29.9, 'paid', $2, '{glm-4.6}', 'subscription')`, uid, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	snap, err := s.GetLatestPaidBenefitOrder(ctx, uid, "cp_read")
	if err != nil {
		t.Fatal(err)
	}
	if snap.OrderID == "" || snap.PolicyVersionID == "" || len(snap.ModelIDs) != 1 || snap.ModelIDs[0] != "glm-4.6" || snap.GrantMode != "subscription" {
		t.Fatalf("benefit snapshot = %+v", snap)
	}
}
