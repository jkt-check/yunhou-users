# Kaya Coding Plan — Website 对接交付（后端契约）

> 面向 Website/BFF 团队的交接文档。范围约定（控制者决定）：**本任务只验收
> 后端契约**；页面实现属 Website 工作项，不在本仓库交付范围内。本文不声明
> 任何页面已完成。
>
> 权威契约文件：`docs/api/kaya-coding-plan.openapi.yaml`（0.13.0，客户面
> 逐字段对齐实现）+ `docs/api/fixtures/`（响应 fixture）。本文是导航与约
> 定汇总；字段级真相以 OpenAPI 为准。

## 1. 架构与身份传递（Cookie/BFF）

```
浏览器 ──Cookie──▶ Website BFF ──JWT──▶ yunhou-users（本仓库）──▶ 上游模型供应商
```

- 浏览器与 BFF 之间用 **HttpOnly Cookie 会话**（Website 工作项自定）。
- BFF 与本服务之间用 **JWT**：`Authorization: Bearer <access_token>`。
  BFF 负责把 Cookie 会话换成本服务 JWT 并附加到每个 `/user/*` 请求。
- **BFF 不得做的事（硬约束）**：
  1. **不持有上游明文凭据**。上游凭据只存在于本服务 AEAD 密文保管库
     （`inference_credentials`），任何 API 响应都只含脱敏状态；BFF 永远不
     应接触、存储或转发上游 key/token。客户 API Key（`yk-`）明文也只在
     创建响应中出现一次，BFF 不得入库保存——需要再次展示就请用户重建。
  2. **不重新定价**。价格、额度、窗口、重置时刻全部以服务端响应为准；
     BFF 不得根据本地价格表/本地时钟计算"应扣多少/何时恢复"。客户端
     切时区不改变额度周期（全部 UTC，服务端给 `window_start`/`resets_at`）。
  3. 不绕过 envelope/原生错误形状自行编造错误语义（透传 `message`）。
- **运营面身份双腿**：`/admin/*` 运营端点要求 ① 服务身份
  `X-App-ID` + `X-App-Secret`（内部应用认证）+ ② 运营者用户 JWT
  （`Authorization: Bearer`）+ ③ `operator_roles` 中的角色权限。BFF
  运营控制台同样只做转发，不自行判定角色。

## 2. 客户接口清单（用户 JWT；只有本人数据，端点不接收用户 ID）

字段逐字对齐 OpenAPI；64 位额度/金额一律**十进制整数字符串**
（`DecimalInt64`，避免 JS `Number` 精度丢失），时间全部 RFC3339 UTC。

| 方法/路径 | 用途 | 备注 |
|---|---|---|
| `POST /user/api-keys` | 创建 API Key | 明文仅在本次响应 `data.key` 出现一次 |
| `GET /user/api-keys` | Key 列表 | 无明文、无哈希，含 prefix/预算/状态 |
| `GET /user/api-keys/{id}` | Key 详情 | 他人 ID 与不存在 ID 响应逐字节相同 |
| `PATCH /user/api-keys/{id}` | 改名/模型权限/预算/RPM/过期 | 字段存在性 PATCH（显式 null ≠ 缺省） |
| `DELETE /user/api-keys/{id}` | 撤销 | 立即生效 |
| `GET /user/model-quotas` | **三窗口额度卡片**（权威当前状态） | 见 §4 |
| `GET /user/model-usage/summary` | 用量汇总（模型/Key 分组 + UTC 日序列） | 带 `as_of`/`complete_through` |
| `GET /user/model-usage/requests` | 用量明细（keyset 游标分页） | 见 §5 |
| `GET /user/model-subscriptions` | Coding Plan 套餐/权益视图（grant 来源分类） | 不含上游账号信息 |
| `GET /user/wallet` | 钱包总览（派生余额，现金/赠送分列） | 账本派生，无缓存余额 |
| `GET /user/wallet/entries` | 钱包流水（游标分页） | topup/consume/refund/reversal/adjustment |
| `PUT /user/wallet/overage` | 套餐外消费开关 + UTC 自然月支出上限 | 开启必设上限；默认关 |
| `POST /user/wallet/payg` | 无套餐时显式开启 PAYG 调用 | 需运营已发布 PAYG 配置 |

编程工具协议面（API Key 鉴权，**各协议原生形状**，不走 envelope）：

| 方法/路径 | 协议 | 凭据头 |
|---|---|---|
| `GET /v1/models` | OpenAI 形状 | `Authorization: Bearer yk-...` |
| `POST /v1/chat/completions` | OpenAI Chat（流式/非流式） | 同上 |
| `POST /v1/messages` | Anthropic Messages（2023-06-01 子集） | `X-Api-Key: yk-...` 或 Bearer；建议带 `anthropic-version: 2023-06-01` |
| `POST /v1/responses` | OpenAI Responses（子集，含 `previous_response_id` 会话链） | Bearer |

旧 Kaya 入口（兼容，不属于 Coding Plan 契约）：`POST /chat`、
`GET /chat/models`（用户 JWT；行为契约未变）。

## 3. 运营接口清单（服务身份 + 运营 JWT + 权限）

全部挂在 `/admin` 下（`InternalAppAuth` → `JWTAuth` → 权限中间件）。
管理 envelope `{code,data,message}`；写操作要求 `reason` 并全部落审计
（`inference_audit_log`，人员 + 服务双重归因）。

| 权限 | 端点 | 用途 |
|---|---|---|
| `models:manage` | `POST/GET/PATCH/DELETE /admin/catalog/models*`、`/admin/catalog/providers*`、`/admin/catalog/deployments*`、`/admin/catalog/models/:id/routes*` | 目录 CRUD（草稿语义） |
| `models:manage` | `POST /admin/catalog/publish`、`POST /admin/catalog/rollback`、`GET /admin/catalog/revisions`、`GET /admin/catalog/active` | 原子发布/回滚（回滚=新 revision 重发旧内容） |
| `models:manage` | `POST /admin/catalog/bulk-import` | 批量导入（dry-run 逐项预演；全部有效才落库；task_id 幂等；≤200 项；落库即草稿） |
| `models:manage` | `POST /admin/model-prices/preview`、`POST /admin/quota-policies/preview` | 价格/策略变更只读影响预览 |
| `credentials:manage` | `POST/GET /admin/credentials*`、`POST /admin/credentials/:id/{rotate,test,status}` | 上游凭据生命周期（脱敏视图） |
| `credentials:manage` | `POST /admin/oauth/authorizations`、`POST /admin/oauth/callback`、`POST /admin/oauth/credentials/:id/{revoke,refresh}`、`GET /admin/upstream-accounts` | OAuth 连接器授权/撤销/刷新、账号池 |
| `credentials:manage` | `POST /admin/upstream-accounts`、`POST /admin/upstream-accounts/:id/status`、`PATCH /admin/upstream-accounts/:id` | 可调度账号：凭据→账号绑定（静态 key 接入最后一步，重复建 409+已存在视图）、启停（吊销恢复唯一入口，凭据 revoked 时禁激活）、调并发/显示名；写 + 审计同事务 |
| `billing:adjust` | `POST /admin/wallet/adjustments`、`POST /admin/wallet/reversals`、`GET /admin/wallet/adjustments`、`GET /admin/wallet` | 补偿/冲正（幂等键 + 原因 + 同事务审计） |
| `billing:adjust` | `GET/PUT /admin/payg-config` | PAYG 发布配置 |
| `usage:read` | `GET /admin/model-usage/summary`、`GET /admin/model-usage/exceptions`、`GET /admin/upstream-accounts/shared`、`GET /admin/model-adjustments` | 运营统计/异常筛选/共享账号检测/补偿追踪 |
| `admin` 角色 | `GET/POST /admin/operators`、`DELETE /admin/operators/:user_id/roles/:role` | 运营角色管理 |
| （内部应用即可） | `GET /admin/catalog/models*`（只读面）、`GET /admin/stats/*` | 目录只读/旧活跃统计 |

**权限矩阵**（角色 → 权限，`internal/inference/management/operators.go`
权威）：

| 角色 | models:manage | credentials:manage | billing:adjust | usage:read |
|---|---|---|---|---|
| `admin` | ✓ | ✓ | ✓ | ✓ |
| `operator` | ✓ | ✓ | — | ✓ |
| `auditor` | — | — | — | ✓ |

首个运营管理员由离线命令 `cmd/admin-bootstrap` 幂等创建（生产无默认
管理员）。

## 4. 三窗口卡片契约（`GET /user/model-quotas`）

响应骨架（fixture 指针：`docs/api/fixtures/model-quotas-*.json`，测试逐
字段断言）：

```json
{ "code": 0,
  "data": {
    "server_time": "2026-09-09T02:18:18Z",   // 服务端时间——卡片一切"现在"以它为准
    "as_of": "...", "unit": "microcredit",
    "entitlement": { "status": "active", "source_type": "subscription",
                     "model_ids": ["glm-4.6"], "effective_from": "...", "effective_to": null, "revision": 1 },
    "blocked_by": [ /* 当前全部阻断原因，多窗同时阻断全返回 */ ],
    "windows": [
      { "kind": "five_hour", "mode": "anchored_duration",
        "disabled": false, "limit": "1000000", "used": "0",
        "reserved": "7500", "remaining": "992500",
        "window_start": "...", "resets_at": "..." },
      { "kind": "weekly",  "mode": "anchored_period_7d", ... },
      { "kind": "monthly", "mode": "anchored_calendar_month", ... }
    ] } }
```

**前端不需要计算任何窗口**：`window_start`/`resets_at`/`remaining` 全部
服务端给出；`remaining = limit − used − reserved`（禁用窗口除外）。

卡片各态与显示口径（每态都有 fixture，可直接驱动 Storybook/mock）：

| 状态 | 判定字段 | fixture | 显示要点 |
|---|---|---|---|
| 正常（活跃） | `blocked_by=[]`，窗口 `disabled=false` | `model-quotas-reserved.json` | 三窗口 used/reserved/remaining + resets_at |
| 未激活（五小时） | five_hour 窗口 `window_start=null` 且 `resets_at=null`（`activation=on_first_consumption`，见 OpenAPI 描述） | `model-quotas-unactivated.json` | "首次消费后开始计时"，禁止编造倒计时 |
| 零额度（未授权） | 无权益或 limit `"0"`；`blocked_by` 给出原因 | `model-quotas-zero-limit.json` | 引导购买/联系运营 |
| 耗尽 | `blocked_by[].kind="five_hour"|"weekly"|"monthly"`，`remaining="0"`，带 `resets_at` | `model-quotas-exhausted.json` | 显示恢复时刻（服务端给的 UTC） |
| 预占中 | `reserved > "0"` | `model-quotas-reserved.json` | "进行中请求占用 X" |
| 已过期（权益） | `blocked_by[].reason="entitlement_expired"` 或 entitlement `status != active` | `model-quotas-expired.json` | 引导续费 |
| 跨月（锚点裁剪） | monthly 窗口 `mode=anchored_calendar_month`，`resets_at` 已按原始锚点日裁剪 | `model-quotas-cross-month.json` | 月界/闰年由服务端处理，前端不推算 |
| 待核对 | 用量明细行 `usage_status="unknown"` / `reconciliation_pending=true` | `model-usage-requests-reconciliation.json` | "用量核对中，可能调整"，不显示为 0 |

要点：
- **`server_time` 是卡片的唯一"现在"**：倒计时、相对时间都以它为基准，
  不用客户端本地时钟。
- 恢复时刻未知时 `resets_at=null`（如未激活的零限额五小时窗口）——
  显示"恢复时间未知"，**绝不编造**。
- `/v1/*` 调用侧 429（`quota_exceeded`）带 `Retry-After` 头（可计算时）
  与 `blocked by five_hour[,weekly,monthly]` 消息；客户端可原样展示。

## 5. 分页与错误码

**分页**：
- 用量明细/钱包流水/审计列表：**keyset 游标**（`next_cursor`，稳定排序），
  伪造游标 400 `invalid_input`；时间范围上限 92 天（可多次 range 查询）。
- 运营列表 `limit`：默认 100，上限 500（超上限**钳到 500**，与 OpenAPI
  `maximum` 一致；Task 16 起不再静默回退默认值）。
- 运营统计 `group_by=model|provider|customer`：**零活动分组缺席**（不补
  零行；OpenAPI 0.13.0 已声明）。

**错误码**：
- `/user/*`、`/admin/*`：envelope `code = HTTP 状态码`，`message` 可读；
  错误表见 OpenAPI「错误码」节（400 invalid_input / 401 / 403
  model_not_allowed / 404 not_found（他人资源与不存在不区分）/ 409
  conflict / 429 rate_limited / 500 internal）。
- `/v1/*`：各协议原生形状——OpenAI 面 `{"error":{"message,type,code}}`；
  Anthropic 面 `{"type":"error","error":{"type,message}}`（含 529
  `overloaded_error` 映射）。429 `quota_exceeded` 带 `Retry-After`。
- 客户可见提示不得含内部操作名/上游细节（脱敏在服务端完成）。

## 6. DTO / fixture 指针汇总

| 资产 | 位置 |
|---|---|
| OpenAPI 契约（0.13.0） | `docs/api/kaya-coding-plan.openapi.yaml` |
| 三窗口卡片七态响应 fixture | `docs/api/fixtures/model-quotas-{zero-limit,unactivated,exhausted,expired,reserved,cross-month}.json` |
| 待核对用量明细 fixture | `docs/api/fixtures/model-usage-requests-reconciliation.json` |
| 官方 SDK 请求形状 fixture（契约核对用） | `docs/api/fixtures/client/client-request-{anthropic-messages,openai-responses,openai-chat}.json` |
| 已知差异 fixture（多模态 400 / truncation:auto 400 / thinking 历史块不回放） | `docs/api/fixtures/client/client-request-{anthropic-messages-multimodal,openai-responses-truncation,anthropic-messages-thinking-history}.json` |
| 客户端集成指南（含能力矩阵与版本记录） | `docs/api-integration-guide.md` |
| 运营操作手册（批量/补偿/成本口径/一账号一部署） | `docs/runbooks/kaya-coding-plan-operations.md` |
| 发布演练（灰度/回滚/压测/升级实测） | `docs/runbooks/kaya-coding-plan-rollout.md` |

**已知协议差异**（Website 提示语可直接引用；均已在 OpenAPI/指南声明）：
`truncation:"auto"` 400；`GET /v1/models` 不提供 Anthropic 形状；历史
thinking/redacted_thinking 块接受但不回放；多模态/服务端工具
（web_search/computer/bash 等）400。

## 7. 联调环境说明

- Coding Plan 商品默认**不在售**（发布开关见 rollout runbook §灰度），
  联调环境需先由运营配置 `plan_benefit_configs` 并打开商品开关。
- 本仓库 `go run ./cmd/migrate` 必须先于服务启动完成（迁移先于流量）。
- 真实 Claude Code/Codex 客户端联调是**发布前置待办**（本环境以官方
  SDK 形状 fixture 完成契约级核对，见 rollout runbook §真实客户端联调）。
