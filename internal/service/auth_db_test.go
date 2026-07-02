package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// authSetupDB connects to the test DB, wipes tables, seeds plans + app + user.
// Returns *sqlx.DB and an alice user ID.
func authSetupDB(t *testing.T) (*sqlx.DB, string) {
	t.Helper()
	db, err := sqlx.Connect("postgres", dbURL2())
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	tables := []string{
		"refunds", "payments", "webhook_events", "orders",
		"sessions", "subscriptions", "social_identities",
		"plans", "apps", "users", "audit_log",
	}
	_, _ = db.ExecContext(context.Background(), `UPDATE plans SET is_default = false`)
	for _, tbl := range tables {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM "+tbl)
	}
	for _, p := range []struct {
		id, name string
		price    float64
		days     int
		apps     []string
		isDef    bool
	}{
		{"free", "Free", 0, 0, []string{"yundian"}, true},
		{"monthly", "Monthly", 29.9, 30, []string{"yundian", "yundash"}, false},
	} {
		_, _ = db.ExecContext(context.Background(), `
			INSERT INTO plans (id, name, price, interval_days, apps, is_default)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, p.id, p.name, p.price, p.days, p.apps, p.isDef)
	}
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active) VALUES ('yundian', 'Yundian', true)
	`)
	aliceID := uuid.New().String()
	_, _ = db.ExecContext(context.Background(),
		`INSERT INTO users (id, status) VALUES ($1, 'active')`, aliceID)
	return db, aliceID
}

// newAuthServiceWithDB builds an *AuthService against the live test DB. We
// skip NewTokenService (which loads from disk) and construct the TokenService
// directly with in-memory RSA keys, mirroring how integration tests wire it.
func newAuthServiceWithDB(t *testing.T, db *sqlx.DB) (*AuthService, *TokenService) {
	t.Helper()
	sr := repo.NewSessionRepo(db)
	subR := repo.NewSubscriptionRepo(db)
	pr := repo.NewPlanRepo(db)
	ar := repo.NewAppRepo(db)
	ur := repo.NewUserRepo(db)
	ir := repo.NewSocialIdentityRepo(db)

	priv, pub := generateTestRSAKeyPair()
	tok := &TokenService{
		PrivateKey: priv, PublicKey: pub,
		AccessTTL: 15 * time.Minute, RefreshTTL: 168 * time.Hour,
		SessionRepo: sr, SubRepo: subR,
	}
	return NewAuthService(ur, ir, pr, subR, sr, ar, tok), tok
}

func TestAuthService_Login_DB(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	resp, err := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "logintok", AppID: "yundian",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if resp.AccessToken == "" {
		t.Errorf("AccessToken empty")
	}
	if resp.RefreshToken == "" {
		t.Errorf("RefreshToken empty")
	}
	if resp.User.ID == "" {
		t.Errorf("User.ID empty")
	}
	if !resp.Subscription.HasAccess {
		t.Errorf("HasAccess = false on free plan with yundian app")
	}
}

func TestAuthService_Login_AppNotFound(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)
	_, err := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok", AppID: "nope",
	})
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want ErrAppNotFound", err)
	}
}

func TestAuthService_Login_AppInactive(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	_, _ = db.ExecContext(context.Background(),
		`UPDATE apps SET is_active = false WHERE app_id = 'yundian'`)
	_, err := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok", AppID: "yundian",
	})
	if !errors.Is(err, ErrAppInactive) {
		t.Errorf("err = %v, want ErrAppInactive", err)
	}
}

func TestAuthService_Login_UserSuspended(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	// First login creates a user; suspend only that user.
	login, err := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-susp-1", AppID: "yundian",
	})
	if err != nil {
		t.Fatalf("setup login: %v", err)
	}
	_, _ = db.ExecContext(context.Background(),
		`UPDATE users SET status = 'suspended' WHERE id = $1`, login.User.ID)

	// Same token → same providerUID → existing identity → existing user → suspended.
	_, err = auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-susp-1", AppID: "yundian",
	})
	if !errors.Is(err, ErrUserSuspended) {
		t.Errorf("err = %v, want ErrUserSuspended", err)
	}
}

func TestAuthService_RefreshToken_Rotate(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	login, err := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-rot", AppID: "yundian",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	refreshed, err := auth.RefreshToken(context.Background(), login.RefreshToken, "yundian")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if refreshed.RefreshToken == login.RefreshToken {
		t.Errorf("refresh token did not rotate")
	}

	_, err = auth.RefreshToken(context.Background(), login.RefreshToken, "yundian")
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("after rotate, old refresh: err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestAuthService_RefreshToken_NotFound(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)
	_, err := auth.RefreshToken(context.Background(), "no-such-token", "yundian")
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestAuthService_RefreshToken_SuspendedUser(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	login, _ := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-susp", AppID: "yundian",
	})
	_, _ = db.ExecContext(context.Background(),
		`UPDATE users SET status = 'suspended'`)
	_, err := auth.RefreshToken(context.Background(), login.RefreshToken, "yundian")
	if !errors.Is(err, ErrUserSuspended) {
		t.Errorf("err = %v, want ErrUserSuspended", err)
	}
}

func TestAuthService_RefreshToken_AppInactive(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	login, _ := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-ai", AppID: "yundian",
	})
	_, _ = db.ExecContext(context.Background(),
		`UPDATE apps SET is_active = false WHERE app_id = 'yundian'`)
	_, err := auth.RefreshToken(context.Background(), login.RefreshToken, "yundian")
	if !errors.Is(err, ErrAppInactive) {
		t.Errorf("err = %v, want ErrAppInactive", err)
	}
}

func TestAuthService_RefreshToken_AppNotFound(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	login, _ := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-anf", AppID: "yundian",
	})
	_, err := auth.RefreshToken(context.Background(), login.RefreshToken, "nope")
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want ErrAppNotFound", err)
	}
}

func TestAuthService_Logout_DB(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)

	login, _ := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "tok-lo", AppID: "yundian",
	})
	if err := auth.Logout(context.Background(), login.RefreshToken); err != nil {
		t.Errorf("Logout: %v", err)
	}
	// Idempotent — second logout returns no error.
	if err := auth.Logout(context.Background(), login.RefreshToken); err != nil {
		t.Errorf("Logout (idempotent): %v", err)
	}
}

func TestAuthService_Login_DBError(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)
	_ = db.Close()
	_, err := auth.Login(context.Background(), LoginRequest{
		Provider: "github", ProviderToken: "x", AppID: "yundian",
	})
	if err == nil {
		t.Errorf("expected error after db close")
	}
}

func TestAuthService_RefreshToken_DBError(t *testing.T) {
	withStubProvider(t)
	db, _ := authSetupDB(t)
	auth, _ := newAuthServiceWithDB(t, db)
	_ = db.Close()
	_, err := auth.RefreshToken(context.Background(), "any", "yundian")
	if err == nil {
		t.Errorf("expected error after db close")
	}
}

// Ensure imports stay referenced.
var _ = model.User{}