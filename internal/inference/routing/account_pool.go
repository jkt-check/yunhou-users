// account_pool.go — 账号池调度补充（Task 12，设计 §8: 账号容量、冷却、
// 健康度、上游额度 observed_at）。
//
// 现有 service.go 提供 round-robin + 进程内冷却 + 并发租约；本文件补齐:
//   - 上游额度感知：已知耗尽（remaining==0 且 reset 未到）的账号不参与
//     调度；未知额度照常调度（未知保持未知，不由客户余额反推）；
//   - 额度快照写（observed_at 单调守卫由持久层执行）；
//   - 账号健康视图（状态 + 冷却 + 额度的调度可读合成）。

package routing

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// QuotaStore is the persistence surface the pool additions need; satisfied
// by inference/postgres.Store.
type QuotaStore interface {
	// UpdateUpstreamAccountQuota stores one observation with a monotonic
	// observed_at guard (older observations never overwrite newer ones).
	UpdateUpstreamAccountQuota(ctx context.Context, id string, q domain.UpstreamQuota) (bool, error)
}

// quotaStore is the optional upgrade of AccountStore for quota snapshots.
// postgres.Store satisfies both; narrow fakes may not.
func (s *Service) quotaStore() (QuotaStore, bool) {
	qs, ok := s.store.(QuotaStore)
	return qs, ok
}

// quotaExhausted reports whether the account's OBSERVED upstream quota is
// known-empty: remaining hit zero and the reset (when known) is still in
// the future. Unknown quota (nil fields) is never treated as exhausted, and
// a passed reset makes a stale zero schedulable again (the health worker
// re-observes).
func quotaExhausted(a domain.UpstreamAccount, now time.Time) bool {
	if a.Quota.RemainingMicros == nil || *a.Quota.RemainingMicros > 0 {
		return false
	}
	if a.Quota.ResetAt == nil {
		return true
	}
	return a.Quota.ResetAt.After(now)
}

// orderAccounts extends the round-robin with the quota-exhaustion skip:
// capacity-known-depleted accounts leave the candidate order entirely until
// the reset passes or a newer observation arrives (设计 §8: 调度遵守每账号
// 并发与上游容量；重试不用于规避账号限额).
func (s *Service) orderAccountsWithQuota(providerID string, accounts []domain.UpstreamAccount, now time.Time) []domain.UpstreamAccount {
	live := make([]domain.UpstreamAccount, 0, len(accounts))
	for _, a := range accounts {
		if quotaExhausted(a, now) {
			continue
		}
		live = append(live, a)
	}
	return s.orderAccounts(providerID, live, now)
}

// RecordQuotaSnapshot persists one upstream quota observation. A stale
// observation (observed_at older than the stored one) is dropped by the
// store's monotonic guard — the boolean reports whether the write landed.
// A store without quota support (narrow test fake) is a no-op false.
func (s *Service) RecordQuotaSnapshot(ctx context.Context, accountID string, q domain.UpstreamQuota) (bool, error) {
	qs, ok := s.quotaStore()
	if !ok {
		return false, nil
	}
	return qs.UpdateUpstreamAccountQuota(ctx, accountID, q)
}

// AccountHealth is the scheduling-readable health view of one account.
// It is operational state only — never serialized to customers (设计 §8:
// 上游配额只用于运营与调度；客户控制台显示 Yunhou 自己的额度).
type AccountHealth struct {
	AccountID       string     `json:"account_id"`
	Status          string     `json:"status"`
	CoolingUntil    *time.Time `json:"cooling_until,omitempty"`
	QuotaLimit      *int64     `json:"quota_limit_micros,omitempty"`
	QuotaRemaining  *int64     `json:"quota_remaining_micros,omitempty"`
	QuotaObservedAt *time.Time `json:"quota_observed_at,omitempty"`
	QuotaSource     *string    `json:"quota_source,omitempty"`
	QuotaResetAt    *time.Time `json:"quota_reset_at,omitempty"`
	// Schedulable is the dispatch decision at the observation instant.
	Schedulable bool `json:"schedulable"`
}

// HealthOf computes the current health view of one account (in-process
// cooldown memory + DB state the caller loaded).
func (s *Service) HealthOf(a domain.UpstreamAccount) AccountHealth {
	now := s.clock.Now()
	h := AccountHealth{
		AccountID: a.ID, Status: string(a.Status),
		QuotaLimit:      microToInt64(a.Quota.LimitMicros),
		QuotaRemaining:  microToInt64(a.Quota.RemainingMicros),
		QuotaObservedAt: a.Quota.ObservedAt, QuotaSource: a.Quota.Source,
		QuotaResetAt: a.Quota.ResetAt,
	}
	s.mu.Lock()
	if until, ok := s.cooled[a.ID]; ok && until.After(now) {
		u := until
		h.CoolingUntil = &u
	}
	s.mu.Unlock()
	h.Schedulable = a.Status == domain.AccountActive &&
		h.CoolingUntil == nil && !quotaExhausted(a, now)
	return h
}

func microToInt64(m *domain.Microcredit) *int64 {
	if m == nil {
		return nil
	}
	v := int64(*m)
	return &v
}
