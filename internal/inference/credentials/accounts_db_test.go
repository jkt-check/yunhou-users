package credentials_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// accounts_db_test.go — AccountService 在真实库上的同事务纪律（复用
// db_atomic_test.go 的 atomicSetup/failRecorder）：写 + 审计行同生共死；
// UNIQUE(provider_id, credential_id) 幂等冲突回读出已存在账号视图。

func accountAtomicSetup(t *testing.T) (context.Context, *sqlx.DB, *postgres.Store, credentials.Operator, string, string) {
	t.Helper()
	db, store, _, op, providerID := atomicSetup(t)
	ctx := context.Background()
	credID := uuid.NewString()
	if err := store.InsertCredential(ctx, &domain.Credential{
		ID: credID, ProviderID: providerID, Label: "acct-key", AuthType: "api_key",
		Ciphertext: []byte("ct"), KeyVersion: 1, Generation: 1,
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return ctx, db, store, op, providerID, credID
}

func TestAccountCreateAuditFailureRollsBack(t *testing.T) {
	ctx, db, store, op, providerID, credID := accountAtomicSetup(t)

	svc := credentials.NewAccountService(store, &failRecorder{err: fmt.Errorf("audit store down")})
	_, err := svc.Create(ctx, op, credentials.CreateAccountInput{
		ProviderID: providerID, CredentialID: credID, Reason: "接入"})
	if err == nil {
		t.Fatal("create with failing audit must error")
	}
	// 账号行与审计行都不得落库。
	if _, err := store.GetUpstreamAccountByPair(ctx, providerID, credID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("account committed despite audit failure: %v", err)
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM inference_audit_log
		WHERE object_type = 'upstream_account' AND actor_app_id = 'task4-atomic-app'`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("partial audit state after rollback: %d rows", n)
	}
}

func TestAccountCreateDuplicateConflictOnRealDB(t *testing.T) {
	ctx, _, store, op, providerID, credID := accountAtomicSetup(t)

	svc := credentials.NewAccountService(store, store)
	first, err := svc.Create(ctx, op, credentials.CreateAccountInput{
		ProviderID: providerID, CredentialID: credID, Reason: "接入"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// 重放同一对：409 语义（AccountExistsError）+ 已存在账号视图；无第二行。
	_, err = svc.Create(ctx, op, credentials.CreateAccountInput{
		ProviderID: providerID, CredentialID: credID, Reason: "重放"})
	var exists *credentials.AccountExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("err = %v, want AccountExistsError", err)
	}
	if exists.Existing == nil || exists.Existing.ID != first.ID {
		t.Fatalf("existing = %+v, want first account %s", exists.Existing, first.ID)
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
	}
}

func TestAccountDisableEndsBindingsAndAuditAtomically(t *testing.T) {
	ctx, db, store, op, providerID, credID := accountAtomicSetup(t)

	svc := credentials.NewAccountService(store, store)
	acct, err := svc.Create(ctx, op, credentials.CreateAccountInput{
		ProviderID: providerID, CredentialID: credID, Reason: "接入"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 挂一条活跃会话绑定，停用必须同事务终止它。
	modelID := "m-" + uuid.NewString()[:8]
	if err := store.InsertModel(ctx, &domain.Model{
		ID: modelID, DisplayName: "M", ContextTokens: 1000, MaxOutputTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}
	b := &domain.SessionBinding{
		SessionKey: "s-" + uuid.NewString()[:6], ModelID: modelID, AccountID: acct.ID,
		Status: domain.BindingActive, ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.InsertSessionBinding(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStatus(ctx, op, acct.ID, "disabled", "厂商限流"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	var status, bindStatus string
	if err := db.Get(&status, `SELECT status FROM inference_upstream_accounts WHERE id = $1`, acct.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&bindStatus, `SELECT status FROM inference_session_bindings WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || bindStatus != "ended" {
		t.Fatalf("account=%s binding=%s, want disabled/ended", status, bindStatus)
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM inference_audit_log
		WHERE action = 'upstream_account.status' AND object_id = $1`, acct.ID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("status audit rows = %d, want 1", n)
	}
}
