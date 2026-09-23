package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// memStore is a fake of Store for unit tests.
type memStore struct {
	mu     sync.Mutex
	creds  map[string]*domain.Credential
	rotate int
}

func newMemStore() *memStore { return &memStore{creds: map[string]*domain.Credential{}} }

func (m *memStore) InsertCredential(_ context.Context, c *domain.Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.creds[c.ID]; dup {
		return domain.NewError(domain.CodeConflict, "duplicate")
	}
	cp := *c
	m.creds[c.ID] = &cp
	return nil
}

func (m *memStore) GetCredential(_ context.Context, id string) (*domain.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "not found")
	}
	cp := *c
	return &cp, nil
}

func (m *memStore) ListCredentials(_ context.Context, providerID string, limit int) ([]domain.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Credential{}
	for _, c := range m.creds {
		if providerID == "" || c.ProviderID == providerID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (m *memStore) RotateCredentialSecret(_ context.Context, id string, ciphertext []byte, keyVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "not found")
	}
	c.Ciphertext = ciphertext
	c.KeyVersion = keyVersion
	c.Generation++
	m.rotate++
	return nil
}

func (m *memStore) SetCredentialStatus(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[id]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "not found")
	}
	c.Status = status
	return nil
}

func (m *memStore) DisableUpstreamAccountsByCredential(_ context.Context, credentialID string) (int64, error) {
	return 1, nil
}

// memRecorder collects audit events.
type memRecorder struct {
	mu     sync.Mutex
	events []management.AuditEvent
	fail   bool
}

func (r *memRecorder) Record(_ context.Context, ev management.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("audit store down")
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *memRecorder) find(action string) *management.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.events {
		if r.events[i].Action == action {
			return &r.events[i]
		}
	}
	return nil
}

func testOp() Operator {
	return Operator{UserID: "user-1", AppID: "yunhou-website", Roles: []string{"admin"}}
}

func TestServiceLifecycleAndAudit(t *testing.T) {
	v, err := NewVault(testKeys(t, 1, 2), 2)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	rec := &memRecorder{}
	svc := NewService(v, store, rec)

	view, err := svc.Create(context.Background(), testOp(), "prov-1", "prod key", "api_key", "sk-super-secret", "initial onboarding", nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.KeyVersion != 2 || view.Generation != 1 || view.Status != "active" {
		t.Fatalf("unexpected view: %+v", view)
	}
	// Stored row: ciphertext present but the audit event must not carry it.
	stored, _ := store.GetCredential(context.Background(), view.ID)
	if len(stored.Ciphertext) == 0 {
		t.Fatal("ciphertext must be stored")
	}
	ev := rec.find("credential.create")
	if ev == nil {
		t.Fatal("credential.create audit missing")
	}
	if ev.ActorUser != "user-1" || ev.ActorApp != "yunhou-website" {
		t.Fatalf("dual attribution: %+v", ev)
	}
	if ev.ObjectID != view.ID || ev.Reason == "" {
		t.Fatalf("object/reason: %+v", ev)
	}

	// Rotate bumps generation and re-encrypts under the current version.
	rotated, err := svc.Rotate(context.Background(), testOp(), view.ID, "sk-rotated", "scheduled rotation")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Generation != 2 || rotated.KeyVersion != 2 {
		t.Fatalf("rotate: %+v", rotated)
	}
	if rec.find("credential.rotate") == nil {
		t.Fatal("credential.rotate audit missing")
	}

	// Test (decrypt check).
	if _, err := svc.Test(context.Background(), testOp(), view.ID, "post-rotate check"); err != nil {
		t.Fatal(err)
	}

	// Disable: resolve must refuse afterwards.
	if _, err := svc.SetStatus(context.Background(), testOp(), view.ID, "revoked", "compromised"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSecret(context.Background(), view.ID, nil); err == nil {
		t.Fatal("resolve of disabled credential must fail")
	}
	if rec.find("credential.disable") == nil {
		t.Fatal("credential.disable audit missing")
	}
}

func TestServiceResponsesNeverLeakSecrets(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	rec := &memRecorder{}
	svc := NewService(v, newMemStore(), rec)
	secret := "sk-must-never-appear"
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", secret, "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("view leaks plaintext secret")
	}
	for _, ev := range rec.events {
		d, _ := json.Marshal(ev.Detail)
		if strings.Contains(string(d), secret) {
			t.Fatalf("audit detail leaks secret: %s", d)
		}
	}
}

func TestServiceValidation(t *testing.T) {
	svc := NewService(nil, newMemStore(), &memRecorder{})
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "s", "r", nil); err == nil {
		t.Fatal("nil vault must fail closed")
	}
	v, _ := NewVault(testKeys(t, 1), 1)
	svc = NewService(v, newMemStore(), &memRecorder{})
	if _, err := svc.Create(context.Background(), testOp(), "", "l", "api_key", "s", "r", nil); err == nil {
		t.Fatal("empty provider must fail")
	}
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "badtype", "s", "r", nil); err == nil {
		t.Fatal("bad auth_type must fail")
	}
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "", "r", nil); err == nil {
		t.Fatal("empty secret must fail")
	}
}

func TestServiceResolvePinnedGeneration(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	store := newMemStore()
	svc := NewService(v, store, &memRecorder{})
	view, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "gen-1-secret", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Admission pins generation 1.
	pin := int64(1)
	plain, _, err := svc.ResolveSecret(context.Background(), view.ID, &pin)
	if err != nil || string(plain) != "gen-1-secret" {
		t.Fatalf("pinned resolve: %v", err)
	}
	// Rotate → new generation; a NEW resolve pinned at the old generation is
	// refused (no silent cross-generation secret use)...
	svc.Rotate(context.Background(), testOp(), view.ID, "gen-2-secret", "r")
	if _, _, err := svc.ResolveSecret(context.Background(), view.ID, &pin); err == nil {
		t.Fatal("stale pinned generation must fail")
	}
	// ...while an unpinned resolve (new dispatch) gets the current secret.
	plain, cred, err := svc.ResolveSecret(context.Background(), view.ID, nil)
	if err != nil || string(plain) != "gen-2-secret" || cred.Generation != 2 {
		t.Fatalf("current resolve: %v", err)
	}
}

func TestServiceAuditFailureFailsClosed(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	rec := &memRecorder{fail: true}
	svc := NewService(v, newMemStore(), rec)
	if _, err := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "s", "r", nil); err == nil {
		t.Fatal("audit failure must surface as an error")
	}
}

func TestServiceRotateDisabledRejected(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	view, _ := svc.Create(context.Background(), testOp(), "p", "l", "api_key", "s", "r", nil)
	svc.SetStatus(context.Background(), testOp(), view.ID, "revoked", "r")
	if _, err := svc.Rotate(context.Background(), testOp(), view.ID, "new", "r"); err == nil {
		t.Fatal("rotate of disabled credential must fail")
	}
	if _, err := svc.Test(context.Background(), testOp(), view.ID, "r"); err == nil {
		t.Fatal("test of disabled credential must fail")
	}
}

