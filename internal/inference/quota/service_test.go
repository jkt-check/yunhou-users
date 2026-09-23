package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// service_test.go — 准入编排的单元测试（内存 fake store；真实库并发语义
// 由 internal/inference/postgres/quota_concurrency_test.go 钉牢，本文件
// 不替代它们）。

// fakeUow records terminal calls.
type fakeUow struct {
	commits, rollbacks int
	commitErr          error
}

func (u *fakeUow) Commit(ctx context.Context) error   { u.commits++; return u.commitErr }
func (u *fakeUow) Rollback(ctx context.Context) error { u.rollbacks++; return nil }

// fakeStore emulates the persistence surface with in-memory state.
type fakeStore struct {
	uow *fakeUow

	beginErr error

	activateErrs []error // consumed one per ActivateWindowsTx call
	activateN    int
	windows      []domain.QuotaWindow

	reserveErr  error
	reserveCmds []domain.ReserveCommand

	walletErr  error
	walletCmds []domain.ReserveWalletCommand

	releaseErr error
	released   []string

	leaseErr error
	leases   []domain.AcquireLeaseCommand
}

func (s *fakeStore) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	s.uow = &fakeUow{}
	return s.uow, nil
}

func (s *fakeStore) ActivateWindowsTx(ctx context.Context, uow domain.UnitOfWork, cmd ActivateWindowsCommand) ([]domain.QuotaWindow, error) {
	s.activateN++
	if len(s.activateErrs) > 0 {
		err := s.activateErrs[0]
		s.activateErrs = s.activateErrs[1:]
		return nil, err
	}
	return s.windows, nil
}

func (s *fakeStore) Reserve(ctx context.Context, uow domain.UnitOfWork, cmd domain.ReserveCommand) (*domain.Admission, error) {
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	s.reserveCmds = append(s.reserveCmds, cmd)
	return &domain.Admission{RequestID: cmd.Request.ID}, nil
}

func (s *fakeStore) ReserveWallet(ctx context.Context, uow domain.UnitOfWork, cmd domain.ReserveWalletCommand) (*domain.Admission, error) {
	if s.walletErr != nil {
		return nil, s.walletErr
	}
	s.walletCmds = append(s.walletCmds, cmd)
	return &domain.Admission{RequestID: cmd.Request.ID}, nil
}

func (s *fakeStore) Release(ctx context.Context, uow domain.UnitOfWork, requestID string) error {	if s.releaseErr != nil {
		return s.releaseErr
	}
	s.released = append(s.released, requestID)
	return nil
}

func (s *fakeStore) AcquireLeaseTx(ctx context.Context, uow domain.UnitOfWork, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error) {
	if s.leaseErr != nil {
		return nil, s.leaseErr
	}
	s.leases = append(s.leases, cmd)
	return &domain.ConcurrencyLease{
		ID: uuid.NewString(), Scope: cmd.Scope, ScopeID: cmd.ScopeID,
		RequestID: cmd.RequestID, OwnerToken: cmd.OwnerToken, FencingToken: 1,
		State: domain.LeaseHeld, AcquiredAt: cmd.Now, ExpiresAt: cmd.Now.Add(cmd.TTL),
	}, nil
}

func serviceFixture(t *testing.T) (*Service, *fakeStore, AdmitCommand) {
	t.Helper()
	fs := &fakeStore{}
	svc := NewService(fs, domain.FixedClock{T: time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)})
	keyID := uuid.NewString()
	limit := domain.Microcredit(1_000_000)
	fs.windows = []domain.QuotaWindow{
		{ID: uuid.NewString(), Kind: domain.WindowFiveHour, Limit: limit,
			Start: time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)},
		{ID: uuid.NewString(), Kind: domain.WindowWeekly, Limit: limit,
			Start: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)},
		{ID: uuid.NewString(), Kind: domain.WindowMonthly, Limit: limit,
			Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	}
	cmd := AdmitCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: uuid.NewString(), APIKeyID: &keyID,
			EntitlementID: uuid.NewString(), ModelID: "glm-4.6",
			Protocol: domain.ProtocolOpenAIChat, Stream: true,
		},
		Entitlement: domain.Entitlement{
			ID: uuid.NewString(), AnchorAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			PolicyVersionID: "pol-1",
		},
		Policy: Policy{
			Name: "coding-plan", Revision: 1, ModelIDs: []string{"glm-4.6"},
			FiveHourLimit: &limit, WeeklyLimit: &limit, MonthlyLimit: &limit,
		},
		CreditPrice:          creditPrice(t, 1_000_000, 1_000_000, nil),
		Model:                domain.Model{ID: "glm-4.6", ContextTokens: 200_000, MaxOutputTokens: 8192},
		EstimatedInputTokens: 10_000,
	}
	return svc, fs, cmd
}

func TestAdmitHappyPath(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	// 预占上界 = 输入 10_000×1 + 输出 8192×1（强制输出上限 = 模型硬上限，
	// 客户端未声明）= 18_192 micros。
	if res.HoldMicros != 18_192 {
		t.Errorf("hold = %d, want 18192", res.HoldMicros)
	}
	if res.OutputCap != 8192 {
		t.Errorf("output cap = %d, want 8192 (forced model cap)", res.OutputCap)
	}
	if len(res.WindowIDs) != 3 {
		t.Errorf("bound windows = %v, want 3", res.WindowIDs)
	}
	if fs.uow.commits != 1 || fs.uow.rollbacks != 0 {
		t.Errorf("commits=%d rollbacks=%d, want 1/0", fs.uow.commits, fs.uow.rollbacks)
	}
	// 同事务内预占：三窗口 + Key 预算四个 hold，同一 admitted_at 绑定。
	if len(fs.reserveCmds) != 1 {
		t.Fatalf("reserve calls = %d, want 1", len(fs.reserveCmds))
	}
	rc := fs.reserveCmds[0]
	if len(rc.Holds) != 4 {
		t.Errorf("holds = %d, want 4 (3 windows + key budget)", len(rc.Holds))
	}
	if !rc.AdmittedAt.Equal(res.AdmittedAt) {
		t.Error("admitted_at must be pinned at reservation time")
	}
	if rc.Request.PolicyVersionID != "pol-1" || rc.Request.PriceVersionID == nil {
		t.Errorf("price/policy not pinned: %+v", rc.Request)
	}
	if rc.Request.Status != "" {
		// 状态由 store 置 reserved；service 不越权。
	}
	if len(fs.leases) != 0 {
		t.Errorf("no concurrency limit configured → no lease, got %d", len(fs.leases))
	}
}

func TestAdmitTakesAccountLeaseWhenPolicyCapsConcurrency(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	conc := 2
	cmd.Policy.ConcurrencyLimit = &conc
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(fs.leases))
	}
	l := fs.leases[0]
	if l.Scope != domain.LeaseScopeBillingAccount || l.ScopeID != cmd.Request.BillingAccountID {
		t.Errorf("lease scope = %s/%s, want billing account", l.Scope, l.ScopeID)
	}
	if l.RequestID != res.RequestID || l.Limit != 2 || l.OwnerToken == "" {
		t.Errorf("lease = %+v", l)
	}
	if res.AccountLease == nil || res.AccountLease.FencingToken != 1 {
		t.Errorf("result lease = %+v", res.AccountLease)
	}
}

func TestAdmitLeaseFailureRollsBackReservation(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	conc := 1
	cmd.Policy.ConcurrencyLimit = &conc
	fs.leaseErr = domain.NewError(domain.CodeInsufficientCapacity, "lease: full")
	_, err := svc.Admit(context.Background(), cmd)
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("err = %v, want insufficient_capacity", err)
	}
	// 租约失败 → 整个事务回滚：不得留下 已预占但未获租约 的半状态。
	if fs.uow.commits != 0 || fs.uow.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d, want 0/1", fs.uow.commits, fs.uow.rollbacks)
	}
}

func TestAdmitFailsClosedWhenStoreUnavailable(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	fs.beginErr = errors.New("connection refused")
	_, err := svc.Admit(context.Background(), cmd)
	if err == nil {
		t.Fatal("storage unavailable must refuse admission (fail-closed)")
	}
	if fs.activateN != 0 || len(fs.reserveCmds) != 0 {
		t.Error("no store call may proceed after Begin failure")
	}
}

func TestAdmitRetriesWindowActivationConflict(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	fs.activateErrs = []error{ErrWindowActivationConflict}
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if fs.activateN != 2 {
		t.Errorf("activate calls = %d, want 2 (one retried conflict)", fs.activateN)
	}
	if res == nil {
		t.Error("want admission after retry")
	}
}

func TestAdmitDoesNotRetryDuplicateRequest(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	// 请求主键冲突（重复投递）不是激活冲突：立即失败，不重试。
	fs.reserveErr = domain.NewError(domain.CodeConflict, "insert request: duplicate key")
	_, err := svc.Admit(context.Background(), cmd)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	if fs.activateN != 1 || len(fs.reserveCmds) != 0 {
		t.Errorf("activate=%d reserves=%d, want no retry", fs.activateN, len(fs.reserveCmds))
	}
}

func TestAdmitQuotaExceededCarriesDeficit(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	// 五小时窗口只剩 5_000；预占上界 18_192 → 拒绝并带缺口信息，不静默钳制。
	fs.windows[0].Used = 995_000
	_, err := svc.Admit(context.Background(), cmd)
	var qe *domain.QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want QuotaExceededError", err)
	}
	if len(qe.BlockedBy) != 1 || qe.BlockedBy[0].Kind != domain.WindowFiveHour {
		t.Errorf("blocked by %+v, want five_hour", qe.BlockedBy)
	}
	if qe.DeficitMicros == nil || *qe.DeficitMicros != 18_192-5_000 {
		t.Errorf("deficit = %v, want %d", qe.DeficitMicros, 18_192-5_000)
	}
	if len(fs.reserveCmds) != 0 || fs.uow.commits != 0 || fs.uow.rollbacks != 1 {
		t.Errorf("reserve=%d commits=%d rollbacks=%d, want rejected before reserve",
			len(fs.reserveCmds), fs.uow.commits, fs.uow.rollbacks)
	}
}

func TestAdmitDisabledWindowIsSkippedNotUnlimited(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	cmd.Policy.MonthlyLimit = nil // 月窗口禁用（显式，≠ 无限）
	fs.windows = fs.windows[:2]
	res, err := svc.Admit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.WindowIDs) != 2 {
		t.Errorf("bound windows = %v, want five_hour+weekly only", res.WindowIDs)
	}
	// limits 传给激活的只有启用窗口。
	if len(fs.reserveCmds) != 1 || len(fs.reserveCmds[0].Holds) != 3 {
		t.Errorf("holds = %+v, want 2 windows + key", fs.reserveCmds)
	}
}

func TestReleaseAdmissionDelegates(t *testing.T) {
	svc, fs, _ := serviceFixture(t)
	if err := svc.ReleaseAdmission(context.Background(), "req-1"); err != nil {
		t.Fatal(err)
	}
	if len(fs.released) != 1 || fs.released[0] != "req-1" {
		t.Errorf("released = %v", fs.released)
	}
	if fs.uow.commits != 1 {
		t.Errorf("commits = %d, want 1", fs.uow.commits)
	}
}
