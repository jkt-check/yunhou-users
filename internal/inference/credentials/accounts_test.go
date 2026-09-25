package credentials

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// accounts_test.go — AccountService（可调度账号的创建/启停/变更）单元测试。
// fake store 为纯内存非事务实现，驱动 runAtomicAccounts 的顺序回退路径；
// 同事务路径由 db 级测试（accounts_db_test.go）与 HTTP 验收覆盖。

type memAccountStore struct {
	mu        sync.Mutex
	providers map[string]*domain.Provider
	creds     map[string]*domain.Credential
	accounts  map[string]*domain.UpstreamAccount
	seq       int
	// endedBindings records EndSessionBindingsForAccount calls.
	endedBindings []string
}

func newMemAccountStore() *memAccountStore {
	return &memAccountStore{
		providers: map[string]*domain.Provider{},
		creds:     map[string]*domain.Credential{},
		accounts:  map[string]*domain.UpstreamAccount{},
	}
}

func (m *memAccountStore) addProvider(p *domain.Provider) { m.providers[p.ID] = p }

func (m *memAccountStore) addCredential(c *domain.Credential) { m.creds[c.ID] = c }

func (m *memAccountStore) GetProvider(_ context.Context, id string) (*domain.Provider, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.providers[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "get provider: not found")
	}
	cp := *p
	return &cp, nil
}

func (m *memAccountStore) GetCredential(_ context.Context, id string) (*domain.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "get credential: not found")
	}
	cp := *c
	return &cp, nil
}

func (m *memAccountStore) GetUpstreamAccount(_ context.Context, id string) (*domain.UpstreamAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "get upstream account: not found")
	}
	cp := *a
	return &cp, nil
}

func (m *memAccountStore) GetUpstreamAccountByPair(_ context.Context, providerID, credentialID string) (*domain.UpstreamAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.ProviderID == providerID && a.CredentialID == credentialID {
			cp := *a
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.CodeNotFound, "get upstream account by pair: not found")
}

func (m *memAccountStore) InsertUpstreamAccount(_ context.Context, a *domain.UpstreamAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.accounts {
		if ex.ProviderID == a.ProviderID && ex.CredentialID == a.CredentialID {
			return domain.NewError(domain.CodeConflict, "insert upstream account: duplicate key")
		}
	}
	m.seq++
	cp := *a
	cp.ID = fmt.Sprintf("acct-%d", m.seq)
	if cp.Status == "" {
		cp.Status = domain.AccountActive
	}
	cp.CreatedAt = time.Now()
	cp.UpdatedAt = cp.CreatedAt
	m.accounts[cp.ID] = &cp
	a.ID = cp.ID
	a.CreatedAt = cp.CreatedAt
	a.UpdatedAt = cp.UpdatedAt
	return nil
}

func (m *memAccountStore) SetUpstreamAccountStatusConditional(_ context.Context, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return false, domain.NewError(domain.CodeNotFound, "set upstream account status: not found")
	}
	allowed := false
	for _, f := range from {
		if a.Status == f {
			allowed = true
			break
		}
	}
	if !allowed {
		return false, nil
	}
	a.Status = to
	return true, nil
}

func (m *memAccountStore) UpdateUpstreamAccountProfile(_ context.Context, id string, displayName *string, concurrencyLimit *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "update upstream account profile: not found")
	}
	if displayName != nil {
		a.DisplayName = *displayName
	}
	if concurrencyLimit != nil {
		a.ConcurrencyLimit = *concurrencyLimit
	}
	return nil
}

func (m *memAccountStore) EndSessionBindingsForAccount(_ context.Context, accountID, _ string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endedBindings = append(m.endedBindings, accountID)
	return 2, nil
}

func seedAccountFixtures(s *memAccountStore) (provID, credID string) {
	provID, credID = "prov-1", "cred-1"
	s.addProvider(&domain.Provider{ID: provID, Code: "deepseek", AccessType: domain.AccessOfficialAPI, Status: "active"})
	s.addCredential(&domain.Credential{ID: credID, ProviderID: provID, Label: "deepseek 主 key", AuthType: "api_key", Status: "active"})
	return provID, credID
}

func validCreateInput(provID, credID string) CreateAccountInput {
	return CreateAccountInput{ProviderID: provID, CredentialID: credID, Reason: "接入 deepseek 静态 key"}
}

func TestAccountServiceCreateDefaults(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	rec := &memRecorder{}
	svc := NewAccountService(store, rec)

	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acct.ID == "" || acct.Status != domain.AccountActive {
		t.Fatalf("account = %+v, want id set + active", acct)
	}
	if acct.DisplayName != "deepseek 主 key" {
		t.Fatalf("display_name default = %q, want credential label", acct.DisplayName)
	}
	if acct.ConcurrencyLimit != 1 {
		t.Fatalf("concurrency default = %d, want 1", acct.ConcurrencyLimit)
	}
	if acct.Quota.Source != nil || acct.Quota.ObservedAt != nil {
		t.Fatalf("quota must stay unknown, got %+v", acct.Quota)
	}
	ev := rec.find("upstream_account.create")
	if ev == nil {
		t.Fatal("missing audit event upstream_account.create")
	}
	if ev.ObjectType != "upstream_account" || ev.ObjectID != acct.ID {
		t.Fatalf("audit object = %s/%s, want upstream_account/%s", ev.ObjectType, ev.ObjectID, acct.ID)
	}
	if ev.Detail["provider_id"] != provID || ev.Detail["credential_id"] != credID || ev.Detail["concurrency_limit"] != 1 {
		t.Fatalf("audit detail = %v", ev.Detail)
	}
	if ev.ActorUser != "user-1" || ev.ActorApp != "yunhou-website" {
		t.Fatalf("audit attribution = %s/%s", ev.ActorUser, ev.ActorApp)
	}
}

func TestAccountServiceCreateExplicitZeroConcurrency(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	svc := NewAccountService(store, &memRecorder{})

	zero := 0
	in := validCreateInput(provID, credID)
	in.ConcurrencyLimit = &zero
	acct, err := svc.Create(context.Background(), testOp(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acct.ConcurrencyLimit != 0 {
		t.Fatalf("concurrency = %d, want explicit 0 preserved (备而不用)", acct.ConcurrencyLimit)
	}
	stored, err := store.GetUpstreamAccount(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConcurrencyLimit != 0 {
		t.Fatalf("stored concurrency = %d, want 0", stored.ConcurrencyLimit)
	}
}

func TestAccountServiceCreateWithQuota(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	svc := NewAccountService(store, &memRecorder{})

	limit, remaining := int64(1_000_000), int64(400_000)
	reset := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	in := validCreateInput(provID, credID)
	in.QuotaLimitMicros = &limit
	in.QuotaRemainingMicros = &remaining
	in.QuotaResetAt = &reset
	acct, err := svc.Create(context.Background(), testOp(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acct.Quota.LimitMicros == nil || *acct.Quota.LimitMicros != domain.Microcredit(limit) {
		t.Fatalf("quota limit = %v", acct.Quota.LimitMicros)
	}
	if acct.Quota.Source == nil || *acct.Quota.Source != "reported" {
		t.Fatalf("quota source = %v, want reported", acct.Quota.Source)
	}
	if acct.Quota.ObservedAt == nil {
		t.Fatal("quota observed_at must be set when quota is reported")
	}
}

func TestAccountServiceCreateValidation(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	svc := NewAccountService(store, &memRecorder{})

	neg := -1
	quotaVal := int64(10)
	cases := []struct {
		name   string
		mutate func(*CreateAccountInput)
	}{
		{"missing provider_id", func(in *CreateAccountInput) { in.ProviderID = "" }},
		{"missing credential_id", func(in *CreateAccountInput) { in.CredentialID = "" }},
		{"missing reason", func(in *CreateAccountInput) { in.Reason = "" }},
		{"reason too long", func(in *CreateAccountInput) { in.Reason = strings.Repeat("r", 201) }},
		{"display_name too long", func(in *CreateAccountInput) { in.DisplayName = strings.Repeat("d", 129) }},
		{"negative concurrency", func(in *CreateAccountInput) { in.ConcurrencyLimit = &neg }},
		{"quota limit without remaining", func(in *CreateAccountInput) { in.QuotaLimitMicros = &quotaVal }},
		{"quota reset without limit", func(in *CreateAccountInput) { in.QuotaResetAt = &time.Time{} }},
		{"negative quota", func(in *CreateAccountInput) {
			negQ := int64(-1)
			in.QuotaLimitMicros = &negQ
			in.QuotaRemainingMicros = &quotaVal
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validCreateInput(provID, credID)
			tc.mutate(&in)
			_, err := svc.Create(context.Background(), testOp(), in)
			if domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Fatalf("code = %s, want invalid_input (err=%v)", domain.CodeOf(err), err)
			}
		})
	}
}

func TestAccountServiceCreateGuards(t *testing.T) {
	newSeeded := func() (*memAccountStore, string, string) {
		s := newMemAccountStore()
		p, c := seedAccountFixtures(s)
		return s, p, c
	}

	t.Run("provider missing -> 404", func(t *testing.T) {
		store, _, credID := newSeeded()
		svc := NewAccountService(store, &memRecorder{})
		in := validCreateInput("prov-missing", credID)
		_, err := svc.Create(context.Background(), testOp(), in)
		if domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("code = %s, want not_found", domain.CodeOf(err))
		}
	})

	t.Run("disabled provider -> 409", func(t *testing.T) {
		store, provID, credID := newSeeded()
		store.providers[provID].Status = "disabled"
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
		}
	})

	t.Run("credential missing -> 404", func(t *testing.T) {
		store, provID, _ := newSeeded()
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, "cred-missing"))
		if domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("code = %s, want not_found", domain.CodeOf(err))
		}
	})

	t.Run("provider mismatch -> 400", func(t *testing.T) {
		store, provID, _ := newSeeded()
		store.addCredential(&domain.Credential{ID: "cred-other", ProviderID: "prov-other", AuthType: "api_key", Status: "active"})
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, "cred-other"))
		if domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Fatalf("code = %s, want invalid_input", domain.CodeOf(err))
		}
	})

	t.Run("oauth credential -> 400", func(t *testing.T) {
		store, provID, credID := newSeeded()
		store.creds[credID].AuthType = "oauth"
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Fatalf("code = %s, want invalid_input", domain.CodeOf(err))
		}
	})

	t.Run("revoked credential -> 409", func(t *testing.T) {
		store, provID, credID := newSeeded()
		store.creds[credID].Status = "revoked"
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
		}
	})

	t.Run("rotating credential -> 409", func(t *testing.T) {
		store, provID, credID := newSeeded()
		store.creds[credID].Status = "rotating"
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
		}
	})

	t.Run("service auth_type allowed", func(t *testing.T) {
		store, provID, credID := newSeeded()
		store.creds[credID].AuthType = "service"
		svc := NewAccountService(store, &memRecorder{})
		if _, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID)); err != nil {
			t.Fatalf("service credential must be bindable: %v", err)
		}
	})
}

func TestAccountServiceCreateDuplicateReturnsExisting(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	rec := &memRecorder{}
	svc := NewAccountService(store, rec)

	first, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	var exists *AccountExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("err = %v, want AccountExistsError", err)
	}
	if exists.Existing == nil || exists.Existing.ID != first.ID {
		t.Fatalf("existing = %+v, want the first account", exists.Existing)
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
	}
	if n := len(store.accounts); n != 1 {
		t.Fatalf("accounts = %d, want 1 (no duplicate row)", n)
	}
}

func TestAccountServiceSetStatusRoundTrip(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	rec := &memRecorder{}
	svc := NewAccountService(store, rec)
	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}

	disabled, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "disabled", "厂商侧限流，先停用")
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.Status != domain.AccountDisabled {
		t.Fatalf("status = %s, want disabled", disabled.Status)
	}
	ev := rec.find("upstream_account.status")
	if ev == nil {
		t.Fatal("missing status audit")
	}
	if ev.Detail["from"] != "active" || ev.Detail["to"] != "disabled" {
		t.Fatalf("audit detail = %v", ev.Detail)
	}
	if ev.Detail["credential_id"] != credID || ev.Detail["provider_id"] != provID {
		t.Fatalf("audit detail missing binding ids: %v", ev.Detail)
	}

	restored, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "限流解除")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if restored.Status != domain.AccountActive {
		t.Fatalf("status = %s, want active", restored.Status)
	}
}

func TestAccountServiceDisableEndsSessionBindings(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	svc := NewAccountService(store, &memRecorder{})
	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "disabled", "off"); err != nil {
		t.Fatal(err)
	}
	if len(store.endedBindings) != 1 || store.endedBindings[0] != acct.ID {
		t.Fatalf("ended bindings = %v, want [%s]", store.endedBindings, acct.ID)
	}
}

func TestAccountServiceSetStatusNoopStillAudited(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	rec := &memRecorder{}
	svc := NewAccountService(store, rec)
	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "重复恢复调用")
	if err != nil {
		t.Fatalf("noop activate must succeed: %v", err)
	}
	if got.Status != domain.AccountActive {
		t.Fatalf("status = %s", got.Status)
	}
	ev := rec.find("upstream_account.status")
	if ev == nil || ev.Detail["noop"] != true {
		t.Fatalf("noop audit = %+v, want noop:true", ev)
	}
}

func TestAccountServiceSetStatusRejectsRuntimeStates(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	svc := NewAccountService(store, &memRecorder{})
	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"refreshing", "cooldown", "reauth_required"} {
		_, err := svc.SetStatus(context.Background(), testOp(), acct.ID, target, "x")
		if domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Fatalf("target %s: code = %s, want invalid_input", target, domain.CodeOf(err))
		}
	}
}

func TestAccountServiceActivateGuards(t *testing.T) {
	t.Run("revoked credential -> 409", func(t *testing.T) {
		store := newMemAccountStore()
		provID, credID := seedAccountFixtures(store)
		svc := NewAccountService(store, &memRecorder{})
		acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "disabled", "off"); err != nil {
			t.Fatal(err)
		}
		store.creds[credID].Status = "revoked"
		_, err = svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "restore")
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("code = %s, want conflict (restore the credential first)", domain.CodeOf(err))
		}
	})

	t.Run("noop activate with revoked credential -> 409", func(t *testing.T) {
		// 账号已 active 但凭据 revoked（正常路径不可达，仅直写库可造成）：
		// 幂等激活也按 409 拒绝,不做 noop 成功。
		store := newMemAccountStore()
		provID, credID := seedAccountFixtures(store)
		svc := NewAccountService(store, &memRecorder{})
		acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if err != nil {
			t.Fatal(err)
		}
		store.creds[credID].Status = "revoked"
		_, err = svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "repeat")
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
		}
	})

	t.Run("runtime state -> 409", func(t *testing.T) {
		store := newMemAccountStore()
		provID, credID := seedAccountFixtures(store)
		svc := NewAccountService(store, &memRecorder{})
		acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
		if err != nil {
			t.Fatal(err)
		}
		// 运行时状态机的内部态（cooldown）不接受人工激活。
		store.accounts[acct.ID].Status = domain.AccountCooldown
		_, err = svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "force")
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("code = %s, want conflict", domain.CodeOf(err))
		}
	})

	t.Run("unknown account -> 404", func(t *testing.T) {
		store := newMemAccountStore()
		svc := NewAccountService(store, &memRecorder{})
		_, err := svc.SetStatus(context.Background(), testOp(), "acct-missing", "disabled", "x")
		if domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("code = %s, want not_found", domain.CodeOf(err))
		}
	})
}

func TestAccountServiceUpdate(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	rec := &memRecorder{}
	svc := NewAccountService(store, rec)
	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}

	name, conc := "deepseek 主账号", 4
	got, err := svc.Update(context.Background(), testOp(), acct.ID, &name, &conc, "主 key 调并发到 4")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.DisplayName != name || got.ConcurrencyLimit != 4 {
		t.Fatalf("updated = %+v", got)
	}
	stored, err := store.GetUpstreamAccount(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DisplayName != name || stored.ConcurrencyLimit != 4 {
		t.Fatalf("stored = %+v", stored)
	}
	ev := rec.find("upstream_account.update")
	if ev == nil {
		t.Fatal("missing update audit")
	}
	before, ok := ev.Detail["before"].(map[string]any)
	if !ok || before["concurrency_limit"] != 1 {
		t.Fatalf("audit before = %v", ev.Detail["before"])
	}
	after, ok := ev.Detail["after"].(map[string]any)
	if !ok || after["concurrency_limit"] != 4 || after["display_name"] != name {
		t.Fatalf("audit after = %v", ev.Detail["after"])
	}
}

func TestAccountServiceUpdateValidation(t *testing.T) {
	store := newMemAccountStore()
	provID, credID := seedAccountFixtures(store)
	svc := NewAccountService(store, &memRecorder{})
	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(context.Background(), testOp(), acct.ID, nil, nil, "x"); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("no fields: code = %s, want invalid_input", domain.CodeOf(err))
	}
	neg := -1
	if _, err := svc.Update(context.Background(), testOp(), acct.ID, nil, &neg, "x"); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("negative: code = %s, want invalid_input", domain.CodeOf(err))
	}
	long := strings.Repeat("d", 129)
	if _, err := svc.Update(context.Background(), testOp(), acct.ID, &long, nil, "x"); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("long name: code = %s, want invalid_input", domain.CodeOf(err))
	}
	if _, err := svc.Update(context.Background(), testOp(), acct.ID, nil, nil, ""); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("missing reason: code = %s, want invalid_input", domain.CodeOf(err))
	}
	name := "y"
	if _, err := svc.Update(context.Background(), testOp(), "acct-missing", &name, nil, "x"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unknown: code = %s, want not_found", domain.CodeOf(err))
	}
}

// ---------------------------------------------------------------------------
// 事务路径 fake（驱动 runAtomicAccounts 的同事务分支 + 凭据行锁复查）
// ---------------------------------------------------------------------------

type fakeUoW struct {
	committed  bool
	rolledBack bool
}

func (u *fakeUoW) Commit(context.Context) error   { u.committed = true; return nil }
func (u *fakeUoW) Rollback(context.Context) error { u.rolledBack = true; return nil }

// txAccountStore 在 memAccountStore 之上实现 AccountTxStore +
// credentialLockTx（可选升级接口）。lockedCred 非nil 时,
// GetCredentialForUpdateTx 返回它 —— 模拟"并发吊销在事务内复查时可见"
// 的定序。
type txAccountStore struct {
	*memAccountStore
	lastUoW    *fakeUoW
	lockedCred *domain.Credential
	inserted   bool
	// 审查修复 I-5：记录行锁复查与状态写入各自见到的 UnitOfWork 及
	// 调用次数，断言两者在同一事务内、且复查先于状态写入。
	lockUoW     domain.UnitOfWork
	lockCalls   int
	statusUoW   domain.UnitOfWork
	statusCalls int
}

func (s *txAccountStore) Begin(context.Context) (domain.UnitOfWork, error) {
	s.lastUoW = &fakeUoW{}
	return s.lastUoW, nil
}

func (s *txAccountStore) GetCredentialForUpdateTx(_ context.Context, w domain.UnitOfWork, id string) (*domain.Credential, error) {
	s.lockUoW = w
	s.lockCalls++
	if s.lockedCred != nil {
		cp := *s.lockedCred
		return &cp, nil
	}
	return s.GetCredential(context.Background(), id)
}

func (s *txAccountStore) InsertUpstreamAccountTx(ctx context.Context, _ domain.UnitOfWork, a *domain.UpstreamAccount) error {
	s.inserted = true
	return s.InsertUpstreamAccount(ctx, a)
}

func (s *txAccountStore) SetUpstreamAccountStatusConditionalTx(ctx context.Context, w domain.UnitOfWork, id string, from []domain.UpstreamAccountStatus, to domain.UpstreamAccountStatus) (bool, error) {
	s.statusUoW = w
	s.statusCalls++
	return s.SetUpstreamAccountStatusConditional(ctx, id, from, to)
}

func (s *txAccountStore) UpdateUpstreamAccountProfileTx(ctx context.Context, _ domain.UnitOfWork, id string, displayName *string, concurrencyLimit *int) error {
	return s.UpdateUpstreamAccountProfile(ctx, id, displayName, concurrencyLimit)
}

func (s *txAccountStore) EndSessionBindingsForAccountTx(ctx context.Context, _ domain.UnitOfWork, accountID, reason string) ([]string, error) {
	n, err := s.EndSessionBindingsForAccount(ctx, accountID, reason)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, n)
	for i := int64(0); i < n; i++ {
		ids = append(ids, accountID)
	}
	return ids, nil
}

type txMemRecorder struct{ memRecorder }

func (r *txMemRecorder) RecordTx(_ context.Context, _ domain.UnitOfWork, ev management.AuditEvent) error {
	return r.Record(context.Background(), ev)
}

func TestAccountServiceCreateCredentialRevokedInTx(t *testing.T) {
	// 事务内持行锁复查（credentialLockTx）看到凭据已吊销 → 409,账号行
	// 不落库、审计不提交（整个 UnitOfWork 回滚）。
	mem := newMemAccountStore()
	provID, credID := seedAccountFixtures(mem)
	store := &txAccountStore{memAccountStore: mem}
	revoked := *mem.creds[credID]
	revoked.Status = "revoked"
	store.lockedCred = &revoked
	svc := NewAccountService(store, &txMemRecorder{})

	_, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %s, want conflict (err=%v)", domain.CodeOf(err), err)
	}
	if store.inserted {
		t.Fatal("account row must not be inserted when the in-tx re-check sees revoked")
	}
	if len(store.accounts) != 0 {
		t.Fatalf("accounts = %d, want 0", len(store.accounts))
	}
	if store.lastUoW == nil || !store.lastUoW.rolledBack || store.lastUoW.committed {
		t.Fatalf("uow = %+v, want rolled back and not committed", store.lastUoW)
	}
}

func TestAccountServiceCreateTxPathCommitsAtomically(t *testing.T) {
	// 事务路径正常定序：行锁复查通过 → 插入 → 审计 → 提交。
	mem := newMemAccountStore()
	provID, credID := seedAccountFixtures(mem)
	store := &txAccountStore{memAccountStore: mem}
	rec := &txMemRecorder{}
	svc := NewAccountService(store, rec)

	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !store.inserted || len(store.accounts) != 1 {
		t.Fatalf("inserted=%v accounts=%d, want 1 row", store.inserted, len(store.accounts))
	}
	if store.lastUoW == nil || !store.lastUoW.committed || store.lastUoW.rolledBack {
		t.Fatalf("uow = %+v, want committed and not rolled back", store.lastUoW)
	}
	if rec.find("upstream_account.create") == nil {
		t.Fatal("missing create audit on tx path")
	}
	if acct.Status != domain.AccountActive {
		t.Fatalf("status = %s, want active", acct.Status)
	}
}

// ---------------------------------------------------------------------------
// 审查修复 I-5：SetStatus 激活路径的凭据 revoked 检查必须在事务内
// ---------------------------------------------------------------------------

// 激活路径：凭据的 FOR UPDATE 复查与账号状态写入必须见到同一个
// UnitOfWork，且复查先于状态写入。
func TestAccountServiceSetStatusActivateCredentialCheckInSameTx(t *testing.T) {
	mem := newMemAccountStore()
	provID, credID := seedAccountFixtures(mem)
	store := &txAccountStore{memAccountStore: mem}
	rec := &txMemRecorder{}
	svc := NewAccountService(store, rec)

	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "disabled", "off"); err != nil {
		t.Fatal(err)
	}
	store.lockCalls, store.statusCalls = 0, 0

	if _, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "恢复调度"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if store.lockCalls != 1 {
		t.Fatalf("in-tx credential re-checks = %d, want 1", store.lockCalls)
	}
	if store.statusCalls != 1 {
		t.Fatalf("status writes = %d, want 1", store.statusCalls)
	}
	if store.lockUoW == nil || store.lockUoW != store.statusUoW {
		t.Fatalf("credential re-check uow = %p, status write uow = %p — must be the SAME UnitOfWork",
			store.lockUoW, store.statusUoW)
	}
	if store.lastUoW == nil || !store.lastUoW.committed || store.lastUoW.rolledBack {
		t.Fatalf("uow = %+v, want committed and not rolled back", store.lastUoW)
	}
	if store.accounts[acct.ID].Status != domain.AccountActive {
		t.Fatalf("status = %s, want active", store.accounts[acct.ID].Status)
	}
	if rec.find("upstream_account.status") == nil {
		t.Fatal("missing status audit on activate tx path")
	}
}

// TOCTOU：预检通过但事务内复查（持行锁）看到凭据已被并发吊销 → 激活
// 整体 409 回滚：状态保持 disabled、状态写入未执行、审计未提交。
func TestAccountServiceSetStatusActivateRevokedMidTxRollsBack(t *testing.T) {
	mem := newMemAccountStore()
	provID, credID := seedAccountFixtures(mem)
	store := &txAccountStore{memAccountStore: mem}
	svc := NewAccountService(store, &txMemRecorder{})

	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "disabled", "off"); err != nil {
		t.Fatal(err)
	}
	store.lockCalls, store.statusCalls = 0, 0

	// 预检可见的凭据仍是 active，但事务内复查看到 revoked（并发吊销在
	// 预检与提交之间落地——正是事务外检查闭不掉的窗口）。
	revoked := *mem.creds[credID]
	revoked.Status = "revoked"
	store.lockedCred = &revoked

	_, err = svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "restore")
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %s, want conflict (err=%v)", domain.CodeOf(err), err)
	}
	if store.statusCalls != 0 {
		t.Fatalf("status writes = %d, want 0 (re-check must run before the write)", store.statusCalls)
	}
	if store.accounts[acct.ID].Status != domain.AccountDisabled {
		t.Fatalf("status = %s, want disabled (tx rolled back)", store.accounts[acct.ID].Status)
	}
	if store.lastUoW == nil || store.lastUoW.committed || !store.lastUoW.rolledBack {
		t.Fatalf("uow = %+v, want rolled back and not committed", store.lastUoW)
	}
}

// 幂等激活分支：同样的事务内复查——账号已 active、凭据复查见 revoked →
// 409 回滚，不得 noop 成功，审计不得提交。
func TestAccountServiceSetStatusNoopActivateRevokedInTx(t *testing.T) {
	mem := newMemAccountStore()
	provID, credID := seedAccountFixtures(mem)
	store := &txAccountStore{memAccountStore: mem}
	rec := &txMemRecorder{}
	svc := NewAccountService(store, rec)

	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	// 预检可见的凭据是 active；事务内复查看到 revoked。
	revoked := *mem.creds[credID]
	revoked.Status = "revoked"
	store.lockedCred = &revoked

	_, err = svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "repeat")
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("code = %s, want conflict (err=%v)", domain.CodeOf(err), err)
	}
	if store.lastUoW == nil || store.lastUoW.committed || !store.lastUoW.rolledBack {
		t.Fatalf("uow = %+v, want rolled back and not committed", store.lastUoW)
	}
	if n := len(rec.events); n != 1 { // 只有 create 一条；noop 审计不得提交
		t.Fatalf("audit events = %d, want 1 (create only; noop audit must not commit)", n)
	}
}

// 幂等激活正常定序：事务内复查通过 → noop 审计同事务提交。
func TestAccountServiceSetStatusNoopActivateInTxCommits(t *testing.T) {
	mem := newMemAccountStore()
	provID, credID := seedAccountFixtures(mem)
	store := &txAccountStore{memAccountStore: mem}
	rec := &txMemRecorder{}
	svc := NewAccountService(store, rec)

	acct, err := svc.Create(context.Background(), testOp(), validCreateInput(provID, credID))
	if err != nil {
		t.Fatal(err)
	}
	store.lockCalls = 0 // Create 自身也做一次行锁复查，复位后再数 SetStatus 的
	got, err := svc.SetStatus(context.Background(), testOp(), acct.ID, "active", "repeat")
	if err != nil {
		t.Fatalf("noop activate must succeed: %v", err)
	}
	if got.Status != domain.AccountActive {
		t.Fatalf("status = %s, want active", got.Status)
	}
	if store.lockCalls != 1 {
		t.Fatalf("in-tx credential re-checks = %d, want 1", store.lockCalls)
	}
	if store.lastUoW == nil || !store.lastUoW.committed || store.lastUoW.rolledBack {
		t.Fatalf("uow = %+v, want committed and not rolled back", store.lastUoW)
	}
	ev := rec.find("upstream_account.status")
	if ev == nil || ev.Detail["noop"] != true {
		t.Fatalf("noop audit = %+v, want noop:true", ev)
	}
}
