package domain

import (
	"context"
	"encoding/json"
	"time"
)

// Protocol is a supported wire protocol. Upstream protocol and upstream
// auth type are modeled separately (设计 §8).
type Protocol string

const (
	ProtocolOpenAIChat       Protocol = "openai_chat"
	ProtocolOpenAIResponses  Protocol = "openai_responses"
	ProtocolAnthropicMessage Protocol = "anthropic_messages"
	// ProtocolKayaChat is the legacy Kaya facade shape adapted into the
	// gateway; it never appears on an upstream Deployment.
	ProtocolKayaChat Protocol = "kaya_chat"
)

// Lifecycle is the commercial lifecycle of a public model. New models are
// Draft and NOT sellable by default (设计 §5).
type Lifecycle string

const (
	LifecycleDraft      Lifecycle = "draft"
	LifecycleActive     Lifecycle = "active"
	LifecycleDeprecated Lifecycle = "deprecated"
	LifecycleRetired    Lifecycle = "retired"
)

// DeploymentStatus is the operational state of an upstream deployment.
type DeploymentStatus string

const (
	DeploymentDraft    DeploymentStatus = "draft"
	DeploymentActive   DeploymentStatus = "active"
	DeploymentDisabled DeploymentStatus = "disabled"
)

// AccessType describes how a Provider is reached (设计 §5/§8).
type AccessType string

const (
	AccessOfficialAPI    AccessType = "official_api"
	AccessOAuthConnector AccessType = "oauth_connector"
	AccessSelfHosted     AccessType = "self_hosted"
)

// ExtensionConfig is the only JSONB-carried shape in the module: protocol
// extension configuration, always schema-versioned (设计 §5: 仅协议扩展
// 配置使用带 schema 版本的 JSONB). Everything else is typed columns.
type ExtensionConfig struct {
	SchemaVersion int
	Raw           json.RawMessage
}

// Model is the public catalog entry. ID is the stable public model ID
// (the /v1/models id), deliberately not a UUID (设计 §5).
type Model struct {
	ID               string
	DisplayName      string
	Lifecycle        Lifecycle
	ModelVersion     string
	Aliases          []string
	InputModalities  []string
	OutputModalities []string
	// ContextTokens and MaxOutputTokens are hard bounds; unlimited output
	// is not representable (设计 §7.2).
	ContextTokens        int
	MaxOutputTokens      int
	Protocols            []Protocol
	SupportsTools        bool
	SupportsReasoning    bool
	CreatedAt, UpdatedAt time.Time
}

// Provider is a vendor/upstream family; never bound to a single account.
type Provider struct {
	ID          string
	Code        string
	DisplayName string
	AccessType  AccessType
	Status      string // "active" | "disabled"
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Deployment is one upstream endpoint serving one upstream model name.
type Deployment struct {
	ID             string
	ProviderID     string
	UpstreamModel  string
	BaseURL        string
	Protocol       Protocol
	Region         string
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	// ConfigVersion is the optimistic-lock version for edits (设计 §5).
	ConfigVersion int
	Config        ExtensionConfig
	Status        DeploymentStatus
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PoolStrategy selects how upstream accounts are picked for a route.
type PoolStrategy string

const (
	PoolRoundRobin    PoolStrategy = "round_robin"
	PoolLeastLoaded   PoolStrategy = "least_loaded"
	PoolSessionSticky PoolStrategy = "session_sticky"
)

// ModelRoute maps a public Model to one of its Deployments (设计 §5:
// Model → 多 Deployment; priority/weight/capabilities/pool strategy).
type ModelRoute struct {
	ID           string
	ModelID      string
	DeploymentID string
	Priority     int
	Weight       int
	Capabilities []string
	PoolStrategy PoolStrategy
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ConfigScope is the publishable scope of a config revision.
type ConfigScope string

const (
	ScopeCatalog ConfigScope = "catalog"
	ScopePricing ConfigScope = "pricing"
	ScopePolicy  ConfigScope = "policy"
)

// RevisionStatus is the lifecycle of an immutable config revision.
type RevisionStatus string

const (
	RevisionDraft      RevisionStatus = "draft"
	RevisionPublished  RevisionStatus = "published"
	RevisionSuperseded RevisionStatus = "superseded"
)

// ConfigRevision is an immutable published snapshot (设计 §5: 草稿 +
// 不可变发布版本；发布原子切换 active revision). At most one revision per
// scope has IsActive — enforced by a partial unique index.
type ConfigRevision struct {
	ID          int64
	Scope       ConfigScope
	Revision    int
	Payload     ExtensionConfig
	Status      RevisionStatus
	IsActive    bool
	PublishedAt *time.Time
	CreatedBy   string
	CreatedAt   time.Time
}

// RevisionMeta 是 ConfigRevision 的元数据投影（安全审查 M-1）：历史/审计
// 列表只需要发布信息（谁、何时、什么状态），不读 payload 快照体——修订
// 的 blob 可能很大，列表查询绝不做全表 SELECT *。Payload 仅经
// GetRevision/ActiveRevision 按需加载。
type RevisionMeta struct {
	ID          int64
	Scope       ConfigScope
	Revision    int
	Status      RevisionStatus
	IsActive    bool
	PublishedAt *time.Time
	CreatedBy   string
	CreatedAt   time.Time
}

// ModelFilter filters catalog listings. Limit <= 0 means the
// implementation's default page size; AfterID is the stable pagination
// cursor (last seen model ID).
type ModelFilter struct {
	Lifecycle *Lifecycle
	AfterID   string
	Limit     int
}

// DeploymentFilter filters deployment listings. Empty ProviderID / Status
// match all; AfterID is the keyset cursor on id; Limit <= 0 uses the
// implementation's default page size.
type DeploymentFilter struct {
	ProviderID string
	Status     DeploymentStatus
	AfterID    string
	Limit      int
}

// CatalogReader is the read side of the publishable catalog. A gateway
// request pins ONE active revision snapshot for its whole lifetime
// (设计 §5: 一次调用固定使用一个快照).
type CatalogReader interface {
	GetModel(ctx context.Context, id string) (*Model, error)
	ListModels(ctx context.Context, filter ModelFilter) ([]Model, error)
	GetDeployment(ctx context.Context, id string) (*Deployment, error)
	RoutesForModel(ctx context.Context, modelID string) ([]ModelRoute, error)
	ActiveRevision(ctx context.Context, scope ConfigScope) (*ConfigRevision, error)
}
