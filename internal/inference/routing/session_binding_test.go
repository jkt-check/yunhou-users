// session_binding_test.go — Task 12 粘性会话绑定真库验收（设计 §8: 不能
// 无条件切账号续接；账号失效时绑定可迁移并停止向失效账号分配新请求）。

package routing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/migrate"
)

var bindingDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn != "" {
		db, err := sqlx.Connect("postgres", dsn)
		if err == nil {
			migs, err := migrate.LoadFiles("../../../migrations")
			if err != nil {
				fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
				os.Exit(1)
			}
			if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
				fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
				os.Exit(1)
			}
			bindingDSN = dsn
			db.Close()
		}
	}
	os.Exit(m.Run())
}

type bindingEnv struct {
	binder  *SessionBinder
	store   *postgres.Store
	modelID string
	account []string
}

// newBindingEnv seeds provider + credential + model + N accounts.
func newBindingEnv(t *testing.T, accountN int) bindingEnv {
	t.Helper()
	if bindingDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", bindingDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	provCode := "task12bind-" + suffix
	modelID := "task12-bind-model-" + suffix
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inference_providers (code, display_name, access_type) VALUES ($1,'Bind Provider','oauth_connector')`, provCode); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	var providerID string
	if err := db.GetContext(ctx, &providerID, `SELECT id FROM inference_providers WHERE code = $1`, provCode); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO inference_models
		(id, display_name, context_tokens, max_output_tokens) VALUES ($1,'M',128000,8192)`, modelID); err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db)
	env := bindingEnv{binder: NewSessionBinder(store, nil), store: store, modelID: modelID}
	for i := 0; i < accountN; i++ {
		// UNIQUE(provider_id, credential_id)：每账号一个凭据。
		if _, err := db.ExecContext(ctx, `INSERT INTO inference_credentials
			(provider_id, label, auth_type, ciphertext, key_version, connector)
			VALUES ($1,'c','oauth', $2, 1, 'testvendor')`, providerID, []byte("not-real-ciphertext")); err != nil {
			t.Fatal(err)
		}
		var credID string
		if err := db.GetContext(ctx, &credID, `SELECT id FROM inference_credentials WHERE provider_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, providerID); err != nil {
			t.Fatal(err)
		}
		a := &domain.UpstreamAccount{
			ProviderID: providerID, CredentialID: credID,
			DisplayName: fmt.Sprintf("acct-%d", i),
			Status:      domain.AccountActive, ConcurrencyLimit: 1,
		}
		if err := store.InsertUpstreamAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		env.account = append(env.account, a.ID)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM inference_session_bindings WHERE model_id = $1`, modelID)
		db.Exec(`DELETE FROM inference_upstream_accounts WHERE provider_id = $1`, providerID)
		db.Exec(`DELETE FROM inference_credentials WHERE provider_id = $1`, providerID)
		db.Exec(`DELETE FROM inference_models WHERE id = $1`, modelID)
		db.Exec(`DELETE FROM inference_providers WHERE code = $1`, provCode)
		db.Close()
	})
	return env
}

func TestSessionBinding_BindResolve(t *testing.T) {
	env := newBindingEnv(t, 2)
	ctx := context.Background()

	b, err := env.binder.Bind(ctx, "sess-1", env.modelID, env.account[0])
	if err != nil {
		t.Fatal(err)
	}
	got, account, err := env.binder.Resolve(ctx, "sess-1", env.modelID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != b.ID || account.ID != env.account[0] {
		t.Fatalf("resolve mismatch: %+v %+v", got, account)
	}

	// 同一 (session, model) 的第二活跃绑定冲突（不能无条件换账号）。
	if _, err := env.binder.Bind(ctx, "sess-1", env.modelID, env.account[1]); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second live binding must conflict, got %v", err)
	}
}

func TestSessionBinding_BoundAccountInvalidNeverSilentlySwitches(t *testing.T) {
	env := newBindingEnv(t, 2)
	ctx := context.Background()

	b, err := env.binder.Bind(ctx, "sess-2", env.modelID, env.account[0])
	if err != nil {
		t.Fatal(err)
	}
	// 绑定账号失效（reauth_required）。
	if _, err := env.store.SetUpstreamAccountStatusConditional(ctx, env.account[0],
		[]domain.UpstreamAccountStatus{domain.AccountActive}, domain.AccountReauthRequired); err != nil {
		t.Fatal(err)
	}
	_, _, err = env.binder.Resolve(ctx, "sess-2", env.modelID)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("invalid bound account must conflict (no silent switch), got %v", err)
	}
	// 失效账号不再可分配（绑定新账号指向它也拒绝）。
	if _, err := env.binder.Bind(ctx, "sess-2b", env.modelID, env.account[0]); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("binding to an invalid account must conflict, got %v", err)
	}

	// 显式迁移 = 终止旧绑定 + 同事务建新绑定。
	nb, err := env.binder.Migrate(ctx, "sess-2", env.modelID, env.account[1])
	if err != nil {
		t.Fatal(err)
	}
	if nb.AccountID != env.account[1] {
		t.Fatalf("migrated binding points at %s", nb.AccountID)
	}
	if _, got, err := env.binder.Resolve(ctx, "sess-2", env.modelID); err != nil || got.ID != env.account[1] {
		t.Fatalf("post-migrate resolve must hit new account: %v %+v", err, got)
	}
	// 旧绑定已终止且带迁移原因（审计可读）。
	old, err := env.store.GetActiveSessionBinding(ctx, "sess-2", env.modelID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if old.ID != nb.ID {
		t.Fatalf("active binding after migrate must be the new one: %s vs %s", old.ID, nb.ID)
	}
	db, err := sqlx.Connect("postgres", bindingDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var oldStatus, oldReason string
	if err := db.QueryRow(`SELECT status, ended_reason FROM inference_session_bindings WHERE id = $1`, b.ID).
		Scan(&oldStatus, &oldReason); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "ended" || oldReason != domain.BindingEndedMigrated {
		t.Fatalf("old binding must be ended(migrated): %s %s", oldStatus, oldReason)
	}
}

func TestSessionBinding_EndAndExpiry(t *testing.T) {
	env := newBindingEnv(t, 1)
	ctx := context.Background()

	if _, err := env.binder.Bind(ctx, "sess-3", env.modelID, env.account[0]); err != nil {
		t.Fatal(err)
	}
	ended, err := env.binder.End(ctx, "sess-3", env.modelID, "")
	if err != nil || !ended {
		t.Fatalf("end failed: %v %v", ended, err)
	}
	if _, _, err := env.binder.Resolve(ctx, "sess-3", env.modelID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("ended binding must resolve not_found, got %v", err)
	}
	// 终止后可按新会话语义重建。
	if _, err := env.binder.Bind(ctx, "sess-3", env.modelID, env.account[0]); err != nil {
		t.Fatalf("rebind after end must succeed: %v", err)
	}

	// 到期清扫：过期的活跃绑定被终止，Resolve 不再命中。
	b2, err := env.binder.Bind(ctx, "sess-4", env.modelID, env.account[0])
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	db, err := sqlx.Connect("postgres", bindingDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE inference_session_bindings SET expires_at = $1 WHERE id = $2`, past, b2.ID); err != nil {
		t.Fatal(err)
	}
	n, err := env.store.EndExpiredSessionBindings(ctx, time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("expiry sweep ended %d bindings", n)
	}
	if _, _, err := env.binder.Resolve(ctx, "sess-4", env.modelID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired binding must resolve not_found, got %v", err)
	}
}
