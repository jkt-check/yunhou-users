package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// billing_account_repo.go — inference_billing_accounts 的 Task 5 访问面。
// 基础 Insert/Get 在 accounts_repo.go（Task 1）；本文件提供幂等建立与
// 按 ID 读取。

// EnsureBillingAccount idempotently establishes the user's personal
// billing account: INSERT ... ON CONFLICT (user_id) DO NOTHING, then read
// back the winner on conflict. Ownership always comes from the
// server-verified user identity (Task 5; 设计 §4.2).
func (s *Store) EnsureBillingAccount(ctx context.Context, userID string) (*domain.BillingAccount, error) {
	var row struct {
		ID          string    `db:"id"`
		UserID      string    `db:"user_id"`
		SubjectType string    `db:"subject_type"`
		Status      string    `db:"status"`
		CreatedAt   time.Time `db:"created_at"`
		UpdatedAt   time.Time `db:"updated_at"`
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_billing_accounts (user_id)
		 VALUES ($1)
		 ON CONFLICT (user_id) DO NOTHING
		 RETURNING id, user_id, subject_type, status, created_at, updated_at`,
		userID).
		Scan(&row.ID, &row.UserID, &row.SubjectType, &row.Status, &row.CreatedAt, &row.UpdatedAt)
	if err == nil {
		return &domain.BillingAccount{
			ID: row.ID, UserID: row.UserID, SubjectType: row.SubjectType,
			Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		// Lost the insert race (or account predates the call) — read the
		// existing row.
		return s.GetBillingAccountByUser(ctx, userID)
	}
	return nil, mapError("ensure billing account", err)
}

// GetBillingAccountByID loads an account by primary key — the resolve
// path checks the owner account's status on every call.
func (s *Store) GetBillingAccountByID(ctx context.Context, id string) (*domain.BillingAccount, error) {
	var row struct {
		ID          string    `db:"id"`
		UserID      string    `db:"user_id"`
		SubjectType string    `db:"subject_type"`
		Status      string    `db:"status"`
		CreatedAt   time.Time `db:"created_at"`
		UpdatedAt   time.Time `db:"updated_at"`
	}
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_billing_accounts WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get billing account by id", err)
	}
	return &domain.BillingAccount{
		ID: row.ID, UserID: row.UserID, SubjectType: row.SubjectType,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}
