// credential_refresh.go — OAuth 凭据主动刷新 worker（Task 12，设计 §8）。
//
// 每轮扫描 expires_at 落在 (now, now+skew) 内的 active oauth 凭据，经
// credentials.Refresher 轮换：pg_advisory_xact_lock 跨实例互斥 +
// generation CAS 写库——多实例同跑安全（输家收敛不覆盖）；厂商明确拒绝
// （invalid_grant/401/403）时 Refresher 已把绑定账号翻转 reauth_required
// 并终止会话绑定；可重试错误按轮次重试，绝不动账号状态（连接器暂时不
// 可用 ≠ 授权失效）。

package workers

import (
	"context"
	"log"
	"time"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
)

// CredentialRefreshStore is the scan surface the worker needs; satisfied by
// *postgres.Store.
type CredentialRefreshStore interface {
	ListOAuthCredentialsExpiring(ctx context.Context, before time.Time, limit int) ([]domain.Credential, error)
}

// CredentialRefreshConfig tunes the worker; zero values take the defaults.
type CredentialRefreshConfig struct {
	// Interval between passes (default 60s).
	Interval time.Duration
	// BatchLimit caps credentials per pass (default 50).
	BatchLimit int
	// RefreshSkew is how early before expiry a credential rotates
	// (default 5min — covers several worker intervals of vendor outage
	// before the access token actually dies).
	RefreshSkew time.Duration
}

func (c *CredentialRefreshConfig) withDefaults() CredentialRefreshConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = time.Minute
	}
	if out.BatchLimit <= 0 {
		out.BatchLimit = 50
	}
	if out.RefreshSkew <= 0 {
		out.RefreshSkew = 5 * time.Minute
	}
	return out
}

// CredentialRefresh is the proactive rotation worker.
type CredentialRefresh struct {
	store     CredentialRefreshStore
	refresher *credentials.Refresher
	clock     domain.Clock
	cfg       CredentialRefreshConfig
}

func NewCredentialRefresh(store CredentialRefreshStore, refresher *credentials.Refresher, cfg CredentialRefreshConfig, clock domain.Clock) *CredentialRefresh {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &CredentialRefresh{store: store, refresher: refresher, clock: clock, cfg: cfg.withDefaults()}
}

// CredentialRefreshMetrics is one pass's structured-log snapshot.
type CredentialRefreshMetrics struct {
	Scanned        int `json:"scanned"`
	Rotated        int `json:"rotated"`
	Converged      int `json:"converged"`
	ReauthRequired int `json:"reauth_required"`
	Retryable      int `json:"retryable_failures"`
}

// RunPass executes one refresh round. Idempotent and multi-instance safe
// (advisory lock + generation CAS); safe to call from tests directly.
func (w *CredentialRefresh) RunPass(ctx context.Context) (CredentialRefreshMetrics, error) {
	var m CredentialRefreshMetrics
	before := w.clock.Now().Add(w.cfg.RefreshSkew)
	creds, err := w.store.ListOAuthCredentialsExpiring(ctx, before, w.cfg.BatchLimit)
	if err != nil {
		return m, err
	}
	m.Scanned = len(creds)
	for i := range creds {
		outcome, err := w.refresher.RefreshCredential(ctx, creds[i].ID, "scheduled refresh")
		if err != nil {
			m.Retryable++
			log.Printf("WARN credential refresh failed id=%s: %v", creds[i].ID, err)
			continue
		}
		switch {
		case outcome.Rotated:
			m.Rotated++
		case outcome.Converged:
			m.Converged++
		case outcome.ReauthRequired:
			m.ReauthRequired++
			log.Printf("WARN oauth grant rejected by vendor; accounts moved to reauth_required credential=%s", creds[i].ID)
		}
	}
	return m, nil
}

// Start runs the worker loop until ctx is done. The first pass runs
// immediately so a booting instance converges expiring credentials without
// waiting a full interval.
func (w *CredentialRefresh) Start(ctx context.Context) {
	if m, err := w.RunPass(ctx); err != nil {
		log.Printf("WARN credential refresh pass failed: %v", err)
	} else if m.Scanned > 0 {
		log.Printf("credential refresh pass: %+v", m)
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m, err := w.RunPass(ctx)
			if err != nil {
				log.Printf("WARN credential refresh pass failed: %v", err)
				continue
			}
			if m.Scanned > 0 || m.Retryable > 0 || m.ReauthRequired > 0 {
				log.Printf("credential refresh pass: %+v", m)
			}
		}
	}
}
