# Kaya Coding Plan 运营面 runbook（Task 15；T9 补探测/预演/告警）

对应实现：`internal/inference/management/{bulk_import,pricing_preview,operations}.go`、
`internal/inference/httpapi/{admin_bulk,admin_usage}.go`、`admin_adjustments.go`（Task 14 建、
本任务扩展）、`internal/inference/management/{probe,catalog}.go`（T9：上游连通性探测与发布
预演）、`internal/inference/httpapi/kaya_models.go`（T9：模型能力描述）、
`internal/inference/quota/service.go` 与 `internal/inference/workers/settlement_recovery.go`
（T9：运行告警）。API 契约：`docs/api/kaya-coding-plan.openapi.yaml`（0.14.0）。

## 权限矩阵（server 端强制，自报无效）

| 端点 | 权限 | 角色 |
|---|---|---|
| `POST /admin/catalog/bulk-import`、`/admin/model-prices/preview`、`/admin/quota-policies/preview` | `models:manage` | admin / operator |
| `POST /admin/wallet/adjustments`、`/admin/wallet/reversals`、`GET /admin/wallet*`、`/admin/payg-config` | `billing:adjust` | admin |
| `GET /admin/model-usage/*`、`/admin/upstream-accounts/shared`、`/admin/model-adjustments` | `usage:read` | admin / operator / auditor |

每次批量导入 commit、补偿、冲正、PAYG 发布都追加审计（`inference_audit_log`），
人员及服务双重归因（`user:<uid>@app:<appid>`）；补偿与冲正的审计行与效果同事务
提交或回滚。

## 批量导入（POST /admin/catalog/bulk-import）

- **dry_run=true**：逐项预演（`would_insert` / `would_skip` / `error`），一行不写。
  部分错误的预览只在 dry-run 存在。
- **commit（dry_run=false）**：全部有效才落库；任一 item 出错 → 400 +
  `data.items` 逐项错误，**一行不写（绝不半发布）**。要导入有效子集：删掉
  错误项、换新 `task_id` 重提——不存在隐式的"只发布有效部分"。
- **task_id 幂等**：重复提交（哪怕文档不同）返回 200 + `replayed=true` 与
  首次已记录结果；不重复创建。自然键（provider code / model id /
  provider+upstream_model+base_url / model+deployment）已存在的项跳过不覆盖。
- **大小上限**：条目总数 ≤ 200（另有全局 1 MiB body 上限）。
- 落库即草稿（默认不可售）；补齐价格（sale_credit 版本）与授权后
  `POST /admin/catalog/publish` 发布。停用上游（status=disabled）不删除历史
  路由/尝试记录；有路由或历史尝试的部署不可 DELETE。

## 一账号一部署（显式运营约束）

**一个上游账号只挂一个部署。** 两部署共享同一上游账号时，故障切换会撞并发
租约唯一键（`inference_concurrency_leases` 的 (scope, scope_id, request_id)
UNIQUE，表现为 500）。调度语义不在线变更（Task 13 移交决定）；运营面用检测
视图发现违规：

```
GET /admin/upstream-accounts/shared?from=&to=&limit=
```

观测窗口（默认最近 7 天）内同一上游账号服务过 >1 个部署（attempts 落库事实）
即列出；命中应把账号拆到各自部署。

## 统计与成本口径（GET /admin/model-usage/summary）

- 数据源只有 `inference_requests / inference_attempts / inference_usage_records /
  inference_ledger_entries`；绝不使用 `usage_events` 心跳表。
- 客户消费为账本派生（charge − reversed ± adjustment），与客户面逐字同口径；
  聚合可与账本抽样核对（测试内置断言）。
- 采购成本来自 attempts 落库的 `cost_micros/cost_currency/cost_basis`：按
  **币种 × basis（reported/estimated/allocated）分片**，绝不跨币种合并；
  成本未知的尝试单列 `cost_unknown_attempts`。
- 供应商分组：请求级计数按**最终尝试**归属（重试不双计）；供应商维度不归属
  请求级账本金额（恒 0，金额权威维度是模型/客户）。
- 统计是只读派生路径：额度闸门继续用权威事务状态，统计不引入对写路径的
  共享锁。延迟统计不得用作实时放行依据。

## 异常筛选（GET /admin/model-usage/exceptions?kind=）

- `stuck_reservations`：held 超过 1 小时的异常预占（恢复 worker 的处理对象）。
- `reauth_required_accounts`：授权失效的上游账号（重新走 OAuth 授权后恢复调度）。
- `settlement_backlog`：pending/running/escalated 核对任务；`overdue=true` 是
  超期告警——**超期不等于自动释放**，必须人工/恢复流程核对。

## 补偿（POST /admin/wallet/adjustments 等）

补偿必须有原因、权限（billing:adjust）、幂等键；幂等键重放返回
`applied=false` 不重复补偿。追踪读面：`GET /admin/model-adjustments`
（usage:read）与 `GET /admin/wallet/adjustments`（billing:adjust），同一口径。

## 权益增补（entitlement-model-amendment，deploy PR #345）

模型发布的固定收尾动作：存量订阅的 `inference_entitlements.model_ids` 是
购买时快照的显式集合，新模型上线后需增补，否则存量用户调新模型 403、
picker 不可见。**全程无 SQL。**

**推荐发布顺序（红线 6）**：目录 publish + 配价 → 权益增补。反过来不炸
（增补了一个目录未发布的模型时，调用走 unknown model 400，`/chat/models`
只列已发布模型），但先增补会让用户权益里出现一个还调不通的模型，应避免。

### 端点

| 端点 | 权限 | 角色 |
|---|---|---|
| `POST /admin/entitlements/:id/revise`（R1 单权益修订） | `billing:adjust` | admin |
| `POST /admin/entitlements/amend-models`（R2 批量增补） | `billing:adjust` | admin |
| `GET /admin/entitlements/:id`、`GET /admin/entitlements`（R3 只读） | `usage:read` | admin / operator / auditor |

### R2 批量增补（模型发布主路径）

```
POST /admin/entitlements/amend-models
{"model_id":"kimi-k4","action":"add","selector":"all_active","reason":"发布 kimi-k4"}
```

- **dry_run 默认 true**：预演只返回将影响行数（`amended`）+ 抽样
  （`samples`，扫描顺序前 N 行含前后集合，默认 10、`sample` 上限 50），
  一行不写。正式执行显式传 `"dry_run":false`。
- **selector**：`all_active`（全部 active 权益）；`source_plan:<plan_id>`
  （按权益来源订阅的套餐过滤，命中**全部状态**行——revoked/expired 行
  不被修改，出现在 `skipped_detail` 明细，AC6）。按计费账户单个增补走
  R1（先 `GET /admin/entitlements?billing_account_id=` 定位）。
- **幂等（AC3）**：已含/已不含目标模型的行计入 `skipped`；重跑同一增补
  `amended=0`，无报错，不重复写审计。
- **行级隔离**：每行独立事务 + 行级乐观锁；乐观锁冲突计入 `conflicts`
  并进 `errors[]`（reason 前缀 `conflict:`），其它行故障也进 `errors[]`——
  **行级失败不置整体失败**，调用方解析 body 而非只看 HTTP 状态（N6 迁移
  工具先例）。`errors[]` 与 `skipped_detail` 明细各上限 500 行（超出见
  `errors_truncated` / `skipped_detail_truncated`）；计数始终是权威全量。
  `selector` 命中 0 行时 report 带 `note` 提示核对选择器（防 plan id 打错）。
  `all_active` 一次把候选集载入内存逐行处理，适用于万行级以内；更大规模
  分批按 `source_plan` 执行。
- **语义红线**：只改 `model_ids`；权益 ID/anchor/有效期不动，配额窗口
  used/reserved 不清零（AC8）；仅 active 可修订，绝不复活退役权益；
  生效即时（每请求查库，无缓存）。

### R1 单权益修订

```
POST /admin/entitlements/<id>/revise
{"add_model_ids":["kimi-k4"],"reason":"..."}   # 或 remove_model_ids
```

集合无变化 → 200 `changed=false`（幂等短路，不写库不审计）；权益非
active → 409；乐观锁冲突 → 409。`add` 与 `remove` 不得含同一模型（400）。
响应携带修订后完整视图（id/revision/model_ids/effective range/status）。

### 审计（AC5）

每次实际变更（`changed=true` / 每个 amended 行）落一条
`inference_audit_log`：`entitlement.revise` / `entitlement.amend`，含
actor 双腿归因、reason、前后 model_ids、前后 policy_version_id、前后
revision，与变更同事务提交。查询：

```sql
SELECT occurred_at, actor_user_id, actor_app_id, reason, detail
  FROM inference_audit_log
 WHERE action IN ('entitlement.revise','entitlement.amend')
   AND object_id = '<entitlement_id>'
 ORDER BY occurred_at DESC;
```

## 模型模态（input_modalities）：缺省放行与关闭视觉

`/chat` 的图片输入按模型 `input_modalities` 门控：**缺省/空 = 放行图片
（default-on，等效 `["text","image"]`）**；要关闭某模型的视觉能力，显式
声明 `["text"]`——未含 `"image"` 的显式声明收到带图请求一律 400
`the selected model does not support image input`（先于上游调用，不计费）；
`GET /chat/models` 透出该字段（空值归一为 `["text","image"]`）供 picker 判定。
注意：未声明模型的上游若实际无视觉能力，带图请求会在上游报错（400→502），
属预期暴露——确认上游支持视觉的模型才保持未声明或显式点亮。

**T9：`/chat/models` 还追加了能力字段**（`supports_images` /
`supports_tools` / `supports_thinking` / `context_tokens` /
`max_output_tokens` / `capability_description`）。它们是**派发判定**（当前
已发布入口 + 协议适配层能不能承载），不是模型条目的自我声明；因此一个
只有 `openai_responses` 部署的模型 `supports_images` 恒为 `false`。
`GET /v1/models` 保持原生基本形状，**不带**这些字段。

- **网关目录（本条路径）**：无需逐模型配置即可放行图片；要**关闭**视觉，
  `PATCH /admin/models/:id`（models:manage）设
  `{"input_modalities":["text"],"reason":"..."}`，然后
  `POST /admin/catalog/publish` 发布目录版本生效——`/chat/models` 与
  视觉门控都读已发布快照，未发布不生效。协议约束：`openai_responses`
  协议的部署**不接受图片**（适配层显式 `InvalidInput`，绝不静默丢弃），
  视觉模型只能挂 `openai_chat` / `anthropic_messages` 部署。
- **legacy `/chat` 目录**：`LLM_PROVIDERS_JSON` 中未声明
  `input_modalities` 的模型缺省放行；显式 `"input_modalities":["text"]`
  并重启即关闭。`DEEPSEEK_*` 合成的单模型目录（LegacyCatalog）在代码里
  **钉死 `["text"]`**（DeepSeek 上游无视觉模型），不随缺省放行。
- 客户端侧契约（parts 数组、≤4 张、单张 ≤1 MiB wire、合计 ≤3 MiB、
  magic bytes 校验）见 `docs/api-integration-guide.md`「Chat 接口」；
  `/v1/*` 对外协议面本期仍显式拒绝多模态（后续工作）。

## 配置支持集（T7：只支持已实现的那部分）

本期口径是**明确支持集 + 拒绝不支持的配置**：不实现的能力不收下（收下就是
「保存后假装生效」—— 运营以为配了，实际跑的是别的行为）。下面三处是运营会
碰到「配了但没生效/被拒」的入口。

### `pool_strategy`（路由的账号池策略）

| 写面 | 口径 |
|---|---|
| `POST/PATCH /admin/routes`（operator CRUD） | 只接受 `round_robin`；`least_loaded` / 通用 `session_sticky` **明确 400**（错误点名替代配置 `round_robin`） |
| `POST /admin/catalog/bulk-import`（批量导入） | 派生路由恒为 `round_robin`（文档里没有 pool_strategy 字段 —— 严格解码会拒绝未知字段） |
| `INFERENCE_CATALOG_JSON`（环境导入） | 派生路由恒为 `round_robin` |
| `POST /admin/catalog/publish`（正式发布） | 发布闸与上面同一口径：**拒绝**并点名路由 ID 与替代配置；`?dry_run=1` 的发布预演给出同一个错误 |

**存量已发布快照里的不支持值**：旧快照**仍可读**（读不出来整个目录就变砖），
但该路由被标记为**不可调度** —— 绝不静默当成 `round_robin`：

- 模型还有其它可调度路由 → 正常服务，只是那条路由不参与调度；运维侧收到
  告警（结构化日志 `UNSUPPORTED_POOL_STRATEGY` + `AUDIT_ALERT` 通道，动作
  `model.route.pool_strategy_unsupported`）。
- 模型的**全部**路由都不支持 → 客户请求在**预占/派发前**失败：HTTP 503
  `upstream_unavailable`（Anthropic 面 529），message 点名路由与替代配置；
  零预占、零出站、零计费。

**Responses 的 `previous_response_id` 会话链不受影响**：那条链路走的是会话
绑定器（`routing.SessionBinder`，按 session key 钉住上游账号），与路由的
`pool_strategy` 是两件事。

**修法**：把该路由的 `pool_strategy` 改成 `round_robin`（`PATCH /admin/routes/:id`），
再 `POST /admin/catalog/publish`。发布预演（`?dry_run=1`）会先告诉你哪条路由
不合格。

**客户面看到的**：`/v1/*` 是 503（`upstream_unavailable` / `service_unavailable`，
message 点名配置）；legacy `/chat` 是 503 `chat configuration unavailable`
（**不带 `Retry-After`** —— 配置不会自愈，重试没有意义）。两者都在预占前
失败：零计费、零出站。

### `capabilities`（路由的能力清单）

路由的 `capabilities` 列声明**这条路由承载哪些能力**。词表是**封闭**的：

| 能力名 | 含义 |
|---|---|
| `tools` | 这条路由承载带工具的请求 |
| `reasoning` | 这条路由承载带推理的请求 |
| `image` | 这条路由承载带图的请求 |

| 写面 | 口径 |
|---|---|
| `POST/PATCH /admin/routes`（operator CRUD） | 词表外的名字 **明确 400**（错误点名那个名字与支持集）；清单先归一（trim / 去空 / 去重）再落库，**不折叠大小写**（`Tools` 是拼写错误，要指出来而不是悄悄接受） |
| `POST /admin/catalog/publish`（正式发布） | 发布闸与写面同一口径：**拒绝**并点名路由 ID 与那个能力名；`?dry_run=1` 的发布预演给出同一个错误 |
| 已发布快照的**读**面 | **容忍**未知能力名（旧目录必须仍能加载，否则升级即砖整个目录）：未知名字**不授予任何能力**，存量行**不会被自动改写** |

**空清单 = 全部能力**（数据库默认 `'{}'`）：存量绝大多数路由是空清单，行为
不变。

**⚠️ 已发布的非空清单被有意收窄（T8）**：`image` 是一把**新钥匙**。存量
非空清单（例如 `["tools","reasoning"]`）里没有它，因此**不再承载带图请求**：

- 该路由上的带图请求 → 网关在**准入前**明确 400（零预占、零出站、零计费）；
- 若某个模型的**全部**路由都是这类清单 → 该模型不再 advertise 图片能力
  （`GET /chat/models` 的 `input_modalities` 与 legacy `/chat` picker 都只
  透出 `["text"]`），picker 因此不会让客户发图。

**修法**：把要承载图片的路由的 `capabilities` 显式加上 `"image"`
（`PATCH /admin/routes/:id`），再 `POST /admin/catalog/publish`。发布预演
（`?dry_run=1`）会先告诉你哪条路由不合格。

**客户面看到的**：带图请求是 400（`invalid_input`，Anthropic 面
`invalid_request_error`），message 点名模型与「没有能承载图片的部署」；
legacy `/chat` 是 400（能力不兼容哨兵，不是「上游拒绝」的 502）。都在预占前
失败：零计费、零出站。

### `weight`（同 priority 下的候选排序，不是比例分流）

`weight` 只决定**同 `priority` 组内**的候选顺序（降序）。它**不是**概率分流：
组内每个候选都留在候选列表里，只是顺序不同（本期不做比例分流）。`priority`
决定分组（数字小者优先 = failover 顺序）。

### `overage_policy`（配额策略的超额模式）

| 值 | 口径 |
|---|---|
| `reject` | 额度耗尽即停（默认） |
| `allow_overage` | 允许套餐外消费（走钱包，需客户显式开启） |
| `clamp_if_declared` | **未实现** —— 依赖客户端「显式声明允许钳制」的协议字段，本期协议层没有该字段。**新写一律拒绝**（400，错误点名 `reject` / `allow_overage` 两个可迁移目标） |

**存量策略行仍可查询**（`GET /admin/quota-policies`、详情页照常返回原值）——
读面不做隐式改写，**也不自动迁移旧策略**。新推理引用到该模式时会明确报
「策略配置不可用」（HTTP 503 `service_unavailable`，message 点名策略名与
版本），**绝不静默当 `reject`**（那会悄悄改变客户的额度语义）。

**人工迁移步骤**（不自动改旧策略）：

1. `GET /admin/quota-policies?name=<name>` 找到仍在使用 `clamp_if_declared` 的
   已发布版本（`overage_policy` 字段）。
2. `POST /admin/quota-policies` 用**同一个 name** 出一个新 draft，把
   `overage_policy` 改成 `reject`（保持客户语义）或 `allow_overage`（若客户
   已开启钱包），其余限制项照抄。
3. `POST /admin/quota-policies/:id/publish` 发布新版本（旧 published 自动转
   superseded）。
4. 既有权益**钉住**它们发放时的策略版本 —— 已发放的权益继续按旧版本执行，
   新发放的权益用新版本。老版本要收口时用 `POST /admin/quota-policies/:id/retire`
   （有活跃权益引用时默认 409，`?force=true` 放行）。

### 部署超时：`connect_timeout` 与 `request_timeout`

| 字段 | 管什么 | 不管什么 |
|---|---|---|
| `connect_timeout` | **建连阶段**：DNS 解析 + TCP 建连的上限 | 已建立的连接（长 SSE 流不被它截断） |
| `request_timeout` | 请求**总时长**（整条响应/流的上限） | 建连之外的调度细节 |

- 两者都在部署写入时校验为正数；`request_timeout` 还必须低于生效的
  `INFERENCE_RECOVERY_GRACE`（默认 15m）。
- **缺省值**：落库缺省 `connect_timeout_ms = 5000`（5s）、`request_timeout_ms
  = 600000`（600s）；写路径（operator CRUD / env 导入 / 批量导入）未声明时
  补同样的值。
- **存量部署的有效建连预算从 10s 变成 5s**：`connect_timeout` 过去是死字段
  （共享客户端写死 10s 拨号超时），现在按落库值执行 —— 落库是 5s 的那些
  部署（也就是绝大多数没显式配过的）实际少了 5s 建连预算。慢但可达的上游
  可能因此撞上建连超时；处置办法是**调大 `connect_timeout`**
  （`PATCH /admin/deployments/:id` → 重新 publish），不是等重试。
- 建连超时只影响「还没连上」的请求：连接已建立后，流的存活只看
  `request_timeout`（长 SSE 流不会被 `connect_timeout` 截断）。
- **建连超时的当前口径是保守的**：传输层把它归入「上游执行状态未知」，
  请求进核对（保留预占）、**绝不盲目重放到下一候选** —— 这是仓库的核心
  钱账安全约束（不确定是否已执行的上游请求不得重放）。因此把
  `connect_timeout` 配得过小会让请求**停在核对**而不是干净地切换候选；
  运维配这个值时按「宁可配大一点」处理。若要让建连超时可切换候选，需要
  单独任务（改的是执行状态判定，不是超时口径）。

## 发布预演（`POST /admin/catalog/publish?dry_run=1`）

预演与正式发布**共用同一套抽取与校验、同一事务快照**，差别只有「写不写库」。
正式发布**不跑任何探测**、**不引入网络依赖**。

### 报告结构

| 段 | 回答什么 |
|---|---|
| `would_publish` | 候选集摘要：`active_deployments` 是**真正会承接流量**的部署数；`disabled_providers` / `deployments_behind_disabled_providers` 如实汇总停用的 provider 与被它带走的部署 |
| `probes[]` | 每条**会生效**部署的探测结论（语义见下节） |
| `issues[]` | 发布前**可判定**的问题：`severity=blocker` 是配置问题，`severity=hint` 是网络探测提示 |
| `summary` | 判定摘要：探测结论分桶 + blocker/hint 计数 + `all_probes_ok` |

### 停用 provider 的部署既不算 active，也不被探测

`PATCH /admin/providers/:id`（`status=disabled`）→ 重新 publish 后，该 provider
及其**全部**部署退出轮转。预演因此：

- **不**把它们算进 `would_publish.active_deployments`；
- **不**对它们出站探测（一条永远不会承接流量的部署不值得一次外呼）；
- 在 `issues[]` 里给一条 `provider_disabled`（**blocker**）；
- `would_publish.disabled_providers` 如实给出停用 provider 的数量。

> 这是 T3 携带到 T9 的必修发现：预演此前只按 deployment 状态过滤，把停用
> provider 背后的部署当成「会生效」并真的探测。

### 问题分类：可行动的配置发现 vs 网络探测提示

**整份 `issues` 清单都是 advisory**：预演报告**不**参与正式发布的判定。
正式发布**不读**这份报告。

| severity | 含义 | code |
|---|---|---|
| `blocker` | **可行动的配置 / 可用流量发现**：发布这份目录后，某个模型/路由**跑不起来**。要么修好再发，要么明确接受 | `provider_disabled`、`deployment_no_account`、`route_capabilities_narrowed`、`model_not_dispatchable` |
| `hint` | **网络探测提示**：advisory，也**不代表**模型故障 | `probe_failed`、`probe_unsupported`、`probe_unconfirmed`、`account_inventory_unavailable` |

- `deployment_no_account`：该 provider **没有可用上游账号** → 这条路由无法
  出站（可判定的事实，不是猜测）。
- `account_inventory_unavailable`：账号清单**读不到** → 该判定被**跳过**，
  报告说「未知」，**绝不**谎称「无账号」。此时
  `summary.account_inventory_checked` 为 **false**（它表示「无账号判定对
  每个 provider 都真的做了」，**不是**「读过一次」）。
- `route_capabilities_narrowed`：路由的能力清单收窄了模型声明的能力（存量
  非空清单常常不含 `image`）→ 带图请求在**准入前**被 400 拒绝。
- `model_not_dispatchable`：模型在这次发布后**没有任何可调度路由**。

**真正会拒绝发布的是共享发布闸的硬校验**：草稿里**不支持**的策略/路由配置
（不在支持集的 `pool_strategy`、词表外的路由 `capabilities`）由**发布闸**
拒绝 —— 预演与正式发布给出**同一个错误**（点名对象 ID 与替代配置，
HTTP 400），因此它出现在**错误**里而不是 200 报告里。

**HTTP 200 + 一条 `blocker` ≠ 正式发布会被拒**：本期**不为** `blocker` 类
发现加硬闸。停用 provider / 无账号 / 能力收窄由目录发布态与调度判定负责
（停用的 provider 本来就不会承接流量），预演只负责把它们**如实说出来**，
由运营决定修还是接受。

### HTTP 200 不等于每个 probe.ok 都是 true

200 只说明「预演报告生成成功」。逐条结论看 `probes[].probe_outcome`，汇总看
`summary`（`probes_ok` / `probes_failed` / `probes_unsupported` /
`probes_unconfirmed` / `all_probes_ok`）。

## 上游连通性探测（`POST /admin/deployments/:id/probe`）

只读、零写库、**默认零付费**：探测读的是供应商的**模型列表端点**
（`GET {base_url}/models`），不是推理端点。

### 三件事必须分开看

| 维度 | 字段 | 取值 |
|---|---|---|
| 凭据**材料**测试 | `credential_state` | `decrypted` / `missing` / `unusable` |
| **指定**连通性探测 | `probe_method` + `probe_outcome` | `connectivity`+`verified` / `failed` / `unsupported` / `unconfirmed` |
| 本次**没有**做真实推理 | `inference_verified` | 恒为 `false` |

- `ok=true` **只**表示「这条部署的上游连通」，**绝不**表示「模型答过」——
  本次没有发起任何推理（`inference_verified=false` 显式发布出来，免得被
  读成「模型健康」）。
- **供应商不提供 `/models`** 时结论是 `unsupported`：此探测方式**无法确认**，
  **不是**模型故障（`base_url` 缺一段路径也会得到同样的 404 —— 两者在报告里
  都说清楚，运营据 `status=404` 与文案一起判断）。
- 没得到任何答复（DNS 失败、拒绝、超时、egress 拒绝、凭据不可用、探测没跑）
  一律是 `unconfirmed`。

### 鉴权/版本头按协议设置

与**派发路径同一个适配器**的同一份鉴权规则：

| 协议 | 探测头 |
|---|---|
| `openai_chat` / `openai_responses` | `Authorization: Bearer <access token 或静态密钥>` |
| `anthropic_messages` | `x-api-key: <静态密钥>` + `anthropic-version: 2023-06-01` |

- OAuth 凭据在 OpenAI 兼容协议上走 bearer，且**只有 access token** 出边界
  （密封闭包与 refresh token 永远不出 credentials 包）。
- **`anthropic_messages` + OAuth 是明确不支持的组合**：出站前拒绝，绝不把
  OAuth 令牌塞进 API-key 槽。该协议的支持集里没有 OAuth 供应商（仓库自有的
  OAuth 连接器说 OpenAI 兼容协议；Anthropic 官方 claude.ai OAuth 需要本部署
  不发的 `anthropic-beta` 头）。

## 运行告警（复用现有结构化日志；未引入新监控平台）

仓库唯一的监控通道是**结构化日志行**（`log.Printf`）。下表是本期明确化的
四类运行告警 —— 每类都给出**可 grep 的稳定前缀**，运维按前缀配告警规则即可。

| 告警 | 日志前缀 | 触发条件 | 处置 |
|---|---|---|---|
| **非幂等结算冲突** | 网关侧**只有**这三条带原因词：`ALARM gateway: settlement_mismatch` / `ALARM gateway: ledger_drift` / `ALARM gateway: late_usage_after_release`（另有 `ALARM gateway: request … charge … exceeds hold …` 的超额告警） | 同一请求的**重复结算金额不一致**、账本无 charge 行、已终态请求收到晚到用量 —— 这些**不是**幂等重投，绝不当成功吞掉 | 请求进入专门核对（`inference_reconciliation_jobs`），**只叫人**；人工处置见 `GET /admin/model-usage/exceptions?kind=settlement_backlog` → 每个任务的 `reason` |
| **缺预占 / 预占漂移**（与上一行**同一族**，但**没有** `ALARM gateway:` 前缀） | 网关打的是 `gateway: settlement failed for <request id>: … — parking in reconciliation`（**行里没有原因词**）；另有一条 `ERROR gateway: settlement failed for request <id>: … (parked for reconciliation)` | 预占数与准入不符（`reservation_drift`）、缺预占（`missing_reservation`）、消费事实未知（`unknown_usage`）—— 请求被**停放**（保留预占、绝不记零），不是终态异常 | **按原因查**，不要按日志前缀查：`GET /admin/model-usage/exceptions?kind=settlement_backlog` 的 `reason` 字段（`reservation_drift` / `missing_reservation` / `unknown_usage` / …）。**网关从不为这两个原因打带 `ALARM gateway:` 前缀的行** —— 该前缀只属于上一行那三条终态异常 |
| **未知用量积压** | `ALARM inference recovery: unknown-usage backlog` | 消费事实未知的请求积压 **≥ 10** | 这些请求按**保守估算**入账（绝不静默记零），真实用量要靠人核对；按 `usage_status='unknown'` 查请求，逐笔核对。**阈值目前是代码内的常量（默认 10）**：服务端**没有**暴露 env/config 旋钮，运维改不了它（详见下方说明） |
| **限速存储失败** | `ALARM quota: rate limit store unavailable` | 限速闸门的存储不可用（**不是**限速拒绝） | 准入 **fail-closed**（503 `service_unavailable`，绝不回退成单机放行）；修存储后自动恢复。正常的限速拒绝**不打**这条告警（否则告警会被噪声淹没） |
| **不支持存量配置** | `UNSUPPORTED_POOL_STRATEGY model=… routes=… strategies=…` + `AUDIT_ALERT` | 已发布快照里存在支持集外的 `pool_strategy`（存量脏配置） | 该路由被标记**不可调度**（绝不静默当 `round_robin`）；模型还有别的可调度路由时正常服务，全部不支持时客户请求在**预占前** 503 `upstream_unavailable`（Anthropic 面 529）。修法：把路由的 `pool_strategy` 改成 `round_robin` 再 publish |

**三类告警的稳定前缀不是同一族**：`ALARM gateway: …`（网关的终态异常）、
`ALARM inference recovery: …`（恢复 worker 的期限/账本/积压）、
`ALARM quota: …`（限速存储故障）。配规则时按**前缀族**分别配置，不要假设
一个前缀能覆盖另一族。

**未知用量积压阈值目前是「代码内常量」**：`RecoveryConfig.UnknownUsageAlarmThreshold`
的缺省值是 **10**，而**服务端没有把它接到任何 env / 配置项**上 —— 因此
运维**无法**在运行期调整它，也没有对应的 env 旋钮（本期不新增）。想改
阈值只能改代码重新部署；把该字段设成负数会**关闭**这条告警，但那只对
**自建嵌入**与**测试**有意义，生产服务端永远走缺省值 10。

另有两类**期限/账本**告警沿用既有前缀：`ALARM inference recovery:
reconciliation job … past deadline`（核对任务超期升级）与
`ALARM inference recovery: ledger mismatch window …`（账本重建差异）。
