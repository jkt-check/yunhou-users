// response_chain_repo.go — OpenAI Responses 会话链持久面（Task 13，
// migration 033）。
//
// 语义:
//   - 写入只在成功响应后发生（handler 侧）；store:false 不落链。
//   - 读取按 (id, billing_account_id) 限定：跨客户引用与不存在/过期无差别
//     （CodeNotFound），不泄漏存在性（与 ownedKey NotFound 同模式）。
//   - 过期行不算命中；真正的删除由 DeleteExpiredResponseChains 清扫
//     （挂 upstream_health 轮次，与绑定 TTL 清扫同处）。

package postgres

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// InsertResponseChain persists one completed turn's chain row. The id is
// client-visible (resp_<hex>); a collision is a CodeConflict like any other
// unique-key hit.
func (s *Store) InsertResponseChain(ctx context.Context, c *domain.ResponseChain) error {
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_response_chains
		 (id, chain_id, billing_account_id, request_id, model_id, upstream_account_id,
		  transcript, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING created_at`,
		c.ID, c.ChainID, c.BillingAccountID, c.RequestID, c.ModelID, c.UpstreamAccountID,
		c.Transcript, c.ExpiresAt).
		Scan(&c.CreatedAt)
	return mapError("insert response chain", err)
}

// GetResponseChainForAccount loads one chain row scoped to the owning
// billing account. Missing, expired, and cross-customer ids ALL answer
// CodeNotFound — an attacker cannot distinguish "exists but not yours".
func (s *Store) GetResponseChainForAccount(ctx context.Context, id, billingAccountID string, now time.Time) (*domain.ResponseChain, error) {
	var c domain.ResponseChain
	err := s.db.QueryRowxContext(ctx,
		`SELECT id, chain_id, billing_account_id, request_id, model_id, upstream_account_id,
		        transcript, created_at, expires_at
		   FROM inference_response_chains
		  WHERE id = $1 AND billing_account_id = $2 AND expires_at > $3`,
		id, billingAccountID, now).
		Scan(&c.ID, &c.ChainID, &c.BillingAccountID, &c.RequestID, &c.ModelID,
			&c.UpstreamAccountID, &c.Transcript, &c.CreatedAt, &c.ExpiresAt)
	if err != nil {
		return nil, mapError("get response chain", err)
	}
	return &c, nil
}

// DeleteExpiredResponseChains sweeps TTL-expired rows (bounded batch);
// wired into the upstream-health pass alongside the binding sweep.
func (s *Store) DeleteExpiredResponseChains(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inference_response_chains
		  WHERE id IN (SELECT id FROM inference_response_chains
		                WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2)`,
		now, limit)
	if err != nil {
		return 0, mapError("sweep expired response chains", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
