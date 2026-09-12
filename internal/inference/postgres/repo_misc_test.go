package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// repo_misc_test.go — Task 16 覆盖率补强：reconciliation（CorrectSettlement
// 冲正补差/任务读取面）、catalog_repo（分页/检索/更新/删除）、
// accounts_repo（凭据与账号读取面/Tx 变体）的真实库行为。

// ---------------------------------------------------------------------------
// reconciliation：CorrectSettlement 冲正补差 + 任务读取面
// ---------------------------------------------------------------------------

func TestCorrectSettlement_ReversalAndDelta(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	// 结算 60_000（与 quota_settlement_test 同一链路）。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(800), int64(200)
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 60_000, AttemptID: &attemptID,
		SettledAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 修正到 40_000：冲正 60_000 + 补差 −20_000；窗口 used 60_000→40_000。
	correct := func(key string) error {
		uow, err := s.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = s.CorrectSettlement(ctx, uow, CorrectionCommand{
			RequestID: adm.RequestID,
			Corrected: domain.UsageRecord{
				RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageReported,
				Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
			},
			CorrectedMicros: 40_000, Reason: "上游账单核对后下调",
			OperatorSubject: "user:ops@app:test", IdempotencyKey: key,
		})
		if err != nil {
			_ = uow.Rollback(ctx)
			return err
		}
		return uow.Commit(ctx)
	}
	if err := correct("corr-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{w5, ww, wm} {
		used, reserved := windowState(t, s, id)
		if used != 40_000 || reserved != 0 {
			t.Errorf("window %s after correction: used=%d reserved=%d, want 40000/0", id, used, reserved)
		}
	}
	// 账本（Task 9 补差语义）：charge 60_000 保留 + adjustment(credit)
	// 20_000；reversal 只用于全额作废——此处为 0。净额 40_000。
	var charge, reversal, adjustment int64
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='charge'),0),
		        COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='reversal'),0),
		        COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='adjustment'),0)
		   FROM inference_ledger_entries WHERE request_id = $1`, adm.RequestID).
		Scan(&charge, &reversal, &adjustment); err != nil {
		t.Fatal(err)
	}
	if charge != 60_000 || reversal != 0 || adjustment != 20_000 {
		t.Errorf("ledger = %d/%d/%d, want 60000/0/20000 (补差语义)", charge, reversal, adjustment)
	}
	// 幂等重放：同幂等键 → CodeConflict（调用方视为已应用），状态不动。
	if err := correct("corr-1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replay = %v, want conflict (already applied)", err)
	}
	for _, id := range []string{w5, ww, wm} {
		used, _ := windowState(t, s, id)
		if used != 40_000 {
			t.Errorf("window %s after replay: used=%d, want 40000 (幂等)", id, used)
		}
	}
	// 全额作废（corrected=0）→ reversal 分录冲正当前效应 + 窗口归零。
	voidToZero := func() error {
		uow, err := s.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = s.CorrectSettlement(ctx, uow, CorrectionCommand{
			RequestID: adm.RequestID,
			Corrected: domain.UsageRecord{
				RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageReported,
				Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
			},
			CorrectedMicros: 0, Reason: "确认未执行",
			OperatorSubject: "user:ops@app:test", IdempotencyKey: "corr-void-1",
		})
		if err != nil {
			_ = uow.Rollback(ctx)
			return err
		}
		return uow.Commit(ctx)
	}
	if err := voidToZero(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='reversal'),0)
		   FROM inference_ledger_entries WHERE request_id = $1`, adm.RequestID).
		Scan(&reversal); err != nil {
		t.Fatal(err)
	}
	if reversal != 40_000 {
		t.Errorf("void reversal = %d, want 40000 (当前效应全额冲正)", reversal)
	}
	for _, id := range []string{w5, ww, wm} {
		used, _ := windowState(t, s, id)
		if used != 0 {
			t.Errorf("window %s after void: used=%d, want 0", id, used)
		}
	}
	// 契约：缺原因/操作者/幂等键 → invalid_input。
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CorrectSettlement(ctx, uow, CorrectionCommand{RequestID: adm.RequestID}); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("empty correction = %v, want invalid_input", err)
	}
	_ = uow.Rollback(ctx)
}

func TestReconciliationJob_ReadFaces(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 10_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 入队（Tx 变体）→ pending 列表 → 详情合并 → resolve → 不再出现。
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueReconciliationJobTx(ctx, uow, EnqueueReconciliationCommand{
		RequestID: &adm.RequestID, Reason: "unknown_usage",
		Detail: []byte(`{"drill":true}`), Deadline: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListPendingReconciliationJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("pending jobs = %d/%v", len(jobs), err)
	}
	stale, err := s.ListStaleOpenRequests(ctx, time.Now().UTC().Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale // 请求刚创建不 stale；读取面覆盖即可
	if _, err := s.ListStaleOpenRequests(ctx, time.Now().UTC().Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveReconciliationJob(ctx, jobs[0].ID, "resolved in drill"); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.ListPendingReconciliationJobs(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("pending after resolve = %d/%v", len(jobs), err)
	}
	// 超期升级读取面（无超期任务 → 0）。
	if ids, err := s.EscalateOverdueReconciliationJobs(ctx, time.Now().UTC(), 10); err != nil || len(ids) != 0 {
		t.Fatalf("escalate = %v/%v", ids, err)
	}
}

// ---------------------------------------------------------------------------
// catalog_repo：分页/检索/更新/删除
// ---------------------------------------------------------------------------

func TestCatalogRepo_ListAndMutate(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()

	// Providers：keyset 分页 + GetProvider。
	var provIDs []string
	for i := 0; i < 3; i++ {
		p := &domain.Provider{Code: "p" + uuid.NewString()[:6], DisplayName: "P",
			AccessType: domain.AccessOfficialAPI, Status: "active"}
		if err := s.InsertProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
		provIDs = append(provIDs, p.ID)
	}
	page1, err := s.ListProviders(ctx, "", 2)
	if err != nil || len(page1) != 2 {
		t.Fatalf("providers page1 = %d/%v", len(page1), err)
	}
	page2, err := s.ListProviders(ctx, page1[1].ID, 2)
	if err != nil || len(page2) != 1 {
		t.Fatalf("providers page2 = %d/%v (keyset)", len(page2), err)
	}
	if _, err := s.GetProvider(ctx, provIDs[0]); err != nil {
		t.Fatal(err)
	}

	// Models：过滤分页 + UpdateModel + DeleteModel。
	for _, id := range []string{"m-a", "m-b"} {
		if err := s.InsertModel(ctx, &domain.Model{
			ID: id, DisplayName: id, Lifecycle: domain.LifecycleDraft,
			ContextTokens: 1000, MaxOutputTokens: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	models, err := s.ListModels(ctx, domain.ModelFilter{})
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %d/%v", len(models), err)
	}
	active := domain.LifecycleActive
	models, err = s.ListModels(ctx, domain.ModelFilter{Lifecycle: &active})
	if err != nil || len(models) != 0 {
		t.Fatalf("models lifecycle filter = %d/%v", len(models), err)
	}
	m, err := s.GetModel(ctx, "m-a")
	if err != nil {
		t.Fatal(err)
	}
	m.DisplayName = "m-a2"
	if err := s.UpdateModel(ctx, m); err != nil {
		t.Fatal(err)
	}

	// Deployments：FindDeployment + UpdateDeployment + DeleteDeployment。
	dep := &domain.Deployment{
		ProviderID: provIDs[0], UpstreamModel: "up", BaseURL: "https://api.example.com",
		Protocol: domain.ProtocolOpenAIChat, ConnectTimeout: time.Second, RequestTimeout: time.Second,
		Status: domain.DeploymentDraft, ConfigVersion: 1,
	}
	if err := s.InsertDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	found, err := s.FindDeployment(ctx, provIDs[0], "up", "https://api.example.com")
	if err != nil || found.ID != dep.ID {
		t.Fatalf("find deployment = %v/%v", found, err)
	}
	dep.Status = domain.DeploymentActive // ConfigVersion=1 = 当前版本令牌
	stale := *dep                        // 值拷贝留陈旧版本（首次更新会回写新令牌）
	if err := s.UpdateDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	// 陈旧 config_version（1；库已推进到 2）→ 乐观锁冲突。
	if err := s.UpdateDeployment(ctx, &stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale deployment update = %v, want conflict", err)
	}

	// Routes：GetRoute + UpdateRoute + DeleteRoute。
	rt := &domain.ModelRoute{ModelID: "m-a", DeploymentID: dep.ID, Weight: 1, Enabled: true}
	if err := s.InsertRoute(ctx, rt); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRoute(ctx, rt.ID)
	if err != nil || got.ModelID != "m-a" {
		t.Fatalf("get route = %+v/%v", got, err)
	}
	got.Weight = 3
	got.Enabled = false
	if err := s.UpdateRoute(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRoute(ctx, rt.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(ctx, "m-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProvider(ctx, provIDs[2]); err != nil {
		t.Fatal(err)
	}

	// ActivateRevision + 读面。
	rev := &domain.ConfigRevision{
		Scope: domain.ScopeCatalog, Revision: 1,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: []byte(`{"schema_version":1}`)},
		Status:  domain.RevisionDraft, CreatedBy: "drill",
	}
	if err := s.InsertConfigRevision(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateRevision(ctx, domain.ScopeCatalog, 1); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// accounts_repo：凭据/账号/Key 读取面 + Tx 变体
// ---------------------------------------------------------------------------

func TestAccountsRepo_CredentialAndAccountFaces(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	prov := &domain.Provider{Code: "p-" + uuid.NewString()[:6], DisplayName: "P",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := s.InsertProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	cred := &domain.Credential{
		ID: uuid.NewString(), ProviderID: prov.ID, Label: "main", AuthType: "api_key",
		Ciphertext: []byte("ct"), KeyVersion: 1, Generation: 1,
	}
	if err := s.InsertCredential(ctx, cred); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCredential(ctx, cred.ID)
	if err != nil || got.Label != "main" {
		t.Fatalf("get credential = %+v/%v", got, err)
	}
	creds, err := s.ListCredentials(ctx, prov.ID, 10)
	if err != nil || len(creds) != 1 {
		t.Fatalf("list credentials = %d/%v", len(creds), err)
	}
	// Tx 变体：轮换 + 状态 + 按凭据禁用账号。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateCredentialSecretTx(ctx, uow, cred.ID, []byte("ct2"), 2); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	acct := &domain.UpstreamAccount{ProviderID: prov.ID, CredentialID: cred.ID, Status: domain.AccountActive, ConcurrencyLimit: 2}
	if err := s.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	gotA, err := s.GetUpstreamAccount(ctx, acct.ID)
	if err != nil || gotA.ConcurrencyLimit != 2 {
		t.Fatalf("get account = %+v/%v", gotA, err)
	}
	acts, err := s.ListActiveUpstreamAccounts(ctx, prov.ID)
	if err != nil || len(acts) != 1 {
		t.Fatalf("active accounts = %d/%v", len(acts), err)
	}
	uow, err = s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.DisableUpstreamAccountsByCredentialTx(ctx, uow, cred.ID)
	if err != nil || n != 1 {
		t.Fatalf("disable by credential = %d/%v", n, err)
	}
	if err := s.SetCredentialStatusTx(ctx, uow, cred.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	acts, err = s.ListActiveUpstreamAccounts(ctx, prov.ID)
	if err != nil || len(acts) != 0 {
		t.Fatalf("active after disable = %d/%v", len(acts), err)
	}

	// Key 前缀查找（含 key_hash 供验证）。
	var prefix string
	if err := s.db.Get(&prefix, `SELECT key_prefix FROM inference_api_keys WHERE id = $1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	key2, hash2, err := s.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil || key2.ID != f.keyID || hash2 == "" {
		t.Fatalf("key by prefix = %+v/%v", key2, err)
	}
}
