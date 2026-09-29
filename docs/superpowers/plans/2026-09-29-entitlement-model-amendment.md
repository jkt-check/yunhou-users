# 存量订阅权益增补 admin 面（entitlement-model-amendment）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 operator 提供授权 + 审计的 admin API 完成存量权益 model_ids 增补（模型发布收尾环节），全程无 SQL。

**Architecture:** 复用既有骨架，零新表零新迁移：postgres repo 增加三个读/写方法（GetEntitlementTx / ListEntitlements / ListEntitlementsForAmend）；业务逻辑放 `management.EntitlementAdminService`（R1 单权益修订、R2 批量增补、R3 只读）；HTTP 薄层 `AdminEntitlementsHandler` 挂进 `AdminOps`，写面 `billing:adjust`、读面 `usage:read`；审计复用 `management.AuditEvent` → `inference_audit_log`（与变更同事务，RecordTx）。

**Tech Stack:** Go 1.25 / gin / sqlx / pgx 语义(lib/pq) / 既有 domain 错误码映射（fail/ok）。

**Spec:** `/Users/jack-tian/Downloads/github/yunhou-deploy/docs/entitlement-model-amendment-requirement.md`（deploy 侧 PR #345）。实现不得违反其 §5 七条红线；验收为 AC1–AC8（deploy 侧 staging 独立执行）。

## Global Constraints

- 显式集合：`model_ids` 空数组 = 无任何模型，永不引入 NULL/省略 = 全放行写法（红线 1）。
- 消费主体连续：只改 model_ids；ID/anchor/effective range 不动，used/reserved 窗口不清零（红线 2，由 `ReviseEntitlementTx` 既有 SQL 保证，不得改该 SQL）。
- 仅 active 可修订：非 active → 409 CodeConflict；绝不复活退役权益（红线 3）。
- 乐观锁：stale revision → 409，不得强制覆盖（红线 4）。
- 生效即时：不得引入权益缓存（红线 5）。
- 0 价模型兼容（红线 7）：增补不查价格/配价状态。
- 审计 fail-closed：缺 tx 能力的 store/audit → CodeInternal 拒绝变更（对齐 `AdminAuthHandler.grantAuditSupport`，admin_auth.go:230）。
- 注释用中文，口径与相邻文件一致；commit 信息中文（见 git log 风格）。
- 测试命令：`DATABASE_URL=postgres://localhost/yunhou_users?sslmode=disable ~/sdk/go/bin/go test -race -p 1 ./internal/... ./cmd/... ./tests/integration/...`（已知 `TestDrill_DatabaseBriefOutage` 本地必败 = 既有环境问题，master 同样败，不算回归）。
- 不得自行 merge；CI 绿 + 评审零发现后报告用户。

## 关键既有事实（实现者无需再查）

- `access.Upgrade(ent *domain.Entitlement, newPolicyVersionID string, newModelIDs []string) (domain.EntitlementPatch, error)` — access/entitlement.go:230。非 active→CodeConflict；no-op→CodeInvalidInput（R1 须在调用前自行短路幂等，不调 Upgrade）。
- `Store.Begin(ctx) (domain.UnitOfWork, error)` — postgres/postgres.go:56。
- `Store.ReviseEntitlementTx(ctx, w, id, patch) (*domain.Entitlement, error)` — entitlement_repo.go:263，乐观锁 `WHERE id AND revision AND status='active'`，stale→CodeConflict；退役策略版本守卫已刻意豁免（注释 :258-262）。
- `Store.GetEntitlement(ctx, id)` — entitlement_repo.go:410（非 tx；R1 需要 tx 内读版本，避免「持 tx 期间用 s.db 第二连接读」死锁，先例 getLatestEntitlementBySourceTx:245）。
- 审计：`management.AuditEvent{Action, ObjectType, ObjectID, Reason, ActorUser, ActorApp, Detail}`；`Store.RecordTx(ctx, w, ev)`（operators_repo.go:119）；`management.SanitizeDetail` 必调。
- HTTP 帮手（admin_models.go）：`ok(c, data)` :344、`fail(c, err)` :348（domain 码→HTTP 映射含 409/400/404）、`strictBindJSON`（拒未知字段，admin_auth.go:182）、`requiredReason(c, s)` :403、`parseLimit(c, def)` :423（上限 500）、`OperatorOf(c) credentials.Operator{UserID, AppID}`（admin_auth.go:170）。
- 挂载：`AdminOps` 结构体 admin_ops.go:12，`Mount(g)` :57；`RequireBilling`/`RequireUsage` 字段已存在；main.go:337 构造。
- 权限常量：`management.PermBillingAdjust="billing:adjust"`、`management.PermUsageRead="usage:read"`（operators.go）。
- N6 errors[] 先例（cmd/inference-entitlement-backfill/main.go:104）：`rowError{user_id, reason}`、行级失败不置整体失败。
- 权益来源→套餐：`inference_entitlements.source_type='subscription'` 时 `source_id = subscriptions.id`，按套餐过滤走 `source_id IN (SELECT id FROM subscriptions WHERE plan_id=$1)`。

---

### Task 1: postgres repo 三个方法 + DB 测试

**Files:**
- Modify: `internal/inference/postgres/entitlement_repo.go`（尾部追加）
- Test: `internal/inference/postgres/entitlement_admin_repo_test.go`（新建）

**Interfaces:**
- Produces（Task 2 的 store 接口直接消费这三个签名）:
  - `func (s *Store) GetEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string) (*domain.Entitlement, error)` — tx 内按 id 读；不存在 → CodeNotFound。
  - `type EntitlementListFilter struct { BillingAccountID string; Status string; Limit, Offset int }`
  - `func (s *Store) ListEntitlements(ctx context.Context, f EntitlementListFilter) ([]domain.Entitlement, error)` — 可选过滤 billing_account_id / status（空=不过滤），`ORDER BY created_at DESC, id`，LIMIT/OFFSET。
  - `func (s *Store) ListEntitlementsForAmend(ctx context.Context, selectorKey, selectorVal string) ([]domain.Entitlement, error)` — selectorKey="all_active" → `WHERE status='active'`；selectorKey="source_plan" → `WHERE source_type='subscription' AND source_id IN (SELECT id FROM subscriptions WHERE plan_id=$1)`（**不限 status**，非 active 行留给服务层计入 skipped，AC6）。`ORDER BY id` 保证批处理确定性。selectorKey 非法 → CodeInvalidInput（防御，服务层已校验）。

- [ ] **Step 1: 写失败测试** `entitlement_admin_repo_test.go`：复用同目录既有 DB 测试的 fixture 模式（参考 entitlement_repo 既有测试文件的 setup：建 billing account + entitlement 行）。
  - TestGetEntitlementTx_Found / NotFound(CodeNotFound)
  - TestListEntitlements_Filters：造 3 行（两账户 × 不同 status），断言 billing_account_id 过滤、status 过滤、limit/offset 分页、空过滤全量。
  - TestListEntitlementsForAmend：造 subscription 来源行两条（不同 plan）+ 一条 revoked；all_active 只回 active；source_plan 回该 plan 全部状态（含 revoked）；非法 key → CodeInvalidInput。
- [ ] **Step 2: 跑测试确认编译失败**（方法不存在）。
  `DATABASE_URL=postgres://localhost/yunhou_users?sslmode=disable ~/sdk/go/bin/go test -race -p 1 ./internal/inference/postgres/ -run 'EntitlementTx|ListEntitlements' -v`
- [ ] **Step 3: 实现三个方法**（SQL 照既有 entitlementRow 扫描模式；GetEntitlementTx 内 `sqlTx(w)` 解包，同 ReviseEntitlementTx:264）。
- [ ] **Step 4: 跑测试全绿**（同上命令，去掉 -run 过滤跑整个 postgres 包）。
- [ ] **Step 5: Commit** `feat(inference): 权益 admin 面 repo 读/写方法（GetEntitlementTx/ListEntitlements/ListEntitlementsForAmend）`

### Task 2: management EntitlementAdminService + 单测

**Files:**
- Create: `internal/inference/management/entitlement_admin.go`
- Test: `internal/inference/management/entitlement_admin_test.go`

**Interfaces:**
- Consumes: Task 1 三个 repo 方法 + `Store.Begin` + `access.Upgrade` + `AuditTxRecorder`。
- Produces（Task 3 handler 消费）:
  ```go
  type EntitlementAdminStore interface {
      Begin(ctx context.Context) (domain.UnitOfWork, error)
      GetEntitlement(ctx context.Context, id string) (*domain.Entitlement, error)
      GetEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string) (*domain.Entitlement, error)
      ReviseEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error)
      ListEntitlements(ctx context.Context, f inferencepostgres.EntitlementListFilter) ([]domain.Entitlement, error)
      ListEntitlementsForAmend(ctx context.Context, key, val string) ([]domain.Entitlement, error)
  }
  ```
  （注意：filter 类型定义放 management 包，postgres 方法签名改收该类型——避免 management→postgres 反向依赖；Task 1 实现时把 `EntitlementListFilter` 定义在 management/entitlement_admin.go，repo 方法 import management 已有先例：operators_repo.go 即如此。）
  ```go
  type Operator struct{ UserID, AppID string } // 若 credentials.Operator 可 import 则复用，否则本地定义
  type ReviseInput struct{ AddModelIDs, RemoveModelIDs []string; Reason string }
  type ReviseResult struct{ Changed bool; Entitlement *domain.Entitlement }
  func (s *EntitlementAdminService) Revise(ctx context.Context, op Operator, id string, in ReviseInput) (*ReviseResult, error)
  type AmendRequest struct{ ModelID, Action, Selector, Reason string; DryRun bool; SampleN int }
  type AmendRowError struct{ EntitlementID, BillingAccountID, Reason string }
  type AmendSample struct{ EntitlementID, BillingAccountID string; Before, After []string; Note string }
  type AmendReport struct{ DryRun bool; ModelID, Action, Selector string; Scanned, Amended, Skipped, Conflicts int; Errors []AmendRowError; Samples []AmendSample }
  func (s *EntitlementAdminService) AmendModels(ctx context.Context, op Operator, in AmendRequest) (*AmendReport, error)
  ```
- 语义钉死：
  - Revise：Begin → GetEntitlementTx → 计算新集合 `newSet = (current ∪ add) \ remove`（dedupe，保序：既有顺序 + add 追加序）→ **集合无变化则短路返回 Changed=false**（不调 Upgrade、不写库、不写审计、Rollback）→ `access.Upgrade(ent, ent.PolicyVersionID, newSet)` → ReviseEntitlementTx → RecordTx(Action="entitlement.revise", ObjectType="entitlement", Detail 含 before/after model_ids、policy_version_id、before/after revision) → Commit。audit 缺 tx 能力 → fail-closed（grantAuditSupport 模式）。
  - 校验：add/remove 全空 → CodeInvalidInput；add∩remove 非空 → CodeInvalidInput；reason 空由 handler 层 requiredReason 把关，服务层再防御一次。
  - AmendModels：selector 解析（"all_active" / "source_plan:<plan_id>"，其余 → CodeInvalidInput）；action ∈ {add, remove}；ListEntitlementsForAmend 取候选；dry_run=true → 纯读，逐行计算 would_amend/would_skip（已含/已不含、非 active），Samples 取前 SampleN（默认 10，上限 50）；dry_run=false → 逐行独立事务（Revise 同路径，action 语义化为 add=[model]/remove=[model]），CodeConflict 计入 Conflicts（不进 Errors），其余行错误进 Errors[]，整体始终返回 200 语义（错误只在 report 内）。审计每 amended 行一条 Action="entitlement.amend"，Detail 附 selector/action/model_id。
  - remove 至空集合法（空集=无模型，红线 1），不特殊拦截。

- [ ] **Step 1: 写失败单测**（fake in-memory store 实现 EntitlementAdminStore + fake AuditTxRecorder；参考 admin_auth_test.go 的 stubRecorder 与 management 既有测试风格）：
  - Revise 幂等：已含 add 目标 → Changed=false、无 audit、无写；
  - Revise 正常路径：集合合并正确、audit detail 前后值完整、revision 传递 = ent.Revision；
  - Revise 非 active → CodeConflict（Upgrade 守卫）；
  - Revise add∩remove → CodeInvalidInput；全空 → CodeInvalidInput；
  - Revise stale（fake 返回 CodeConflict）透传；
  - Revise 审计写失败 → 整体 error 且 fake store 无提交（rollback 被调）；
  - Amend dry_run：不写库（fake store 断言 Begin 未被调）、计数与 samples 正确、非 active 行计入 skipped；
  - Amend 正式：amended/skipped/conflicts 分流正确（造一行 fake 返 CodeConflict）、errors[] 收集行错误、每行独立事务（fake 记录 Begin 次数 = 变更行数）、审计行数 = amended；
  - Amend 非法 selector/action → CodeInvalidInput。
- [ ] **Step 2: 跑测试确认失败** `~/sdk/go/bin/go test ./internal/inference/management/ -run 'EntitlementAdmin|Amend' -v`
- [ ] **Step 3: 实现 entitlement_admin.go**。
- [ ] **Step 4: 测试全绿**（整个 management 包）。
- [ ] **Step 5: Commit** `feat(inference): 权益增补服务——单权益修订 + 批量增补（dry_run/errors[] 对齐 N6）`

### Task 3: httpapi AdminEntitlementsHandler + 挂载 + handler 测试

**Files:**
- Create: `internal/inference/httpapi/admin_entitlements.go`
- Test: `internal/inference/httpapi/admin_entitlements_test.go`
- Modify: `internal/inference/httpapi/admin_ops.go`（AdminOps 加 `Entitlements *AdminEntitlementsHandler` 字段 + Mount 两分支）

**Interfaces:**
- Consumes: Task 2 服务；ok/fail/strictBindJSON/requiredReason/parseLimit/OperatorOf。
- Produces（Task 4 接线 + deploy 验收的 wire 契约）:
  - `GET /admin/entitlements/:id` → 200 `{code:0,data:{entitlement}}`；404 CodeNotFound
  - `GET /admin/entitlements?billing_account_id=&status=&limit=&offset=` → `{entitlements:[...]}`；limit 默认 50、上限 500（parseLimit）；status 非法值 → 400
  - `POST /admin/entitlements/:id/revise` body `{add_model_ids?, remove_model_ids?, reason}` → `{changed, entitlement}`；幂等 200 changed=false；非 active/乐观锁 → 409
  - `POST /admin/entitlements/amend-models` body `{model_id, action, selector, reason, dry_run?, sample?}` → `{report}`（dry_run 默认 true）
  - entitlement DTO 字段：id, billing_account_id, source_type, source_id, model_ids, policy_version_id, anchor_at, effective_from, effective_to, revision, stackable, status, created_at, updated_at
  - 权限：写两端点挂 RequireBilling（billing:adjust），读两端点挂 RequireUsage（usage:read）。
- Mount 追加：
  ```go
  if o.Entitlements != nil && o.RequireBilling != nil {
      o.Entitlements.RegisterWrite(g.Group("", o.RequireBilling))
  }
  if o.Entitlements != nil && o.RequireUsage != nil {
      o.Entitlements.RegisterRead(g.Group("", o.RequireUsage))
  }
  ```

- [ ] **Step 1: 写失败 handler 测试**（httptest + fake service；权限矩阵参考 admin_price_versions_test.go 的 TestAdminPriceVersionsAuthChain：401 缺身份 / 403 auditor 写 / 200 admin；auditor 读 → 200）：
  - revise：happy（changed=true）、幂等 changed=false、非 active → 409、缺 reason → 400、未知字段 body → 400（strictBindJSON）、add/remove 全空 → 400；
  - amend-models：dry_run 默认 true（不传 dry_run 时 fake 收到的 DryRun=true）、非法 action/selector → 400、正式执行透传 report；
  - get/list：200、404、limit 上限截断、status 非法 → 400；
  - 权限：无 JWT → 401；auditor 调写 → 403；auditor 调读 → 200。
- [ ] **Step 2: 跑确认失败** `~/sdk/go/bin/go test ./internal/inference/httpapi/ -run 'AdminEntitlements' -v`
- [ ] **Step 3: 实现 admin_entitlements.go + admin_ops.go 挂载**。
- [ ] **Step 4: 测试全绿**（整个 httpapi 包，含既有 admin_surface_coverage_test.go —— 新端点可能需登记，若该测试失败按其提示补登记）。
- [ ] **Step 5: Commit** `feat(admin): 权益修订/增补/只读四端点（写 billing:adjust、读 usage:read）`

### Task 4: main.go 接线 + runbook 文档 + 全量验证

**Files:**
- Modify: `cmd/server/main.go`（AdminOps 字面量 :337 起，加 `Entitlements: inferencehttpapi.NewAdminEntitlementsHandler(inferencemanagement.NewEntitlementAdminService(infStore, infStore))`）
- Modify: `docs/runbooks/kaya-coding-plan-operations.md`（新增「权益增补」一节：四端点、selector 语义、dry_run 用法、**推荐发布顺序 = 目录 publish + 配价 → 权益增补**（红线 6）、幂等/409 语义、审计查询方式 `SELECT ... FROM inference_audit_log WHERE action IN ('entitlement.revise','entitlement.amend')`）

- [ ] **Step 1: 接线 main.go**（确认 NewEntitlementAdminService 签名与 Task 2 一致；infStore 已实现全部接口——编译即验证）。
- [ ] **Step 2: 写 runbook 段落**。
- [ ] **Step 3: 全量验证**：
  ```bash
  ~/sdk/go/bin/go vet ./...
  ~/sdk/go/bin/go build ./...
  DATABASE_URL=postgres://localhost/yunhou_users?sslmode=disable ~/sdk/go/bin/go test -race -p 1 ./internal/... ./cmd/... ./tests/integration/...
  ```
  仅允许 `TestDrill_DatabaseBriefOutage` 这一既有本地失败。
- [ ] **Step 4: Commit** `feat(admin): 权益增补面接线与 runbook（发布顺序文档化）`
- [ ] **Step 5: 开 PR**（base master；body 含：需求文档链接、R1–R5 落点表、红线逐条自查、AC1–AC8 中 deploy 侧才能验的条目标注、审计面选择说明（复用 inference_audit_log，无新表）、遗留说明）。**不 merge**，等 deploy 侧复核。

## Self-Review 记录

- Spec 覆盖：R1→Task2/3，R2→Task2/3，R3→Task1/3，R4→Task2 审计（复用现有面，AC5 用 SQL 查 inference_audit_log，runbook 写明），R5→Task3 权限挂载。红线 1–5/7 由 repo 既有 SQL + 服务层语义钉死；红线 6→Task4 runbook。
- 类型一致性：EntitlementListFilter 定义在 management（repo import management，先例 operators_repo.go）；handler 只依赖 Task 2 的服务方法签名。
- 已知不覆盖：AC1/AC2/AC8 需 staging 环境，由 deploy 侧验收；本仓以单测/DB 测试钉住等价语义（幂等、409、窗口不清零由 ReviseEntitlementTx 既有测试与本计划 DB 测试覆盖）。
