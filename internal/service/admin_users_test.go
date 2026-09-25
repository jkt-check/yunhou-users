package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/repo"
)

// fakeAdminUsersRepo implements repo.AdminUsersRepo for unit tests. WithTx
// simulates commit/rollback: when fn returns an error, everything the fake
// tx recorded is discarded (mirroring a real rollback) so tests can assert
// "nothing persisted on rollback".
type fakeAdminUsersRepo struct {
	tx *fakeAdminUsersTx

	searchRows []repo.AdminUserSearchRow
	searchErr  error
	detailRows []repo.AdminUserSearchRow
	activeRow  *repo.AdminActiveSubscriptionRow
	histRows   []repo.AdminSubscriptionHistoryRow

	usersCounts repo.AdminOpsCounts
	paidCum     repo.AdminOpsCounts
	paidAct     repo.AdminOpsCounts
	revenue     repo.AdminOpsAmounts
	opsErr      error

	// replayRecord backs the non-tx GetIdempotencyRecord re-read after a
	// same-key race rollback.
	replayRecord *repo.AdminIdempotencyRecord

	gotSearchExact string
	gotSearchLike  string
	gotHistLimit   int
	gotOpsBounds   [3]time.Time
}

func (f *fakeAdminUsersRepo) SearchUsers(_ context.Context, exact, likePattern string) ([]repo.AdminUserSearchRow, error) {
	f.gotSearchExact, f.gotSearchLike = exact, likePattern
	return f.searchRows, f.searchErr
}

func (f *fakeAdminUsersRepo) FindUserWithIdentities(_ context.Context, _ string) ([]repo.AdminUserSearchRow, error) {
	return f.detailRows, nil
}

func (f *fakeAdminUsersRepo) FindActiveMembership(_ context.Context, _ string) (*repo.AdminActiveSubscriptionRow, error) {
	return f.activeRow, nil
}

func (f *fakeAdminUsersRepo) ListMembershipHistory(_ context.Context, _ string, limit int) ([]repo.AdminSubscriptionHistoryRow, error) {
	f.gotHistLimit = limit
	return f.histRows, nil
}

func (f *fakeAdminUsersRepo) OpsUserCounts(_ context.Context, d, w, m time.Time) (repo.AdminOpsCounts, error) {
	f.gotOpsBounds = [3]time.Time{d, w, m}
	return f.usersCounts, f.opsErr
}

func (f *fakeAdminUsersRepo) OpsPaidUsersCumulative(_ context.Context, _, _, _ time.Time) (repo.AdminOpsCounts, error) {
	return f.paidCum, f.opsErr
}

func (f *fakeAdminUsersRepo) OpsPaidUsersActive(_ context.Context, _, _, _ time.Time) (repo.AdminOpsCounts, error) {
	return f.paidAct, f.opsErr
}

func (f *fakeAdminUsersRepo) OpsRevenue(_ context.Context, _, _, _ time.Time) (repo.AdminOpsAmounts, error) {
	return f.revenue, f.opsErr
}

func (f *fakeAdminUsersRepo) GetIdempotencyRecord(_ context.Context, _, _ string) (*repo.AdminIdempotencyRecord, error) {
	return f.replayRecord, nil
}

func (f *fakeAdminUsersRepo) WithTx(_ context.Context, fn func(tx repo.AdminUsersTx) error) error {
	if f.tx == nil {
		f.tx = &fakeAdminUsersTx{}
	}
	f.tx.reset()
	if err := fn(f.tx); err != nil {
		// Simulated rollback: drop everything recorded inside the tx.
		f.tx.reset()
		return err
	}
	f.tx.committed = true
	return nil
}

type fakeAuditCall struct {
	actor, action, target string
	ctx                   map[string]any
}

type fakeAdminUsersTx struct {
	committed bool

	userExists bool
	activeRow  *repo.AdminActiveSubscriptionRow
	activeErr  error

	insertPlanID   string
	insertExpiry   time.Time
	insertErr      error
	insertCalled   bool
	extendUpdated  bool
	extendPlanID   string
	extendExpiry   time.Time
	extendCalled   bool
	idemStored     json.RawMessage
	idemHash       *string
	idemInsertOK   bool
	idemInsertCall bool
	idemAction     string
	idemHashOut    string

	audits []fakeAuditCall
}

func (t *fakeAdminUsersTx) reset() {
	t.committed = false
	t.insertCalled, t.extendCalled, t.idemInsertCall = false, false, false
	t.audits = nil
}

func (t *fakeAdminUsersTx) UserExists(_ context.Context, _ string) (bool, error) {
	return t.userExists, nil
}

func (t *fakeAdminUsersTx) FindActiveMembershipForUpdate(_ context.Context, _ string) (*repo.AdminActiveSubscriptionRow, error) {
	return t.activeRow, t.activeErr
}

func (t *fakeAdminUsersTx) InsertMembershipSub(_ context.Context, _ string, _ int) (string, time.Time, error) {
	t.insertCalled = true
	if t.insertErr != nil {
		return "", time.Time{}, t.insertErr
	}
	return t.insertPlanID, t.insertExpiry, nil
}

func (t *fakeAdminUsersTx) ExtendMembershipSub(_ context.Context, _ string, _ int) (bool, string, time.Time, error) {
	t.extendCalled = true
	return t.extendUpdated, t.extendPlanID, t.extendExpiry, nil
}

func (t *fakeAdminUsersTx) GetIdempotencyRecord(_ context.Context, _, _ string) (*repo.AdminIdempotencyRecord, error) {
	if t.idemStored == nil {
		return nil, nil
	}
	return &repo.AdminIdempotencyRecord{Response: t.idemStored, RequestHash: t.idemHash}, nil
}

func (t *fakeAdminUsersTx) InsertIdempotencyKey(_ context.Context, _, _, action, _, requestHash string, _ json.RawMessage) (bool, error) {
	t.idemInsertCall = true
	t.idemAction = action
	t.idemHashOut = requestHash
	return t.idemInsertOK, nil
}

func (t *fakeAdminUsersTx) InsertAudit(_ context.Context, actor, action, target string, ctxData map[string]any) error {
	t.audits = append(t.audits, fakeAuditCall{actor: actor, action: action, target: target, ctx: ctxData})
	return nil
}

var (
	adminTestUserID = "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11"
	adminTestExpiry = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
)

func activeMembershipRow(planID string, expiresAt *time.Time, planIsActive *bool) *repo.AdminActiveSubscriptionRow {
	return &repo.AdminActiveSubscriptionRow{
		PlanID:       planID,
		StartedAt:    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:    expiresAt,
		PlanIsActive: planIsActive,
		Price:        func() *float64 { v := 19.9; return &v }(),
	}
}

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

func TestAddVipDaysGrant(t *testing.T) {
	expiry := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists:   true,
		activeRow:    nil,
		insertPlanID: "monthly",
		insertExpiry: expiry,
		idemInsertOK: true,
	}}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Action != "granted" || res.PlanID != "monthly" || res.Before != nil {
		t.Fatalf("result: %+v", res)
	}
	if res.After == nil || res.After.ExpiresAt == nil || *res.After.ExpiresAt != "2026-10-24T08:00:00Z" {
		t.Fatalf("after: %+v", res.After)
	}
	if !fake.tx.insertCalled || fake.tx.extendCalled {
		t.Fatalf("insert=%v extend=%v", fake.tx.insertCalled, fake.tx.extendCalled)
	}
	if len(fake.tx.audits) != 1 || fake.tx.audits[0].action != "vip.grant" {
		t.Fatalf("audits: %+v", fake.tx.audits)
	}
	if fake.tx.audits[0].actor != "admin:yundash" || fake.tx.audits[0].target != "user:"+adminTestUserID {
		t.Fatalf("audit attribution: %+v", fake.tx.audits[0])
	}
	if !fake.tx.idemInsertCall {
		t.Fatal("idempotency key not recorded")
	}
	// 幂等表 action 必须是 vip.grant(与 037 迁移注释/spec §5.3 一致),
	// 而不是响应里的 "granted"。
	if fake.tx.idemAction != "vip.grant" {
		t.Fatalf("idempotency action = %q, want vip.grant", fake.tx.idemAction)
	}
	// 载荷摘要随键落库(038),重放比对用。
	if fake.tx.idemHashOut != adminVipRequestHash(adminTestUserID, 30) {
		t.Fatalf("idempotency request hash = %q", fake.tx.idemHashOut)
	}
}

func TestAddVipDaysExtend(t *testing.T) {
	newExpiry := adminTestExpiry.Add(30 * 24 * time.Hour)
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists:    true,
		activeRow:     activeMembershipRow("monthly", &adminTestExpiry, boolPtr(true)),
		extendUpdated: true,
		extendPlanID:  "monthly",
		extendExpiry:  newExpiry,
	}}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Action != "extended" || res.Before == nil || res.After == nil {
		t.Fatalf("result: %+v", res)
	}
	if *res.Before.ExpiresAt != "2026-10-01T12:00:00Z" {
		t.Fatalf("before: %+v", res.Before)
	}
	if len(fake.tx.audits) != 1 || fake.tx.audits[0].action != "vip.extend" {
		t.Fatalf("audits: %+v", fake.tx.audits)
	}
	// No idempotency key → no idempotency write.
	if fake.tx.idemInsertCall {
		t.Fatal("idempotency write without a key")
	}
}

func TestAddVipDaysExtendTrial(t *testing.T) {
	// Rule 4: trial rows extend (it extends the trial period).
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists:    true,
		activeRow:     activeMembershipRow("trial", &adminTestExpiry, boolPtr(true)),
		extendUpdated: true,
		extendPlanID:  "trial",
		extendExpiry:  adminTestExpiry.Add(7 * 24 * time.Hour),
	}}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 7, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Action != "extended" || res.PlanID != "trial" {
		t.Fatalf("result: %+v", res)
	}
}

func TestAddVipDaysLifetimeRejected(t *testing.T) {
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists: true,
		activeRow:  activeMembershipRow("yearly", nil, boolPtr(true)),
	}}
	svc := NewAdminUsersService(fake)

	_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "")
	if !errors.Is(err, ErrAdminVipRejected) {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(err.Error(), "终身 VIP") {
		t.Fatalf("message: %v", err)
	}
	// Rejection audit commits; no subscription write.
	if len(fake.tx.audits) != 1 || fake.tx.audits[0].action != "vip.reject" {
		t.Fatalf("audits: %+v", fake.tx.audits)
	}
	if fake.tx.audits[0].ctx["reject_reason"] == nil {
		t.Fatalf("audit ctx missing reject_reason: %+v", fake.tx.audits[0].ctx)
	}
	if fake.tx.insertCalled || fake.tx.extendCalled {
		t.Fatal("subscription written on rejection")
	}
}

func TestAddVipDaysRetiredPlanRejected(t *testing.T) {
	for _, tc := range []struct {
		name         string
		planID       string
		planIsActive *bool
	}{
		{"quarterly always retired", "quarterly", boolPtr(true)},
		{"free always retired", "free", boolPtr(true)},
		{"inactive plan", "yearly", boolPtr(false)},
		{"missing plan row counts as retired", "ghost", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
				userExists: true,
				activeRow:  activeMembershipRow(tc.planID, &adminTestExpiry, tc.planIsActive),
			}}
			svc := NewAdminUsersService(fake)

			_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "")
			if !errors.Is(err, ErrAdminVipRejected) {
				t.Fatalf("err: %v", err)
			}
			if !strings.Contains(err.Error(), "已退役套餐（"+tc.planID+"）") {
				t.Fatalf("message: %v", err)
			}
			if fake.tx.extendCalled {
				t.Fatal("subscription written on rejection")
			}
			if len(fake.tx.audits) != 1 || fake.tx.audits[0].action != "vip.reject" {
				t.Fatalf("audits: %+v", fake.tx.audits)
			}
		})
	}
}

func TestAddVipDaysConcurrentCancel(t *testing.T) {
	// Rule 7: the UPDATE matched 0 rows — the pre-read active row was
	// concurrently cancelled.
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists:    true,
		activeRow:     activeMembershipRow("monthly", &adminTestExpiry, boolPtr(true)),
		extendUpdated: false,
	}}
	svc := NewAdminUsersService(fake)

	_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "")
	if !errors.Is(err, ErrAdminVipRejected) {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(err.Error(), "订阅状态已变化") {
		t.Fatalf("message: %v", err)
	}
}

func TestAddVipDaysUserNotFound(t *testing.T) {
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{userExists: false}}
	svc := NewAdminUsersService(fake)

	_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err: %v", err)
	}
	// Rolled back: the fake discards tx state on error.
	if len(fake.tx.audits) != 0 {
		t.Fatalf("audit written for 404: %+v", fake.tx.audits)
	}
}

func TestAddVipDaysIdempotentReplay(t *testing.T) {
	stored := json.RawMessage(`{"action":"granted","planId":"monthly","before":null,"after":{"planId":"monthly","expiresAt":"2026-10-24T08:00:00Z"}}`)
	matchingHash := adminVipRequestHash(adminTestUserID, 30)
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists: true,
		idemStored: stored,
		idemHash:   &matchingHash,
	}}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Action != "granted" || *res.After.ExpiresAt != "2026-10-24T08:00:00Z" {
		t.Fatalf("replay result: %+v", res)
	}
	if fake.tx.insertCalled || fake.tx.extendCalled || len(fake.tx.audits) != 0 {
		t.Fatal("replay must not write subscription or audit")
	}
}

func TestAddVipDaysIdempotentReplayLegacyRow(t *testing.T) {
	// 037 时代写入的存量行没有 request_hash(NULL):跳过载荷比对,
	// 保持既有重放语义(038 迁移口径)。
	stored := json.RawMessage(`{"action":"granted","planId":"monthly","before":null,"after":{"planId":"monthly","expiresAt":"2026-10-24T08:00:00Z"}}`)
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists: true,
		idemStored: stored,
	}}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-legacy")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Action != "granted" {
		t.Fatalf("replay result: %+v", res)
	}
}

func TestAddVipDaysIdempotentReplayPayloadMismatch(t *testing.T) {
	// I-8:同键不同载荷(这里 days 30→60)是调用方键复用错误,按 409 拒绝
	// (ErrAdminIdemPayloadMismatch),不当良性重放——对齐 wallet
	// adjustments 面 admin_adjustments.go 的同键异载荷 409。
	stored := json.RawMessage(`{"action":"granted","planId":"monthly","before":null,"after":{"planId":"monthly","expiresAt":"2026-10-24T08:00:00Z"}}`)
	otherHash := adminVipRequestHash(adminTestUserID, 60)
	fake := &fakeAdminUsersRepo{tx: &fakeAdminUsersTx{
		userExists: true,
		idemStored: stored,
		idemHash:   &otherHash,
	}}
	svc := NewAdminUsersService(fake)

	_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-1")
	if !errors.Is(err, ErrAdminIdemPayloadMismatch) {
		t.Fatalf("err: %v", err)
	}
	if fake.tx.insertCalled || fake.tx.extendCalled || len(fake.tx.audits) != 0 || fake.tx.idemInsertCall {
		t.Fatal("mismatch replay must not write anything")
	}
}

func TestAddVipDaysIdempotencyRace(t *testing.T) {
	// Our tx won the business write but lost the idempotency-key race: the
	// concurrent same-key request committed first. The service rolls back
	// and replays the winner's response.
	winner := json.RawMessage(`{"action":"extended","planId":"monthly","before":{"planId":"monthly","expiresAt":"2026-10-01T12:00:00Z"},"after":{"planId":"monthly","expiresAt":"2026-10-31T12:00:00Z"}}`)
	fake := &fakeAdminUsersRepo{
		tx: &fakeAdminUsersTx{
			userExists:    true,
			activeRow:     activeMembershipRow("monthly", &adminTestExpiry, boolPtr(true)),
			extendUpdated: true,
			extendPlanID:  "monthly",
			extendExpiry:  adminTestExpiry.Add(30 * 24 * time.Hour),
			idemInsertOK:  false,
		},
		replayRecord: &repo.AdminIdempotencyRecord{
			Response:    winner,
			RequestHash: strPtr(adminVipRequestHash(adminTestUserID, 30)),
		},
	}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-race")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if *res.After.ExpiresAt != "2026-10-31T12:00:00Z" {
		t.Fatalf("should replay winner response, got %+v", res)
	}
	// Rollback discarded our audit row.
	if len(fake.tx.audits) != 0 {
		t.Fatalf("loser audit must roll back: %+v", fake.tx.audits)
	}
}

func TestAddVipDaysRaceReplayPayloadMismatch(t *testing.T) {
	// I-8:两个不同载荷的请求撞同一幂等键——胜者已提交,败者回滚后重放
	// 时也必须比对载荷,不一致按 409 拒绝而不是把胜者的结果错发给
	// 另一个载荷的调用方。
	winner := json.RawMessage(`{"action":"extended","planId":"monthly","before":{"planId":"monthly","expiresAt":"2026-10-01T12:00:00Z"},"after":{"planId":"monthly","expiresAt":"2026-10-31T12:00:00Z"}}`)
	fake := &fakeAdminUsersRepo{
		tx: &fakeAdminUsersTx{
			userExists:    true,
			activeRow:     activeMembershipRow("monthly", &adminTestExpiry, boolPtr(true)),
			extendUpdated: true,
			extendPlanID:  "monthly",
			extendExpiry:  adminTestExpiry.Add(30 * 24 * time.Hour),
			idemInsertOK:  false,
		},
		replayRecord: &repo.AdminIdempotencyRecord{
			Response:    winner,
			RequestHash: strPtr(adminVipRequestHash(adminTestUserID, 60)),
		},
	}
	svc := NewAdminUsersService(fake)

	_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-race")
	if !errors.Is(err, ErrAdminIdemPayloadMismatch) {
		t.Fatalf("err: %v", err)
	}
}

func TestAddVipDaysGrantConflictReplay(t *testing.T) {
	// 并发同 (app_id, key) 的 grant 路径:双方预读都看到无 active 行
	// (FOR UPDATE 锁不到不存在的行),败者的 INSERT 撞 027 的部分唯一
	// 索引 → repo 返回 ErrAdminSubscriptionConflict → 回滚 + 重放赢家响应,
	// 而不是 500。
	winner := json.RawMessage(`{"action":"granted","planId":"monthly","before":null,"after":{"planId":"monthly","expiresAt":"2026-10-24T08:00:00Z"}}`)
	fake := &fakeAdminUsersRepo{
		tx: &fakeAdminUsersTx{
			userExists: true,
			activeRow:  nil,
			insertErr:  repo.ErrAdminSubscriptionConflict,
		},
		replayRecord: &repo.AdminIdempotencyRecord{
			Response:    winner,
			RequestHash: strPtr(adminVipRequestHash(adminTestUserID, 30)),
		},
	}
	svc := NewAdminUsersService(fake)

	res, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "k-grant-race")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Action != "granted" || *res.After.ExpiresAt != "2026-10-24T08:00:00Z" {
		t.Fatalf("should replay winner response, got %+v", res)
	}
	// 回滚:无审计、无幂等键写入残留。
	if len(fake.tx.audits) != 0 || fake.tx.idemInsertCall {
		t.Fatalf("loser state must roll back: audits=%+v idem=%v", fake.tx.audits, fake.tx.idemInsertCall)
	}
}

func TestAddVipDaysGrantConflictWithoutKey(t *testing.T) {
	// 同样的唯一索引冲突,但请求没带 Idempotency-Key:没有可重放的响应。
	// spec §1.2:映射为 vip.reject 审计(同事务提交)+ 409,而不是 500。
	fake := &fakeAdminUsersRepo{
		tx: &fakeAdminUsersTx{
			userExists: true,
			activeRow:  nil,
			insertErr:  repo.ErrAdminSubscriptionConflict,
		},
	}
	svc := NewAdminUsersService(fake)

	_, err := svc.AddVipDays(context.Background(), "yundash", "admin:yundash", adminTestUserID, 30, "")
	if !errors.Is(err, ErrAdminVipRejected) {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(err.Error(), "并发开通冲突") {
		t.Fatalf("message: %v", err)
	}
	// Rejection audit commits in the same tx (I-4); the subscription insert
	// rolled back to its savepoint so the row never lands.
	if len(fake.tx.audits) != 1 || fake.tx.audits[0].action != "vip.reject" {
		t.Fatalf("audits: %+v", fake.tx.audits)
	}
	if fake.tx.audits[0].ctx["reject_reason"] == nil {
		t.Fatalf("audit ctx missing reject_reason: %+v", fake.tx.audits[0].ctx)
	}
	if fake.tx.idemInsertCall {
		t.Fatal("idempotency write on a rejection")
	}
}

func TestSearchUsersAggregation(t *testing.T) {
	nick := "爱丽丝"
	email := "a@example.com"
	uid2 := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	fake := &fakeAdminUsersRepo{searchRows: []repo.AdminUserSearchRow{
		{ID: adminTestUserID, Nickname: &nick, Status: "active", CreatedAt: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			Provider: func() *string { s := "wechat"; return &s }(), ProviderUID: func() *string { s := "wx-1"; return &s }(), Email: &email},
		// Same user via a second identity (row-level match).
		{ID: adminTestUserID, Nickname: &nick, Status: "active", CreatedAt: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
			Provider: func() *string { s := "github"; return &s }(), ProviderUID: func() *string { s := "gh-1"; return &s }()},
		// Second user with no identities.
		{ID: uid2, Status: "active", CreatedAt: time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)},
	}}
	svc := NewAdminUsersService(fake)

	res, err := svc.SearchUsers(context.Background(), "  alice  ")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.gotSearchExact != "alice" {
		t.Fatalf("exact = %q (trim failed)", fake.gotSearchExact)
	}
	if len(res.Users) != 2 {
		t.Fatalf("users: %+v", res.Users)
	}
	if len(res.Users[0].Identities) != 2 || len(res.Users[1].Identities) != 0 {
		t.Fatalf("identities: %+v / %+v", res.Users[0].Identities, res.Users[1].Identities)
	}
	if res.Users[0].CreatedAt != "2026-09-20T00:00:00Z" {
		t.Fatalf("createdAt: %q", res.Users[0].CreatedAt)
	}
}

func TestSearchUsersValidation(t *testing.T) {
	svc := NewAdminUsersService(&fakeAdminUsersRepo{})

	if _, err := svc.SearchUsers(context.Background(), "   "); !errors.Is(err, ErrAdminInvalidParam) {
		t.Fatalf("empty q err: %v", err)
	}

	// Truncation at 64 chars + LIKE escaping of %, _, \.
	long := strings.Repeat("x", 70)
	fake := &fakeAdminUsersRepo{}
	svc = NewAdminUsersService(fake)
	if _, err := svc.SearchUsers(context.Background(), long); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(fake.gotSearchExact) != 64 {
		t.Fatalf("exact len = %d, want 64", len(fake.gotSearchExact))
	}

	if _, err := svc.SearchUsers(context.Background(), `10%_off\now`); err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.gotSearchLike != `10\%\_off\\now` {
		t.Fatalf("like = %q", fake.gotSearchLike)
	}
}

func TestSearchUsersCap(t *testing.T) {
	rows := make([]repo.AdminUserSearchRow, 0, 60)
	for i := 0; i < 60; i++ {
		rows = append(rows, repo.AdminUserSearchRow{
			ID:        fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", i),
			Status:    "active",
			CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour),
		})
	}
	svc := NewAdminUsersService(&fakeAdminUsersRepo{searchRows: rows})
	res, err := svc.SearchUsers(context.Background(), "x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(res.Users) != 20 {
		t.Fatalf("users = %d, want cap 20", len(res.Users))
	}
}

func TestGetUserDetail(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		svc := NewAdminUsersService(&fakeAdminUsersRepo{})
		_, err := svc.GetUserDetail(context.Background(), adminTestUserID)
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("err: %v", err)
		}
	})

	t.Run("assembly", func(t *testing.T) {
		nick := "n"
		provider := "wechat"
		fake := &fakeAdminUsersRepo{
			detailRows: []repo.AdminUserSearchRow{
				{ID: adminTestUserID, Nickname: &nick, Status: "active", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Provider: &provider, ProviderUID: func() *string { s := "u1"; return &s }()},
			},
			activeRow: activeMembershipRow("monthly", &adminTestExpiry, boolPtr(true)),
			histRows: []repo.AdminSubscriptionHistoryRow{
				{PlanID: "monthly", Status: "active", StartedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: &adminTestExpiry, CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
			},
		}
		svc := NewAdminUsersService(fake)
		detail, err := svc.GetUserDetail(context.Background(), adminTestUserID)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if detail.User.ID != adminTestUserID || len(detail.Identities) != 1 {
			t.Fatalf("detail: %+v", detail)
		}
		if detail.ActiveSubscription == nil || detail.ActiveSubscription.PlanID != "monthly" {
			t.Fatalf("active: %+v", detail.ActiveSubscription)
		}
		if len(detail.History) != 1 || fake.gotHistLimit != 5 {
			t.Fatalf("history: %+v limit=%d", detail.History, fake.gotHistLimit)
		}
	})

	t.Run("no active subscription is null", func(t *testing.T) {
		fake := &fakeAdminUsersRepo{
			detailRows: []repo.AdminUserSearchRow{{ID: adminTestUserID, Status: "active", CreatedAt: time.Now()}},
		}
		svc := NewAdminUsersService(fake)
		detail, err := svc.GetUserDetail(context.Background(), adminTestUserID)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if detail.ActiveSubscription != nil {
			t.Fatalf("active: %+v", detail.ActiveSubscription)
		}
	})
}
