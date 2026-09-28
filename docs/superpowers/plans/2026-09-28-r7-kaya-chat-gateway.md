# R7 /chat 切换 inference gateway facade — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `POST /chat` 与 `GET /chat/models` 的 inference gateway facade 从「能开」做到「敢开」：失败隔离（N1–N3）、契约对齐 legacy（N4）、0 价配额通路 + 授权迁移工具（N5+N6）、publish 前上游探测（N7）。

**Architecture:** facade 已在仓内（`cmd/server/main.go:430` 的 `INFERENCE_KAYA_CHAT_GATEWAY` 分支），本计划不新增挂载点，只加固：boot fatal 改为请求期 503 降级；SnapshotCache 冷启动错误以哨兵穿透到两个入口映射 503；双后端契约套件钉死 legacy 一致性与 N4a/N4b 拆分；迁移工具按 `cmd/admin-bootstrap` 模式新增 CLI；探测端点复用 `credentials.Service.ResolveSecret` + egress validator。

**Tech Stack:** Go, gin, sqlx/Postgres, 既有 `internal/inference/*`（catalog/gateway/access/quota/credentials/management/httpapi）。

**Spec:** `/Users/jack-tian/Downloads/github/yunhou-deploy/docs/r7-kaya-chat-gateway-requirement.md`（deploy 侧 PR #338 已合入）

## Global Constraints

- kaya 客户端零改动：所有行为差异在服务端对齐 legacy 现状（spec §2.1）。
- 进程 boot 只允许因「进程无法运行」fatal，绝不因「配置内容/DB 数据状态」fatal（spec §2.2）。
- 不得静默回退 legacy 路径（spec N1）。
- 不改动历史 migration、不触碰账本表既有行；目录回滚只能以新 revision 重发旧内容（spec §2.4）。
- 每个 PR 过 pr-ci：`go-ci`（vet → build → migrate×2 → `go test -race -p 1 -coverprofile ./internal/... ./cmd/... ./tests/integration/...`，覆盖率地板 ≥80%）+ `go-e2e`（`go test -race -count=1 ./tests/e2e/...`）。
- 分支均基于 `origin/master`（当前 7fe4d1c）。每 PR 一个分支：`r7/n1-n3-failure-isolation`、`r7/n4-contract-alignment`、`r7/n5-n6-zero-price-backfill`、`r7/n7-upstream-probe`。
- PR 描述必须书面回答 spec §8 对应开放问题（PR1→问题4；PR2→问题2、3；PR3→问题1、5；PR4→探测相关说明 + dashboard 按钮归属说明）。
- 测试约定：repo 层接口 + hand-rolled mock（无外部 mock 库）；DB 测试用 `tests/integration` 的 `dbURL()` 与 e2e 的 `tests/e2e/testhelpers.go`（E2E_DATABASE_URL）。

## 关键现状事实（已复核，基于 origin/master 7fe4d1c）

- boot fatal：`cmd/server/main.go:446-449`，开关 ON 且 `KAYA_CHAT_MODEL` 不在目录 → `log.Fatalf`。`internal/config/config.go:556-560` 静态校验（`KAYA_CHAT_MODEL` 为空 → fatal）**保留**。
- SnapshotCache 冷启动：`internal/inference/catalog/revision.go:376-409`，三处冷启动 return（:383、:394、:405）当前无统一哨兵；gateway 在 `internal/inference/gateway/service.go:308-311` 包成 `CodeInternal`（message "gateway: catalog snapshot unavailable"）。
- 模型不在快照：`gateway/service.go:312-315` → `CodeNotFound` "model X not found"。
- `mapGatewayError`（`internal/service/chat_facade.go:154-173`）：`CodeModelNotAllowed/CodeInvalidKey/CodeNotFound` → `ErrChatNoAccess`(403)。
- `KayaModelsHandler.List`（`internal/inference/httpapi/kaya_models.go:47-65`）：`ResolveUserSession` → `CodeNotFound` 时返回 200 + 空数组（N4a 要推翻）。
- handler 错误映射：`internal/handler/chat.go:230-257 chatErrorMapping` + `:600-609 writeChatError`（envelope `{code:status, data, message}`）。503 + Retry-After 无现成 helper；Retry-After 先例：`internal/middleware/ratelimit.go:133-138`。
- legacy 参照：`internal/service/chat.go` — 未知模型 `ErrChatUnknownModel`→400（:157）；`accessPlan` 无订阅 → `ErrChatNoAccess`→403；plan.ChatModels 不允许 → `ErrChatModelNotAllowed`→403。
- 契约套件基座已存在：`tests/e2e/chat_gateway_test.go` 双模式（legacy/facade）跑同一 /chat 套件 + stub upstream；harness 在 `tests/e2e/testhelpers.go`。
- 无 admin 前端代码在本仓（无 web/、无 html/tsx）——N7 dashboard 按钮不在本仓交付，PR4 描述中书面说明。
- migrations 最新编号 040，下一个可用 041；`cmd/migrate`（flag 风格）与 `cmd/admin-bootstrap`（可测 `run()` seam + `main_test.go`）是 CLI 工具范式。
- publish：`POST /admin/catalog/publish`（`internal/inference/httpapi/admin_models.go:836-848`，mandatory `?reason=`）→ `management.CatalogManager.Publish` → `catalog.Service.Publish`（单事务原子）。**publish 目前无 dry_run**；dry_run 先例在 bulk-import（`admin_bulk.go:44`）；preview 先例在 `admin_usage.go`。
- 凭据：`internal/inference/credentials/vault.go`（XChaCha20-Poly1305）+ `credentials.Service.ResolveSecret(ctx, id, pinGeneration)`（`service.go:388`）；`credentials.Service.Test`（:270）仅做解密验证，探测上游是本需求新增。
- 探测相关既有件：`workers/upstream_health.go` 的 `probe()`（按账户，connector.Client 路径），与 N7 的 deployment 级按需探测不同但可参考。

---

## PR1 — N1+N2+N3：失败隔离（先行，不依赖其它 PR）

分支：`r7/n1-n3-failure-isolation`。PR 描述回答 §8 问题 4。

### Task 1: catalog 冷启动哨兵 + 503 语义底座

**Files:**
- Modify: `internal/inference/catalog/revision.go:372-409`
- Test: `internal/inference/catalog/snapshot_cache_edges_test.go`

**Interfaces:**
- Produces: `catalog.ErrNoVerifiedSnapshot`（哨兵，`errors.Is` 可判）。PR1 后续 task 与 PR2 都依赖它。

- [ ] **Step 1: 失败测试** — 冷启动三路径（head 失败 / ActiveRevision 失败 / ParseSnapshot 失败，且无缓存）各自 `errors.Is(err, catalog.ErrNoVerifiedSnapshot)` 为 true；有缓存时刷新失败仍返回旧快照且不携带哨兵。

- [ ] **Step 2: 实现** — `revision.go` 新增：

```go
// ErrNoVerifiedSnapshot marks the cold-start failure: no active revision
// reachable and no verified snapshot in hand. Callers map it to 503
// (service-not-ready), never to a client error or an empty catalog.
var ErrNoVerifiedSnapshot = errors.New("catalog: no verified snapshot")
```

`Current` 三处冷启动 return 全部包上哨兵：

```go
return nil, fmt.Errorf("%w: active revision unavailable: %v", ErrNoVerifiedSnapshot, err)   // :383（保留原语意）
return nil, fmt.Errorf("%w: %v", ErrNoVerifiedSnapshot, err)                                // :394、:405
```

- [ ] **Step 3: 跑测试通过** — `go test -race ./internal/inference/catalog/`
- [ ] **Step 4: Commit** — `feat(inference): catalog 冷启动失败携带 ErrNoVerifiedSnapshot 哨兵（R7-N2 底座）`

### Task 2: 503 哨兵 + handler Retry-After

**Files:**
- Modify: `internal/service/errors.go:70-78`
- Modify: `internal/handler/chat.go`（chatErrorMapping :230-257 + StreamChat 错误分支）
- Test: `internal/handler/chat_test.go`

**Interfaces:**
- Produces: `service.ErrChatNotReady`；handler 对它返回 503 + `Retry-After: 5` + envelope `{code:503,data:null,message:"chat service is temporarily unavailable"}`。

- [ ] **Step 1: 失败测试** — hand-rolled mock ChatStreamer 返回 `ErrChatNotReady` → 断言 503、Retry-After 头、envelope 形状、审计日志一行（status=error）。

- [ ] **Step 2: 实现**

`internal/service/errors.go` chat 区块新增：

```go
// ErrChatNotReady: 服务未就绪（目录冷启动无快照 / 默认模型未发布）。
// 映射 503 + Retry-After，与 403（无权限）严格区分。
ErrChatNotReady = errors.New("chat service is temporarily unavailable")
```

`internal/handler/chat.go` `chatErrorMapping` 在 `ErrChatNotEnabled` case 前插入：

```go
case errors.Is(err, service.ErrChatNotReady):
    return http.StatusServiceUnavailable, service.ErrChatNotReady.Error()
```

StreamChat 错误分支（`chatErrorMapping` 之后、`writeChatError` 之前）：

```go
if errors.Is(err, service.ErrChatNotReady) {
    c.Header("Retry-After", "5")
}
```

Retry-After 取 5s：短于 chatLimiter  refill、避免重试风暴放大（§8.4 书面回答的论据）。

- [ ] **Step 3: 跑测试** — `go test -race ./internal/handler/ -run Chat`
- [ ] **Step 4: Commit** — `feat(chat): ErrChatNotReady → 503 + Retry-After（R7-N1/N2 handler 底座）`

### Task 3: N1 — 拆 boot fatal，facade 请求期降级

**Files:**
- Modify: `cmd/server/main.go:446-449`（删除 fatal 块）
- Modify: `internal/service/chat_facade.go`（StreamChat/streamChatModel 加就绪检查）
- Test: `internal/service/chat_facade_test.go`

**Interfaces:**
- Consumes: `catalog.ErrNoVerifiedSnapshot`（Task 1）、`service.ErrChatNotReady`（Task 2）、**`SnapshotCache.Current`（既有，main.go:280 已建并喂给 gatewaySvc）**、`Snapshot.Model(id)`（既有，gateway 已用）。
- ⚠️ 评审修正（fix round 1 拍板）：facade 的就绪闸门**不得**走 `catalog.Service.LoadSnapshot`——它绕过 SnapshotCache 直读 store，冷启动错误不带哨兵且每请求全量加载。facade 构造器改为接收 `Current(ctx) (*catalog.Snapshot, error)` 快照源（main.go 把既有 `catalogCache` 传入），`AllowedModels` 里的 LoadSnapshot 同样换 cache。

- [ ] **Step 1: 失败测试**（chat_facade_test.go，沿用既有 `facadeKeyStore` stub 风格）：
  - 默认模型不在快照 → StreamChat 返回 `ErrChatNotReady`（errors.Is），且**不触碰** gateway。
  - 快照冷启动错误（含哨兵）→ `ErrChatNotReady`。
  - 就绪检查在 ResolveUserSession **之前**（无计费账户用户也拿到 503，验收 §7.1/7.2 与用户状态无关）。

- [ ] **Step 2: 实现** — `streamChatModel` 开头插入就绪闸门：

```go
// N1/N2 就绪闸门：目录不可用（冷启动）或默认模型不在已发布目录 → 503，
// 进程不死、不回退 legacy。先于 ResolveUserSession，使「服务未就绪」
// 与「用户无权限」严格分层。
snap, err := f.catalog.LoadSnapshot(ctx)
if err != nil {
    return nil, mapGatewayError(err) // ErrNoVerifiedSnapshot → ErrChatNotReady
}
if _, ok := snap.Model(f.defaultModel); !ok {
    f.logNotReady() // 结构化 ERROR，30s 节流，含 model id + 快照 revision
    return nil, ErrChatNotReady
}
```

`mapGatewayError` 在 QuotaExceededError 判断后、switch 前插入：

```go
if errors.Is(err, catalog.ErrNoVerifiedSnapshot) {
    return ErrChatNotReady
}
```

日志节流：facade 加 `lastNotReadyLog atomic.Int64`（unix 秒），间隔 <30s 跳过。

`cmd/server/main.go:446-449`：删除 fatal 块，替换为注释：

```go
// R7-N1: KAYA_CHAT_MODEL 不在已发布目录不再 fatal —— facade 请求期返回
// 503 + Retry-After 并输出结构化 ERROR（进程其余端点不受影响）。部署期
// env 错误（KAYA_CHAT_MODEL 为空）仍由 config.Validate 当场 fatal。
```

- [ ] **Step 3: 跑测试** — `go test -race ./internal/service/ -run Facade`
- [ ] **Step 4: Commit** — `feat(inference): /chat facade 请求期就绪闸门，拆除 boot fatal（R7-N1）`

### Task 4: N2 — GET /chat/models 冷启动 503

**Files:**
- Modify: `internal/inference/httpapi/kaya_models.go`（List）
- Test: `internal/inference/httpapi/`（新增 `kaya_models_test.go` 或并入既有测试文件，跟随该包既有测试布局）

- [ ] **Step 1: 失败测试** — 快照源（SnapshotCache）返回含 `ErrNoVerifiedSnapshot` 的错误 → 503 + Retry-After: 5 + envelope `{code:503,data:null,message:"chat catalog is not ready"}`；且该检查先于 ResolveUserSession（新库无 revision 时无账户用户也拿 503，验收 §7.2）。

- [ ] **Step 2: 实现** — ⚠️ 评审修正：KayaModelsHandler 同样改为接收快照源（`Current(ctx) (*catalog.Snapshot, error)`，main.go 传入既有 `catalogCache`），**不得**走 `Catalog.LoadSnapshot`（绕过缓存、无哨兵）。`List` 开头插入：

```go
// N2: 目录冷启动无快照 → 503（服务未就绪），先于用户解析——与
// 「已发布但无权限」（N4a 403）严格区分。
if _, err := h.Snapshots.Current(c.Request.Context()); err != nil {
    if errors.Is(err, catalog.ErrNoVerifiedSnapshot) {
        c.Header("Retry-After", "5")
        c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "data": nil, "message": "chat catalog is not ready"})
        return
    }
    fail(c, err)
    return
}
```

（后续 Current 命中 head-probe 缓存，成本可忽略；import 加 `errors`。原 `LoadSnapshot` 调用一并换成 Snapshots.Current。）

- [ ] **Step 3: 跑测试**
- [ ] **Step 4: Commit** — `feat(inference): GET /chat/models 冷启动 503 + Retry-After（R7-N2）`

### Task 5: N3 — 故障隔离回归测试

**Files:**
- Test: `internal/inference/gateway/gateway_test.go`（或新增 `isolation_test.go`）
- Test: `internal/service/chat_facade_test.go`
- Test: `tests/integration/` 或 `tests/e2e/chat_gateway_test.go`（facade 模式 harness 已存在）

- [ ] **Step 1: 模型 A 故障不影响模型 B** — gateway 级：目录含模型 A（stub upstream 返回 500）+ 模型 B（正常）；A 请求得 502 类错误，B 请求 200 SSE；key 失效（凭据解密错误 stub）与冷却中（routing cooldown）各一个 case 同样断言 B 不受影响。
- [ ] **Step 2: 目录刷新失败继续服务旧快照** — SnapshotCache 层（flaky revisionSource：首次成功后失败）+ facade 层（LoadSnapshot 第二次起失败 → StreamChat 仍 200，OnRefreshError 被调用）。既有 `snapshot_cache_edges_test.go` 若已覆盖则在 facade 层补钉。
- [ ] **Step 3: 单 deployment 凭据解密失败隔离** — gateway 级：模型含两个 deployment，其中一个 SecretResolver 返回错误；断言仅该路由失败（多 deployment 时重试另一个成功 / 单 deployment 模型失败不影响其它模型）。
- [ ] **Step 4: N1/N2 行为级测试** — e2e facade harness 空目录变体：进程（in-process engine）存活、`GET /healthz` 200、`POST /chat` → 503+Retry-After、`GET /chat/models` → 503+Retry-After、无关端点（如 `/apps/:id/plans`）正常。
- [ ] **Step 5: 全量跑 + Commit** — `go test -race -p 1 ./internal/... ./tests/integration/...`；`feat(test): /chat facade 故障隔离回归套件（R7-N3）`

### Task 6: PR1 收尾

- [ ] `make lint && make build`；本地有 Postgres 则跑 `make test` + integration，否则推分支靠 pr-ci。
- [ ] 推分支、开 PR（base master），描述含：变更摘要、§7 第 1/2/8 条自测结果、**§8 问题 4 书面回答**（503 与 chatLimiter/timeoutMiddleware 的交互：/chat 在 timeout 豁免名单；503 发生在请求早期不占上游；Retry-After=5s 的理由与重试风暴评估）。

---

## PR2 — N4：契约对齐（kaya 零改动核心）

分支：`r7/n4-contract-alignment`（基于 master，不依赖 PR1 合并；若冲突以后合者 rebase）。PR 描述回答 §8 问题 2、3。

**注意：N4a（无计费账户 403）依赖 N6 迁移先行，PR 描述须显著标注部署顺序约束（先跑 N6，再翻开关）。**

### Task 1: 双后端契约套件盘点与补全

**Files:**
- Test: `tests/e2e/chat_gateway_test.go`（双模式基座）、`tests/e2e/testhelpers.go`

- [ ] **Step 1: 盘点** — 既有双模式套件已覆盖哪些用例，列出缺口清单（写入 PR 描述，§8 问题 2 的素材）。
- [ ] **Step 2: 补全矩阵**（同一用例跑 legacy 与 facade，逐字段断言）：
  1. 错误 envelope `{code,data,message}` + 状态码矩阵：401（无/坏 token）、403（无权限）、404（未启用）、429（额度）、400（未知模型/上游拒绝 shape）、502（上游故障）。
  2. SSE 形状：content 增量、`reasoning_content` 增量（thinking_enabled=true）、终止 `[DONE]`、**末尾 usage chunk（`choices:[]`）** — facade 须透传上游 `stream_options.include_usage` 或服务端合成；若 gateway 当前不透传，本 task 在 gateway/facade 补上并对 legacy 钉一致。
  3. tools 透传 + tool_calls 回合形状。
  4. 审计日志：每请求一行 JSON，字段集合与 legacy 完全一致（user_id、session_id、status、tokens、duration、thinking_enabled、错误行保留 tool_calls）。
  5. 默认模型语义：不带 model → `KAYA_CHAT_MODEL`；`default:true` 在 /chat/models 响应的位置与含义。
- [ ] **Step 3: Commit** — `feat(test): /chat 双后端契约套件补全（R7-N4）`

### Task 2: N4a — 无计费账户 → 403

**Files:**
- Modify: `internal/inference/httpapi/kaya_models.go:49-57`
- Modify: `internal/service/chat_facade.go:73-84`（AllowedModels）
- Test: 对应两包测试 + 契约套件新增用例

- [ ] **Step 1: 失败测试** — 无计费账户用户 GET /chat/models → 403，envelope 与 legacy 逐字节一致 `{code:403,data:null,message:"active subscription with access to this app is required"}`；facade `AllowedModels` 返回 `ErrChatNoAccess`。
- [ ] **Step 2: 实现**

`kaya_models.go` CodeNotFound 分支改为：

```go
// N4a（E6 决策，推翻原 200 空数组设计）：无计费账户 = 无有效订阅，
// 对齐 legacy 403 ErrChatNoAccess —— picker 对 403 已有处理（隐藏
// 选择器），而 200 空数组会让 picker 静默消失且与服务故障无法区分。
if domain.CodeOf(err) == domain.CodeNotFound {
    c.JSON(http.StatusForbidden, gin.H{"code": 403, "data": nil, "message": "active subscription with access to this app is required"})
    return
}
```

`chat_facade.go` AllowedModels CodeNotFound 分支：`return nil, ErrChatNoAccess`。两处旧注释（"picker 显示空列表"等）同步重写。文件头 doc comment 同步更新。

- [ ] **Step 3: 跑测试**（既有「no-account→empty」用例改为 403 断言）
- [ ] **Step 4: Commit** — `feat(inference)!: /chat/models 无计费账户改回 403 对齐 legacy（R7-N4a，依赖 N6 先行）`

### Task 3: N4b — 未知模型 400 / 无权益 403 拆分

**Files:**
- Modify: `internal/service/chat_facade.go`（streamChatModel + mapGatewayError）
- Test: `internal/service/chat_facade_test.go` + 契约套件用例

- [ ] **Step 1: 失败测试** — resolver CodeNotFound（无计费账户）→ `ErrChatNoAccess`；gateway CodeNotFound（目录查无此 id）→ `ErrChatUnknownModel`；CodeModelNotAllowed → `ErrChatModelNotAllowed`（对齐 legacy `checkAccess` 的 403 文案）；CodeInvalidKey → `ErrChatNoAccess`。

- [ ] **Step 2: 实现** — `streamChatModel` resolver 分支显式化：

```go
p, err := f.resolver.ResolveUserSession(ctx, userID)
if err != nil {
    if domain.CodeOf(err) == domain.CodeNotFound {
        return nil, ErrChatNoAccess // 无计费账户 = 无访问权限（403）
    }
    return nil, mapGatewayError(err)
}
```

`mapGatewayError` 拆分：

```go
case domain.CodeNotFound:
    // N4b：目录查无此模型 id → 400 未知模型（picker 回退默认）。
    // 无计费账户的 CodeNotFound 已在 resolver 分支拦截，不到这里。
    return ErrChatUnknownModel
case domain.CodeModelNotAllowed:
    return ErrChatModelNotAllowed // 模型存在但无权益 → 403（对齐 legacy 文案）
case domain.CodeInvalidKey:
    return ErrChatNoAccess
```

- [ ] **Step 3: 跑测试**
- [ ] **Step 4: Commit** — `feat(inference): 未知模型 400 与无权益 403 拆分对齐 legacy（R7-N4b）`

### Task 4: PR2 收尾

- [ ] 全量测试 + lint。
- [ ] 开 PR，描述含：**§8 问题 2**（契约套件跑出的全部现存差异清单，含已对齐项核对结果）、**§8 问题 3**（`/v1/models` 协议面「无计费账户 → 空数组」保持不变 —— 那是 /v1 协议契约，与 /chat Kaya 面分层清晰：`v1_protocol_test.go` 钉住不动；两个 handler 分层说明）、N4a 部署顺序警示（先 N6 后开关）。

---

## PR3 — N5+N6：0 价通路 + 授权迁移工具

分支：`r7/n5-n6-zero-price-backfill`。PR 描述回答 §8 问题 1、5。

### Task 1: N5 现状实测（测试先行）

**Files:**
- Test: `tests/integration/`（新增 `inference_zero_price_test.go` 或并入既有 inference 集成测试）

- [ ] **Step 1: 写集成测试** — 场景：发布目录含 0 价模型（sale_credit 价格版本 rate=0）；用户有 billing account + entitlement（显式 model_ids），**无 wallet / 余额 0、从未充值**；POST /chat（不带 model）→ 断言 200 SSE 全程（content 增量 + [DONE] + usage chunk），无 429/403。
- [ ] **Step 2: 跑测试看真实路径** — 若绿：N5 现状已满足，测试即钉子，§8 问题 1 回答「现状已通，路径为 plan 路径 pinCreditPrice(0) → quota.Admit，不触 wallet」。若红：定位阻断点（`gateway/service.go` plan 路径 :442 pinCreditPrice / :390 quotaSvc.Admit / 钱包回退 :403-409），进入 Task 2。

### Task 2: N5 开通路（仅当 Task 1 红）

**Files:**
- Modify: 阻断点所在文件（`internal/inference/gateway/service.go` 或 `internal/inference/quota/service.go`）

- [ ] 0 价模型（sale_credit rate=0）预占/结算不得要求余额>0、不得要求 wallet 存在；改动最小化，仅对 0 价生效，非 0 价路径行为不变（既有 wallet 测试必须全绿）。
- [ ] §8 问题 1 书面回答改动面。

### Task 3: N6 迁移工具

**Files:**
- Create: `cmd/inference-entitlement-backfill/main.go`（仿 `cmd/admin-bootstrap/main.go` 的 flag + 可测 `run()` seam）
- Test: `cmd/inference-entitlement-backfill/main_test.go`
- 如需 schema 支撑（entitlement 幂等键）：Create `migrations/041_*.sql`（先查既有 entitlements 表结构，041 仅在没有可用唯一约束时新增；遵循 migrations/README 的 IF NOT EXISTS 幂等 DDL 规则）

**Interfaces:**
- Consumes: `subscriptions`/`plans`（`plans.chat_models`：NULL=全部 /chat 目录模型；显式数组=子集）、已发布目录模型集合（catalog 表读 published revision）、inference billing accounts + entitlements 表（结构以 `internal/inference/postgres/` 为准）。

- [ ] **Step 1: 失败测试**（main_test.go，DB 集成风格仿 admin-bootstrap）：
  - dry_run：报告计数正确，零写库。
  - 实跑：NULL chat_models → entitlement model_ids = 已发布目录全集；数组 → 子集；无 billing account → 新建。
  - 幂等：第二次实跑零增量（created=0）。
  - 只增不删：已有 entitlement 含目录外模型 → 保留。
  - 失败行单列：一行数据损坏（如 plan 缺失）计入 errors，不阻断整批。
- [ ] **Step 2: 实现** — flags：`-dsn`（或 DATABASE_URL）、`-dry-run`、`-sample N`（报告样本数）、`-timeout`。输出审计报告 JSON：{processed, accounts_created, entitlements_created, skipped, errors: [{user_id, reason}...], samples: [...]}。逐用户事务；`ON CONFLICT DO NOTHING` 幂等。
- [ ] **Step 3: 跑测试 + migrate 幂等冒烟（若有 041）**
- [ ] **Step 4: Commit** — `feat(inference): 订阅→inference 权益幂等迁移工具（R7-N6）`

### Task 4: PR3 收尾

- [ ] 全量测试 + lint。
- [ ] 开 PR，描述含：**§8 问题 1**（实测路径 + 改动面）、**§8 问题 5**（增量用户：查证 Task 10 购买闭环是否已联动 entitlement —— 仓内既有「payment pipeline entitlement-grant backstop」注释（errors.go:45-50）指向已联动，需核实代码路径并书面回答；结论决定 deploy 侧是否需在翻开关前再跑一次工具）、迁移工具使用说明（dry_run → 实跑 → 再实跑验证零增量，对应验收 §7.3）。

---

## PR4 — N7：publish 前上游探测

分支：`r7/n7-upstream-probe`。

### Task 1: deployment 连通性探测端点

**Files:**
- Modify: `internal/inference/httpapi/admin_models.go`（注册新路由 + handler）
- Modify/Create: `internal/inference/management/`（probe 服务逻辑；仿 pricing_preview.go 的 service 分层）
- Test: 对应包测试

**Interfaces:**
- Consumes: `credentials.Service.ResolveSecret(ctx, id, pinGeneration)`、egress validator（`providers.EgressChecker`，SSRF 防线必须过）、catalog 的 deployment 读取。
- Produces: `POST /admin/catalog/deployments/:id/probe`（确切路径跟随 admin_models.go 既有命名），响应 `{ok, status, latency_ms, error_summary}`；永不写库。

- [ ] **Step 1: 失败测试** — stub upstream：200 → ok=true + latency；404（缺 /v1 的 k3 事故形状）→ ok=false + status=404 + 摘要；连接拒绝/超时（10s 上限）→ ok=false + error_summary；凭据解密失败 → ok=false 且不泄露明文；响应不含 secret 任何字节。
- [ ] **Step 2: 实现** — 探测 = 用真实凭据对 `base_url` 发 `GET {base}/models`（Authorization: Bearer），10s timeout，egress validator 校验 base_url；只读，零写库。
- [ ] **Step 3: Commit** — `feat(admin): deployment 上游连通性探测端点（R7-N7）`

### Task 2: publish dry_run 探测报告（advisory）

**Files:**
- Modify: `internal/inference/httpapi/admin_models.go:836-848`（publish handler）
- Modify: `internal/inference/management/catalog.go:260-266`（Publish 增加可选探测前置）

- [ ] **Step 1: 失败测试** — `POST /admin/catalog/publish?dry_run=1`：返回校验结果 + 各 deployment 探测摘要，**不写库**（active revision 不变）；`dry_run` 缺省 = 现状行为不变（直接 publish）。探测失败不阻断 publish（dry_run 报告标注，运营判断；正式 publish 不跑探测、不受探测结果影响）。
- [ ] **Step 2: 实现** — publish handler 解析可选 `dry_run` query；dry_run 路径复用 Task 1 probe + 既有校验，报告 `{would_publish: {revision 内容摘要}, probes: [{deployment_id, ok, status, latency_ms, error_summary}]}`。
- [ ] **Step 3: Commit** — `feat(admin): publish dry_run 附带 deployment 探测报告（R7-N7，非阻断）`

### Task 3: PR4 收尾

- [ ] 全量测试 + lint + OpenAPI/ docs 更新（若 `docs/api/kaya-coding-plan.openapi.yaml` 覆盖 admin 面，同步加端点；`openapi_contract_test.go` 会钉）。
- [ ] 开 PR，描述含：探测端点契约（验收 §7.9 自测）、**dashboard「模型管理」Tab 按钮不在本仓**（本仓无前端代码）的书面说明 + 给 dashboard 侧的对接契约（端点、请求/响应形状）。

---

## Self-Review 记录

- Spec 覆盖：N1→PR1/Task3；N2→PR1/Task1,2,4；N3→PR1/Task5；N4（含 4a/4b）→PR2 全部；N5→PR3/Task1,2；N6→PR3/Task3；N7→PR4 全部。§8 五问分别落在 PR1/PR2/PR2/PR3/PR3 + PR4 dashboard 说明。验收 §7 十一条逐条对应到各 PR 测试/自测步骤。
- 非目标（spec §6）均不触碰：legacy 路径保留、不做探针改造（deploy 侧）、不动 nginx、不动 kaya、不动用量口径。
- 类型一致性：`ErrNoVerifiedSnapshot`（catalog）、`ErrChatNotReady`（service）在 PR1 定义、PR2 复用；`mapGatewayError` 拆分后 PR1 的 `CodeNotFound→ErrChatNoAccess` 行为在 PR2 变为 `ErrChatUnknownModel` —— PR1 的 facade 测试若钉了该映射，PR2 须同步更新（已在 PR2/Task3 Step1 列出）。
