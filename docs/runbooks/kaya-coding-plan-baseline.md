# Kaya Coding Plan — Task 0 基线报告

日期：2026-09-08
分支：`kaya-coding-plan`，HEAD `299feeb`（= `master` `ec25565` + 一个纯文档提交）
执行范围：Task 0（基线确认与已有分支复用评估），未修改任何生产代码。

## 1. 基线 Commit 与环境

- 基线代码：`kaya-coding-plan` @ `299feeb`（功能代码与 `master` @ `ec255653ffccae7bd0b9f9ef792a2ebb58bfc642` 一致）。
- 测试数据库：本机一次性 PostgreSQL 16.14（Homebrew）实例，`initdb -D /tmp/kaya-baseline-pg -U postgres --auth=trust -E UTF8`，unix socket 目录 `/tmp/kaya-baseline-pg-sock`，端口 55433，库名 `yunhou_baseline`。测试结束后已 `pg_ctl stop` 并删除数据目录与 socket 目录。
- 环境变量：`DATABASE_URL` 与 `E2E_DATABASE_URL` 均设为 `postgres://postgres@localhost:55433/yunhou_baseline?sslmode=disable`（变量名与格式参照 `.env.example` 模板）。**未读取、未使用仓库根目录 `.env` 的任何值。**
- Docker 不可用，未使用。

## 2. 基线测试结果（未修改代码上运行）

| # | 命令 | 结果 |
|---|---|---|
| 1 | `go vet ./internal/... ./cmd/...` | 通过，exit 0，无输出 |
| 2 | `go build ./...` | 通过，exit 0 |
| 3 | `go run ./cmd/migrate`（第一次） | 通过：`[migrate] applied=21 skipped=0`（001–021 全部应用） |
| 3 | `go run ./cmd/migrate`（第二次） | 通过：`[migrate] applied=0 skipped=21`（幂等） |
| 4 | `go test -race -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...` | 全部 `ok`，exit 0。覆盖率：handler 88.1%、service 88.1%、repo 84.9%、middleware 95.9%、router 98.8%、billing/paypal 90.0%、billing/wechat 72.1%、config/model 100%、migrate 76.0%、util 87.0%、cmd/server 5.3%；`go tool cover -func=coverage.out` 总覆盖率 **83.2%**（高于 CI 80% 门禁） |
| 5 | `go test -race -count=1 ./tests/e2e/...` | 通过：`ok github.com/yunhou/users/tests/e2e 106.549s`，exit 0。e2e 用 httptest 内嵌服务 + 临时 RSA 密钥，仅需 `E2E_DATABASE_URL`，无需额外外部环境 |

结论：基线全绿，无失败需要复现或豁免。后续任务的失败不得归因于"预置失败"。

## 3. 复用评审：`feat/multi-model-gateway`

- 位置：`.worktrees/multi-model-gateway`，HEAD `1d0281d`，领先 master 24 个提交（`4f552fb`…`1d0281d`），**未合入主线**（`git merge-base --is-ancestor 1d0281d master` 为否）。
- 分支自测证据（只读运行，未改动 worktree）：`go test ./internal/llm/...` 通过（0.546s）；`go test ./internal/service/ -run TestChatService` 通过（6.250s）；`go vet` 通过。
- 复用方式：分支不合入主线；后续任务按文件摘取（归属该分支 24 个提交），在 `internal/inference/` 下改造，避免双份实现。

### 3.1 对照设计 §2 八条语义差距的逐条决定

| # | 已有方案 | 本产品所需变化 | 决定 |
|---|---|---|---|
| 1 | `LLM_PROVIDERS_JSON` 环境变量目录 | 数据库配置、版本发布、后台批量维护 | **改造后复用**。`catalog.go` 的 `ParseCatalog`/`Validate`（引用完整性、协议枚举、URL 校验、`DisallowUnknownFields` 启动失败）改造为 Task 3 环境变量兼容导入器的输入校验；运行期目录改为 DB 草稿/发布快照，环境变量不再作为运行期真相 |
| 2 | 模型单一 `Provider` 引用 | 模型与多个上游部署映射 | **改造后复用**。`Catalog.Resolve` 的逻辑 ID 解析思路保留；`Model.Provider` 单引用结构不继承，改为 ModelRoute 多 Deployment（Task 1/3 新 DDL） |
| 3 | `plans.chat_models = NULL` 表示全部允许 | 新模型商品默认无授权 | **拒绝继承该语义**。023 的 `plans.chat_models` 列仅保留为旧 `/chat` 套餐级授权（Kaya 兼容面）；inference 权益使用显式模型集合，新模型默认无授权。`NULL 全放行` 不得进入 entitlement 判定 |
| 4 | 用量写入失败只记录日志 | 持久化预占、幂等结算、失败恢复 | **拒绝继承**。`ChatService.RecordUsage` 的 "insert 失败 log 并吞掉" 模式只适用于不计费的 022 流水；inference 结算走 Task 9 的事务化幂等结算，计费不可静默丢失 |
| 5 | 缺失 usage 写 0 | reported/estimated/unknown 状态 | **拒绝继承**。`BuildOpenAIPayload` 注释中"上游忽略 include_usage 则记 0"与 022 的 `tokens DEFAULT 0` 口径不进入 inference 账本；缺失用量必须标记 estimated/unknown 并进入核对 |
| 6 | 单笔用量事件（`llm_usage_events`） | 逻辑请求、上游尝试、计量事实与客户账本分开 | **拒绝继承该数据模型**。022 表不扩容为账本；inference 使用 `requests/attempts/usage_records/ledger_entries` 分层（Task 1/9）。`repo/llm_usage_repo.go` 的 `SumByModel` 半开区间聚合查询模式可被 Task 15 统计参考 |
| 7 | `float64` 模型价格（`InputPerMtok/OutputPerMtok`） | 定点金额与有理数/Decimal 换算、版本化售价及成本 | **拒绝继承 float64 计价**。可借鉴的是 `cost_micros` 整数微金额落库方向；inference 按设计 §7.1 用整数微额度 + Decimal/有理数价格 + 版本化 PriceVersion，禁止 float64 累计 |
| 8 | 随用户删除用量行（`ON DELETE CASCADE`） | 账本保留去标识化主体 | **拒绝继承**。022 的 `user_id ... ON DELETE CASCADE` 仅适用于该非账本流水；inference 账本/计量事实不级联删除，走去标识化 |

### 3.2 定向复用清单（文件与测试，归属 `feat/multi-model-gateway` 24 个提交）

改造后复用（协议适配与流式计量，移入 `internal/inference/providers/` 并补齐）：

- `internal/llm/openai.go`：`BuildOpenAIPayload`（已显式请求 `stream_options.include_usage`，符合设计 §7.1）、`UsageTracker`（增量 SSE usage 提取，与内容日志截断、relay 结束方式解耦——设计 Task 8 要求的能力）；测试 `openai_test.go`（含 split-across-reads、last-wins、无 usage、超长行丢弃）。
- `internal/llm/anthropic.go`：`BuildAnthropicPayload`（user/assistant 交替合并、tool_use/tool_result 翻译、非法形状前置 400）；测试 `anthropic_test.go`。
- `internal/llm/anthropic_stream.go`：`TranslateAnthropicStream`（Anthropic SSE → OpenAI chunk；异常终止不发 `[DONE]`、已消耗 token 的 usage 冲刷、`message_delta` 累计 usage 最新值生效）；测试 `anthropic_stream_test.go`。
- `internal/llm/keypool.go`：`KeyPool`（轮询 + 冷却）作为 Task 12 账号池的单实例起点；多实例/DB 协调、`fencing`、账号状态机为新增工作。测试 `keypool_test.go`。
- `internal/llm/catalog.go`：校验逻辑改造为 Task 3 导入器校验（见差距 1）。测试 `catalog_test.go`。
- `internal/handler/chat.go` 的 relay 机制参考：`relayChatSSE`（tracker 在写客户端之前 Feed）、`SetWriteDeadline` 覆盖全局 WriteTimeout、`chatUpstreamBrokeEvent` 注入、审计截断（`truncateUTF8`/`chatErrInputLogCap`）；`internal/service/chat.go` 的传输超时参数与 `cancelOnCloseBody`、`chatAccessTimeout` 闸门超时模式。改造点：计量回调从"吞错记一行"改为 Task 9 结算链。
- `GET /chat/models` 返回形状（`{code:0, data:{models:[{id,display_name,provider,default}]}}`）作为 Kaya 模型选择契约的对齐基准（设计 §9.1）。
- 旧 `/chat` 契约：`internal/model/chat.go` 的校验上限（消息 20 条/256 KiB、tools 16 个/32 KiB、session_id 64、model id 64）、错误 envelope 与状态码映射——facade 兼容的基线，但不把旧 20 条消息限制硬套标准 `/v1/*` 客户端。

不继承（除八条差距外）：`llm_usage_events` 表与 `LLMUsageService`/`GET /admin/stats/llm-usage` 不作为模型用量统计的正式实现——Task 15 的统计以 inference 账本为数据源；如需过渡统计可另行评估，但不进入售卖链路。

## 4. 复用评审：`feat/usage-analytics`

- 位置：`.worktrees/usage-analytics`，HEAD `3fdab4c`。
- **该分支内容已在主线**：`git diff 3fdab4c ec25565 --stat` 为空（两树完全一致），即 PR #17 以 squash 形式合入为 master `ec25565`。不构成"第三份用量统计实现"风险，无需挑选提交。
- 已合入能力（现属基线）：migration 021 `usage_events` 心跳流水（`(user_id, client_event_id)` 幂等键）；`POST /user/usage/heartbeat`；`/admin/stats/active`（DAU/WAU/MAU）、`/admin/stats/usage-duration`、`/admin/stats/new-users`；`UsageService` 的日期/粒度/groupBy 校验与半开区间查询模式。
- 与 Task 15 的重叠评估与决定：**模式复用、数据不继承**。Task 15（运营统计/成本分析）可复用其 admin stats 路由挂载、参数校验（`validateUsageGroupBy`/`validateUsageRange`）、半开区间聚合的查询模式；但统计的数据源必须是 inference 请求/账本表，**不得**使用 `usage_events` 心跳表（设计 §2 已认定心跳不能用于计费与模型用量）。`docs/runbooks` 无需为此分支另立迁移编号（021 已在主线）。

## 5. 最终迁移序列

- master 已应用至 `021_usage_events`。
- `022_llm_usage_events.sql`、`023_plan_chat_models.sql` 编号**保留**给 `feat/multi-model-gateway` 将来合入；inference 模块不得重用 022/023 表达不同 DDL。
- inference 及本计划新迁移从 **024** 起追加（编号可不连续，`migrations/README.md` 允许留空档）：

| 序号 | 文件（计划 `NNN_*` 占位的落实） | 所属任务 |
|---|---|---|
| 024 | `024_inference_catalog.sql` | Task 1 |
| 025 | `025_inference_accounts.sql` | Task 1 |
| 026 | `026_inference_accounting.sql` | Task 1 |
| 027 | `027_subscription_product_scope.sql` | Task 2 |
| 028 | `028_operator_permissions.sql` | Task 4 |
| 029 | `029_order_benefit_snapshot.sql` | Task 10 |
| 030 | `030_inference_wallet.sql` | Task 14 |

编码前若候选分支先合入或有热修插入，以"最新序号 +1 追加"为准重新分配；曾应用到任何环境的 SQL 不得修改（`migrations/README.md` 约定）。

## 6. 待业务方确认事项

- **"OOS / SUM TO API" 术语**：本环境无法向业务方求证。当前工作假设为 **OAuth / Sub2API 连接器**；若实为 OSS 自托管模型，按设计 §1 声明走同一上游部署接口，不影响模型目录与账本设计。**Task 12 启动前必须取得明确结论**（计划 Task 0 的该项复选框因此保持未勾选）。

## 7. 明确不成立的说法

本报告不包含"已有实现足够计费"的结论：候选分支的计量是"上游 200 即记一行流水、写库失败只记日志、缺 usage 记 0、float64 计价、随用户级联删除"——按设计 §2 全部不满足计费要求，仅协议适配、增量 SSE usage 提取与 relay 机制达到定向复用标准。
