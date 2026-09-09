package domain

import (
	"context"
	"time"
)

// PrincipalKind distinguishes the caller classes entering the gateway.
// Customer API Keys and Kaya JWTs (via facade) converge on ONE internal
// principal shape; operator principals never mix with callers (设计 §3,
// Task 5: 明确调用 principal 与运营 principal 的区别).
type PrincipalKind string

const (
	PrincipalAPIKey   PrincipalKind = "api_key"
	PrincipalKayaJWT  PrincipalKind = "kaya_jwt"
	PrincipalOperator PrincipalKind = "operator"
	PrincipalService  PrincipalKind = "service"
)

// Principal is the authenticated caller of a model request.
type Principal struct {
	Kind             PrincipalKind
	BillingAccountID string
	// APIKeyID is set for PrincipalAPIKey callers.
	APIKeyID string
	// UserID is the server-verified user identity (facade path).
	UserID string
	// OperatorSubject is set for PrincipalOperator callers only.
	OperatorSubject string
}

// BillingAccountStatus mirrors the DB CHECK on
// inference_billing_accounts.status (migration 025).
type BillingAccountStatus string

const (
	BillingAccountActive    BillingAccountStatus = "active"
	BillingAccountSuspended BillingAccountStatus = "suspended"
	BillingAccountClosed    BillingAccountStatus = "closed"
)

// BillingAccount is the billing owner of usage. Phase 1: exactly one per
// user (UNIQUE(user_id)); SubjectType reserves organization seats (设计
// §4.2). Deleting a user must NOT cascade here — the de-identification
// policy of later tasks retains the ledger.
type BillingAccount struct {
	ID          string
	UserID      string
	SubjectType string // "user" | "organization"
	Status      string // BillingAccountStatus（active/suspended/closed）
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// APIKeyStatus mirrors the DB CHECK.
type APIKeyStatus string

const (
	APIKeyActive  APIKeyStatus = "active"
	APIKeyRevoked APIKeyStatus = "revoked"
	APIKeyExpired APIKeyStatus = "expired"
)

// APIKey is a customer key. The plaintext is shown exactly once at
// creation; the DB holds only the lookup prefix and the verification
// digest (设计 §9.2).
type APIKey struct {
	ID               string
	BillingAccountID string
	Name             string
	Prefix           string
	// ModelAllow narrows the account entitlement for this key; nil means
	// no key-level narrowing. It can never widen beyond the entitlement
	// model set (设计 §4.2).
	ModelAllow []string
	// BudgetLimit is the optional per-Key budget in microcredits; nil =
	// no budget. BudgetUsed is maintained by QuotaStore in the same
	// transaction as the three windows.
	BudgetLimit      *Microcredit
	BudgetUsed       Microcredit
	RPMLimit         *int
	ConcurrencyLimit *int
	Status           APIKeyStatus
	ExpiresAt        *time.Time
	RevokedAt        *time.Time
	LastUsedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// EntitlementSource identifies where an entitlement came from (设计
// §4.2: 来源订阅/订单/赠送规则).
type EntitlementSource string

const (
	SourceSubscription EntitlementSource = "subscription"
	SourceOrder        EntitlementSource = "order"
	SourceGrant        EntitlementSource = "grant"
)

// EntitlementStatus mirrors the DB CHECK.
type EntitlementStatus string

const (
	EntitlementActive     EntitlementStatus = "active"
	EntitlementExpired    EntitlementStatus = "expired"
	EntitlementRevoked    EntitlementStatus = "revoked"
	EntitlementSuperseded EntitlementStatus = "superseded"
)

// Entitlement is the authorization source for model calls. ModelIDs is
// ALWAYS explicit — there is no NULL-means-everything semantics (基线报告
// 差距 3: 拒绝继承 plans.chat_models NULL 全放行). AnchorAt is the
// original effective anchor driving weekly/monthly windows; upgrades keep
// the same anchor and consumption subject (设计 §6).
type Entitlement struct {
	ID               string
	BillingAccountID string
	SourceType       EntitlementSource
	SourceID         string
	ModelIDs         []string
	PolicyVersionID  string
	AnchorAt         time.Time
	EffectiveFrom    time.Time
	EffectiveTo      *time.Time
	Revision         int
	// Stackable=false (default): gifted entitlements do not stack with an
	// explicit plan nor auto-fallback after exhaustion (设计 §4.2).
	Stackable bool
	Status    EntitlementStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AllowsModel reports whether the entitlement's explicit model set
// contains modelID. An empty set allows nothing.
func (e *Entitlement) AllowsModel(modelID string) bool {
	for _, id := range e.ModelIDs {
		if id == modelID {
			return true
		}
	}
	return false
}

// EntitlementPatch is the in-place revision payload for upgrade/renewal
// (设计 §4.2/§6). Revision bumps by exactly one; ID, anchor and account
// NEVER change — the entitlement stays the same consumption subject, and
// because quota windows key on entitlement ID, their used/reserved carry
// over (更新限额不清空 used/reserved；不能通过新 policy ID 获得全新空窗口).
type EntitlementPatch struct {
	// ExpectedRevision is the optimistic-lock guard: the writer must have
	// read this revision; a stale writer conflicts.
	ExpectedRevision int
	// PolicyVersionID, when non-nil, replaces the quota policy version.
	PolicyVersionID *string
	// ModelIDs, when non-nil, replaces the explicit model set (an empty
	// non-nil slice grants NO models — never NULL-means-all).
	ModelIDs []string
	// EffectiveTo, when non-nil, extends/repoints the effective range
	// (续费延长有效期，不提前重置窗口).
	EffectiveTo *time.Time
	// ClearEffectiveTo, when true, writes effective_to = NULL — the
	// finite → open-ended transition (e.g. upgrade to a lifetime plan,
	// 开放型订阅). COALESCE alone cannot express that write (审查修复:
	// nil EffectiveTo previously meant only "keep the current value", so
	// 有限→开放 在持久层不可表达). ClearEffectiveTo wins over
	// EffectiveTo when both are set (caller error, fail deterministically).
	ClearEffectiveTo bool
}

// UpstreamAccountStatus is the account state machine of 设计 §8.
type UpstreamAccountStatus string

const (
	AccountActive         UpstreamAccountStatus = "active"
	AccountRefreshing     UpstreamAccountStatus = "refreshing"
	AccountCooldown       UpstreamAccountStatus = "cooldown"
	AccountReauthRequired UpstreamAccountStatus = "reauth_required"
	AccountDisabled       UpstreamAccountStatus = "disabled"
)

// UpstreamQuota is the cached view of an upstream account's own quota.
// Every field is optional: unknown stays unknown (设计 §8: 上游额度缓存
// 包含 observed_at/source/reset_at，未知保持未知).
type UpstreamQuota struct {
	LimitMicros     *Microcredit
	RemainingMicros *Microcredit
	ObservedAt      *time.Time
	// Source is "reported" | "estimated"; nil when unknown.
	Source  *string
	ResetAt *time.Time
}

// UpstreamAccount is one schedulable upstream account (设计 §8).
type UpstreamAccount struct {
	ID                string
	ProviderID        string
	CredentialID      string
	ExternalAccountID string
	DisplayName       string
	Status            UpstreamAccountStatus
	ConcurrencyLimit  int
	Quota             UpstreamQuota
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Credential is the stored upstream credential reference. Ciphertext is
// AEAD-encrypted (Task 4); KeyVersion supports rotation, Generation is
// the CAS counter that prevents a stale refresh token overwriting a newer
// one (设计 §8). No API response ever carries usable secrets.
type Credential struct {
	ID         string
	ProviderID string
	Label      string
	AuthType   string // "api_key" | "oauth" | "service"
	// Ciphertext never leaves the credentials package decrypted.
	Ciphertext []byte
	KeyVersion int
	Generation int64
	ExpiresAt  *time.Time
	// LastRotatedAt is set by the rotate path (migration 025).
	LastRotatedAt *time.Time
	Status        string // "active" | "rotating" | "revoked"
	// Connector is the OAuth connector registry key this credential was
	// authorized through (migration 032); empty for api_key/service rows.
	Connector string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CredentialResolver resolves caller/upstream credentials into principals
// or secret references. Implemented by access/credentials packages; the
// plaintext of a customer key is never recoverable after creation.
type CredentialResolver interface {
	// ResolveAPIKey authenticates a presented customer key and returns the
	// caller principal. Unknown/revoked/expired keys yield CodeInvalidKey.
	ResolveAPIKey(ctx context.Context, presentedKey string) (*Principal, error)
	// ResolveUpstream returns the credential reference for a schedulable
	// upstream account (secret material stays inside the credentials
	// boundary).
	ResolveUpstream(ctx context.Context, upstreamAccountID string) (*Credential, error)
}

// EntitlementResolver picks the effective entitlement for a call. Rules
// (设计 §4.2): an explicit purchased plan wins; gifted entitlements apply
// only when no explicit plan exists and never stack or auto-fallback.
type EntitlementResolver interface {
	Resolve(ctx context.Context, billingAccountID, modelID string, at time.Time) (*Entitlement, error)
}

// ---------------------------------------------------------------------------
// Task 12: OAuth authorization grants & sticky session bindings (设计 §8)
// ---------------------------------------------------------------------------

// OAuthGrant is one pending authorization flow: a one-time state bound to
// the initiating operator (user + app), the target provider, and an expiry
// (设计 §8: state 绑定发起运营人员、目标账号/供应商及有效期). The PKCE
// verifier is a short-lived server-side secret and never leaves the
// credentials boundary. Consumed rows are kept for audit.
type OAuthGrant struct {
	ID             string
	State          string
	Connector      string
	ProviderID     string
	AccountLabel   string
	CodeVerifier   string
	OperatorUserID string
	OperatorAppID  string
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
	CreatedAt      time.Time
}

// SessionBindingStatus is the sticky-session binding lifecycle.
type SessionBindingStatus string

const (
	BindingActive SessionBindingStatus = "active"
	BindingEnded  SessionBindingStatus = "ended"
)

// Well-known ended reasons (审计可读口径).
const (
	BindingEndedAccountInvalid = "account_invalid" // 账号失效：reauth/disabled/revoked
	BindingEndedMigrated       = "migrated"        // 显式迁移（新会话语义重建）
	BindingEndedExpired        = "expired"         // 到期清扫
	BindingEndedOperator       = "operator_end"    // 运营显式终止
)

// SessionBinding pins one (session_key, model_id) conversation to one
// upstream account (设计 §8: 需要会话黏性时绑定具体账号，撤销或失效后按照
// 协议要求终止/重建会话，不能无条件切账号续接).
type SessionBinding struct {
	ID          string
	SessionKey  string
	ModelID     string
	AccountID   string
	Status      SessionBindingStatus
	EndedReason *string
	BoundAt     time.Time
	LastUsedAt  time.Time
	ExpiresAt   time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
