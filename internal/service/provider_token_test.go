package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// stubAppRepo is the AppRepo subset needed by ProviderTokenService.
type stubAppRepo struct {
	app *model.App
	err error
}

func (s *stubAppRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.app != nil && s.app.AppID == id {
		return s.app, nil
	}
	return nil, sql.ErrNoRows
}
// Create/Update/List unused by ProviderTokenService; if the test file's
// package-level interface requires them, add stubs that panic.

type stubPaypal struct {
	called   bool
	cid, cs  string
	returnTok *model.ProviderToken
	returnErr error
}

func (s *stubPaypal) FetchToken(ctx context.Context, clientID, clientSecret string) (*model.ProviderToken, error) {
	s.called = true
	s.cid, s.cs = clientID, clientSecret
	return s.returnTok, s.returnErr
}

func mustJSONRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("invalid test JSON: %s", s)
	}
	return json.RawMessage(s)
}

func TestProviderToken_Get_Paypal(t *testing.T) {
	app := &model.App{
		AppID:    "site",
		IsActive: true,
		Config:   mustJSONRaw(t, `{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"cs","webhook_id":"W","mode":"live"}}}`),
	}
	pp := &stubPaypal{returnTok: &model.ProviderToken{Channel: "paypal", AccessToken: "AT", ExpiresIn: 3600}}
	svc := NewProviderTokenService(&stubAppRepo{app: app}, pp)

	got, err := svc.Get(context.Background(), "site", "paypal")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "AT" || !pp.called || pp.cid != "cid" || pp.cs != "cs" {
		t.Errorf("unexpected: tok=%+v called=%v cid=%q cs=%q", got, pp.called, pp.cid, pp.cs)
	}
}

func TestProviderToken_Get_UnsupportedChannel(t *testing.T) {
	app := &model.App{AppID: "site", IsActive: true}
	svc := NewProviderTokenService(&stubAppRepo{app: app}, &stubPaypal{})
	if _, err := svc.Get(context.Background(), "site", "stripe"); !errors.Is(err, ErrUnsupportedChannel) {
		t.Errorf("err = %v, want ErrUnsupportedChannel", err)
	}
}

func TestProviderToken_Get_AppNotFound(t *testing.T) {
	svc := NewProviderTokenService(&stubAppRepo{err: sql.ErrNoRows}, &stubPaypal{})
	_, err := svc.Get(context.Background(), "missing", "paypal")
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want ErrAppNotFound", err)
	}
}

// TestProviderToken_Get_DBError verifies that a non-NotFound repo error is
// wrapped and propagated rather than collapsed to ErrAppNotFound, which
// would surface a DB outage as a misleading 404.
func TestProviderToken_Get_DBError(t *testing.T) {
	dbErr := errors.New("connection refused")
	svc := NewProviderTokenService(&stubAppRepo{err: dbErr}, &stubPaypal{})
	_, err := svc.Get(context.Background(), "site", "paypal")
	if errors.Is(err, ErrAppNotFound) {
		t.Errorf("DB error collapsed to ErrAppNotFound; expected wrap, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "find app") {
		t.Errorf("expected wrapped DB error, got %v", err)
	}
}

func TestProviderToken_Get_AppInactive(t *testing.T) {
	app := &model.App{AppID: "site", IsActive: false}
	svc := NewProviderTokenService(&stubAppRepo{app: app}, &stubPaypal{})
	_, err := svc.Get(context.Background(), "site", "paypal")
	if !errors.Is(err, ErrAppInactive) {
		t.Errorf("err = %v, want ErrAppInactive", err)
	}
}

func TestProviderToken_Get_ProviderNotConfigured_Paypal(t *testing.T) {
	app := &model.App{AppID: "site", IsActive: true, Config: mustJSONRaw(t, `{}`)}
	svc := NewProviderTokenService(&stubAppRepo{app: app}, &stubPaypal{})
	_, err := svc.Get(context.Background(), "site", "paypal")
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Errorf("err = %v, want ErrProviderNotConfigured", err)
	}
}