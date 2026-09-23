// oauth.go — 独立于社交登录的上游 OAuth 授权流（设计 §8: OAuth 授权与用
// 户登录 OAuth 分离，不能混用 apps.config.oauth_providers 或社交身份）。
//
// 流程:
//   1. BeginAuthorization：生成一次性 state + PKCE S256 verifier，state
//      绑定发起运营人员（user+app）、目标供应商、连接器与有效期，落库；
//      返回授权 URL（含 state 与 challenge，永不含 verifier）。
//   2. HandleCallback：state 单语句原子消费（并发/重放回调只有一个赢家），
//      校验发起身份仍在场，用暂存的 verifier 向连接器换 token，整体打包
//      （OAuthBundle JSON）AEAD 加密落凭据表，并同事务创建上游账号 + 审计。
//   3. Revoke：本地吊销为准（凭据 revoked + 账号 disabled 同事务传播，
//      复用 Task 4 SetStatus）；厂商侧吊销尽力而为，永不阻塞本地吊销。
//
// state/PKCE/密文全部停留在 credentials 边界内；HTTP 层只见脱敏视图。

package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

// StateTTL bounds the authorization round-trip (设计 §8: state 绑定有效期).
const StateTTL = 10 * time.Minute

// OAuthBundle is the plaintext shape sealed into inference_credentials.
// ciphertext for auth_type='oauth' rows. The access token authenticates
// dispatch; the refresh token (when present) drives the refresh worker.
type OAuthBundle struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ObtainedAt   time.Time `json:"obtained_at"`
}

// MarshalBundle / UnmarshalBundle are the bundle's canonical codecs.
func MarshalBundle(b *OAuthBundle) ([]byte, error) { return json.Marshal(b) }

func UnmarshalBundle(raw []byte) (*OAuthBundle, error) {
	var b OAuthBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "oauth bundle unparsable", err)
	}
	return &b, nil
}

// OAuthStore is the persistence surface of the authorization flow;
// satisfied by inference/postgres.Store.
type OAuthStore interface {
	GetProvider(ctx context.Context, id string) (*domain.Provider, error)
	InsertOAuthGrant(ctx context.Context, g *domain.OAuthGrant) error
	// ConsumeOAuthGrant atomically consumes a state (one-time; unknown /
	// consumed / expired → CodeConflict).
	ConsumeOAuthGrant(ctx context.Context, state string, now time.Time) (*domain.OAuthGrant, error)
}

// OAuthTxStore is the transactional upgrade used to commit credential +
// upstream account + audit in ONE UnitOfWork at callback time.
type OAuthTxStore interface {
	OAuthStore
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	InsertCredentialTx(ctx context.Context, w domain.UnitOfWork, c *domain.Credential) error
	InsertUpstreamAccountTx(ctx context.Context, w domain.UnitOfWork, a *domain.UpstreamAccount) error
}

// AuthorizationStart is returned to the operator: the vendor URL to visit
// plus the state to echo back at the callback (state comparison is the
// operator-visible CSRF check; the server additionally enforces one-time
// consumption and operator binding).
type AuthorizationStart struct {
	AuthorizeURL string    `json:"authorize_url"`
	State        string    `json:"state"`
	ExpiresAt    time.Time `json:"expires_at"`
	Connector    string    `json:"connector"`
	ProviderID   string    `json:"provider_id"`
}

// CallbackResult is the masked outcome of a completed authorization.
type CallbackResult struct {
	Credential *View  `json:"credential"`
	AccountID  string `json:"account_id"`
}

// OAuthService runs the authorization lifecycle. Vault nil = fail closed
// (same contract as Service). The registry comes from the deployment config
// (INFERENCE_OAUTH_CONNECTORS_JSON); an unconfigured connector key is a
// client-visible invalid_input, not a panic.
type OAuthService struct {
	vault    *Vault
	store    OAuthStore
	audit    management.AuditRecorder
	client   *connector.Client
	registry connector.Registry
	clock    domain.Clock
	// svc is the Task 4 credential lifecycle (revoke reuses its atomic
	// disable propagation); may be nil in narrow tests.
	svc *Service
}

func NewOAuthService(vault *Vault, store OAuthStore, audit management.AuditRecorder, client *connector.Client, registry connector.Registry, svc *Service, clock domain.Clock) *OAuthService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	if client == nil {
		client = &connector.Client{}
	}
	return &OAuthService{vault: vault, store: store, audit: audit, client: client, registry: registry, svc: svc, clock: clock}
}

func (s *OAuthService) record(ctx context.Context, op Operator, action, objectID, reason string, detail map[string]any) error {
	if err := s.audit.Record(ctx, management.AuditEvent{
		Action: action, ObjectType: "oauth_authorization", ObjectID: objectID,
		Reason: reason, ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: management.SanitizeDetail(detail),
	}); err != nil {
		return domain.WrapError(domain.CodeInternal, "audit write failed", err)
	}
	return nil
}

// BeginAuthorization starts one flow. The provider must be an
// oauth_connector access-type provider (OAuth 语义只适用于连接器接入；
// official_api/self_hosted 走静态凭据，设计 §8).
func (s *OAuthService) BeginAuthorization(ctx context.Context, op Operator, providerID, connectorKey, accountLabel, reason string) (*AuthorizationStart, error) {
	spec, ok := s.registry[connectorKey]
	if !ok {
		return nil, domain.NewError(domain.CodeInvalidInput, "unknown oauth connector "+connectorKey)
	}
	prov, err := s.store.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if prov.AccessType != domain.AccessOAuthConnector {
		return nil, domain.NewError(domain.CodeInvalidInput, "provider access_type is not oauth_connector")
	}
	if prov.Status != "active" {
		return nil, domain.NewError(domain.CodeConflict, "provider is disabled")
	}
	state, err := randomURLToken()
	if err != nil {
		return nil, err
	}
	verifier, err := randomURLToken()
	if err != nil {
		return nil, err
	}
	grant := &domain.OAuthGrant{
		State:          state,
		Connector:      connectorKey,
		ProviderID:     providerID,
		AccountLabel:   accountLabel,
		CodeVerifier:   verifier,
		OperatorUserID: op.UserID,
		OperatorAppID:  op.AppID,
		ExpiresAt:      s.clock.Now().Add(StateTTL),
	}
	if err := s.store.InsertOAuthGrant(ctx, grant); err != nil {
		return nil, err
	}
	challenge := ""
	if spec.SupportsPKCE() {
		challenge = pkceChallenge(verifier)
	}
	authorizeURL, err := s.client.BuildAuthorizeURL(spec, state, challenge)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "build authorize url", err)
	}
	if err := s.record(ctx, op, "oauth.authorize.begin", grant.ID, reason, map[string]any{
		"provider_id": providerID, "connector": connectorKey, "account_label": accountLabel,
		"expires_at": grant.ExpiresAt,
	}); err != nil {
		return nil, err
	}
	return &AuthorizationStart{
		AuthorizeURL: authorizeURL, State: state, ExpiresAt: grant.ExpiresAt,
		Connector: connectorKey, ProviderID: providerID,
	}, nil
}

// HandleCallback completes the flow: one-time state consumption, operator
// binding check, code exchange, sealed storage, account creation — the
// credential, account and audit commit in ONE UnitOfWork when the store is
// transaction-aware (a partial callback must never leave a credential
// without its account or an unaudited secret).
func (s *OAuthService) HandleCallback(ctx context.Context, op Operator, state, code, reason string) (*CallbackResult, error) {
	if s.vault == nil {
		return nil, domain.NewError(domain.CodeInternal, "credential vault not configured")
	}
	if state == "" || code == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "state and code are required")
	}
	grant, err := s.store.ConsumeOAuthGrant(ctx, state, s.clock.Now())
	if err != nil {
		return nil, err // CodeConflict: unknown / consumed / expired
	}
	if grant.OperatorUserID != op.UserID || grant.OperatorAppID != op.AppID {
		// state 绑定发起运营人员：换人/换 app 的回调一律拒绝（state 已在
		// 上一步消费，拒绝后不能重放）。
		return nil, domain.NewError(domain.CodeConflict, "oauth state is bound to a different operator")
	}
	spec, ok := s.registry[grant.Connector]
	if !ok {
		return nil, domain.NewError(domain.CodeInternal, "oauth connector "+grant.Connector+" no longer configured")
	}
	ts, err := s.client.ExchangeCode(ctx, spec, code, grant.CodeVerifier)
	if err != nil {
		return nil, domain.WrapError(domain.CodeUpstreamUnavailable, "oauth code exchange failed", err)
	}
	bundle, err := MarshalBundle(&OAuthBundle{
		AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken,
		TokenType: ts.TokenType, ObtainedAt: s.clock.Now(),
	})
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "marshal oauth bundle", err)
	}
	credID, err := newID()
	if err != nil {
		return nil, err
	}
	ct, keyVersion, err := s.vault.Encrypt(credID, grant.ProviderID, bundle)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "encrypt oauth bundle", err)
	}
	var expiry *time.Time
	if !ts.Expiry.IsZero() {
		exp := ts.Expiry.UTC()
		expiry = &exp
	}
	cred := &domain.Credential{
		ID: credID, ProviderID: grant.ProviderID, Label: grant.AccountLabel,
		AuthType: "oauth", Ciphertext: ct, KeyVersion: keyVersion,
		Generation: 1, ExpiresAt: expiry, Status: "active", Connector: grant.Connector,
	}
	account := &domain.UpstreamAccount{
		ProviderID: grant.ProviderID, CredentialID: credID,
		ExternalAccountID: ts.ExternalAccountID, DisplayName: grant.AccountLabel,
		Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	detail := map[string]any{
		"provider_id": grant.ProviderID, "connector": grant.Connector,
		"grant_id": grant.ID, "key_version": keyVersion,
	}
	if ts.ExternalAccountID != "" {
		detail["external_account_id"] = ts.ExternalAccountID
	}
	if ts.RefreshToken != "" {
		detail["refreshable"] = true
	}
	if ran, err := s.runAtomicOAuth(ctx, func(w domain.UnitOfWork) error {
		ts := s.store.(OAuthTxStore)
		if err := ts.InsertCredentialTx(ctx, w, cred); err != nil {
			return err
		}
		if err := ts.InsertUpstreamAccountTx(ctx, w, account); err != nil {
			return err
		}
		return s.audit.(TxRecorder).RecordTx(ctx, w, management.AuditEvent{
			Action: "oauth.authorize.complete", ObjectType: "oauth_authorization",
			ObjectID: credID, Reason: reason, ActorUser: op.UserID, ActorApp: op.AppID,
			Detail: management.SanitizeDetail(detail),
		})
	}); ran {
		if err != nil {
			return nil, err
		}
		return &CallbackResult{Credential: toView(cred), AccountID: account.ID}, nil
	}
	// Sequential fallback for plain (non-transactional) test stores.
	if err := s.store.(interface {
		InsertCredential(ctx context.Context, c *domain.Credential) error
	}).InsertCredential(ctx, cred); err != nil {
		return nil, err
	}
	if err := s.store.(interface {
		InsertUpstreamAccount(ctx context.Context, a *domain.UpstreamAccount) error
	}).InsertUpstreamAccount(ctx, account); err != nil {
		return nil, err
	}
	if err := s.record(ctx, op, "oauth.authorize.complete", credID, reason, detail); err != nil {
		return nil, err
	}
	return &CallbackResult{Credential: toView(cred), AccountID: account.ID}, nil
}

// runAtomic mirrors Service.runAtomic for the OAuth store contract.
func (s *OAuthService) runAtomicOAuth(ctx context.Context, fn func(w domain.UnitOfWork) error) (bool, error) {
	ts, ok := s.store.(OAuthTxStore)
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

// Revoke ends an authorization: vendor-side revocation is best-effort
// (never blocks), the LOCAL revocation — credential revoked plus every
// bound account disabled in one UnitOfWork — is the authoritative part
// (复用 Task 4 SetStatus 的原子传播与审计).
func (s *OAuthService) Revoke(ctx context.Context, op Operator, credentialID, reason string) (*View, error) {
	if s.svc == nil {
		return nil, domain.NewError(domain.CodeInternal, "credential lifecycle service not wired")
	}
	cred, err := s.svc.store.GetCredential(ctx, credentialID)
	if err != nil {
		return nil, err
	}
	vendorRevoke := "skipped"
	if cred.AuthType == "oauth" && s.vault != nil {
		if spec, ok := s.registry[cred.Connector]; ok {
			if plain, derr := s.vault.Decrypt(cred.ID, cred.ProviderID, cred.KeyVersion, cred.Ciphertext); derr == nil {
				if bundle, berr := UnmarshalBundle(plain); berr == nil {
					token := bundle.RefreshToken
					if token == "" {
						token = bundle.AccessToken
					}
					// 厂商吊销是尽力而为的附属动作：独立 10s 超时，慢/挂的
					// 厂商端点不得拖住本地吊销（审查修复 M-3）。
					vendorCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					rerr := s.client.Revoke(vendorCtx, spec, token)
					cancel()
					if rerr != nil {
						vendorRevoke = "failed"
					} else if spec.RevokeURL != "" {
						vendorRevoke = "ok"
					}
				}
			}
		}
	}
	view, err := s.svc.SetStatus(ctx, op, credentialID, "revoked", reason)
	if err != nil {
		return nil, err
	}
	// 本地吊销已由 SetStatus 审计（credential.disable）；厂商侧结果补一条
	// 同对象审计，失败也可见。
	if err := s.record(ctx, op, "oauth.revoke", credentialID, reason, map[string]any{
		"provider_id": cred.ProviderID, "connector": cred.Connector, "vendor_revoke": vendorRevoke,
	}); err != nil {
		return nil, err
	}
	return view, nil
}

// randomURLToken returns 32 crypto-random bytes in base64url (state/PKCE
// verifier material).
func randomURLToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", domain.WrapError(domain.CodeInternal, "oauth random token", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// pkceChallenge is the RFC 7636 S256 challenge of a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
