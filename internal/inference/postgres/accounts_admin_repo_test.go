package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// accounts_admin_repo_test.go — 可调度账号管理面新增 repo 方法的集成测试
// （真实库）：UNIQUE(provider_id, credential_id) 冲突映射、显式 0 并发不
// 被改写、配对回读、profile 更新（含 Tx 变体）、账号级会话绑定终止。

func TestAccountsAdminRepo_PairLookupConflictAndProfile(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	seedFixture(t, s, true)

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

	// 显式 0 并发合法落库（备而不用；不再有 0→1 静默改写）。
	acct := &domain.UpstreamAccount{
		ProviderID: prov.ID, CredentialID: cred.ID,
		Status: domain.AccountActive, ConcurrencyLimit: 0,
	}
	if err := s.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUpstreamAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConcurrencyLimit != 0 {
		t.Fatalf("concurrency = %d, want explicit 0 preserved", got.ConcurrencyLimit)
	}

	// 配对回读 + 未知配对 CodeNotFound。
	byPair, err := s.GetUpstreamAccountByPair(ctx, prov.ID, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byPair.ID != acct.ID {
		t.Fatalf("pair lookup = %s, want %s", byPair.ID, acct.ID)
	}
	if _, err := s.GetUpstreamAccountByPair(ctx, prov.ID, uuid.NewString()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unknown pair code = %s, want not_found", domain.CodeOf(err))
	}

	// UNIQUE(provider_id, credential_id)：重复插入 → CodeConflict。
	dup := &domain.UpstreamAccount{ProviderID: prov.ID, CredentialID: cred.ID, Status: domain.AccountActive, ConcurrencyLimit: 1}
	if err := s.InsertUpstreamAccount(ctx, dup); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate insert code = %s, want conflict", domain.CodeOf(err))
	}

	// profile 更新：单字段不动另一字段；两字段同时；未知 id → not_found。
	name := "主账号"
	if err := s.UpdateUpstreamAccountProfile(ctx, acct.ID, &name, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUpstreamAccount(ctx, acct.ID)
	if got.DisplayName != name || got.ConcurrencyLimit != 0 {
		t.Fatalf("after name-only update = %+v", got)
	}
	conc := 4
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUpstreamAccountProfileTx(ctx, uow, acct.ID, nil, &conc); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUpstreamAccount(ctx, acct.ID)
	if got.DisplayName != name || got.ConcurrencyLimit != 4 {
		t.Fatalf("after tx conc update = %+v", got)
	}
	if err := s.UpdateUpstreamAccountProfile(ctx, uuid.NewString(), &name, nil); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unknown id code = %s, want not_found", domain.CodeOf(err))
	}
}

func TestAccountsAdminRepo_EndSessionBindingsForAccount(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	seedFixture(t, s, true)

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
	acct := &domain.UpstreamAccount{ProviderID: prov.ID, CredentialID: cred.ID, Status: domain.AccountActive, ConcurrencyLimit: 1}
	if err := s.InsertUpstreamAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	b := &domain.SessionBinding{
		SessionKey: "s-" + uuid.NewString()[:6], ModelID: "glm-4.6", AccountID: acct.ID,
		Status: domain.BindingActive, ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := s.InsertSessionBinding(ctx, b); err != nil {
		t.Fatal(err)
	}

	// 非事务变体：终止活跃绑定，幂等（第二次 0）。
	n, err := s.EndSessionBindingsForAccount(ctx, acct.ID, domain.BindingEndedAccountInvalid)
	if err != nil || n != 1 {
		t.Fatalf("ended = %d/%v, want 1", n, err)
	}
	n, err = s.EndSessionBindingsForAccount(ctx, acct.ID, domain.BindingEndedAccountInvalid)
	if err != nil || n != 0 {
		t.Fatalf("second end = %d/%v, want 0", n, err)
	}
}
