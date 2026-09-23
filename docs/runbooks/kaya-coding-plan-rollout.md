# Kaya Coding Plan — 发布演练 Runbook

> Task 16 交付。覆盖：迁移/部署顺序、灰度计划、发布开关与回滚纪律、
> 升级演练实测、故障演练实测、压测实测与容量结论、真实客户端联调前置
> 待办、商品/上游/历史用户迁移的现状与遗留。
>
> 本文件所有"实测记录"均来自 2026-09-09 一次性 PostgreSQL 16.14
> （本机端口 55449，测毕销毁）上的真实运行；重跑命令随节给出。

## 0. 发布前置检查（每次发布必过）

1. **迁移先于服务流量。** 目标环境先跑 `go run ./cmd/migrate`（幂等；
   连跑两次第二次应 `applied=0`），确认 `_migrations` 与代码期望一致
   再滚动服务。**尤其 027（`subscriptions.product_code`）**：新代码的
   登录/聊天鉴权/支付路径直接引用该列，二进制先于迁移滚动会造成大面积
   500——027 未应用的库**不得**滚动双产品代码（Task 2 部署顺序约束）。
   同理 024–034 任一缺失都不得放行对应功能流量。
2. 迁移连跑命令：
   ```bash
   DATABASE_URL=... go run ./cmd/migrate        # 第一次：applied=N
   DATABASE_URL=... go run ./cmd/migrate        # 第二次：applied=0 skipped=32（幂等）
   DATABASE_URL=... go run ./cmd/migrate -status
   ```
3. 升级异常诊断：027 含异常数据可读诊断 DO 块（重复 active 订阅等会在
   迁移日志给出具体行），迁移失败**不得**绕过诊断强推。
4. 基线与连接器评估：`docs/runbooks/kaya-coding-plan-baseline.md`、
   `docs/runbooks/kaya-coding-plan-connector-evaluation.md`；
   运营操作：`docs/runbooks/kaya-coding-plan-operations.md`。

## 1. 灰度计划（四阶段，逐阶段开门）

| 阶段 | 内容 | 开门条件 | 退出/观察指标 |
|---|---|---|---|
| G0 影子计量 | 新网关代码上线、商品关闭、无新流量；恢复 worker/健康 worker/结算链空转 | 迁移全部应用（§0）；`INFERENCE_KAYA_CHAT_GATEWAY` 关；无 coding-plan 商品配置 | 日志无持续错误；恢复 worker 零积压 |
| G1 内部权益账户强制配额 | 运营 SQL 给内部账户发 grant 权益（`source_type=grant`），内部 API Key 走全量配额/结算链 | G0 至少一个恢复周期无异常；目录已发布（模型+价格+策略） | 三窗口视图与账本逐笔一致；耗尽→429→重置循环正确 |
| G2 小范围购买 | coding-plan 商品配置（`plan_benefit_configs`）+ 打开 `accepting_new_subscriptions`，白名单渠道小额真实支付 | G1 期间无对账差异；支付渠道 mock 之外的真实回调链路验证完成 | 购买→权益→调用→退款全链路幂等；outbox 无永久滞留 |
| G3 逐步开放 | 分 app/分渠道放量；观察压测结论（§6）设定 RPM 与池 | G2 无财务差异；支持 runbook 就位 | 结算延迟/积压/未知 usage 率稳定 |

**每步灰度前重新执行 §0 迁移检查**（尤其滚动到新实例组时）。

## 2. 发布开关清单

| 开关 | 位置 | 默认 | 效果 |
|---|---|---|---|
| 新商品售卖 | `plans.is_active` / `plans.accepting_new_subscriptions` + `plan_benefit_configs`（DB） | 关闭 | 无支付配置的 coding-plan 商品不可购买（服务端强制） |
| Kaya /chat 网关接管 | `INFERENCE_KAYA_CHAT_GATEWAY`（env） | 关（旧 DeepSeek 直通） | 开启后 `/chat` 走 inference 网关；**必须**配 `KAYA_CHAT_MODEL` |
| OAuth 连接器 | `INFERENCE_OAUTH_CONNECTORS_JSON`（env） | 空 = fail closed | 空注册表时授权端点拒绝一切 connector key |
| 超额消费（套餐外） | 策略 `allow_overage`（产品门）+ 客户 `PUT /user/wallet/overage`（客户门，默认关，开启必设 UTC 自然月上限） | 双门默认关 | 未开启时耗尽即停，余额一分不动 |
| PAYG（无套餐按量） | `payg_config` 单行（`GET/PUT /admin/payg-config`，billing:adjust）+ 客户 `POST /user/wallet/payg` | 未发布 | 未发布时客户开启返回 404/409 |
| 账户/Key RPM | `INFERENCE_ACCOUNT_RPM`（默认 120）+ Key 级 `rpm_limit` | 进程内 | 进程内滑动窗口（多实例≈配置×实例数，见 §6 结论） |
| 上游出口 | `INFERENCE_UPSTREAM_ALLOWLIST`（env） | 空 | SSRF allowlist；部署 URL 写路径强校验 |
| 凭据密钥 | `INFERENCE_CREDENTIAL_KEYS`（env） | 空 | 未配置时凭据写面不可用（fail closed） |

**关闭新流量时的纪律**：
- 关闭商品/网关/OAuth 只影响**新**请求/新授权；**在途请求继续结算**
  （detached context 结算不随流量开关或客户端断开丢失；恢复 worker
  兜底滞留预占）。不得为"止血"直接清空预占/窗口/账本行。
- **回滚纪律**：回滚只回退二进制与开关；**不删除账本**
  （`inference_ledger_entries`/`inference_adjustments`/钱包分录一律
  追加型，错误用冲正/补差分录修正）；**不恢复全局单订阅假设**
  （002 的全局 `idx_subscriptions_user_active` 已被 027 的
  `(user_id, product_code)` 部分唯一索引取代，回滚不得重建旧全局
  索引）；不回滚迁移文件本身（已应用的 SQL 不可修改，修正走新序号）。

## 3. 升级演练（合成历史库，实测记录）

**目的**：验证含真实历史数据的 021 基线库可平滑升到 034，且候选分支
022/023 已应用/未应用两条路径都安全。

**合成历史数据**（021 年代结构）：空 `expires_at` 历史订阅行、原会员
（kaya-membership 前身 monthly 订阅，正常有效期）、历史 expired/cancelled
订阅、未完成订单（pending 无支付）、已支付+退款记录（payment paid +
refund paid + order refunded）、失败订单。种子 SQL 全文：
`/tmp/kaya-task16-pg/seed-legacy.sql`（演练临时文件；本节后附要点）。

**路径 A：022/023 未应用**
```bash
# 基线：仅 001–021（MIGRATIONS_DIR 指向 21 个文件的目录）
MIGRATIONS_DIR=<mig-021> DATABASE_URL=.../kaya_upg_a go run ./cmd/migrate
# → [migrate] applied=21 skipped=0
psql ... -f seed-legacy.sql
# 升级：主线全量（001–021 + 024–034 共 32 文件）
DATABASE_URL=.../kaya_upg_a go run ./cmd/migrate
# → [migrate] applied=11 skipped=21（024–034 应用）
DATABASE_URL=.../kaya_upg_a go run ./cmd/migrate
# → [migrate] applied=0 skipped=32（幂等）
```
实测断言（全部通过）：
- `_migrations` = 32 行；
- 全部订阅 `product_code='kaya-membership'`（027 回填完成）；
- 空 `expires_at` 行保持 NULL（017 语义：不回填）；
- pending/refunded/failed 订单、payment、refund 行逐字保留（029 新增
  快照列对存量为 NULL）；
- users=3 行不变；034 表组已建立；
- 升级后新不变式真实生效：双产品共存可插入、同产品重复 active 被
  `idx_subscriptions_user_product_active` 拒绝、套餐↔订阅产品不一致被
  触发器拒绝（实测错误：`subscriptions.product_code kaya-membership does
  not match plan cp_smoke (product_code coding-plan)`）。

**路径 B：候选分支 022/023 已应用**
```bash
# 基线：001–021 + 候选分支 022_llm_usage_events / 023_plan_chat_models
# （022/023 SQL 取自 feat/multi-model-gateway 分支原文件）
MIGRATIONS_DIR=<mig-023> DATABASE_URL=.../kaya_upg_b go run ./cmd/migrate
# → applied=23（含 022/023，账本记录之）
# 灌入候选分支数据：llm_usage_events 一行 + plans.chat_models 设置
DATABASE_URL=.../kaya_upg_b go run ./cmd/migrate   # 主线全量
# → [migrate] applied=11 skipped=21（022/023 不在主线文件集，账本行原样保留）
DATABASE_URL=.../kaya_upg_b go run ./cmd/migrate   # → applied=0 skipped=32
```
实测断言（全部通过）：路径 A 全部断言 + `_migrations` = 34 行
（022/023 账本行保留）+ `llm_usage_events` 数据行保留 +
`plans.chat_models` 保留。

**结论**：两条路径均可在线升级到 034，历史数据零变更（除 027 按设计
回填 product_code），候选分支数据面不受主线迁移影响。迁移执行器对
022/023 的账本行不做任何动作（主线无对应文件，不报错、不清理）。
027 应用时输出一条 `trigger "trg_subscriptions_plan_product" does not
exist, skipping` NOTICE（`DROP TRIGGER IF EXISTS`，预期行为）。

## 4. 故障演练（实测记录）

可重复运行的演练测试：`tests/integration/kaya_failure_drill_test.go`
（真实 PG + mock 上游；复用 Task 9 恢复 worker 与 Task 13 断流语义）。
重跑：
```bash
DATABASE_URL=... go test -race -count=1 -run 'TestDrill_' ./tests/integration/
# 实测（2026-09-09）：ok 9.3s，7 场景全绿
```

统一终态不变量（每场景结束断言，见 `drillInvariant`）：
① 非终态请求必须有打开的核对任务（明确待核对，不得悬空）；
② 终态请求不得残留 held 预占；③ 账本重建聚合与窗口一枚。

| 场景 | 注入 | 实测结果 |
|---|---|---|
| 协议 | 正常 SSE 完成 | `settled/reported`，17 micro（7 in×1 + 5 out×2），恰一条 charge |
| 并发 | 8 路并发抢五小时窗口（limit=100，hold 31/次） | 放行/429 混合合计 8；窗口 used≤100 且 reserved=0；被拒请求零痕迹；账户共同封顶无部分预占 |
| 断流（有 usage） | 内容+usage 后无 [DONE] EOF | `settled/estimated` 按已读实际量 17 micro（不漏记） |
| 断流（无 usage） | 仅内容即 EOF | 请求非终态 + `unknown_usage/pending` 核对任务 + 预占保留 + 零 charge（不记零、不静默释放） |
| 进程重启 | 预占后崩溃 / dispatching 后崩溃；新 worker 实例恢复 | 未发送→released（窗口撤销、零 charge）；可能已发送→保守估算 50000 入账 + `crash_recovery` 任务；第二 pass 幂等零效果 |
| 数据库短暂不可用 | 测试内 TCP 代理 cut()（池拨号 ECONNREFUSED） | 故障期调用 500 fail-closed、零请求行；heal 后调用恢复 settled |
| 上游耗尽 | 唯一账号 `quota_remaining=0`（reset +1h） | 调度跳过 → 503 `upstream_unavailable` + 预占释放、零扣费；reset 过期后调用恢复 |

数据库故障注入说明：测试使用进程内 TCP 代理实现确定性的连接层故障
（不依赖 superuser 或实例控制权限；`CONNECTION LIMIT 0` 对 superuser
无效、`pg_terminate_backend` 会被池透明重连吸收，均已在开发中实测排
除）。现场演练可用 `pg_ctl stop -m immediate && pg_ctl start` 在独立
实例上重复同一场景（预期相同结论：fail-closed + 零痕迹 + 自愈）。

## 5. 升级与故障之外的支付幂等回归（既有套件实测）

- 重复支付/重投 webhook：e2e `TestCodingPlan_PurchaseToQuotaEndToEnd`
  与 `TestKayaCodingPlan_FullChain`（重投后 outbox 恰 1 条、worker 第
  二轮零效果）；
- 全链路：`购买→发 Key→调模型→三窗口查询→配额耗尽(429+Retry-After+
  blocked_by=five_hour)→窗口期后正确重置（旧窗口保留/新窗口激活）→
  续费（有效期顺延、权益不重复发放）→退款（权益吊销、调用 403
  model_not_allowed、账本行数不变）`，见
  `tests/e2e/kaya_coding_plan_test.go`，实测通过。

## 6. 压测（实测记录与容量结论）

可重复运行（opt-in）：`KAYA_LOAD=1 go test -count=1 -run 'TestLoadDrill_'
./tests/integration/`。环境：本机一次性 PostgreSQL 16.14（同机、
socket/TCP 直通），网关/存储池 `MaxOpenConns=20`，单账户单模型，
mock 上游零延迟——**数字只代表本机量级，用于判断瓶颈位置而非容量
承诺**。

**吞吐相位**（24 worker × 10 次非流式 = 240 次）实测：
- 吞吐 **145.6 req/s**，0 失败；延迟 p50=160ms / p95=227ms / max=252ms；
- **结算延迟**（响应返回→请求行终态）p50=159µs / p95=255µs——结算与
  响应基本同步，无积压；
- **连接池**：WaitCount +4059 / WaitDuration +6.3s（20 连接饱和，
  请求在池队列排队——第一瓶颈是池大小，不是行锁）；
- **DB 锁等待**：峰值 19 个会话等锁（50ms 采样；pg_locks 未授予峰值
  同为 19）——单账户准入按账户行锁串行（设计内：账户并发租约/锁序
  的封顶机制），无死锁、无失败。

**长连接相位**（32 条并发慢速 SSE × ~1s）实测：
- 全部完成（1.246s），0 失败，全部正确结算；
- **SSE 内存**：堆增量 2986 KiB ≈ **93 KiB/并发流**；
- 池 WaitCount +282 / WaitDuration +1.46s；锁等待峰值同为 19（准入
  突发）。

**结论（是否另立 Redis/独立网关任务）：本期不另立。** 依据：
1. 正确性不依赖任何外部协调器：并发封顶/恢复/核对全部以 PG 事务与
   租约为权威；145 req/s 单账户量级下无失败、无账本差异；
2. 第一可伸缩轴是连接池大小（建议池 ≥ 预期并发准入数，DB 连接预算
   ≈ 2–3× 峰值并发请求）与多实例水平扩展（账户行锁只串行同一账户，
   账户间天然并行）；
3. 已知进程内限制（不影响正确性，多实例部署时接受或后续治理）：RPM
   为进程内滑动窗口（多实例≈配置×实例数，Task 5 已记录）；上游账号
   冷却为进程内（数据库租约兜底，Task 8 已记录）；
4. SSE 内存 ~93 KiB/流 ⇒ 1 万并发流 ≈ 1 GiB 堆——**网关内存先于 DB
   成为上限**；需要 >1 万并发流或精确多实例 RPM 时，再立"Redis 协调/
   独立网关层"任务（候选范围：RPM 集中化、账号池冷却共享、会话亲和）。

## 7. 真实客户端联调（发布前置待办）

本环境无真实 Claude Code/Codex 客户端与真实上游。**契约级核对已完
成**：`tests/integration/kaya_client_contract_test.go` 以官方 SDK 请
求形状 fixture（`docs/api/fixtures/client/`）核对三面——Anthropic
Messages（Claude Code 形状：system + custom tools + stream）、OpenAI
Responses（Codex 形状：instructions + input items + store:false）、
OpenAI Chat Completions（tools + stream_options.include_usage）——
全部被接受且各自原生流式终止语义正确；已知差异 fixture 验证明确
400 + 原生错误形状 + 零痕迹。

支持版本与已知差异（已写进 OpenAPI 能力矩阵与集成指南）：
- `anthropic-version: 2023-06-01`（Messages 子集）；
- `truncation:"auto"` → 400（不做服务端上下文管理）；
- `GET /v1/models` 不提供 Anthropic 形状（路径冲突）；
- 历史 thinking/redacted_thinking 块接受但不回放（交错 thinking 连续
  性不在本期；联调若命中需在 `model.ChatMessage` 增回放通道）；
- 多模态块/服务端工具（web_search/computer/bash/code_execution/mcp）
  → 400。

**发布前置待办（G2 之前必须完成）**：
1. 用真实 Claude Code 与 Codex 客户端指向 staging 网关跑通：登录态
   Key 配置、流式长会话、工具调用多轮、中断/重连、超额 429 展示；
2. 真实上游拨测（凭据 test 端点当前为结构化占位；Task 12 记录的测
   试账号清单待业务方提供）；
3. "OOS / SUM TO API" 术语的业务方确认（Task 0 起保持未决；连接器
   评估 runbook 中的 OOS=OAuth 为显式标注的工作假设）。

## 8. 商品 / 上游可售范围 / 历史用户迁移（现状）

- **商品实际配置**：截至本演练，coding-plan 无任何生产
  `plan_benefit_configs`/`plan_upgrade_rules` 配置——**保持关闭**
  （不凭文档示例直接售卖；验收矩阵"新商品没有支付配置时不可购买"
  有测试钉牢）。G2 前由运营按 §1 配置并走价格/策略预览
  （`/admin/model-prices/preview`、`/admin/quota-policies/preview`）。
- **上游来源可售范围**：连接器评估完成（Sub2API 0.2.3 LGPL-3.0 /
  CLIProxyAPI v7.2.155 MIT，选型=自实现，见 connector-evaluation
  runbook）；真实上游可售模型清单与测试账号待业务方提供（未提供前
  不发布任何 OAuth 连接器注册表，空注册表 fail closed）。
- **历史用户迁移权益**：迁移赠送走独立幂等来源键
  （`migration_gift`，Task 10 语义）；本演练未发现需要迁移的历史
  coding-plan 用户（产品尚未售卖）。kaya-membership 存量订阅已由 027
  回填并验证（§3）。若后续决定给既有会员赠送 coding-plan 额度，按
  Task 10 的迁移赠送路径执行，赠送默认不叠加、不自动兜底。

## 9. 遗留清单（显式不关闭）

| 项 | 状态 | 归属 |
|---|---|---|
| OOS / SUM TO API 术语确认 | **未决**（计划 Task 0 复选框保持未勾选） | 业务方 |
| 真实上游验收（测试账号/拨测） | 未决（凭据 test 端点为结构化占位） | 业务方 + 发布前置 |
| 真实客户端联调 | 契约级核对完成；真实联调未做 | 发布前置（§7） |
| 商品实际配置 | 未配置，保持关闭（§8） | 运营（G2 前） |
| 价格/策略版本发布的管理面端点 | 预览只读；落库走运营 SQL | 后续任务 |
| `plan_benefit_configs`/`plan_upgrade_rules` 管理面 | 运营 SQL 维护 | 后续任务 |
| outbox 永久失败消息死信态 | 慢车道重试 + pending 可查 | 后续任务 |
| 窗口核对全量扫描分页 | 数据量大后再做 | 后续任务 |
| UsageVerifier 生产实现 | 主流上游无按请求查询 API，保持估算+核对 | 视供应商能力 |
| Key scope 租约 / TPM / session_sticky 调度 | 未接（进程内冷却+DB 租约兜底） | 后续任务 |
| PayPal 渠道 coding-plan 跨档改签 | 受既有 PayPal 双扣守卫整体拒绝 | 后续任务 |
| 多实例 RPM 精确化 / Redis 协调层 | 本期不另立（§6 结论与触发条件） | 触发后再立 |
| kin-openapi schema 级校验 | **不引入**（Task 16 决定，见任务报告） | Website SDK 生成阶段再评估 |
