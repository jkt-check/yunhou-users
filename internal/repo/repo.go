package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

type UserRepo interface {
	Create(ctx context.Context, u *model.User) error
	FindByID(ctx context.Context, id string) (*model.User, error)
	Update(ctx context.Context, u *model.User) error
}

type SocialIdentityRepo interface {
	Create(ctx context.Context, si *model.SocialIdentity) error
	FindByProviderUID(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error)
	FindByEmail(ctx context.Context, email string) ([]model.SocialIdentity, error)
	ListByUserID(ctx context.Context, userID string) ([]model.SocialIdentity, error)
	Delete(ctx context.Context, id string) error
	CountByUserID(ctx context.Context, userID string) (int, error)
	DeleteIfNotLast(ctx context.Context, id, userID string) (bool, error)
}

type PlanRepo interface {
	FindAll(ctx context.Context) ([]model.Plan, error)
	FindByID(ctx context.Context, id string) (*model.Plan, error)
	FindDefault(ctx context.Context) (*model.Plan, error)
	Create(ctx context.Context, p *model.Plan) error
	Update(ctx context.Context, p *model.Plan) error
	Delete(ctx context.Context, id string) error
}

type AppRepo interface {
	Create(ctx context.Context, a *model.App) error
	FindByID(ctx context.Context, id string) (*model.App, error)
	Update(ctx context.Context, a *model.App) error
	List(ctx context.Context) ([]model.App, error)
}

type SubscriptionRepo interface {
	Create(ctx context.Context, s *model.Subscription) error
	FindActiveByUserID(ctx context.Context, userID string) (*model.Subscription, error)
	FindByID(ctx context.Context, id string) (*model.Subscription, error)
	ListByUserID(ctx context.Context, userID string) ([]model.Subscription, error)
	UpdateStatus(ctx context.Context, id, status string) error
	Renew(ctx context.Context, id string, expiresAt *time.Time) error
}

type SessionRepo interface {
	Create(ctx context.Context, s *model.Session) error
	FindByRefreshToken(ctx context.Context, token, sessionType string) (*model.Session, error)
	Revoke(ctx context.Context, id string) error
	RevokeIfNotRevoked(ctx context.Context, id string) (bool, error)
	RotateRefresh(ctx context.Context, oldID string, newSession *model.Session) error
	RevokeFamilyByUserApp(ctx context.Context, userID, appID string) error
	ExchangeAuthCode(ctx context.Context, oldID string, newSession *model.Session) (bool, error)
}

// Concrete implementations

type userRepo struct{ db *sqlx.DB }
type socialIdentityRepo struct{ db *sqlx.DB }
type planRepo struct{ db *sqlx.DB }
type appRepo struct{ db *sqlx.DB }
type subscriptionRepo struct{ db *sqlx.DB }
type sessionRepo struct{ db *sqlx.DB }

var (
	_ UserRepo           = (*userRepo)(nil)
	_ SocialIdentityRepo = (*socialIdentityRepo)(nil)
	_ PlanRepo           = (*planRepo)(nil)
	_ AppRepo            = (*appRepo)(nil)
	_ SubscriptionRepo   = (*subscriptionRepo)(nil)
	_ SessionRepo        = (*sessionRepo)(nil)
)

func NewUserRepo(db *sqlx.DB) *userRepo                { return &userRepo{db: db} }
func NewSocialIdentityRepo(db *sqlx.DB) *socialIdentityRepo { return &socialIdentityRepo{db: db} }
func NewPlanRepo(db *sqlx.DB) *planRepo                { return &planRepo{db: db} }
func NewAppRepo(db *sqlx.DB) *appRepo                  { return &appRepo{db: db} }
func NewSubscriptionRepo(db *sqlx.DB) *subscriptionRepo { return &subscriptionRepo{db: db} }
func NewSessionRepo(db *sqlx.DB) *sessionRepo           { return &sessionRepo{db: db} }

// UserRepo implementation

func (r *userRepo) Create(ctx context.Context, u *model.User) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO users (id, nickname, avatar_url, status)
		VALUES (:id, :nickname, :avatar_url, :status)
	`, u)
	return err
}

func (r *userRepo) FindByID(ctx context.Context, id string) (*model.User, error) {
	var u model.User
	err := r.db.GetContext(ctx, &u, `SELECT * FROM users WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *userRepo) Update(ctx context.Context, u *model.User) error {
	_, err := r.db.NamedExecContext(ctx, `
		UPDATE users SET nickname = :nickname, avatar_url = :avatar_url, status = :status, updated_at = now()
		WHERE id = :id
	`, u)
	return err
}

// SocialIdentityRepo implementation

func (r *socialIdentityRepo) Create(ctx context.Context, si *model.SocialIdentity) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO social_identities (id, user_id, provider, provider_uid, email)
		VALUES (:id, :user_id, :provider, :provider_uid, :email)
	`, si)
	return err
}

func (r *socialIdentityRepo) FindByProviderUID(ctx context.Context, provider, providerUID string) (*model.SocialIdentity, error) {
	var si model.SocialIdentity
	err := r.db.GetContext(ctx, &si, `
		SELECT * FROM social_identities WHERE provider = $1 AND provider_uid = $2
	`, provider, providerUID)
	if err != nil {
		return nil, err
	}
	return &si, nil
}

func (r *socialIdentityRepo) FindByEmail(ctx context.Context, email string) ([]model.SocialIdentity, error) {
	var list []model.SocialIdentity
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM social_identities WHERE email = $1
	`, email)
	return list, err
}

func (r *socialIdentityRepo) ListByUserID(ctx context.Context, userID string) ([]model.SocialIdentity, error) {
	var list []model.SocialIdentity
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM social_identities WHERE user_id = $1 ORDER BY created_at, id
	`, userID)
	return list, err
}

func (r *socialIdentityRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM social_identities WHERE id = $1`, id)
	return err
}

func (r *socialIdentityRepo) CountByUserID(ctx context.Context, userID string) (int, error) {
	var count int
	err := r.db.GetContext(ctx, &count, `
		SELECT COUNT(*) FROM social_identities WHERE user_id = $1
	`, userID)
	return count, err
}

func (r *socialIdentityRepo) DeleteIfNotLast(ctx context.Context, id, userID string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM social_identities WHERE id = $1 AND user_id = $2
		AND (SELECT COUNT(*) FROM social_identities WHERE user_id = $2) > 1
	`, id, userID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PlanRepo implementation

func (r *planRepo) FindAll(ctx context.Context) ([]model.Plan, error) {
	var list []model.Plan
	err := r.db.SelectContext(ctx, &list, `SELECT * FROM plans ORDER BY created_at`)
	return list, err
}

func (r *planRepo) FindByID(ctx context.Context, id string) (*model.Plan, error) {
	var p model.Plan
	err := r.db.GetContext(ctx, &p, `SELECT * FROM plans WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *planRepo) FindDefault(ctx context.Context) (*model.Plan, error) {
	var p model.Plan
	err := r.db.GetContext(ctx, &p, `SELECT * FROM plans WHERE is_default = true LIMIT 1`)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *planRepo) Create(ctx context.Context, p *model.Plan) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, is_active, is_default)
		VALUES (:id, :name, :price, :interval_days, :apps, :is_active, :is_default)
	`, p)
	return err
}

func (r *planRepo) Update(ctx context.Context, p *model.Plan) error {
	_, err := r.db.NamedExecContext(ctx, `
		UPDATE plans SET name = :name, price = :price, interval_days = :interval_days,
		apps = :apps, is_active = :is_active, is_default = :is_default
		WHERE id = :id
	`, p)
	return err
}

func (r *planRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM plans WHERE id = $1`, id)
	return err
}

// AppRepo implementation

func (r *appRepo) Create(ctx context.Context, a *model.App) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO apps (app_id, name, description, config, is_active)
		VALUES (:app_id, :name, :description, :config, :is_active)
	`, a)
	return err
}

func (r *appRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	var a model.App
	err := r.db.GetContext(ctx, &a, `SELECT * FROM apps WHERE app_id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *appRepo) Update(ctx context.Context, a *model.App) error {
	_, err := r.db.NamedExecContext(ctx, `
		UPDATE apps SET name = :name, description = :description, config = :config,
		is_active = :is_active WHERE app_id = :app_id
	`, a)
	return err
}

func (r *appRepo) List(ctx context.Context) ([]model.App, error) {
	var list []model.App
	err := r.db.SelectContext(ctx, &list, `SELECT * FROM apps ORDER BY created_at`)
	return list, err
}

// SubscriptionRepo implementation

func (r *subscriptionRepo) Create(ctx context.Context, s *model.Subscription) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO subscriptions (id, user_id, plan_id, status, started_at, expires_at)
		VALUES (:id, :user_id, :plan_id, :status, :started_at, :expires_at)
	`, s)
	return err
}

func (r *subscriptionRepo) FindActiveByUserID(ctx context.Context, userID string) (*model.Subscription, error) {
	var s model.Subscription
	err := r.db.GetContext(ctx, &s, `
		SELECT * FROM subscriptions WHERE user_id = $1 AND status = 'active'
	`, userID)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *subscriptionRepo) FindByID(ctx context.Context, id string) (*model.Subscription, error) {
	var s model.Subscription
	err := r.db.GetContext(ctx, &s, `SELECT * FROM subscriptions WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *subscriptionRepo) ListByUserID(ctx context.Context, userID string) ([]model.Subscription, error) {
	var list []model.Subscription
	err := r.db.SelectContext(ctx, &list, `
		SELECT * FROM subscriptions WHERE user_id = $1 ORDER BY created_at
	`, userID)
	return list, err
}

func (r *subscriptionRepo) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = $1, updated_at = now() WHERE id = $2
	`, status, id)
	return err
}

func (r *subscriptionRepo) Renew(ctx context.Context, id string, expiresAt *time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'active', expires_at = $1, updated_at = now() WHERE id = $2
	`, expiresAt, id)
	return err
}

// SessionRepo implementation

func (r *sessionRepo) Create(ctx context.Context, s *model.Session) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO sessions (id, user_id, app_id, session_type, refresh_token, scope, revoked, expires_at)
		VALUES (:id, :user_id, :app_id, :session_type, :refresh_token, :scope, :revoked, :expires_at)
	`, s)
	return err
}

// RevokeFamilyByUserApp marks every active session for (user_id, app_id) as
// revoked. Used as a security response when refresh-token reuse is detected:
// revoking the whole family limits the blast radius of a stolen token.
func (r *sessionRepo) RevokeFamilyByUserApp(ctx context.Context, userID, appID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked = true
		WHERE user_id = $1 AND app_id = $2 AND revoked = false
	`, userID, appID)
	return err
}

func (r *sessionRepo) FindByRefreshToken(ctx context.Context, token, sessionType string) (*model.Session, error) {
	var s model.Session
	err := r.db.GetContext(ctx, &s, `
		SELECT * FROM sessions WHERE refresh_token = $1 AND session_type = $2 AND revoked = false AND expires_at > $3
	`, token, sessionType, time.Now())
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *sessionRepo) Revoke(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked = true WHERE id = $1
	`, id)
	return err
}

func (r *sessionRepo) RevokeIfNotRevoked(ctx context.Context, id string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE sessions SET revoked = true WHERE id = $1 AND revoked = false
	`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *sessionRepo) RotateRefresh(ctx context.Context, oldID string, newSession *model.Session) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked = true WHERE id = $1 AND revoked = false
	`, oldID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if n == 0 {
		// Return the sentinel directly so AuthService can match with
		// errors.Is and trigger the family-revoke response. A plain
		// fmt.Errorf here would silently break refresh-token reuse
		// detection (errors.Is only matches via %w).
		return model.ErrSessionAlreadyRevoked
	}

	_, err = tx.NamedExecContext(ctx, `
		INSERT INTO sessions (id, user_id, app_id, session_type, refresh_token, scope, revoked, expires_at)
		VALUES (:id, :user_id, :app_id, :session_type, :refresh_token, :scope, :revoked, :expires_at)
	`, newSession)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (r *sessionRepo) ExchangeAuthCode(ctx context.Context, oldID string, newSession *model.Session) (bool, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked = true WHERE id = $1 AND revoked = false
	`, oldID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check rows affected: %w", err)
	}
	if n == 0 {
		// ExchangeAuthCode is a best-effort exchange — the caller treats
		// a false return the same as "no row matched" and the failure is
		// not security-critical like RotateRefresh, so plain false is fine.
		return false, nil
	}

	_, err = tx.NamedExecContext(ctx, `
		INSERT INTO sessions (id, user_id, app_id, session_type, refresh_token, scope, revoked, expires_at)
		VALUES (:id, :user_id, :app_id, :session_type, :refresh_token, :scope, :revoked, :expires_at)
	`, newSession)
	if err != nil {
		return false, err
	}

	err = tx.Commit()
	if err != nil {
		return false, fmt.Errorf("commit exchange: %w", err)
	}
	return true, nil
}
