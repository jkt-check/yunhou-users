# PROGRESS.md — yunhou-users Phase 0 任务跟踪

> **本文档是 yunhou-users 仓库 Phase 0 的实施计划与跟踪信号灯。**
>
> **仓库分工**：本文档对应 `/Users/lili/Downloads/github/yunhou-users/`；`yunhou-deploy/PROGRESS.md §A` 列的是同一组任务的高层摘要（在那里是「我仅列任务，具体代码改动由各自仓库维护者在其仓里执行」）。本文档做**任务细化**——把 A1/A2/A3/A4 拆成 2-5 分钟可执行的子步骤。
>
> **沟通原则**：本仓库我可以**改代码**。yunhou-deploy 和 yunhou-website 仅在 PROGRESS.md / 文档里同步结论，不动它们。
>
> **图例**：⏳ 待启动  · 🟡 进行中  · ✅ 完成  · 🔒 阻塞  · ⏸ 显式延后

---

## 0. 阶段状态总览

| 阶段 | 任务 | 状态 | 关键阻塞 |
|---|---|---|---|
| Phase 0.1 | A1 — migrations ledger | ⏳ 待启动 | — |
| Phase 0.2.a | A2.a — 微信 OAuth mock | ⏳ 待启动（依赖 A1） | — |
| Phase 0.2.b | A2.b — 微信支付 mock | ⏳ 待启动（依赖 A1） | — |
| Phase 0.2.c | A2.c — 生产凭据骨架 | ⏳ 待启动（依赖 A2.a） | — |
| Phase 0.3 | A3 — PR CI 工作流 | ⏳ 待启动（依赖 yunhou-deploy reusable workflow；过渡期用 inline） | — |
| Phase 0.4 | A4 — paypal-l3.yml 修复 | ⏸ 显式延后到 Phase 4（intl） | — |

---

## A1. feat/migrations-ledger（**最紧急，首个 PR**）

### 为什么紧急

`deploy/deploy.sh` 第 32-38 行对 `migrations/*.sql` 无差别重放。`migrations/001_init.sql`、`003_payments.sql`、`007_app_secret.sql`、`008_drop_lemonsqueezy.sql` 含非幂等 DDL（`CREATE TABLE` 无 `IF NOT EXISTS`、`ALTER TABLE ... ADD COLUMN` 无 `IF NOT EXISTS`、`ALTER TABLE ... DROP CONSTRAINT` 等），二次执行直接报错。当前唯一能跑通的部署是"全新 DB 或人工维护 schema"。**没有迁移账本之前，其它 PR 都建立在会爆的基础上**。

### 设计决策（已与 yunhou-deploy/PROGRESS.md §A1 对齐）

| 决策点 | 选择 | 理由 |
|---|---|---|
| 独立二进制 vs 嵌入 server | **独立** `cmd/migrate` | 部署期与运行期关注点分离；migrate 一次性退出，server 长跑 |
| 是否用 `//go:embed` | **不用**，改 Dockerfile COPY | Go embed 规则禁止 `..` 路径，从 `cmd/migrate/` 走 `../../migrations` 编译不过 |
| 账本表 schema | `_migrations(id TEXT PK, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())` | TEXT PK 兼容任意命名；用 `_` 前缀把它从应用层排开 |
| 事务边界 | 每个文件一个 `BEGIN; <sql>; INSERT _migrations; COMMIT;` | SQL 与账本写入原子化；失败不留半截记录 |
| `-status` 输出 | 纯文本表格 + ✅ / ⏳ 前缀 | 人读、CI grep 都友好 |
| `-down` 占位 | **本 PR 不实现**，仅留接口 | 范围控制；首期不引入 down 逻辑 |

### 文件改动

| 文件 | 类型 | 改动 |
|---|---|---|
| `cmd/migrate/main.go` | 新增 | 独立二进制入口。子命令 `apply`（默认）/ `-status`。读 `MIGRATIONS_DIR` env（默认 `/migrations`，dev 用 `./migrations`）。exit code：0=成功，1=任意迁移失败 |
| `internal/migrate/migrate.go` | 新增 | 核心逻辑：建 `_migrations` 表、列已应用 id、逐文件 BEGIN/COMMIT、INSERT 账本 |
| `internal/migrate/migrate_test.go` | 新增 | 单元测试：① 空库全 applied；② 重跑幂等；③ 中间失败后续不跑 + 账本未写入失败 id；④ `-status` 输出格式 |
| `Dockerfile` | 修改 | 多 build target：`build-server`（已有逻辑）→ `/server`；新增 `build-migrate` → `/migrate`。最终 runtime stage COPY `/server`、`/migrate`、`COPY migrations/ /migrations` |
| `deploy/deploy.sh` | 修改 | 删 `for m in migrations/*.sql; do psql ... done`；改为 `docker compose run --rm migrate` |
| `docker-compose.yml` | 修改 | 新增 `migrate` profile（独立 service，临时容器，`--rm`，挂载 migrations 目录只读） |
| `Makefile` | 修改 | `make migrate` 由 "打印 psql 命令" 改为 `go run ./cmd/migrate`；新增 `make migrate-status` |
| `README.md` | 修改 | 删 "按顺序 psql" 段落；记录 "用 `./bin/migrate`" |
| `docs/deployment.md` | 修改 | 同上；增加 `migrate` profile 章节 |
| `migrations/README.md` | 新增 | 命名规范 `NNN_short_description.sql`；**禁止**非幂等 DDL（必须用 `IF NOT EXISTS` / `DROP ... IF EXISTS`）；不许回头改已发布迁移 |

### 核心契约（实现可参考，签名可微调）

```go
// internal/migrate/migrate.go
type Migration struct {
    ID   string // 例如 "001_init"
    SQL  string
}

// Apply: 跳过已应用；按顺序在事务内跑 sql + 写入 _migrations。
// 失败返回 error 并 ROLLBACK，**不**写入失败 id。
func Apply(ctx context.Context, db *sqlx.DB, migrations []Migration) (applied, skipped int, err error)

// Status: 列出每个 migration 是否已应用，输出带 ✅ / ⏳ 的纯文本表格。
func Status(ctx context.Context, db *sqlx.DB, migrations []Migration) error
```

Apply 流程：
1. `CREATE TABLE IF NOT EXISTS _migrations (id TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`
2. `SELECT id FROM _migrations` 装入 map
3. `for _, m := range migrations`：
   - if exists → `skipped++` continue
   - `tx, _ := db.BeginTx(ctx, nil)`
   - `_, err := tx.ExecContext(ctx, m.SQL)` → err 则 `tx.Rollback(); return`
   - `_, err := tx.ExecContext(ctx, "INSERT INTO _migrations (id) VALUES ($1)", m.ID)` → err 则 `tx.Rollback(); return`
   - `tx.Commit()` → err 则 `return`
   - `applied++`

### 实施步骤（每步 2-5 分钟）

- [ ] **A1-1 创建分支**：从 `master` 切出 `feat/migrations-ledger`，本地新建 worktree（`superpowers:using-git-worktrees`）或直接在 master 上拉分支
- [ ] **A1-2 写失败测试 `internal/migrate/migrate_test.go::TestApply_EmptyDB`**：起 `*sqlx.DB`（testcontainers-postgres 或 sqlite-Driver-mock 视现有 fake 决定；当前 internal/repo 已有的 sqlite 风格优先），断言空库 → applied=8、skipped=0
- [ ] **A1-3 跑测试确认红**：`go test -run TestApply_EmptyDB ./internal/migrate/...`
- [ ] **A1-4 写最小 `internal/migrate/migrate.go`**：只实现 `Apply` + `Migration` 类型；不含 `Status`；不含 main
- [ ] **A1-5 跑测试确认绿**：`go test -run TestApply_EmptyDB ./internal/migrate/...`
- [ ] **A1-6 写测试 `TestApply_Idempotent_Rerun`**：同一份 migrations 跑两次，第二次 applied=0、skipped=8、err=nil
- [ ] **A1-7 写测试 `TestApply_MidFileFailure_StopsAndDoesNotWriteLedger`**：构造一份必失败的 SQL（用 `BAD SQL SYNTAX`），确认后续文件不跑 + `_migrations` 不写入失败 id
- [ ] **A1-8 跑 migrate 包测试全绿**：`go test -race ./internal/migrate/...`
- [ ] **A1-9 提交：`feat(migrate): add Apply + ledger`**
- [ ] **A1-10 实现 `Status(ctx, db, files) error`**：遍历 migrations，若已在 `_migrations` 里打 `✅`，否则打 `⏳`
- [ ] **A1-11 写测试 `TestStatus_Output`**：验证输出含每个 id 和正确 emoji
- [ ] **A1-12 跑测试确认绿**
- [ ] **A1-13 提交：`feat(migrate): add Status printer`**
- [ ] **A1-14 新建 `cmd/migrate/main.go`**：解析 `MIGRATIONS_DIR`、scan `*.sql`、按文件名排序、调用 `Apply` 或 `Status`、`-status` flag 切换；exit code 1 on error
- [ ] **A1-15 本地手测**：`go run ./cmd/migrate` → 8 行 INSERT + "applied 8, skipped 0"；`go run ./cmd/migrate -status` → 8 个 ✅
- [ ] **A1-16 本地手测 idempotent**：再跑一次 → "applied 0, skipped 8"
- [ ] **A1-17 本地手测 status**：`delete from _migrations where id='003_payments'` → 重跑 → 1 applied + 7 skipped
- [ ] **A1-18 修改 `Dockerfile` 多 target**：
  - `FROM builder AS build-migrate` 阶段输出 `/out/migrate`
  - runtime stage 加 `COPY --from=builder /out/migrate /migrate`
  - 加 `COPY migrations/ /migrations`（与 README 强调：不走 embed）
- [ ] **A1-19 修改 `docker-compose.yml` 加 migrate service**：
  ```yaml
  services:
    app: { ... 现有 ... }
    migrate:
      build: { context: . }
      profiles: ["migrate"]
      command: ["/migrate"]
      env_file: .env
      depends_on: []  # 不依赖 app
      volumes:
        - ./migrations:/migrations:ro
      restart: "no"
  ```
- [ ] **A1-20 修改 `deploy/deploy.sh` 替换第 32-38 行的 SQL 循环**：改为 `docker compose run --rm migrate`（如果 migrate profile 已挂上）
- [ ] **A1-21 修改 `Makefile`**：`migrate` target 改为 `go run ./cmd/migrate`；新增 `migrate-status`；移除那 8 行 echo
- [ ] **A1-22 更新 `README.md` 删旧 migrate 段落**：保留顶层章节，但"按顺序 psql"那段整段替换；引用 `make migrate` 和 `make migrate-status`
- [ ] **A1-23 更新 `docs/deployment.md`**：增 §"Migration ledger"；说明 migrate profile 流程；列出禁止 DDL 形式
- [ ] **A1-24 新建 `migrations/README.md`**：命名规范 + 幂等 DDL 红线 + "不许回头改已发布"
- [ ] **A1-25 跑完整测试**：`make test`（含 `-race -cover ./internal/...`）全绿
- [ ] **A1-26 构建双二进制**：`make build` 出 `bin/server`；手动 `go build -o bin/migrate ./cmd/migrate`
- [ ] **A1-27 自检 docker 流程**（如本地 docker 可用）：`docker compose build` → `docker compose run --rm migrate` → 成功；再跑一次 → 仍成功
- [ ] **A1-28 提交：`feat(migrate): wire Dockerfile / compose / Makefile / docs`**
- [ ] **A1-29 提 PR**：标题 `feat(migrations-ledger): add migration ledger binary + Dockerfile + compose profile`；描述引用本节验收清单；目标 `master`
- [ ] **A1-30 等 CI 绿 + 合并**：A3 还没上 CI 时，用本地 `make test` + `go vet ./...` 替代；合并后 squash

### 验收清单

- [ ] 空 DB 上 `./bin/migrate` → 8 个迁移全 applied，`_migrations` 表 8 行
- [ ] 再跑一次 → 全部 skipped，0 错误
- [ ] `delete from _migrations where id='003_payments'` 后再跑 → 仅 `003_payments` reapplied
- [ ] 临时把 `001_init.sql` 改成必失败 → 后续文件不跑、`_migrations` 不写入失败 id
- [ ] `./bin/migrate -status` 输出与实际库状态一致（每行带 ✅ / ⏳）
- [ ] `go test ./internal/migrate/...` 全绿
- [ ] `docker compose run --rm migrate` 在干净 staging PG 上成功；再跑仍成功
- [ ] `make build` 同时产出 `bin/server` 和 `bin/migrate`
- [ ] `make test` 含 `./cmd/...` 路径（确认 migrate 包被覆盖）
- [ ] README + docs/deployment.md 已删 "按顺序 psql" 字样
- [ ] migrations/README.md 落地
- [ ] Dockerfile runtime image 含 `/server` + `/migrate` + `/migrations`

---

## A2.a feat/wechat-oauth-mock（**小，先合**）

### 范围与边界

让 mock 模式跑通"浏览器 → `/auth/wechat/redirect` → 直接回到 BFF `redirect_uri?code=mock-code&state=...` → `/auth/wechat/callback` 用固定 unionid 完成登录"。**不**碰真微信上游路径；mock 与真实分支并行存在，由 `cfg.WeChatOAuthMock=true` 守卫。

### 设计决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| mock 触发开关 | env `WECHAT_OAUTH_MOCK=1` → `cfg.WeChatOAuthMock = true` | 与 yunhou-deploy/PROGRESS.md §A2.a 对齐；命名与 §A2.b 的 `WECHAT_PAY_MOCK` 对仗 |
| mock code 常量 | `mock-code`（硬编码字符串） | 简单确定；handler 看到就短路 |
| mock unionid 常量 | `mock-unionid-001`（硬编码字符串） | 与 provider_uid 拼接 → `wechat_mock-unionid-001` |
| mock 注入位置 | **handler 层**（`internal/handler/auth_wechat.go`）而不是 service 层 | handler 已经在做 redirect/callback 编排，加 cfg 守卫最小改动；service 层 `BuildAuthorizeURL` 已有重载位点（注入 mock URL），handler 调它即可 |
| 真分支是否回归 | **零回归** | mock 分支只在 `cfg.WeChatOAuthMock=true` 时走；`false` 时行为完全等同 v3 现状 |

### 文件改动

| 文件 | 改动 |
|---|---|
| `internal/config/config.go` | 加 `WeChatOAuthMock bool` 字段；`Load()` 解析 `WECHAT_OAUTH_MOCK=1` |
| `internal/config/config_test.go` | 加 2 个 case：`WECHAT_OAUTH_MOCK=1` → true；`=` 为空或未设 → false |
| `internal/handler/auth_wechat.go` | `Redirect`：mock 模式下跳过 `d.svc.BuildAuthorizeURL`，直接 `c.Redirect(302, redirectURI + "?code=mock-code&state=" + state)`；`Callback`：mock 模式下跳过 ExchangeCode / FetchWeChatProfile，直接构造 `ProviderUserInfo{Provider:"wechat", ProviderUID:"wechat_mock-unionid-001", Nickname:"mock-user"}` 并走 `d.authSvc.LoginWithProfile` |
| `internal/handler/auth_wechat_test.go` | 加 `TestWeChatRedirect_MockMode_SkipsWeixin` + `TestWeChatCallback_MockMode_HitsLoginWithProfile`；保留所有现有 case 不动 |
| `README.md` | 增 §"Dev mock mode"：列出 `WECHAT_OAUTH_MOCK=1` + 浏览器自测步骤 |

### 实施步骤

- [ ] **A2.a-1 分支**：`feat/wechat-oauth-mock`（从 master）
- [ ] **A2.a-2 加 `WeChatOAuthMock` 到 `internal/config/config.go`**：字段 + `Load()` 一行 + `boolFromEnv` helper（如还没有就手写）
- [ ] **A2.a-3 写配置测试 `TestConfig_WeChatOAuthMock`**：`t.Setenv("WECHAT_OAUTH_MOCK", "1")` → true；未设 → false；设 `0` → false
- [ ] **A2.a-4 跑 `go test ./internal/config/...`** 全绿
- [ ] **A2.a-5 在 `internal/handler/auth_wechat.go` 加 mock 短路**：
  - `Redirect`：在 `app.IsActive` 检查之后、调用 `d.svc.BuildAuthorizeURL` 之前，插入 `if mock { c.Redirect(302, redirectURI + "?code=mock-code&state=" + state); return }` —— state 仍走真实 `util.IssueOAuthState`（让 callback 验签不破）
  - `Callback`：在 `VerifyCallbackState` 之后、调用 `d.svc.ExchangeCode` 之前，插入 `if mock { profile := &service.ProviderUserInfo{Provider:"wechat", ProviderUID:"wechat_mock-unionid-001", Nickname:"mock-user"}; loginResp, err := d.authSvc.LoginWithProfile(...); ...; c.Redirect(302, buildCallbackRedirectURL(...)); return }`
  - mock 模式下跳过 `lookupWeChatConfig` 后面的 `cfg` 索引读取（避免 nil cfg deref）—— callback 拿到的 `verifiedIdx` 必须仍在 `cfg.CallbackURLs` 范围内，**或** mock 分支直接用 `cfg.CallbackURLs[verifiedIdx]` 之外的 fallback：从 `app.Config` 解 `ac.OAuthProviders.WeChat.CallbackURLs[verifiedIdx]`，与真分支等价
- [ ] **A2.a-6 写 `TestWeChatRedirect_MockMode_SkipsWeixin`**：注入 `d.svc` 让 `BuildAuthorizeURL` 返回 error 也能通过（验证根本没被调）；断言 `c.Redirect` 的 URL 含 `code=mock-code`
- [ ] **A2.a-7 写 `TestWeChatCallback_MockMode_HitsLoginWithProfile`**：mock `d.svc` 让 `ExchangeCode` / `FetchWeChatProfile` 返回 error 也能通过（验证根本没被调）；断言 `d.authSvc.LoginWithProfile` 被调用，profile.ProviderUID == `wechat_mock-unionid-001`
- [ ] **A2.a-8 跑 `go test ./internal/handler/... ./internal/service/...`** 全绿（确保真分支不回归）
- [ ] **A2.a-9 更新 `README.md` §"Dev mock mode"**：`WECHAT_OAUTH_MOCK=1` + 浏览器步骤
- [ ] **A2.a-10 提交：`feat(wechat-oauth-mock): add MOCK_MODE short-circuit`**
- [ ] **A2.a-11 提 PR**：标题 `feat(wechat-oauth-mock): add MOCK_MODE redirect/callback short-circuit`；描述引用 yunhou-deploy/PROGRESS.md §A2.a 验收；目标 `master`
- [ ] **A2.a-12 等 CI 绿 + 合并**

### 验收清单

- [ ] `./bin/server` 启动时 `WECHAT_OAUTH_MOCK=1`，浏览器走 `/auth/wechat/redirect?app_id=cn-website&redirect_uri=https://bff/auth/wechat-callback&state=...` 立刻跳回 BFF（不经 `open.weixin.qq.com`）
- [ ] 不开启 mock 时（默认），与现行行为无差异——所有现有 `internal/service/wechat_oauth_test.go` 与 `internal/handler/auth_wechat_test.go` 测试仍绿
- [ ] 单测覆盖 mock 与真实两条路径
- [ ] config 解析 `WECHAT_OAUTH_MOCK=1 / 0 / 未设` 三种情形

---

## A2.b feat/wechat-pay-mock（**中**）

### 范围与边界

mock 模式跑通"下单 → 假微信支付 URL → 收到 webhook → 订单 paid → 订阅生效"。**不**碰真微信支付 v3 上游；webhook 验签在 mock 模式下放行（但仍校验时间戳容差、签名字段存在性）。

### 设计决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 入口开关 | env `WECHAT_PAY_MOCK=1` → `cfg.WeChatPayMock = true` | 与 A2.a 对仗 |
| 新建包路径 | `internal/billing/wechat/`（与 `paypal/` 并列） | 已有的 `paypal/` 结构直接复用 |
| mock 下单返回 URL | `weixin://wxpay/bizpayurl?pr=mock_<order_id>` | 形似 NATIVE code_url；非真实可扫，只是占位 |
| webhook 验签 mock 短路 | 在 `WeChatPayV3Verifier.VerifySignature` 入口加 `if MockMode { 仅校验时间戳 + 字段存在性 → return nil }` | 验签逻辑保留但 bypass HMAC 比对；保留时间戳防过期 |
| NATIVE/H5/JSAPI 三种下单 | v1 仅实现 **NATIVE**（`code_url` 字段）；H5/JSAPI 留接口但 stub | 与真链路第一阶段对齐；`createOrder` 调度按 `trade_type` 字段路由 |

### 文件改动

| 文件 | 改动 |
|---|---|
| `internal/billing/wechat/types.go` | 新建：`WeChatPayOrder` / `WeChatPayUnifiedOrderRequest` / `WeChatPayUnifiedOrderResponse` / `WeChatPayNotifyPayload` 结构体 |
| `internal/billing/wechat/wechat.go` | 新建：`WeChatPayClient` 结构 + `UnifiedOrder(ctx, req)` 方法；`MockMode` 字段；mock 分支直接构造 `code_url` 返回；真分支预留 `httpClient.Do` 调用 `https://api.mch.weixin.qq.com/v3/pay/transactions/native`（接口签名先定下，body 真实拼装放到 A2.c 后续） |
| `internal/billing/wechat/wechat_test.go` | 新建：覆盖 NATIVE 下单 mock 返回 `code_url`；真分支空 happy path（仅断言 HTTP 调用结构） |
| `internal/config/config.go` | 加 `WeChatPayMock bool` 字段；`Load()` 解析 `WECHAT_PAY_MOCK=1` |
| `internal/middleware/webhook_sig.go` | `WeChatPayV3Verifier` 加 `MockMode bool` 字段；`VerifySignature` 入口：mock 模式下仅校验 `Wechatpay-Timestamp` 5 分钟容差 + 三个 header 都存在，其余放行 |
| `internal/service/payment.go` | mock 模式下 `POST /payments/orders` 走 NATIVE → `internal/billing/wechat` → 拿 mock `code_url` 写 `provider_intent`；webhook 处理器在 mock 模式下接受 `internal/billing/wechat/wechat.go` 暴露的 mock 解码 helper |
| `internal/handler/webhook.go` | mock 模式下跳过 `DecryptResource`（mock 报文明文，不走 AES-GCM） |
| `tests/e2e/wechat_pay_mock_test.go` | 新建 e2e：mock 模式全链路 "下单 → mock code_url → POST /webhooks/payment/wechat → 订单 paid → 订阅 insert" |

### 实施步骤

- [ ] **A2.b-1 分支**：`feat/wechat-pay-mock`（从 master）
- [ ] **A2.b-2 加 `WeChatPayMock` 到 `internal/config/config.go`**：字段 + `Load()`
- [ ] **A2.b-3 写配置测试**：1/0/未设 三种情形
- [ ] **A2.b-4 跑 config 测试全绿**
- [ ] **A2.b-5 新建 `internal/billing/wechat/types.go`**：定义所有 struct；不加方法
- [ ] **A2.b-6 写 `wechat_test.go::TestWeChatPayClient_UnifiedOrder_MockMode`**：构造 client，`MockMode=true`，调 `UnifiedOrder`，断言返回 `code_url` 含 `mock_` 前缀 + `out_trade_no == req.OutTradeNo`
- [ ] **A2.b-7 新建 `internal/billing/wechat/wechat.go`**：只实现 mock 分支 + 接口签名；真分支 stub 返回 `errors.New("not implemented")`
- [ ] **A2.b-8 跑 `go test ./internal/billing/wechat/...`** 绿
- [ ] **A2.b-9 修改 `internal/middleware/webhook_sig.go`**：
  - `WeChatPayV3Verifier` 加 `MockMode bool`
  - `VerifySignature` 入口：`if v.MockMode { return v.verifyMockMode(headers) }`
  - `verifyMockMode` 仅校验 timestamp 容差 + 三个 header 存在；HMAC 直接跳过
- [ ] **A2.b-10 加单测 `TestWeChatPayV3Verifier_VerifySignature_MockMode`**：mock=true + 合法 header → nil；mock=true + 缺 header → ErrInvalidSignature；mock=true + 时间戳超 5min → ErrTimestampOutOfRange
- [ ] **A2.b-11 跑 `go test ./internal/middleware/...`** 全绿（验证真分支不回归）
- [ ] **A2.b-12 改 `internal/service/payment.go`**：`createOrder` 加 `if cfg.WeChatPayMock { ... return mockIntent }`；webhook 处理器加 mock 解码路径（明文 JSON 直接读）
- [ ] **A2.b-13 改 `internal/handler/webhook.go`**：mock 模式跳过 `DecryptResource`；直接 `json.Unmarshal(body, &payload)`
- [ ] **A2.b-14 新建 `tests/e2e/wechat_pay_mock_test.go`**：起 test server，`WECHAT_PAY_MOCK=1`，走 "POST /payments/orders" → 拿 `code_url` → POST webhook（明文 JSON） → 验证订单 paid + subscription 生效
- [ ] **A2.b-15 跑 `make e2e`**（注意 e2e 需要本机 Postgres，按 CLAUDE.md 准备）
- [ ] **A2.b-16 跑 `make test`** 全绿
- [ ] **A2.b-17 提交：`feat(wechat-pay-mock): add mock NATIVE order + webhook short-circuit`**
- [ ] **A2.b-18 提 PR**：标题 `feat(wechat-pay-mock): add mock mode for orders + webhook`；描述引用 §A2.b 验收；目标 `master`
- [ ] **A2.b-19 等 CI 绿 + 合并**

### 验收清单

- [ ] `WECHAT_PAY_MOCK=1` 时，端到端 "下单 → 跳假微信支付 → 收 webhook → 订单 paid → 订阅生效" 全链路通
- [ ] `WECHAT_PAY_MOCK=` 未设或为 `0` 时，行为与 v3 现状一致——所有 webhook_sig 真分支单测仍绿
- [ ] 单测覆盖 NATIVE 下单 mock 返回 `code_url`、webhook mock 验签、订阅激活流程
- [ ] e2e 跑通 mock 模式全链路
- [ ] 真分支 HTTP 调用结构单测就位（即便 body 拼装 stub）

---

## A2.c feat/wechat-prod-credential-prep（**小，运维向**）

### 范围与边界

让 `apps.config` 加载 `oauth_providers.wechat` 与 `payment_providers.wechat_pay` 两个嵌套字段；启动期校验：mock 模式下相关 env 可空、非 mock 模式必需。**不**写真实凭据到代码或文档——所有值从 GH Secret / VPS env 注入。

### 设计决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 字段路径 | `apps.config.oauth_providers.wechat.*` 与 `apps.config.payment_providers.wechat_pay.*` | 与 CLAUDE.md 已声明一致；与 A2.a/A2.b 改动共享 schema |
| env 名称策略 | `apps.config.oauth_providers.wechat.app_id_env = "WECHAT_OAUTH_APP_ID"`（**字段名指向 env 名**，不直接存明文） | 凭据不进 DB；DB 只存 env 名 → 配置可移植、可审计 |
| 启动期校验 | `internal/config/config.go::Validate()` 加：`!WeChatOAuthMock && WECHAT_OAUTH_APP_ID == ""` → error；`!WeChatPayMock && WECHAT_PAY_MCH_ID == ""` → error | fail-fast；与 OAUTH_STATE_SECRET 校验形态一致 |

### 文件改动

| 文件 | 改动 |
|---|---|
| `internal/model/app.go` | （如还没 WeChatOAuthConfig / WeChatPayConfig 结构）确认 `ac.OAuthProviders.WeChat` + `ac.PaymentProviders.WeChatPay` 两个嵌套字段存在；缺则补 |
| `internal/config/config.go` | 加 `WeChatOAuthAppID`、`WeChatOAuthAppSecret`、`WeChatPayMchID`、`WeChatPayAPIV3Key`、`WeChatPayMerchantCertPath`、`WeChatPayMerchantKeyPath` 字段 + `Load()` 解析 + `Validate()` 校验 |
| `internal/config/config_test.go` | 加 `TestConfig_WeChatMock_AllowsEmptyProdCreds`（mock=true 不报错）+ `TestConfig_WeChatReal_RequiresAppID`（mock=false + 空 → error） |
| `internal/repo/app.go` | 加载 `ac.OAuthProviders.WeChat` + `ac.PaymentProviders.WeChatPay`（与现有 github provider 加载路径对齐） |
| `README.md` | 增 §"WeChat prod credentials"：列出 env 名清单 + mock 模式跳过校验的说明（**不**写真实值） |

### 实施步骤

- [ ] **A2.c-1 分支**：`feat/wechat-prod-credential-prep`
- [ ] **A2.c-2 确认 `internal/model/app.go` 已有 `ac.OAuthProviders.WeChat` 与 `ac.PaymentProviders.WeChatPay` 字段**（从 CLAUDE.md 看 `oauth_providers.github` 已存在，微信并列即可）
- [ ] **A2.c-3 写失败测试 `TestConfig_WeChatReal_RequiresAppID`**：`t.Setenv("WECHAT_OAUTH_MOCK", "")` + `WECHAT_OAUTH_APP_ID=""` → `Validate()` 返回非 nil error
- [ ] **A2.c-4 跑测试确认红**
- [ ] **A2.c-5 在 `internal/config/config.go` 加新字段** + `Load()` + `Validate()` 加校验分支
- [ ] **A2.c-6 跑测试确认绿**
- [ ] **A2.c-7 写 `TestConfig_WeChatMock_AllowsEmptyProdCreds`**：mock=true + prod creds 全空 → `Validate()` nil
- [ ] **A2.c-8 跑 config 测试全绿**
- [ ] **A2.c-9 写 `TestRepo_LoadApp_WeChatConfigRoundTrip`**：构造 `app.Config` 含 `oauth_providers.wechat.callback_urls` 与 `payment_providers.wechat_pay.plan_mapping`，写入 DB → 读回 → JSON 比对
- [ ] **A2.c-10 跑 `internal/repo` 测试全绿**
- [ ] **A2.c-11 更新 `README.md` §"Required Environment Variables" 表**：增 WECHAT_OAUTH_APP_ID / WECHAT_OAUTH_APP_SECRET / WECHAT_PAY_MCH_ID 等条目，**值列 placeholder**
- [ ] **A2.c-12 提交：`feat(wechat-prod): config layer for prod credentials + mock bypass`**
- [ ] **A2.c-13 提 PR**：标题 `feat(wechat-prod-credential-prep): wire WeChat prod credentials + mock bypass`；描述引用 §A2.c 验收；目标 `master`
- [ ] **A2.c-14 等 CI 绿 + 合并**

### 验收清单

- [ ] 启动期能识别新的 env 引用：`WECHAT_OAUTH_APP_ID`、`WECHAT_OAUTH_APP_SECRET`、`WECHAT_PAY_MCH_ID`、`WECHAT_PAY_API_V3_KEY`、`WECHAT_PAY_MERCHANT_CERT_PATH`、`WECHAT_PAY_MERCHANT_KEY_PATH`
- [ ] mock 模式下全部空 → `Validate()` 通过
- [ ] 非 mock 模式任一关键 env 空 → `Validate()` 返回明确 error
- [ ] `apps.config` 嵌套字段加载回路（写 DB → 读回 → JSON 等价）
- [ ] 在不写真实凭据的前提下，A2.a + A2.b 的 mock 模式端到端仍能跑通
- [ ] README 更新；无真实凭据泄露

---

## A3. ci/pr-ci（**待 yunhou-deploy 的 reusable workflow 就绪；过渡期 inline**）

### 范围与边界

PR CI 工作流：每次 push / PR → 跑 `go vet` + `go test -race -cover` + 双二进制构建（不推）。**过渡期**（yunhou-deploy 的 reusable workflow 还没就绪）用 inline 写法；**后续**切到 `uses: yunhouorg/yunhou-deploy/.github/workflows/reusable-pr-ci-go.yml@main`。

### 设计决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 过渡期 vs 终态 | **过渡期先 inline**（自包含 service postgres），终态切 reusable | 不阻塞 A1/A2/A3 自身；reusable 就绪后单 PR 切即可 |
| Go 版本 | `golang:1.25-alpine`（与 Dockerfile builder stage 对齐） | 一致性 |
| Postgres 版本 | `postgres:16-alpine`（CLAUDE.md migrations 用 PG16 风格） | 与生产对齐 |
| e2e 跑不跑 | **本期不跑**（e2e 依赖本地 DB；CI 容器化 e2e 推到后续 PR） | 范围控制；unit + race + cover 已能挡住大多数回归 |
| 缓存策略 | `actions/cache@v4` 缓存 `~/go/pkg/mod` + `~/.cache/go-build` | 加速 |

### 文件改动

| 文件 | 改动 |
|---|---|
| `.github/workflows/pr-ci.yml` | 新建：触发器 `pull_request` + `push`（master）；jobs `test` 步骤 checkout → setup-go 1.25 → service postgres:16-alpine → `go vet ./...` → `go test -race -coverprofile=coverage.out ./internal/... ./cmd/...` → `go build -o bin/server ./cmd/server` → `go build -o bin/migrate ./cmd/migrate` → 上传 coverage artifact |
| `Makefile` | 新增 `make ci-test` target（`go test -race -cover ./internal/... ./cmd/...`），让本地复制 CI 行为 |

### 实施步骤

- [ ] **A3-1 分支**：`ci/pr-ci`
- [ ] **A3-2 新建 `.github/workflows/pr-ci.yml`**：
  - 触发器：`on: pull_request` + `on: push: branches: [master]`
  - jobs.test：`runs-on: ubuntu-latest`；services.postgres image `postgres:16-alpine`，env `POSTGRES_DB=yunhou_users_test` + `POSTGRES_USER=test` + `POSTGRES_PASSWORD=test`，ports 5432:5432，healthcheck `pg_isready`
  - env：`DATABASE_URL=postgres://test:test@localhost:5432/yunhou_users_test?sslmode=disable` + `OAUTH_STATE_SECRET=$(openssl rand -hex 32)` + `JWT_ACCESS_TTL=15m` + `JWT_REFRESH_TTL=168h`
  - steps：actions/checkout@v4 → actions/setup-go@v5（go-version: 1.25, cache: true）→ `go vet ./...` → `go test -race -coverprofile=coverage.out ./internal/... ./cmd/...` → `go build -o bin/server ./cmd/server` → `go build -o bin/migrate ./cmd/migrate` → `actions/upload-artifact@v4` 上 coverage.out
- [ ] **A3-3 加 `make ci-test`**：`go test -race -coverprofile=coverage.out ./internal/... ./cmd/...`
- [ ] **A3-4 本地跑 `make ci-test`** 验证全绿（先于 push）
- [ ] **A3-5 push 触发 CI**：观察 run，迭代修任何环境相关失败（最常见：缺 env 导致 config.Validate() 报错）
- [ ] **A3-6 提交 + push + 开 PR**
- [ ] **A3-7 等 CI 绿 + 合并**
- [ ] **A3-8 后置**：在 PR 描述加 "TODO: switch to reusable workflow when yunhou-deploy ready"；A3 的终态切换作为单独 follow-up PR

### 验收清单

- [ ] 每次 PR / push 触发 CI
- [ ] CI 红即不可 merge（branch protection 由 yunhou-deploy Phase 0.7 配；本 PR 只交付 workflow 文件）
- [ ] CI 含 `go vet` + `go test -race -cover` + 双二进制构建
- [ ] 本地 `make ci-test` 与 CI 行为一致
- [ ] 后续切 reusable workflow 的 TODO 已标注

---

## A4. fix/paypal-l3-ci（**🔒 显式延后到 Phase 4**）

### 为什么延后

`yunhou-users/.github/workflows/paypal-l3.yml` 现状不可用：缺 RSA 密钥、`OAUTH_STATE_SECRET`、`PAYPAL_L3_E2E_MODE`、app seed、migration 008 注入。**cn 域内不需要 PayPal**——首期三套 cn 环境与 PayPal 无关。Phase 4（intl 启用 PayPal）时一起修。

### 当前处置

- [ ] **A4-1 在 yunhou-users/PROGRESS.md 标 🔒**（已在本节顶部体现）
- [ ] **A4-2 不在本仓库修**：避免引入与 Phase 4 任务无关的代码改动
- [ ] **A4-3 在 yunhou-deploy/PROGRESS.md §F "待启动"清单移除 A4**（若之前列了）
- [ ] **A4-4 Phase 4 启动时复评**：本节预留作为 checklist，下游修改者按需重写

### Phase 4 复评时（**占位**——本 PR 不动）

| 文件 | 改动 |
|---|---|
| `.github/workflows/paypal-l3.yml` | ① `openssl genrsa -out keys/private.pem 2048`；② 注入 `OAUTH_STATE_SECRET=$(openssl rand -hex 32)` + `PAYPAL_L3_E2E_MODE=1`；③ 把 migrations 008 一并应用；④ 增加 application seed 步骤；⑤ ngrok URL → PayPal webhook API 动态更新 |

---

## B/C/D 跨仓同步（**不修代码，仅信号同步**）

> 本节不是任务，是给 yunhou-deploy 维护者的状态广播。每完成一项 → 在 yunhou-deploy/PROGRESS.md §F 更新"待启动"清单。

| Yunhou-users 节点 | 给 yunhou-deploy 的信号 |
|---|---|
| A1 合并 | "yunhou-users 已上线独立 migrate 二进制；`deploy/deploy.sh` 已切到 `docker compose run --rm migrate`——yunhou-deploy 的 compose 模板可以撤掉 8 行 psql 循环" |
| A2.a + A2.b 合并 | "yunhou-users 已支持 `WECHAT_OAUTH_MOCK=1` + `WECHAT_PAY_MOCK=1`；env 模板 `cn.dev.env.example` 与 `cn.staging.env.example` 可以把 mock 开关默认开" |
| A2.c 合并 | "yunhou-users 已加载 `apps.config.oauth_providers.wechat.*` 与 `payment_providers.wechat_pay.*`；seeds JSON 可以写 env 引用名（`WECHAT_OAUTH_APP_ID` 等），无需写真实凭据" |
| A3 合并 | "PR CI 已绿，yunhou-users 仓库可以配 branch protection 要求 CI 通过" |

---

## E. 决策日志（追加式）

| 日期 | 决定 | 原因 |
|---|---|---|
| 2026-07-12 | cn 三套环境（dev-staging-prod），intl 区域推到 Phase 4 | 用户指示先做国内、海外后续 |
| 2026-07-12 | cn 单独用微信 OAuth + 微信支付；alipay 显式延后 | 用户指示先打通微信，支付宝"后面再接入" |
| 2026-07-12 | yunhou-users 一份代码 + region-aware env 切换 | 用户拍板 |
| 2026-07-12 | 模式：VPS + Docker Compose，**不上 K8s** | 用户拍板 |
| 2026-07-12 | 镜像：ghcr.io + GitHub Environments secrets；**不用 Harbor/Vault** | 用户拍板 |
| 2026-07-12 | 启动容器用 multi-stage Dockerfile + non-root user | 沿用现有 yunhou-users Dockerfile |
| 2026-07-12 | 沟通原则：本仓库我改代码；yunhou-deploy / yunhou-website 仅在 PROGRESS.md 同步 | 用户指示 |
| 2026-07-12 | A1 migrate 用独立 `cmd/migrate` 二进制 + Dockerfile COPY，**不用 `//go:embed`** | Go embed 规则禁止 `..` 路径 |
| 2026-07-12 | A2.a mock 短路放在 **handler 层**（而非 service 层） | 改动最小；service 层接口稳定 |
| 2026-07-12 | A2.b webhook 验签 mock 模式仅校验时间戳 + 字段存在性，HMAC 跳过 | 验签逻辑保留但 bypass HMAC 比对，便于 e2e 驱动 |
| 2026-07-12 | A2.c 凭据 env 名存进 `apps.config.*_env` 字段，DB 不存明文 | 可移植、可审计；GH Secret 唯一来源 |
| 2026-07-12 | A3 过渡期 inline 写法启动；reusable workflow 就绪后单 PR 切 | 不阻塞 A1/A2 |
| 2026-07-12 | A4 paypal-l3 显式延后到 Phase 4 | cn 域内不需要 PayPal |

---

## F. 当前活化任务

> 这里只挂**当下该做**的任务；完成一项移到已完成的栏目（在 G 节追加历史）。

### 进行中
- ⏳ **A1 — feat/migrations-ledger**（最紧急；其它 PR 的基础）

### 待启动（按依赖顺序）
- A2.a（微信 OAuth mock；依赖 A1）
- A2.b（微信支付 mock；依赖 A1）
- A2.c（生产凭据骨架；依赖 A2.a）
- A3（PR CI；过渡期 inline，依赖 yunhou-deploy reusable workflow 就绪后可切）

### 显式延后
- ⏸ **A4 — fix/paypal-l3-ci**（推到 Phase 4，intl 启用 PayPal 时一起修）

---

## G. 已完成任务（追加历史）

> 每完成一项追加一行。建议格式：`✅ <PhaseID 任务ID>—<一句话>—<commit hash>`