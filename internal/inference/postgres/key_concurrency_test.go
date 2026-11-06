package postgres

// key_concurrency_test.go — Task 5：Key 级并发上限（inference_api_keys.
// concurrency_limit）的持久协调。真实 PostgreSQL：租约行是唯一事实，
// 进程内计数器不算数。
//
// 覆盖：
//   - 同一 Key 上限 1：第二个并发准入被拒（insufficient_capacity），且整笔
//     准入回滚（不留请求行/预占/速率计数）；
//   - 不同 Key 互不占 Key 槽，但账户总上限照旧生效（Key 限制只收窄，不放宽）；
//   - 未设限的 Key 不占任何 Key 槽（不产生 api_key 租约行）；
//   - api_key 作用域的租约协议：同 Key 串行、异 Key 独立、超时回收后槽位
//     归还（进程崩溃不永久冻结并发槽）。

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// setKeyConcurrencyLimit writes the customer Key's own concurrency limit
// (the management surface's field) directly on the row.
func setKeyConcurrencyLimit(t *testing.T, s *Store, keyID string, limit int) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE inference_api_keys SET concurrency_limit = $2 WHERE id = $1`, keyID, limit); err != nil {
		t.Fatalf("set key concurrency limit: %v", err)
	}
}

// addSiblingKey inserts another customer Key of the SAME billing account
// (one account, many keys — the shared entitlement pool is unchanged).
func addSiblingKey(t *testing.T, s *Store, accountID string) string {
	t.Helper()
	key := &domain.APIKey{
		BillingAccountID: accountID, Name: "sibling",
		Prefix: "yk-test-" + uuid.NewString()[:8],
	}
	if err := s.InsertAPIKey(context.Background(), key, "sha256:"+uuid.NewString()); err != nil {
		t.Fatalf("insert sibling key: %v", err)
	}
	return key.ID
}

// heldKeyLeases counts the still-held api_key-scope leases of one Key.
func heldKeyLeases(t *testing.T, s *Store, keyID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM inference_concurrency_leases
		  WHERE scope = 'api_key' AND scope_id = $1 AND state = 'held'`, keyID).Scan(&n); err != nil {
		t.Fatalf("count key leases: %v", err)
	}
	return n
}

// TestKeyConcurrencyLimit_SameKeySecondAdmissionIsRejected: 同一 Key 上限 1
// → 第二个并发准入被拒，且整笔准入回滚（无请求行、无预占、无速率计数）。
// 策略本身不设并发上限 —— 唯一的闸门是 Key 自己的槽位。
func TestKeyConcurrencyLimit_SameKeySecondAdmissionIsRejected(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)
	setKeyConcurrencyLimit(t, s, f.keyID, 1)

	if _, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil)); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if heldKeyLeases(t, s, f.keyID) != 1 {
		t.Fatalf("held key leases = %d, want 1 (the admitted request holds the Key slot)", heldKeyLeases(t, s, f.keyID))
	}

	// 第二个并发准入：额度充足，但该 Key 的槽位已满。
	_, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("second admit: err = %v, want insufficient_capacity (同一 Key 的并发槽已满)", err)
	}
	// 整笔回滚：不得留下第二个请求行，也不得留下第二笔预占。
	if n := countRequests(t, db, f.entID, ""); n != 1 {
		t.Errorf("requests = %d, want 1 (the rejected admission rolled back)", n)
	}
	if _, reserved := windowSums(t, db, f.entID); reserved != 30_000 {
		t.Errorf("reserved = %d, want 30000 (the rejected admission left no holds)", reserved)
	}
	var rateRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_rate_reservations`).Scan(&rateRows); err != nil {
		t.Fatal(err)
	}
	if rateRows != 2 {
		t.Errorf("rate reservation rows = %d, want 2 (account + key of the ONE admitted request)", rateRows)
	}
	// 释放槽位后准入恢复。
	releaseHeldKeyLease(t, s, f.keyID)
	if _, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil)); err != nil {
		t.Errorf("admit after the key slot was released: %v", err)
	}
}

// TestKeyConcurrencyLimit_CrashedRequestDoesNotFreezeTheSlot: 进程崩溃
// （请求没来得及释放租约）不得永久冻结该 Key 的并发槽 —— 租约 TTL 回收后
// 槽位归还，新准入照常放行（恢复路径的收敛机制）。
func TestKeyConcurrencyLimit_CrashedRequestDoesNotFreezeTheSlot(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)
	setKeyConcurrencyLimit(t, s, f.keyID, 1)

	if _, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil)); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// 模拟进程崩溃：租约仍在库里，但已过期（CHECK(expires_at > acquired_at)
	// 要求同时回拨 acquired_at）。
	if _, err := db.Exec(`UPDATE inference_concurrency_leases
		SET acquired_at = now() - interval '20 minutes',
		    expires_at = now() - interval '10 minutes'
		WHERE scope = 'api_key' AND scope_id = $1`, f.keyID); err != nil {
		t.Fatalf("expire the key lease: %v", err)
	}
	// TTL 回收后槽位归还：新准入放行。
	if _, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil)); err != nil {
		t.Fatalf("admit after the crashed request's lease expired: %v", err)
	}
}

// releaseHeldKeyLease releases the single held api_key-scope lease of a Key
// (the DB row is the fact — the test does not depend on the in-process
// admission result shape).
func releaseHeldKeyLease(t *testing.T, s *Store, keyID string) {
	t.Helper()
	var id, owner string
	var fencing int64
	if err := s.db.QueryRow(
		`SELECT id, owner_token, fencing_token FROM inference_concurrency_leases
		  WHERE scope = 'api_key' AND scope_id = $1 AND state = 'held'`, keyID).
		Scan(&id, &owner, &fencing); err != nil {
		t.Fatalf("load held key lease: %v", err)
	}
	if err := s.ReleaseLease(context.Background(), id, owner, fencing); err != nil {
		t.Fatalf("release key lease: %v", err)
	}
}

// TestKeyConcurrencyLimit_DifferentKeysAreIndependent_AccountCapStillApplies:
// 不同 Key 互不占对方的 Key 槽，但账户总上限（策略并发上限）照旧生效 ——
// Key 限制只收窄共享权益，绝不放宽账户级上限。
func TestKeyConcurrencyLimit_DifferentKeysAreIndependent_AccountCapStillApplies(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)

	key2 := addSiblingKey(t, s, f.accountID)
	key3 := addSiblingKey(t, s, f.accountID)
	setKeyConcurrencyLimit(t, s, f.keyID, 1)
	setKeyConcurrencyLimit(t, s, key2, 1)
	// key3 不设限（NULL）—— 无 Key 级限制。

	accountLimit := 3
	admit := func(keyID string) error {
		cmd := admitCmd(f, priceID, 10_000, big, big, big, &accountLimit)
		cmd.Request.APIKeyID = &keyID
		_, err := svc.Admit(ctx, cmd)
		return err
	}

	// 两个受限 Key 各占自己的槽：互不影响。
	if err := admit(f.keyID); err != nil {
		t.Fatalf("admit key1: %v", err)
	}
	if err := admit(key2); err != nil {
		t.Fatalf("admit key2 (a different key must not occupy key1's slot): %v", err)
	}
	if heldKeyLeases(t, s, f.keyID) != 1 || heldKeyLeases(t, s, key2) != 1 {
		t.Errorf("key leases = %d/%d, want 1/1 (each key holds its own slot)",
			heldKeyLeases(t, s, f.keyID), heldKeyLeases(t, s, key2))
	}
	// 同一 Key 再来一次：账户还有空位（2/3），被拒的只能是 Key 自己的槽。
	if err := admit(f.keyID); domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("second admission on the same key: err = %v, want insufficient_capacity", err)
	}
	// 未设限的 Key 不占 Key 槽，但账户总上限仍然生效。
	if err := admit(key3); err != nil {
		t.Fatalf("admit the unlimited key: %v", err)
	}
	if heldKeyLeases(t, s, key3) != 0 {
		t.Errorf("unlimited key holds %d key leases, want 0 (缺省不加 Key 限制)", heldKeyLeases(t, s, key3))
	}
	// 账户总上限已满（3/3）：未设限的 Key 也进不来 —— Key 限制不放宽账户上限。
	if err := admit(key3); domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("admit beyond the account cap: err = %v, want insufficient_capacity (账户总上限仍然生效)", err)
	}
	// 账户槽位由三个请求占满：billing_account 作用域 held = 3。
	var held int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases
		WHERE scope = 'billing_account' AND state = 'held'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 3 {
		t.Errorf("held account leases = %d, want 3 (the account cap still applies)", held)
	}
}

// TestKeyConcurrencyLimit_UnlimitedKeyTakesNoLease: 未设限的 Key 不产生
// api_key 租约行（缺省不增加 Key 限制）。
func TestKeyConcurrencyLimit_UnlimitedKeyTakesNoLease(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)

	if _, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	var keyLeases int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases WHERE scope = 'api_key'`).Scan(&keyLeases); err != nil {
		t.Fatal(err)
	}
	if keyLeases != 0 {
		t.Errorf("api_key lease rows = %d, want 0 (a key without a limit takes no lease)", keyLeases)
	}
}

// TestAPIKeyLeaseScope_IsolatesKeysAndReclaimsAfterTTL: api_key 作用域的租约
// 协议本身：同 Key 串行、异 Key 独立；超时回收后槽位归还 —— 进程崩溃
// （来不及释放）不得永久冻结并发槽。
func TestAPIKeyLeaseScope_IsolatesKeysAndReclaimsAfterTTL(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)
	now := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)

	mkReq := func() string {
		res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
		if err != nil {
			t.Fatal(err)
		}
		return res.RequestID
	}
	key1 := f.keyID
	key2 := addSiblingKey(t, s, f.accountID)

	acquire := func(scopeID, owner string, at time.Time) (*domain.ConcurrencyLease, error) {
		return s.AcquireLease(ctx, domain.AcquireLeaseCommand{
			Scope: domain.LeaseScopeAPIKey, ScopeID: scopeID, RequestID: mkReq(),
			OwnerToken: owner, Limit: 1, TTL: time.Minute, Now: at,
		})
	}

	l1, err := acquire(key1, "owner-a", now)
	if err != nil {
		t.Fatalf("acquire key1: %v", err)
	}
	// 同一个 Key 的第二个槽位被拒。
	if _, err := acquire(key1, "owner-b", now); domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		t.Fatalf("second slot on the same key: err = %v, want insufficient_capacity", err)
	}
	// 另一个 Key 独立拿槽。
	l2, err := acquire(key2, "owner-b", now)
	if err != nil {
		t.Fatalf("acquire key2 (a different key is independent): %v", err)
	}
	// 释放后槽位立即归还。
	if err := s.ReleaseLease(ctx, l1.ID, "owner-a", l1.FencingToken); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(key1, "owner-b", now); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	// 超时回收（进程崩溃的等价物）：旧租约到期后槽位归还，新持有者拿到更大的
	// fencing token，旧持有者的一切所有权断言失败 —— 不会与仍活跃的旧请求
	// 产生重叠授权。
	l3, err := acquire(key1, "owner-c", now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("acquire after the TTL expired: %v", err)
	}
	if l3.FencingToken <= l2.FencingToken {
		t.Errorf("post-reclaim fencing = %d, want > %d (单调递增)", l3.FencingToken, l2.FencingToken)
	}
	if err := s.RenewLease(ctx, l1.ID, "owner-a", l1.FencingToken, now.Add(11*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("renew a reclaimed lease: err = %v, want conflict (旧持有者失去授权)", err)
	}
	if err := s.CheckLease(ctx, l1.ID, "owner-a", l1.FencingToken, now.Add(10*time.Minute)); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("check a reclaimed lease: err = %v, want conflict (fenced)", err)
	}
}
