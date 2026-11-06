# Users 模型网关完善 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. 用户指定由另一个 Agent 执行，本交接不启动实现。Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Users 推理运行链路的鉴权、停用、配额结算和限速问题，使实际支持的模型接入配置与运行行为一致，并提供可验证的服务端协议契约。

**Architecture:** 保留现有 Gin → inference gateway → quota/routing/providers → PostgreSQL 的分层。扩展既有事务预占、租约、结算与恢复机制，账户级限速统一进入网关准入；不增加独立网关服务，不引入 Redis。优先复用已有凭据、图片、错误映射和会话绑定实现。

**Tech Stack:** Go（以 go.mod 为准，当前最低 1.25）、Gin、sqlx、PostgreSQL、现有 SSE adapter 与测试设施。

**Spec:** 本文“范围与执行决策”“全局约束”是本阶段补充规格；历史架构参考 `docs/superpowers/specs/2026-09-08-kaya-coding-plan-design.md`、`docs/superpowers/specs/2026-10-13-builtin-multimodal-design.md`。与历史待办冲突时，先看当前代码，再以本阶段明确决策为准；不得把旧 runbook 的线上状态当作当前事实。

## 0. 工作区、证据与范围

- 工作区：`/Users/jack-tian/Downloads/github/yunhou-users-model-gateway`。
- 分支：`fix/model-gateway-completion`；基线：`299fc49`。
- 原仓库 `/Users/jack-tian/Downloads/github/yunhou-users` 保持原状；在本 worktree 内实施和提交。
- 本文发现来自代码静态核对。未访问生产数据库、未调用真实供应商；不能据此宣布生产已发生漏账或凭据泄露。
- 交接准备时在新 worktree 实测：`GOPROXY=off go test -count=1` 加 T0 所列七个纯单测包，全部通过。`go mod download` 曾因 `proxy.golang.org` 网络超时失败，已有本地缓存足以完成这七组测试；未修改 go.mod/go.sum。数据库、全量和真实上游验收尚未执行，T0/T10 必须补跑。
- 每项先复现。若未复现，定位实际保护层并记录文件和测试，不要为匹配本清单强行修改。
- 允许修改 `internal/inference/`、相关 `internal/model`、`internal/service/chat_facade.go`、`internal/router`、`internal/config`、`cmd/server`、迁移、测试与对应 API 文档。
- 不修改相邻 website、terminal、deploy 仓库；不做 Token Plan 商品编辑/售卖、支付退款/改签、人工权益与支付同步优先级、组织/项目计费、历史账务自动修正。
- 本阶段不承诺完整 OpenAI/Anthropic API 等价，不实现音视频、文件、服务端工具、完整 encrypted reasoning/签名思考回放。

### 执行决策（交接时采用的默认方案）

这些是为让实施可执行而选定的技术边界，不代表用户已经确认新的商品政策。不要据此改线上配置。

1. 保留“同一用户一个计费账户”的共享权益语义，不偷偷新增 app scope 授权限制。AppID 可用于审计归因；不能把用户可控制的 app 字段当权限依据。
2. 目录态按已发布快照生效，Provider 停用必须过滤普通候选与 sticky 候选。正在进行的请求继续按准入快照结算；不是实时撤销在途请求。
3. 保留现有 model lifecycle observe/enforce 开关及默认值，补齐一致性测试和上线说明；不在本次代码合入中隐式切换线上政策。
4. 无金额窗口的策略合法（例如仅限制并发的赠送），仍须完整记量/记账。不要通过禁止这种策略、制造虚假额度窗口或把费用改为零绕过结算缺陷。
5. 账户/Key RPM 与策略 TPM 使用 PostgreSQL 持久协调，多实例共享；具体表结构可由实施者设计，必须满足 T4 的确定语义。数据库不可用时拒绝新推理，不能回退单机无限放行。
6. Key `budget_micros` 继续只表示套餐 microcredit 子预算。本期不将其悄悄转换为现金预算，也不新增钱包 Key 金额产品；明确文档和测试。
7. 非默认调度策略、额度不足自动缩输出等尚未兑现的配置，本期采用“明确支持集+拒绝不支持配置”，不扩大为智能调度项目。`weight` 保留既有故障候选排序语义，不改为比例分流。
8. 多模态本阶段修复既有 `/chat` 图片链路的能力选择；三种 `/v1` 入口继续保持明确的文本子集。本阶段通过原生 4xx、能力说明和测试兑现边界，不假装已支持图片；完整 `/v1` 多模态另立任务。
9. reasoning effort 不能静默全部变成 medium；可表达且上游支持的值原样传递，不能表达的选项/历史块在出站前明确拒绝。本期不建立通用无损原生协议透传层。

## Global Constraints

- 先读 `CLAUDE.md`。迁移只追加；起点最大编号为 `049`，实施时重新核对后从最新编号 + 1 分配。
- 不读/复制原仓 `.env`、`keys/`、线上凭据。所有网络断言用测试 token、httptest 和独立临时 PostgreSQL。
- 不执行默认连接本地业务库的测试：多个 TestMain 会自动迁移，fixture 会 TRUNCATE。必须显式设置一次性 `DATABASE_URL` / `E2E_DATABASE_URL`，共享测试库时 `-p 1`。
- 保持一逻辑请求一次 charge；重试 attempt 不能重复扣费。reported / estimated / unknown 必须区分，未知用量不能静默记零。
- 金额使用既有定点整数与有理数计价；不引入 float64 账务累计。价格、策略、窗口按准入固定，不回写历史账本。
- 不持有数据库事务等待上游网络。结算 context 与客户端取消解耦；先 finalize 再释放租约，panic 也必须释放。
- 不确定是否已执行的上游请求不得盲目重放。保留出站 DNS/IP 验证、禁止重定向等 SSRF 防护。
- 否决请求不得残留金额预占/TPM 预占/租约；不要为失败请求伪造成功终态。明确区分准入前拒绝和已出站失败。
- 现有 1 秒权益/账号缓存和目录快照约定保持；不得为了通过一条测试把全部缓存永久关闭。
- 所有新增枚举、响应字段和错误必须同步 OpenAPI/集成指南；日志、审计和错误不得包含真实 key、refresh token、图片 base64 或用户完整对话。
- 每项：回归测试失败证据 → 最小实现 → 定向测试 → 独立 commit。只暂存本任务文件，不使用 `git add .`。不推送、不合并、不部署。

## Review Focus

- 两个服务实例争最后一次限速许可/并发许可；一侧失败不能使另一侧漏限或留下部分预占（T4、T5）。
- SSE 在输出之后断开、进程恢复、finalize panic；钱、TPM 预占和所有租约仍须收敛且不重放上游（T2、T4、T5、T10）。
- 用户通过别名、sticky 会话、旧缓存命中已停用 Provider；不能绕过当前请求固定快照的过滤（T3）。
- 合法无窗口策略与真正丢失预占的异常，不能因为同为 CodeConflict 而一起吞掉（T2）。
- 看似接受的图片/reasoning/策略字段实际上丢失或降级；必须原样满足或在出站前明确拒绝（T7–T9）。

## 1. 任务导航与依赖

| 任务 | 交付物 | 依赖 |
|---|---|---|
| T0 | 安全测试环境与基线记录 | 无 |
| T1 | OAuth 出站鉴权正确，凭据材料不越界 | T0 |
| T2 | 无窗口正常结算、精确幂等与恢复 | T0 |
| T3 | Provider/模型停用及会话路径一致 | T0 |
| T4 | 网关统一的持久 RPM/TPM 准入与生命周期 | T2 |
| T5 | Key 并发租约与账户/上游限制组合 | T2、T4 |
| T6 | 所有限流层返回原生协议错误 | T4、T5 |
| T7 | 调度/超时/超额配置诚实生效或拒绝 | T3–T5 |
| T8 | 图片候选筛选、reasoning 不静默降级、协议矩阵 | T1–T7 |
| T9 | 协议感知探测与能力/运维文档 | T1、T3、T7、T8 |
| T10 | 真实 PG + 模拟上游集成、全量回归和交付 | 全部 |

建议顺序 T0→T1→T2→T3→T4→T5→T6→T7→T8→T9→T10。有并行工具时可并行做只读分析和独立测试设计；T2/T4/T5 共享结算和预占代码，不并行修改。

## T0 — 建立隔离测试环境与基线

**Read:** `CLAUDE.md`、`Makefile`、`.github/workflows/pr-ci.yml`、`internal/inference/{gateway,httpapi,postgres,catalog}/*test.go` 的 TestMain。

- [x] 确认当前绝对路径、分支和 `git status --short`，只允许本交接文档产生的变化；记录基线 SHA。
- [x] 运行下面的纯单测命令；前一轮分析已经通过过这七组，但本实施开始时仍应记录自己的结果。
- [x] 创建一次性 PG，显式配置两种测试 DSN，跑迁移两次并确认第二次零新增；使用 `go run ./cmd/migrate -status` 核对。
- [x] 串行运行 inference 和现有 gateway 集成测试；记录 skip，任何数据库测试 skip 不算基线通过。
- [x] 新建 `docs/runbooks/2026-11-05-model-gateway-verification.md`，记录环境、命令、退出码、失败/跳过及后续每项证据。不要写密钥或用户对话。

```bash
go test -count=1 ./internal/inference/access ./internal/inference/accounting ./internal/inference/quota ./internal/inference/management ./internal/inference/domain ./internal/inference/providers ./internal/inference/providers/connector
```

## T1 — 修复 OAuth 热路径鉴权

**Modify:** `internal/inference/gateway/service.go`（SecretResolver、dispatch）、`internal/inference/credentials/service.go`、`internal/inference/providers/adapter.go` 及三个 adapter（仅必要部分）、`cmd/server/main.go`。
**Tests:** `internal/inference/gateway/gateway_test.go`、`internal/inference/credentials/service_test.go`、`internal/inference/providers/adapter_dispatch_test.go`。
**Interfaces:** 热路径复用已有 `ResolveBearerToken(ctx context.Context, id string, pinGeneration *int64) (string, *domain.Credential, error)` 或在 credentials 域新增等价的带 auth-type 出站解析器；bundle 格式不泄露给 gateway/adapter，`ResolveSecret` 保留给确实需要原始材料的内部代码。

**现状：** `credentials/oauth.go` 封存 access+refresh bundle；gateway `ResolveSecret` 取到整体 JSON，再经 `AuthHeaders` 写入 Bearer/x-api-key。仅管理 probe 正确解包。

- [x] 写 `TestGateway_OAuthUsesAccessTokenOnly`：通过真实 gateway → httptest 出站，确认 header 只有测试 access token，无 JSON、refresh token；覆盖流式和非流式至少各一次。
- [x] 补静态 api_key/service 不变、空 access token/坏 bundle/停用凭据不派发；错误和测试日志无凭据材料。Anthropic 的 auth header 必须与目标协议/明确支持的 OAuth 接法一致，不能把解包成功等同任意供应商 OAuth 已兼容。
- [x] 运行定向测试，确认新增断言在基线失败；实现热路径解析并更新相关 fake 接口。
- [x] 保留凭据状态、代次/CAS、轮换和 SSRF 保护；测试旧凭据不可被重放派发。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/credentials ./internal/inference/providers/... ./internal/inference/gateway` 后提交。

## T2 — 修复无窗口结算与冲突分类

**Modify:** `internal/inference/quota/service.go`、`internal/inference/postgres/{quota_repo,ledger_repo,reconciliation_repo}.go`、`internal/inference/gateway/service.go`、必要的 domain 类型和追加迁移。
**Tests:** `internal/inference/gateway/{gateway_test,finish_order_test}.go`、`internal/inference/postgres/{quota_settlement_test,recovery_test}.go`、`internal/inference/workers/settlement_recovery_test.go`。
**Interfaces:** `ReserveCommand`/Request 必须持久化“该请求应有哪些金额预占”或等价的准入事实；新增可识别的已终态结果（typed error/result），不再将所有 `CodeConflict` 视为成功。现有 `Store.Settle(ctx, uow, cmd)` 的事务边界不拆散。

**现状：** 仅 RPM/TPM/并发的策略合法；无 Key 的 `/chat` 没有任何 holds。Reserve 仍成功，Settle 因无 held reservation 返回 Conflict，gateway 一律当已结算吞掉。

- [x] 写 `TestGateway_NoWindowPolicySettles`：无窗口 × 无 Key/有 Key × 零/正价格，至少覆盖 reported 和 estimated；每个完成请求恰一条 charge、有 usage、终态 settled、无悬挂预占/租约。零金额仍有账本事实。
- [x] 写 `TestGateway_MissingExpectedReservationRequiresReconciliation`：本应存在的金额预占被测试删除后，结算不能成功，必须暴露异常并进入待核对。
- [x] 写重复结算、已 released 请求晚到 usage、同请求并发 finalize 测试：只有 settled 且存在一致的唯一 charge/usage 才是重复成功。released 后晚到非零 usage 是账务异常，必须告警并进入专门核对流程，不能因 released 是终态而吞掉。
- [x] 实现无窗口独立计费事实与缺预占异常的区分；不要删掉 `len(rows)==0` 守卫后对所有请求放行，也不要增设虚假窗口。
- [x] 修复 recovery 同类路径：新无窗口请求须持久化独立的可恢复估算上界，不能因 `reserved_micros=0` 把已消费请求当免费。追加迁移须区分新旧请求；旧请求证据不足时保留待核对，不自动重算历史或猜测补扣。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/quota ./internal/inference/postgres ./internal/inference/gateway ./internal/inference/workers` 后提交。

## T3 — Provider 停用、模型状态与 sticky 一致

**Modify:** `internal/inference/routing/service.go`、`internal/inference/catalog/revision.go`、`internal/inference/gateway/{service,lifecycle_gate}.go`、必要的目录列表投影。
**Tests:** `routing/service_test.go`、`gateway/{sticky_candidates_test,lifecycle_gate_test}.go`、`httpapi/{kaya_models_test,catalog_test}.go`。
**Interfaces:** 普通候选与 `stickyCandidates` 必须复用同一个“已发布 route/provider/deployment 可调度”判定；不能 sticky 分支另开后门。

- [x] 写 `TestCandidates_DisabledProviderExcluded` 和 sticky/模型别名/目录缓存刷新变体：快照中 Provider disabled 时不出站，仍能使用另一个 active Provider。
- [x] 验证模型列表可用性与实际调度相符：只有 disabled Provider 的模型不能被当作可用部署展示；草稿编辑未发布不影响当前快照。
- [x] 保留在途固定快照；新请求在缓存按既有约定刷新后生效，不要求强停旧流。
- [x] 验证 lifecycle enforce 拦截 draft/retired，deprecated 按现有契约允许存量；observe 明确只告警。缓存错误不能被伪装成“无权限”。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/catalog ./internal/inference/routing ./internal/inference/gateway ./internal/inference/httpapi` 后提交。

## T4 — PostgreSQL 统一执行 RPM/TPM

**Create:** `internal/inference/access/inference_limits.go`（类型/纯规则）、`internal/inference/postgres/inference_limit_repo.go`（短事务存储）、对应测试及追加迁移。
**Modify:** `quota/service.go`、`gateway/service.go`、`httpapi/apikey_auth.go`、`access/ratelimit.go`、`postgres/ledger_repo.go`、`workers/settlement_recovery.go`、`cmd/server/main.go`、必要 config。
**Interfaces:** 为入场/结算/释放定义持久 rate reservation，以 logical request ID 幂等；存储方法显式接收现有 `domain.UnitOfWork`。确定类型后在本节补充实际签名，T5/T6 复用，禁止在各入口复制计数器。

**实际签名（T4 实施后回填，T5/T6 直接复用）：**

```go
// internal/inference/domain/rate_limits.go —— 跨包共享的数据形状与可识别的拒绝类型
type RateScope string
const (
    RateScopeAccount RateScope = "account" // 账户级：账户 RPM 计数键 + 账户级 TPM 预占/结算载体
    RateScopeKey    RateScope = "key"    // Key 级：Key RPM 计数键
)
type RateLimits struct { AccountRPM int; PolicyRPM int; PolicyTPM int64; KeyRPM int }
type RateReserveCommand struct {
    RequestID        string // logical request id = 幂等身份
    BillingAccountID string
    APIKeyID         *string
    PolicyVersionID  string // 仅审计：计数键不含 policy version
    Limits           RateLimits
    ReservedTokens   int64 // TPM 预占 = 输入上界 + 强制输出 cap
}
type RateUsage struct {
    AccountRPMCount, KeyRPMCount int
    SettledTokens, InflightTokens int64
    AccountRPMRecoverySeconds, PolicyRPMRecoverySeconds int
    KeyRPMRecoverySeconds, SettledTokensRecoverySeconds int // 0 = 无法确定恢复时刻
}
type RateBlock struct { Scope RateScope; Kind string; Limit, Used int64; RecoverySeconds int }
type RateLimitError struct { Detail *Error; Blocks []RateBlock; RetryAfterSeconds int }

// internal/inference/access/inference_limits.go —— 纯规则（无数据库、无时钟）
const RateWindowSeconds = 60
func NormalizedTokens(b domain.UsageBuckets) int64
func KnownTokenBuckets(b domain.UsageBuckets) bool // 归一化口径是否有已知 token 量（unknown ≠ 0）
func EvaluateRateLimits(limits domain.RateLimits, usage domain.RateUsage, reservedTokens int64) *domain.RateLimitError
func RecoverySeconds(blocks []domain.RateBlock) int // 只有每个阻断约束都有可确定恢复时刻才给倒计时

// internal/inference/postgres/inference_limit_repo.go —— 短事务存储（全部显式接收 UnitOfWork）
func (s *Store) ReserveRateTx(ctx context.Context, w domain.UnitOfWork, cmd domain.RateReserveCommand) error
func (s *Store) CorrectRateTokensTx(ctx context.Context, w domain.UnitOfWork, requestID string, correctedTokens int64) error
func (s *Store) PurgeRateReservations(ctx context.Context, cutoff time.Time) (int64, error)
// 结算/释放与终态事务同事务生效（内部同包调用）：
func (s *Store) settleRateTx(ctx context.Context, tx *sqlx.Tx, requestID string, tokens int64) error
func (s *Store) releaseRateTx(ctx context.Context, tx *sqlx.Tx, requestID string) error
// 修正的共享内核（金额修正与独立速率修正共用；同一个 *sqlx.Tx）：
func (s *Store) correctRateTokensTx(ctx context.Context, tx *sqlx.Tx, requestID string, correctedTokens int64) error

// internal/inference/postgres/reconciliation_repo.go —— 金额修正契约（与 token 预算同一个事务）
type CorrectionCommand struct {
	RequestID string
	Corrected domain.UsageRecord // 修正后的用量事实（归一化桶）
	CorrectedMicros domain.Microcredit
	// CorrectedTokens 是**绝对**修正后的归一化 token 总量（nil = 修正证据
	// 没有已知 token 量 → token 预算保留原值，绝不记零）。存储效果是净差额
	// （行上只有一个权威总量），原结算时刻不动。
	CorrectedTokens *int64
	Reason, OperatorSubject, IdempotencyKey string
	At time.Time
}
func (s *Store) CorrectSettlement(ctx context.Context, w domain.UnitOfWork, cmd CorrectionCommand) error

// internal/inference/quota/service.go —— 准入编排（速率闸门与金额/并发预占同一事务）
type Store interface { /* … */ ReserveRateTx(ctx context.Context, uow domain.UnitOfWork, cmd domain.RateReserveCommand) error }
type Service struct { /* … */ AccountRPM int } // 全局账户 RPM（INFERENCE_ACCOUNT_RPM；0 = 关闭）

// internal/inference/workers/rate_reservation_reaper.go —— 保留期清理（不删未结算预占）
type RateReservationStore interface { PurgeRateReservations(ctx context.Context, cutoff time.Time) (int64, error) }
type RateReaperConfig struct { Interval, Retention time.Duration } // 默认 1h / 24h
func NewRateReservationReaper(store RateReservationStore, clock domain.Clock, cfg RateReaperConfig) *RateReservationReaper
func (r *RateReservationReaper) RunPass(ctx context.Context) (int64, error)
```

**实施偏差（已在本任务报告与运行手册记录）：** 共享数据形状落在 `domain` 而不是
`access` —— `access → quota` 是既有依赖方向，而 `quota` 准入服务必须能命名速率
预占命令类型，`quota → access` 会成导入环；`domain` 是三方共同依赖的唯一层。
纯规则仍在 `access/inference_limits.go`（本节最初指定的位置）。`access/ratelimit.go`
（进程内计数器）已**删除**：它是业务速率限制的重复实现（每实例一份、无法跨实例/
跨 Key 共享、且会拒绝根本进不了业务准入的请求），权威实现只剩持久速率预占。
`workers/settlement_recovery.go` 无需改动 —— 速率行的结算/释放挂在 store 的
`Settle`/`Release` 事务里，恢复 worker 的保守结算与证据释放因此自动收敛同一份
速率预占；保留期清理独立成 `workers/rate_reservation_reaper.go`。

**修正通道的接线（T4 修复轮）：** `postgres.CorrectionCommand` 增加
`CorrectedTokens *int64`（**绝对**修正后的归一化 token 总量；nil = 未知 →
token 预算保留原值，绝不记零），`Store.CorrectSettlement` 在**同一个事务**里
同时做金额修正与 token 预算修正（`correctRateTokensTx`）：修正证据带了已知
token 桶却没给总量 → 明确 `invalid_input`（金额修正不得静默漏掉 token 预算）；
速率行**缺失**（迁移前老请求 / 保留期已清理）→ 良性缺席，金额修正照常生效；
速率行**存在但畸形**（已结算请求的速率行仍是在途；只剩 Key 行没有账户行）→
明确冲突，绝不当成良性缺席。已知边界（本期不扩大）：wallet 计费请求的修正仍
被显式拒绝（`invalid_input`，micromoney 单位与钱包补偿分录未适配），因此钱包
请求的 token 预算暂不经这条通道修正；金额修正通道仍无生产调用方。

**确定语义：**

- 推理 admission 统一检查全局账户 RPM（现有 `INFERENCE_ACCOUNT_RPM`）、选中权益策略 RPM、Key RPM；均为硬上限，同时满足，不同 Key 和 `/chat` 共用账户计数。
- RPM 统计滚动 60 秒已成功提交准入的逻辑请求；每个逻辑请求计 1 次，内部 attempt 重试不重复计数。准入事务回滚不计数；准入成功后上游失败仍计 RPM。
- TPM 为账户级策略限制：滚动 60 秒内已结算的归一化 token 消耗 + 所有尚未结算的 token 预占。预占按现有输入上界与强制输出 cap；结算按真实/明确估算值校正，不能重复计算 cache/reasoning 重叠桶。
- 实际 token 超预占仍记录实际总量，不能截到预占值；超限后阻断新准入直到额度恢复。estimated 后收到真实证据只按同一请求净差额校正，不能累加 usage 各 revision；保持原结算时间作为 60 秒窗口归属，不因核对重置消费时间。原结算已在窗口外则只修正请求事实，不重新制造本分钟消费。
- 长流跨 60 秒仍保留在途预占；不能简单 TTL 释放。确认未消费的请求释放 token 预占；unknown 用量随原有 recovery/reconciliation 收敛，不能记零。
- Account/Key 计数键不包含 policy version，切套餐不能重置一分钟消费。策略 nil 不添加该限制，正整数才可设置；遵守现有全局值关闭规则，并在文档写清。
- `/v1/models` 不占推理 TPM/RPM；保留外围防滥用保护。认证失败、模型不允许、请求形状非法不进入业务准入。
- 套餐、显式 PAYG 和 allow_overage 转钱包三条路径应用同一组业务速率限制。速率拒绝用独立可识别的错误类型/错误码，不能混入触发钱包 fallback 的金额 `CodeQuotaExceeded`；不能花钱包绕过 RPM/TPM。套餐预占失败后转钱包只计一次成功准入，失败的第一轮事务不得留下速率计数；同一逻辑请求沿用同一幂等身份。
- DB 故障 fail closed；多实例时间用一致的数据库时间口径。限流拒绝为 429，DB 不可用用明确服务错误，不能伪装成用户额度耗尽。
- 能确定恢复时间时返回 Retry-After；因在途 TPM/累计 Key 预算等无法确定时不承诺某一秒必然恢复。

- [x] 写临界测试：RPM=2，两个独立 Service/DB pool 同账户并发 3 次，最多 2 次成功；不同 Key 与 `/chat`/`/v1` 合计受约束；更换策略不能绕过。
- [x] 写 TPM=100：已有 60 token 在途，再申请 50 被拒；结算实际 20 后 50 可准入；跨分钟仍在途不得放行；reported/estimated/unknown、缓存和 reasoning 重叠计数正确。
- [x] 写实际 token 超预占、estimated→reported 多次校正与幂等测试：按请求净额计一次，修正不改变原结算时刻；过期窗口校正不占当前一分钟额度。
- [x] 证明任一金额/速率/并发检查失败后整次准入无部分持久效果；进程重启、数据库短暂失败、重复结算均不漏限或双校正。
- [x] 补 PAYG/overage 测试：余额充足但 RPM/TPM 满时仍拒绝且钱包零扣款；套餐金额耗尽后转钱包成功只计一次 RPM/TPM；钱包冻结失败时整次准入回滚。套餐与钱包的终态/恢复都释放或校正同一持久 rate reservation。
- [x] 实现持久检查和生命周期接线；从 APIKeyAuth 移除重复的推理业务扣次，认证与外围 IP 防滥用仍保留。
- [x] 给新表索引、清理策略和保留期限；清理不得删掉未结算预占。记录锁顺序与单账户性能开销。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/... ./internal/router ./internal/config ./cmd/server`，包含两个独立实例的 PG 集成测试后提交。

## T5 — 接通 Key 独立并发

**Modify:** `access/apikey.go`、`httpapi/user_api_keys.go`、`quota/service.go`、`routing/account_pool.go`、`gateway/service.go`、`postgres/lease_repo.go` 及相关 DTO；复用已有 `LeaseScopeAPIKey` 与 `APIKey.ConcurrencyLimit`。
**Tests:** `access/apikey_test.go`、`httpapi/user_api_keys_test.go`、`postgres/lease_repo_test.go`、`gateway/{gateway_test,finish_order_test}.go`。
**Interfaces:** AdmissionResult 增加 Key lease，或统一为可续租/释放的请求级 lease 列表；LeaseKeeper/finalizer 同时处理账户、Key、上游 lease，不硬编码只有两把锁。

**实际签名（T5 实施后回填）：**

```go
// internal/inference/quota/service.go —— 请求级租约列表（不再硬编码「两把锁」）
type AdmissionResult struct {
    /* … */
    // Leases 是本次准入的**请求级**并发租约：计费账户租约（权益策略设了账户
    // 并发上限时）+ 客户 Key 租约（Key 自己设了 concurrency_limit 时）。
    // 列表是收尾（finalizer）的唯一事实来源：释放全部，不管有几把。
    Leases []*domain.ConcurrencyLease
}
func (r *AdmissionResult) AccountLease() *domain.ConcurrencyLease // nil = 策略未设账户并发上限
func (r *AdmissionResult) KeyLease() *domain.ConcurrencyLease     // nil = Key 未设并发上限

type Store interface {
    /* … */
    // Key 的并发上限是**数据库事实**：与 Key 的 RPM 限额同构，在准入事务内读，
    // 不信鉴权时的快照。0 = 该 Key 不设 Key 级限制（不取租约）。
    GetAPIKeyConcurrencyLimitTx(ctx context.Context, uow domain.UnitOfWork, keyID string) (int, error)
}
type Service struct { /* … */ RequestLeaseTTL time.Duration } // 账户租约与 Key 租约同一 TTL（原 AccountLeaseTTL）
// 套餐路径与钱包路径共用同一段取锁代码（并发限制不因换扣费来源而绕过）：
func (s *Service) acquireKeyLeaseTx(ctx context.Context, uow domain.UnitOfWork, req domain.Request, requestID string, at time.Time, result *AdmissionResult) error

// internal/inference/postgres/apikey_repo.go —— 管理面持久化 + 准入期读取
func (s *Store) GetAPIKeyConcurrencyLimitTx(ctx context.Context, w domain.UnitOfWork, keyID string) (int, error)
func (s *Store) UpdateAPIKey(ctx context.Context, k *domain.APIKey) error // 现在也写 concurrency_limit

// internal/inference/access/apikey.go —— 客户自管 DTO（缺省/缺席/显式 null/非正整数）
type CreateParams struct { /* … */ ConcurrencyLimit *int }  // nil = 不加 Key 级限制
type KeyPatch struct { /* … */ ConcurrencyLimit *int; ClearConcurrency bool }

// internal/inference/gateway/service.go —— 按**列表**收尾（上游 + 账户 + Key）
func (s *Service) releaseAdmission(requestID string, leases []*domain.ConcurrencyLease)
func (s *Service) releaseRequestLeases(ctx context.Context, leases []*domain.ConcurrencyLease)
func leaseIDSet(leases []*domain.ConcurrencyLease) map[string]struct{} // 按租约 ID 拆分，不按锁的数量
```

**租约作用域与收尾（不变式）**：`inference_concurrency_leases.scope` 取
`billing_account`（账户）/ `api_key`（客户 Key）/ `upstream_account`（上游账号）；
三把（若都有）在**同一个准入/派发事务**里按同一把锁序取得，**先 finalize
（结算落账、请求终态化）再释放租约**，panic 也必须释放。取不到就整笔回滚，
不留「已预占但未上锁」的半状态。

- [x] 对 Key create/patch/read 开放 `concurrency_limit`：缺省不增加 Key 限制、PATCH 缺席不修改、显式 null 清除、非正整数拒绝；保留 owner 检查。
- [x] 写同 Key 上限 1 的并发拒绝、不同 Key 互不占 Key 槽但仍受账户总上限、Key 无限制三种情况。
- [x] 写第一把租约成功第二把失败的事务回滚；流式/非流式、取消、panic、fencing、失败切换和恢复均无租约泄漏。
- [x] 保留 `TestStreamBodyFinish_FinalizeBeforeLeaseRelease`、`TestStreamBodyFinish_PanicStillReleasesLeases`。
- [x] 更新 OpenAPI，运行 `go test -race -count=1 -p 1 ./internal/inference/...` 后提交。

## T6 — 统一协议错误与 Retry-After

**Modify:** `internal/router/router.go`、`internal/middleware/ratelimit.go`（如需可注入响应器）、`internal/inference/httpapi/{apikey_auth,chat_completions,messages,responses}.go`、`internal/service/chat_facade.go`。
**Tests:** `internal/router/router_test.go`、`httpapi/{protocol_error_edges_test,apikey_auth_edges_test,v1_protocol_test}.go`。
**Interfaces:** 错误按实际入口协议渲染；复用已有 native error helper，通用 middleware 不能将管理 envelope 强塞给 `/v1`。

**实际签名（T6 实施后回填）：**

```go
// internal/middleware/ratelimit.go —— 限流拒绝的**可注入响应器**
// 通用中间件只管「谁被限了」，**形状由响应器决定**：管理面用管理 envelope，
// /v1 协议面用协议原生形状（设计 §9.1：通用中间件不得把管理 envelope 强塞给 /v1）。
type RateLimitResponder func(c *gin.Context, retryAfter string)
func ManagementRateLimitResponder(c *gin.Context, retryAfter string) // 默认：{code,error,message}
func RateLimitWithResponder(ctx context.Context, r float64, burst int, respond RateLimitResponder) gin.HandlerFunc
func RateLimitWithKeyResponder(ctx context.Context, r float64, burst int, key func(*gin.Context) string, respond RateLimitResponder) gin.HandlerFunc
// RateLimit / RateLimitWithKey = 上两者的默认组合（管理 envelope）。

// internal/inference/httpapi/apikey_auth.go —— 复用既有 native helper
func v1Error(c *gin.Context, status int, typ, code, message string)     // OpenAI：{"error":{message,type,code}}
func anthropicError(c *gin.Context, status int, typ, message string)   // Anthropic：{"type":"error","error":{type,message}}
func isAnthropicV1Path(path string) bool                              // /v1/messages → Anthropic 形状
func V1RateLimitResponder(c *gin.Context, retryAfter string)           // 外围每 IP 桶的协议原生拒绝

// internal/router/router.go —— /v1 group 挂协议原生的外围限流器
v1 := engine.Group("/v1",
    middleware.RateLimitWithResponder(ctx, 60, 120, httpapi.V1RateLimitResponder),
    accessOps.V1Auth)

// internal/inference/httpapi/chat_completions.go —— Retry-After 的可信性规则
// 只有**每个**阻断条件都有可确定恢复时刻时才给倒计时；Key 的累计预算
// （budget_micros，不按周期清零）是永久阻断项 → 与窗口同时阻断时不给。
func quotaExceededDetails(qe *domain.QuotaExceededError) (message string, retryAfterSecs int)

// internal/inference/quota/service.go —— 把「永久阻断项」一并报齐
type Store interface {
    /* … */
    // Key 的累计预算是**数据库事实**（与 Key 的 RPM/并发限额同构，准入事务内读）。
    // 它是准入的**永久阻断探针**：预算不按周期清零，窗口重置永远解不开它 ——
    // API 层必须先知道这一点，才不会为一个注定失败的请求编造倒计时。
    GetAPIKeyBudgetTx(ctx context.Context, uow domain.UnitOfWork, keyID string) (limit *domain.Microcredit, used domain.Microcredit, err error)
}
func (s *Service) withKeyBudgetBlock(ctx context.Context, uow domain.UnitOfWork, cmd AdmitCommand, hold domain.Microcredit, err error) error

// internal/service/errors.go —— /chat 的本地限制哨兵（保留既有 envelope）
var ErrChatRequestLimited = errors.New("chat request rejected by a usage limit")
type ChatLimitError struct{ Code string } // rate_limited / insufficient_capacity / quota_exceeded / insufficient_balance
func (e *ChatLimitError) Error() string
func (e *ChatLimitError) Unwrap() []error // {ErrChatRequestLimited, ErrChatRateLimited}：准确语义 + 既有 429 退避路径不变
```

- [x] 三个 `/v1` 推理入口分别测试外围 IP 429、业务 RPM/TPM 429、并发/金额配额 429；OpenAI 为 error 对象，Anthropic 为其原生结构，Content-Type 正确，零上游调用。
- [x] `/chat` 保留既有 envelope，新增限制映射为准确的业务码/说明，不能全部降为 generic 502。
- [x] 修复永久 Key 预算耗尽与窗口同时阻断时的恢复时间表达；只有全部阻断条件可确定时才提供可信的重试时刻。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/middleware ./internal/router ./internal/inference/httpapi ./internal/service` 后提交。

## T7 — 配置行为收口与连接超时

**Modify:** `catalog/service.go`、`catalog/revision.go`、`management/{bulk_import,quota_policies}.go`、`routing/service.go`、`providers/adapter.go`；必要的配置能力说明。
**Tests:** `catalog/validate_edges_test.go`、`management/{bulk_import_test,quota_policies_test}.go`、`providers/adapter_dispatch_test.go`。

- [x] 所有新建/修改/批量导入/环境导入/正式 publish 使用一致支持集：常规池仅支持 round_robin；least_loaded、通用 session_sticky 明确拒绝，不能保存后假装生效。Responses 的 previous_response_id 专用绑定继续可用。
- [x] 对存量已发布的不支持 pool_strategy：旧快照可读，路由标记不可用并告警，不能静默当 RR；若无其他候选则在预占/派发前返回明确配置错误。发布预演指出对象 ID 和替代配置。
- [x] `weight` 保留“同 priority 下的候选排序”语义，OpenAPI、代码注释和说明一致；本期不实现概率分流。
- [x] 新写策略拒绝未实现的 `clamp_if_declared`；旧数据可查询，引用该模式的新推理明确报策略配置不可用，不静默当 reject。文档给出改为 reject/allow_overage 的人工迁移说明，不自动改旧策略。
- [x] 让 deployment `ConnectTimeout` 真正控制建连阶段；连接已建立的 SSE 不被它当总超时截断。复用可缓存 transport，不能每请求创建泄漏连接池；保留 DNS/IP 校验。
- [x] httptest/可控 dialer 测试不同部署连接超时、长流、请求总超时和未知执行结果不盲重放。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/catalog ./internal/inference/management ./internal/inference/providers/... ./internal/inference/routing ./internal/inference/gateway` 后提交。

## T8 — 能力路由与协议真实性

**Modify:** `domain/model.go`（AdapterCapabilities）、`routing/service.go`（Needs）、`gateway/service.go`、`providers/{adapter,openai_chat,anthropic,openai_responses,responses_client,messages_client}.go`、`httpapi/{chat_completions,kaya_models}.go`。
**Tests:** `providers/{client_surface_matrix_test,multimodal_guard_test,responses_client_test,messages_client_test}.go`、`gateway/sticky_candidates_test.go`、`httpapi/protocol_surfaces_test.go`。
**Interfaces:** `Needs` 和 adapter 能力增加图片支持判断；模型声明、路由能力、adapter 支持三层一致。reasoning effort 在中间请求显式建模，不能只折成 `ThinkingEnabled`。

- [x] 写 `/chat` 带图、第一条为不支持图片的 Responses deployment、第二条为可用 Chat/Anthropic deployment 的测试：只选兼容候选；完全无兼容候选时出站前明确拒绝；文本仍能走原候选。
- [x] 保留现有图片大小/数量检查、不由本服务抓取远程 image URL、审计不写 base64。核对图片 token 估算标识，不宣称固定 1105 token 对所有模型是数学硬上界；记录未解决的供应商计量边界。
- [x] 三个 `/v1` 入口继续明确拒绝图片/文件/音频及服务端工具，测试原生 400、零预占、零派发；不要仅因上游支持就在入口无计量保护地开放。
- [x] 写 reasoning effort low/medium/high/xhigh 的参数保持或明确拒绝测试，不能一律 medium；adapter 无能力的值不静默忽略。
- [x] 对无法忠实重放的非空 Responses reasoning/encrypted 内容和 Anthropic signed/redacted thinking 历史给出明确 4xx，而非接受后丢弃。同步标明这是对旧“接受但丢弃”行为的兼容性修正。
- [x] 检查本网关自己生成的 Messages thinking 输出：不能伪造空 signature 冒充可重放原生签名思考；在没有无损支持前，对相应请求明确不支持，并保持普通文本/工具流可用。
- [x] 更新能力矩阵：按“客户端入口 × 上游协议 × 文本/工具/图片/思考回放”列明 supported/rejected；包含 SSE 完整结束、半截 EOF、工具多轮和 previous_response_id 归属隔离测试。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/providers/... ./internal/inference/httpapi ./internal/inference/gateway` 后提交。

## T9 — 探测、能力描述与运维可见性

**Modify:** `management/probe.go`、`management/catalog.go`、`httpapi/admin_models.go`、`httpapi/kaya_models.go`、相关测试和 API/runbook。
**Interfaces:** 复用 T1 的出站凭据解析与 adapter 鉴权；保持 probe 端点只读语义，默认不触发付费推理。正式 publish 不自动引入网络依赖。

- [x] deployment probe 按协议设置鉴权/版本头，支持供应商未提供 `/models` 时准确返回“此探测方式不支持/无法确认”，不能伪装模型故障或推理成功。
- [x] 区分 credential test（材料可解密）、connectivity probe（指定探测成功）和 inference verified（本次并未做真实推理）。旧字段能兼容则只追加描述字段。
- [x] 发布 dry-run 汇总 disabled provider、无账号、策略/路由不支持等可判定问题，标明哪些阻断正式配置发布、哪些只是网络探测提示；HTTP 返回 200 不等于每个 probe.ok 为 true。
- [x] `/chat/models` 在既有字段基础上提供准确的图片/工具/思考/上下文与输出上限描述（只追加字段）；不能声明超过当前入口/adapter 能力。`/v1/models` 保持原生基本形状。
- [x] 明确运行告警：非幂等结算冲突、未知用量积压、限速存储失败、不支持存量配置。复用现有日志/监控方式，不在本任务引入新监控平台。
- [x] 更新 `docs/api/kaya-coding-plan.openapi.yaml`、`docs/api-integration-guide.md`、`docs/runbooks/kaya-coding-plan-operations.md`，纠正本次涉及的旧待办和错误说明。
- [x] 运行 `go test -race -count=1 -p 1 ./internal/inference/management ./internal/inference/httpapi ./internal/config ./cmd/server` 后提交。

## T10 — 集成验收与实施报告

**Create/Modify:** `tests/integration/` 中 gateway 集成用例、`docs/runbooks/2026-11-05-model-gateway-verification.md`；测试应复用已有 fixture，避免重复搭整套服务器。

- [x] 真实临时 PG + 模拟上游跑 `/chat`、Chat Completions、Messages、Responses：成功、拒绝、断流、中断、重试、恢复均核对 request/attempt/usage/ledger/holds/leases。
- [x] 两独立 Service + 两 DB pool 验证共享 RPM/TPM/并发，不能只用同一进程单个 counter 的 goroutine 测试替代。
- [x] 保留现有重点用例：`TestExecutedUnknown_NoBlindReplay`、`TestFailoverOn429_SettlesOnce`、`TestSettleOverHold_OverageJobAndRealCharge`、`TestZeroUsageSettle_ChargeRowDistinguishable`、`TestListStaleOpenRequests_LiveLeaseGuard`、`TestCorrectSettlement*`。
- [x] 跑下方完整检查并记录退出码、测试 skip、迁移幂等结果；任何新增失败均修复，不用“原有功能”掩盖。
- [x] 核对工作区只有本阶段范围文件，更新任务复选框、实际新接口/表名、兼容性变更、存量不支持配置处理方法。
- [x] 最终报告明确：完成项、未完成项及原因、测试证据、上线前置、回滚方式；本地模拟通过不能称真实 Claude Code/Codex 或真实上游已验收。

```bash
go build ./...
go vet ./internal/... ./cmd/...
go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...
go test -race -count=1 -p 1 ./tests/e2e/...
git diff --check
```

## 1.1 实际交付（T10 验收后回填：真实接口/表名、兼容性、存量不支持配置）

本节是 T0–T10 实施结果的**唯一事实索引**：计划正文写的是「意图」，这里写的是
**真实落地的东西**。逐条证据见
`docs/runbooks/2026-11-05-model-gateway-verification.md`（T1 §4.1–4.8、
T2 §4.9–4.16、T3 §4.17–4.25、T4 §4.26–4.33、T5 §4.34–4.41、T6 §4.24–4.31、
T7 §4.25–4.31 与 §4.42、T8 §4.32–4.39、T9 §4.40–4.50、T10 §4.51–4.58）。

### 1.1.1 新增/变更的表与列（迁移只追加）

| 迁移 | 表/列 | 用途 |
|---|---|---|
| `050_inference_request_expected_hold.sql` | `inference_requests.expected_hold_micros` | 准入定价的**可恢复估算上界**（无金额窗口的策略用它承载消费事实；`NULL` = 迁移前老行，证据不足） |
| `051_inference_reconciliation_reasons.sql` | `inference_reconciliation_jobs.reason` 词表扩容 | `unknown_usage` / `crash_recovery` / `ledger_mismatch` / `manual` / `settlement_overage` / `missing_reservation` / `late_usage_after_release` / `settlement_mismatch` / `insufficient_evidence` |
| `052_inference_rate_reservations.sql` | **`inference_rate_reservations`**（新表） | 账户/Key 级 RPM 计数 + 账户级 TPM 预占/结算；`scope ∈ {account,key}`、`UNIQUE(request_id, scope)`、`state ∈ {reserved,settled,released}` |
| `053_inference_route_pool_strategy_comments.sql` | 仅列注释 | `pool_strategy` / `weight` / `overage_policy` 的语义注释（无 DDL） |

既有表被复用的关键列：`inference_requests.{status,usage_status,settled_micros,expected_hold_micros,charge_source}`、
`inference_attempts.{status,error_kind,cost_micros,cost_basis}`、
`inference_usage_records.{source,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens}`（缺失即 `NULL`，`NULL ≠ 0`）、
`inference_ledger_entries.{entry_type,amount_micros}`、
`inference_reservations.{state,target_kind}`、
`inference_concurrency_leases.{scope,state,fencing_token}`（`scope ∈ {billing_account,api_key,upstream_account,deployment}`）、
`inference_quota_windows.{used_micros,reserved_micros,state}`、
`inference_reconciliation_jobs.{reason,status,deadline_at}`。

### 1.1.2 实际对外接口（新增/变更）

- **HTTP**：`POST /chat`（Kaya 面，走网关 facade，协议标记 `kaya_chat`）、`POST /v1/chat/completions`、`POST /v1/messages`、`POST /v1/responses`；`GET /chat/models` 追加能力字段（`supports_images` / `supports_tools` / `supports_thinking` / `context_tokens` / `max_output_tokens` / `capability_description`），`GET /v1/models` 形状不变。
- **错误码**：`429 rate_limited`（业务 RPM/TPM）、`429 insufficient_capacity`（并发/容量）、`429 quota_exceeded`（金额窗口/Key 预算）、`403 model_not_allowed`、`404 model_not_found`、`400 invalid_input`（能力不兼容）、`503 service_unavailable`（准入存储不可用）、`503 upstream_unavailable`、`503 chat configuration unavailable`。`/v1` 用**协议原生**形状（OpenAI `{"error":{message,type,code}}` / Anthropic `{"type":"error","error":{type,message}}`），`/chat` 保留 `{code,data,message}` envelope。
- **管理面**：Key `concurrency_limit`（create/read/patch，缺省不加限、PATCH 缺席不改、显式 null 清除、非正整数拒绝）；`budget_micros` **仍然只表示套餐 microcredit 子预算**，本期未变成现金预算、也未新增钱包 Key 金额产品。
- **Go API（跨包）**：`domain.RateLimits` / `RateReserveCommand` / `RateUsage` / `RateBlock` / `RateLimitError`；`domain.SettleError` + `SettleOutcomeKind`（duplicate / released / charge_mismatch / ledger_drift / missing_reservation / reservation_drift）；`domain.RecoverableEstimate`；`domain.AdmissionResult.Leases`（账户 + Key 租约列表）；`Store.ReserveRateTx` / `CorrectRateTokensTx` / `PurgeRateReservations` / `GetAPIKeyConcurrencyLimitTx` / `GetAPIKeyBudgetTx`；`credentials.OutboundAuth` + `ResolveOutboundAuth`（oauth → 仅 access token）。

### 1.1.3 兼容性变更（对调用方可见）

1. **403/404/400 的错误形状**按入口协议渲染（`/v1` 不再套管理 envelope）。
2. **速率拒绝从「每实例 120/分钟」变成「每账户 60 秒滚动窗口」**（跨实例、跨 Key、跨 `/chat` 与 `/v1` 共享）；准入前被拒（形状非法、模型无权）不再消耗速率令牌。
3. **准入存储不可用从「假装额度耗尽」变成显式 503 `service_unavailable`**。
4. **`Retry-After` 只在每个阻断约束都有可确定恢复时刻时出现**；Key 的累计预算（`budget_micros`）不按周期清零 → 与窗口同时阻断时**不给**倒计时。
5. **上游 OAuth 只发 access token**（不再发整包 JSON / refresh token）；Anthropic 协议用 `x-api-key`，OAuth 用 `Authorization: Bearer`。
6. **能力拒绝从「接受后丢弃」改成明确 4xx**：非空 Responses `reasoning`/`encrypted` 内容、Anthropic signed/redacted thinking 历史现在被**明确拒绝**（这是对旧行为的兼容性修正，旧客户端会开始收到 400）。
7. **接口探针结果追加字段**（`credential_state` / `probe_method` / `probe_outcome` / `probe_supported` / `inference_verified` / `description`），**旧字段一个未删**；发布预演追加 `issues` / `summary` 等，旧字段保留。

### 1.1.4 存量不支持配置的处理方法（不静默、不自动改历史）

| 存量配置 | 现在怎么读 | 怎么拒 | 人工迁移动作 |
|---|---|---|---|
| 已发布路由的 `pool_strategy` ∉ {`round_robin`}（`least_loaded`、通用 `session_sticky`） | 旧快照**可读**；该路由被标记为不可调度并告警 | 该模型**全部**路由都不支持时，在**预占/派发前**返回明确配置错误（`503 upstream_unavailable` + 配置文案） | 改成 `round_robin` 后**重新 publish**；Responses 的 `previous_response_id` 专用绑定不受影响 |
| 策略 `overage_policy = clamp_if_declared` | 旧策略**仍可查询** | 引用该模式的新推理明确报**策略配置不可用**（`503 service_unavailable`，文案含策略名与 revision）——**不静默当 `reject`** | 发布新 revision，显式选 `reject` 或 `allow_overage`；**不自动改写存量策略** |
| 迁移 050 之前、**没有** `expected_hold_micros` 的请求行 | 行保留、可查 | 恢复 worker **不按 0 记零放行**：停放 `insufficient_evidence` 等人工处置 | 人工核对真实用量后走既有核对流程；**不自动重算历史、不猜测补扣** |
| `weight` | 保留「同 `priority` 下的候选排序」语义 | 不适用 | 无需迁移；本期**不实现**按权重比例分流 |

### 1.1.5 本期明确**不做**（后续边界）

Token Plan 商品/权益与统一商品发布、真实渠道退款与改签、官网/Kaya UI、完整 `/v1`
多模态、签名思考与 encrypted reasoning 的无损回放、通用 least-loaded/session-sticky
与按权重比例分流、跨实例上游冷却与供应商刷新竞争治理、生产发布与灰度。

## 2. 一次性 PostgreSQL 操作参考

仅在临时目录运行。下面端口必须先确认未被占用；启动失败就换一个未占用端口，不连接到占用它的服务。不将 DSN 写入仓库。若本机缺 PG，可使用一次性容器，要求相同隔离与清理。整段在一个子 shell 执行；任一步失败即退出，成功启动后用 trap 停止自己创建的实例，不接管已有数据库。需要保留临时库排查时，记录临时目录后自行重新启动它。

```bash
(
set -euo pipefail
cd /Users/jack-tian/Downloads/github/yunhou-users-model-gateway
TEST_PG_BIN="$(pg_config --bindir)"
TEST_PG_ROOT="$(mktemp -d /private/tmp/yunhou-gw-pg.XXXXXX)"
TEST_PG_PORT=55479
mkdir -p "$TEST_PG_ROOT/socket"
"$TEST_PG_BIN/initdb" -D "$TEST_PG_ROOT/data" -U postgres -A trust --no-locale
"$TEST_PG_BIN/pg_ctl" -D "$TEST_PG_ROOT/data" -l "$TEST_PG_ROOT/server.log" \
  -o "-h 127.0.0.1 -p $TEST_PG_PORT -k $TEST_PG_ROOT/socket" -w start
trap '"$TEST_PG_BIN/pg_ctl" -D "$TEST_PG_ROOT/data" -m fast -w stop' EXIT
"$TEST_PG_BIN/createdb" -h 127.0.0.1 -p "$TEST_PG_PORT" -U postgres yunhou_gateway_test
export DATABASE_URL="postgres://postgres@127.0.0.1:${TEST_PG_PORT}/yunhou_gateway_test?sslmode=disable"
export E2E_DATABASE_URL="$DATABASE_URL"
go run ./cmd/migrate
go run ./cmd/migrate
go run ./cmd/migrate -status
# T0 基线；T10 时在相同位置替换为完整验收命令。
# 不能与其他 Agent 共享这一个测试库。
go test -race -count=1 -p 1 ./internal/inference/...
)
```

## 3. 完成定义与后续边界

全部 T0–T10 验收完成才可称“本阶段 Users 网关完善完成”。以下仍需后续项目，不能在报告中顺带标成完成：

- Token Plan 的 product_code/benefit/upgrade 管理、统一商品发布、订阅人工修订与订单权益同步、待支付订单的策略退役保护。
- 真实渠道退款、Paddle/PayPal Coding Plan 改签及 outbox 售后工作台。
- 官网购买、用户 Key/额度/钱包页面，Kaya capability DTO 和内置图片 UI、Agent 一键配置。
- `/v1` 图片/文件/音视频、完整签名思考与 encrypted reasoning 无损回放、原生客户端版本验收。
- 通用 least-loaded/session-sticky、按权重比例分流、跨实例上游冷却与供应商刷新竞争治理。
- 生产 `/v1` Nginx 长流配置、迁移执行、Lifecycle enforce 开启、真实账号与生产灰度。

实施期间发现这些边界内的高风险问题，记录证据和建议的后续任务；如果它确实阻断当前任务正确性，先做最小必要修复并说明范围，不自行扩成整个平台改造。
