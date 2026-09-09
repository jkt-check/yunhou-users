// service.go — 上游凭据生命周期：创建 / 轮换 / 测试 / 禁用 + 网关侧密钥
// 解析（ResolveSecret）。所有写操作记录审计（人员 + 服务双重归因、对象、
// 原因）；响应、日志与审计 detail 永远不含可用秘密（脱敏后的状态视图）。

package credentials

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// Store is the persistence boundary the credential service needs. Satisfied
// by internal/inference/postgres.Store; defined here so unit tests can fake
// it without a database.
type Store interface {
	InsertCredential(ctx context.Context, c *domain.Credential) error
	GetCredential(ctx context.Context, id string) (*domain.Credential, error)
	ListCredentials(ctx context.Context, providerID string, limit int) ([]domain.Credential, error)
	// RotateCredentialSecret stores new ciphertext/keyVersion and bumps the
	// generation CAS counter in one statement (设计 §8).
	RotateCredentialSecret(ctx context.Context, id string, ciphertext []byte, keyVersion int) error
	SetCredentialStatus(ctx context.Context, id, status string) error
	// DisableUpstreamAccountsByCredential flips every account bound to the
	// credential. Atomicity with the status flip and the audit row is the
	// caller's job: production stores satisfy TxStore/TxRecorder and the
	// service runs all three in ONE UnitOfWork (an audit failure rolls the
	// whole change back); plain fakes run the sequential fallback below.
	DisableUpstreamAccountsByCredential(ctx context.Context, credentialID string) (int64, error)
}

// TxStore is the optional transaction-aware upgrade of Store: when the
// store implements it (postgres.Store does; in-memory test fakes usually
// don't), credential mutations run change + propagation + audit inside one
// domain.UnitOfWork opened by Begin.
type TxStore interface {
	Store
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	InsertCredentialTx(ctx context.Context, w domain.UnitOfWork, c *domain.Credential) error
	RotateCredentialSecretTx(ctx context.Context, w domain.UnitOfWork, id string, ciphertext []byte, keyVersion int) error
	SetCredentialStatusTx(ctx context.Context, w domain.UnitOfWork, id, status string) error
	DisableUpstreamAccountsByCredentialTx(ctx context.Context, w domain.UnitOfWork, credentialID string) (int64, error)
	// EndSessionBindingsForCredentialTx terminates live sticky session
	// bindings of every account bound to the credential (审查修复 I-2：吊销
	// 传播必须与 invalid_grant 路径一致地终止绑定——悬垂 active 绑定会被
	// 部分唯一索引挡住同 (session_key, model_id) 的重新绑定).
	EndSessionBindingsForCredentialTx(ctx context.Context, w domain.UnitOfWork, credentialID, reason string) (int64, error)
}

// TxRecorder is the optional transaction-aware upgrade of the audit
// recorder. RecordTx MUST write through the given UnitOfWork so the audit
// row commits (or rolls back) with the change it describes.
type TxRecorder interface {
	management.AuditRecorder
	RecordTx(ctx context.Context, w domain.UnitOfWork, ev management.AuditEvent) error
}

// runAtomic executes fn inside one UnitOfWork when both the store and the
// audit recorder are transaction-aware, committing only when fn succeeds —
// so an audit failure (or any step) rolls the whole mutation back. The
// boolean reports whether the transactional path ran; when it is false the
// caller must use its sequential fallback (plain test fakes).
func (s *Service) runAtomic(ctx context.Context, fn func(w domain.UnitOfWork) error) (bool, error) {
	ts, ok := s.store.(TxStore)
	if !ok {
		return false, nil
	}
	if _, ok := s.audit.(TxRecorder); !ok {
		return false, nil
	}
	w, err := ts.Begin(ctx)
	if err != nil {
		return true, domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Rollback(ctx)
		}
	}()
	if err := fn(w); err != nil {
		return true, err
	}
	if err := w.Commit(ctx); err != nil {
		return true, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return true, nil
}

// Operator is the verified operator attribution (设计 §9.2: 每次变更记录人员
// 及服务双重归因). Produced only by the admin authorization middleware —
// never parsed from a request body.
type Operator struct {
	UserID string   `json:"user_id"`
	AppID  string   `json:"app_id"`
	Roles  []string `json:"roles"`
}

// View is the masked credential representation safe for API responses:
// status and metadata only, never ciphertext or plaintext.
type View struct {
	ID            string     `json:"id"`
	ProviderID    string     `json:"provider_id"`
	Label         string     `json:"label"`
	AuthType      string     `json:"auth_type"`
	Status        string     `json:"status"`
	KeyVersion    int        `json:"key_version"`
	Generation    int64      `json:"generation"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	LastRotatedAt *time.Time `json:"last_rotated_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func toView(c *domain.Credential) *View {
	return &View{
		ID: c.ID, ProviderID: c.ProviderID, Label: c.Label, AuthType: c.AuthType,
		Status: c.Status, KeyVersion: c.KeyVersion, Generation: c.Generation,
		ExpiresAt: c.ExpiresAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

var validAuthTypes = map[string]bool{"api_key": true, "oauth": true, "service": true}

// Service is the operator-facing credential lifecycle. A nil Vault means the
// deployment did not inject key material: every operation fails closed.
type Service struct {
	vault *Vault
	store Store
	audit management.AuditRecorder
}

// NewService builds the credential lifecycle service.
func NewService(vault *Vault, store Store, audit management.AuditRecorder) *Service {
	return &Service{vault: vault, store: store, audit: audit}
}

// auditEvent builds the sanitized dual-attribution event shared by the
// transactional and fallback write paths.
func auditEvent(op Operator, action, objectID, reason string, detail map[string]any) management.AuditEvent {
	return management.AuditEvent{
		Action:     action,
		ObjectType: "credential",
		ObjectID:   objectID,
		Reason:     reason,
		ActorUser:  op.UserID,
		ActorApp:   op.AppID,
		Detail:     management.SanitizeDetail(detail),
	}
}

func (s *Service) record(ctx context.Context, op Operator, action, objectID, reason string, detail map[string]any) error {
	if err := s.audit.Record(ctx, auditEvent(op, action, objectID, reason, detail)); err != nil {
		// Fail closed: an unaudited secret mutation must not report success.
		return domain.WrapError(domain.CodeInternal, "audit write failed", err)
	}
	return nil
}

// Create stores a new upstream credential: the plaintext is sealed with AEAD
// before it ever touches the DB. ID is generated first so the AAD binds the
// ciphertext to its final row identity.
func (s *Service) Create(ctx context.Context, op Operator, providerID, label, authType, plaintext, reason string, expiresAt *time.Time) (*View, error) {
	if s.vault == nil {
		return nil, domain.NewError(domain.CodeInternal, "credential vault not configured")
	}
	if providerID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "provider_id is required")
	}
	if !validAuthTypes[authType] {
		return nil, domain.NewError(domain.CodeInvalidInput, "auth_type must be api_key, oauth or service")
	}
	if plaintext == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "secret is required")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	ct, version, err := s.vault.Encrypt(id, providerID, []byte(plaintext))
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "encrypt credential", err)
	}
	cred := &domain.Credential{
		ID: id, ProviderID: providerID, Label: label, AuthType: authType,
		Ciphertext: ct, KeyVersion: version, Generation: 1, ExpiresAt: expiresAt,
		Status: "active",
	}
	createDetail := map[string]any{
		"provider_id": providerID, "label": label, "auth_type": authType,
		"key_version": version,
	}
	if ran, err := s.runAtomic(ctx, func(w domain.UnitOfWork) error {
		if err := s.store.(TxStore).InsertCredentialTx(ctx, w, cred); err != nil {
			return err
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, auditEvent(op, "credential.create", id, reason, createDetail))
	}); ran {
		if err != nil {
			return nil, err
		}
		return toView(cred), nil
	}
	// Sequential fallback for plain (non-transactional) test stores.
	if err := s.store.InsertCredential(ctx, cred); err != nil {
		return nil, err
	}
	if err := s.record(ctx, op, "credential.create", id, reason, createDetail); err != nil {
		return nil, err
	}
	return toView(cred), nil
}

// Rotate replaces the secret material with a new plaintext, re-encrypting
// under the current key version and bumping the generation CAS. In-flight
// attempts that already resolved plaintext keep it (they finish normally);
// attempts pinning a stale generation see CodeConflict on next resolve.
func (s *Service) Rotate(ctx context.Context, op Operator, id, newPlaintext, reason string) (*View, error) {
	if s.vault == nil {
		return nil, domain.NewError(domain.CodeInternal, "credential vault not configured")
	}
	if newPlaintext == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "secret is required")
	}
	cred, err := s.mustActive(ctx, id)
	if err != nil {
		return nil, err
	}
	ct, version, err := s.vault.Encrypt(id, cred.ProviderID, []byte(newPlaintext))
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "encrypt credential", err)
	}
	// Optimistic in-memory view of the CAS bump the SQL performs; on commit
	// this matches the stored row.
	cred.KeyVersion = version
	cred.Generation++
	cred.Status = "active"
	rotateDetail := map[string]any{
		"provider_id": cred.ProviderID, "key_version": version, "generation": cred.Generation,
	}
	if ran, err := s.runAtomic(ctx, func(w domain.UnitOfWork) error {
		if err := s.store.(TxStore).RotateCredentialSecretTx(ctx, w, id, ct, version); err != nil {
			return err
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, auditEvent(op, "credential.rotate", id, reason, rotateDetail))
	}); ran {
		if err != nil {
			return nil, err
		}
		return toView(cred), nil
	}
	if err := s.store.RotateCredentialSecret(ctx, id, ct, version); err != nil {
		return nil, err
	}
	if err := s.record(ctx, op, "credential.rotate", id, reason, rotateDetail); err != nil {
		return nil, err
	}
	return toView(cred), nil
}

// Test verifies a stored credential still decrypts under its recorded key
// version — i.e. the vault can serve it to the gateway. The result carries
// only masked status, never secret bytes.
func (s *Service) Test(ctx context.Context, op Operator, id, reason string) (*View, error) {
	cred, err := s.store.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	if cred.Status == "revoked" {
		return nil, domain.NewError(domain.CodeConflict, "credential is disabled")
	}
	if s.vault == nil {
		return nil, domain.NewError(domain.CodeInternal, "credential vault not configured")
	}
	if _, err := s.vault.Decrypt(id, cred.ProviderID, cred.KeyVersion, cred.Ciphertext); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "credential decrypt check failed", err)
	}
	if err := s.record(ctx, op, "credential.test", id, reason, map[string]any{
		"provider_id": cred.ProviderID, "auth_type": cred.AuthType, "result": "ok",
	}); err != nil {
		return nil, err
	}
	return toView(cred), nil
}

// SetStatus flips a credential's status. Disabling ("revoked") is the
// emergency path: it propagates to every upstream account bound to the
// credential in the SAME UnitOfWork as the status flip and the audit row —
// either all three commit or none do. The propagation bound for already-
// cached route snapshots is one catalog snapshot refresh interval (see
// cmd/server); new dispatches re-check account status against the DB per
// attempt.
func (s *Service) SetStatus(ctx context.Context, op Operator, id, status, reason string) (*View, error) {
	if status != "active" && status != "revoked" && status != "rotating" {
		return nil, domain.NewError(domain.CodeInvalidInput, "status must be active, rotating or revoked")
	}
	cred, err := s.store.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	detail := map[string]any{"provider_id": cred.ProviderID, "from": cred.Status, "to": status}
	if ran, err := s.runAtomic(ctx, func(w domain.UnitOfWork) error {
		ts := s.store.(TxStore)
		if err := ts.SetCredentialStatusTx(ctx, w, id, status); err != nil {
			return err
		}
		if status == "revoked" {
			n, err := ts.DisableUpstreamAccountsByCredentialTx(ctx, w, id)
			if err != nil {
				return err
			}
			detail["upstream_accounts_disabled"] = n
			// 审查修复 I-2：吊销同事务终止这些账号的活跃会话绑定。
			nb, err := ts.EndSessionBindingsForCredentialTx(ctx, w, id, domain.BindingEndedAccountInvalid)
			if err != nil {
				return err
			}
			detail["session_bindings_ended"] = nb
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, auditEvent(op, "credential.disable", id, reason, detail))
	}); ran {
		if err != nil {
			return nil, err
		}
		cred.Status = status
		return toView(cred), nil
	}
	// Sequential fallback for plain (non-transactional) test stores.
	if err := s.store.SetCredentialStatus(ctx, id, status); err != nil {
		return nil, err
	}
	if status == "revoked" {
		n, err := s.store.DisableUpstreamAccountsByCredential(ctx, id)
		if err != nil {
			return nil, err
		}
		detail["upstream_accounts_disabled"] = n
		// 绑定终止在非事务 fake 下走可选接口（生产库必走上面的同事务路径）。
		if eb, ok := s.store.(interface {
			EndSessionBindingsForCredential(ctx context.Context, credentialID, reason string) (int64, error)
		}); ok {
			nb, err := eb.EndSessionBindingsForCredential(ctx, id, domain.BindingEndedAccountInvalid)
			if err != nil {
				return nil, err
			}
			detail["session_bindings_ended"] = nb
		}
	}
	cred.Status = status
	if err := s.record(ctx, op, "credential.disable", id, reason, detail); err != nil {
		return nil, err
	}
	return toView(cred), nil
}

// Get / List return masked views.
func (s *Service) Get(ctx context.Context, id string) (*View, error) {
	cred, err := s.store.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	return toView(cred), nil
}

func (s *Service) List(ctx context.Context, providerID string, limit int) ([]*View, error) {
	creds, err := s.store.ListCredentials(ctx, providerID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*View, 0, len(creds))
	for i := range creds {
		out = append(out, toView(&creds[i]))
	}
	return out, nil
}

// ResolveSecret decrypts a credential for the gateway dispatch path (hot
// path — no audit). pinGeneration, when non-nil, is the generation the
// attempt was admitted under: a mismatch means the credential rotated after
// admission, and the attempt must not silently use the new secret (在途请求
// 用其准入时固定的代次正常收尾；新调度拿新秘密).
func (s *Service) ResolveSecret(ctx context.Context, id string, pinGeneration *int64) ([]byte, *domain.Credential, error) {
	if s.vault == nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "credential vault not configured")
	}
	cred, err := s.store.GetCredential(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if cred.Status != "active" && cred.Status != "rotating" {
		return nil, nil, domain.NewError(domain.CodeConflict, "credential is disabled")
	}
	if pinGeneration != nil && *pinGeneration != cred.Generation {
		return nil, nil, domain.NewError(domain.CodeConflict, "credential rotated after admission")
	}
	plain, err := s.vault.Decrypt(id, cred.ProviderID, cred.KeyVersion, cred.Ciphertext)
	if err != nil {
		return nil, nil, domain.WrapError(domain.CodeInternal, "credential decrypt failed", err)
	}
	return plain, cred, nil
}

func (s *Service) mustActive(ctx context.Context, id string) (*domain.Credential, error) {
	cred, err := s.store.GetCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	if cred.Status == "revoked" {
		return nil, domain.NewError(domain.CodeConflict, "credential is disabled")
	}
	return cred, nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate credential id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	// Canonical dashed UUID form, matching what Postgres RETURNING id
	// yields — the AAD and audit object_id must agree with the stored ID.
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
