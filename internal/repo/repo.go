package repo

import (
	"context"
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
}

type AppRepo interface {
	Create(ctx context.Context, a *model.App) error
	FindByID(ctx context.Context, id string) (*model.App, error)
	Update(ctx context.Context, a *model.App) error
}

type SubscriptionRepo interface {
	Create(ctx context.Context, s *model.Subscription) error
	FindByUserApp(ctx context.Context, userID, appID string) (*model.Subscription, error)
	FindByID(ctx context.Context, id string) (*model.Subscription, error)
	ListByUserID(ctx context.Context, userID string) ([]model.Subscription, error)
	UpdateStatus(ctx context.Context, id, status string) error
	Renew(ctx context.Context, id string, expiresAt interface{}) error
}

type SessionRepo interface {
	Create(ctx context.Context, s *model.Session) error
	FindByRefreshToken(ctx context.Context, token string) (*model.Session, error)
	Revoke(ctx context.Context, id string) error
}

// Concrete implementations

type userRepo struct{ db *sqlx.DB }
type socialIdentityRepo struct{ db *sqlx.DB }
type appRepo struct{ db *sqlx.DB }
type subscriptionRepo struct{ db *sqlx.DB }
type sessionRepo struct{ db *sqlx.DB }

var (
	_ UserRepo            = (*userRepo)(nil)
	_ SocialIdentityRepo  = (*socialIdentityRepo)(nil)
	_ AppRepo             = (*appRepo)(nil)
	_ SubscriptionRepo    = (*subscriptionRepo)(nil)
	_ SessionRepo         = (*sessionRepo)(nil)
)

func NewUserRepo(db *sqlx.DB) *userRepo             { return &userRepo{db: db} }
func NewSocialIdentityRepo(db *sqlx.DB) *socialIdentityRepo { return &socialIdentityRepo{db: db} }
func NewAppRepo(db *sqlx.DB) *appRepo               { return &appRepo{db: db} }
func NewSubscriptionRepo(db *sqlx.DB) *subscriptionRepo { return &subscriptionRepo{db: db} }
func NewSessionRepo(db *sqlx.DB) *sessionRepo        { return &sessionRepo{db: db} }

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
		SELECT * FROM social_identities WHERE user_id = $1 ORDER BY created_at
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

// AppRepo implementation

func (r *appRepo) Create(ctx context.Context, a *model.App) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO apps (id, secret, name, redirect_uris, providers, default_plan)
		VALUES (:id, :secret, :name, :redirect_uris, :providers, :default_plan)
	`, a)
	return err
}

func (r *appRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	var a model.App
	err := r.db.GetContext(ctx, &a, `SELECT * FROM apps WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *appRepo) Update(ctx context.Context, a *model.App) error {
	_, err := r.db.NamedExecContext(ctx, `
		UPDATE apps SET name = :name, secret = :secret, redirect_uris = :redirect_uris,
		providers = :providers, default_plan = :default_plan, updated_at = now()
		WHERE id = :id
	`, a)
	return err
}

// SubscriptionRepo implementation

func (r *subscriptionRepo) Create(ctx context.Context, s *model.Subscription) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO subscriptions (id, user_id, app_id, plan, status, expires_at)
		VALUES (:id, :user_id, :app_id, :plan, :status, :expires_at)
	`, s)
	return err
}

func (r *subscriptionRepo) FindByUserApp(ctx context.Context, userID, appID string) (*model.Subscription, error) {
	var s model.Subscription
	err := r.db.GetContext(ctx, &s, `
		SELECT * FROM subscriptions WHERE user_id = $1 AND app_id = $2
	`, userID, appID)
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

func (r *subscriptionRepo) Renew(ctx context.Context, id string, expiresAt interface{}) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'active', expires_at = $1, updated_at = now() WHERE id = $2
	`, expiresAt, id)
	return err
}

// SessionRepo implementation

func (r *sessionRepo) Create(ctx context.Context, s *model.Session) error {
	_, err := r.db.NamedExecContext(ctx, `
		INSERT INTO sessions (id, user_id, app_id, refresh_token, scope, revoked, expires_at)
		VALUES (:id, :user_id, :app_id, :refresh_token, :scope, :revoked, :expires_at)
	`, s)
	return err
}

func (r *sessionRepo) FindByRefreshToken(ctx context.Context, token string) (*model.Session, error) {
	var s model.Session
	err := r.db.GetContext(ctx, &s, `
		SELECT * FROM sessions WHERE refresh_token = $1 AND revoked = false AND expires_at > $2
	`, token, time.Now())
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
