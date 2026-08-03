# Yunhou Users API 接入文档

本文档面向内部应用开发者，介绍如何接入 Yunhou Users 共享用户管理系统。

## 概述

Yunhou Users 是一个共享用户管理 API，所有接入的应用共享同一套用户身份——每个用户只需一个账号即可使用所有接入应用。系统支持两种 OAuth 重定向登录：GitHub OAuth 和 WeChat Open Platform 网站应用（扫码登录）。`POST /auth/login` 已下线，所有登录必须走下方对应的重定向流程。

核心概念：
- **Plan（订阅计划）**：定义商业属性与可访问的 App 列表。Plan ID 由运营侧自由定义（例：`monthly` / `quarterly` / `yearly`）
- **App（应用）**：接入的系统，如 yundian、yundash
- **Subscription（订阅）**：用户订阅某个 Plan

用户登录时，系统根据其订阅的 Plan 判断可以访问哪些 App。

---

## 快速接入

### 1. 配置 App

管理员在后台创建 App 后，应用获得 `app_id`（如 `yundian`）。

### 2. 用户登录

用户在你的应用中点击登录后：

1. 前端引导用户完成 GitHub 或 WeChat OAuth 重定向流程。
2. 成功时 Yunhou 返回 HTTP 302，跳转到白名单中的 BFF `redirect_uri`，凭据位于 URL fragment：

```text
https://app.example.com/auth/callback#token=<access_token>&refresh_token=<refresh_token>&user_id=<uuid>&has_access=<bool>
```

3. 回调页用客户端 JavaScript 解析 fragment；fragment 不会发送到服务器。BFF 后续使用 JWT 调 API，并安全保存 refresh token 用于延长会话。

`has_access` 只在订阅有效、Plan 启用且 `plan.apps` 包含当前 `app_id` 时为 `true`。无订阅或订阅已过期时 `has_access=false` 且 JWT `scope=[]`。

> **首次登录自动发放 7 天试用**：新用户首次登录时，服务端会 best-effort 自动授予一条 `trial` Plan 的 7 天订阅（`AuthService.grantTrialSubscription`，migration 018）。因此"无订阅"状态在真实用户中很少出现；trial 属于全功能集合（含 `yunhou-website`/`yundash` 等 app），到期后按普通过期处理。trial 不可购买、不出现在目录中。

完整 JSON token 响应由 `POST /auth/refresh` 返回，示例见该接口章节。`POST /auth/logout` 仅撤销 refresh token，单独见登出章节。

**商业 Plan 新增错误**：

| Service sentinel | HTTP | 触发条件 |
|---|---|---|
| `ErrPlanNotAcceptingNew` | 409 | 创建订阅或订单时，Plan 不接受新订阅 |
| `ErrPlanCurrencyMismatch` | 400 | 下单渠道要求的币种与 `plan.currency` 不一致 |
| `ErrInvalidAppID` | 400 | 管理端创建/更新 Plan 时，`apps` 含未知或已停用的 App ID |

### 3. 调用用户信息接口

使用 `access_token` 调用用户接口：

```bash
curl https://your-yunhou-domain/user/profile \
  -H "Authorization: Bearer eyJhbGciOiJSUzI1NiIs..."
```

---

## 认证流程

### 登录时序图

```
用户          你的应用           Yunhou Users         GitHub
 │              │                     │                    │
 │──点击登录→│                     │                    │
 │              │──GET /auth/github/redirect?app_id=...&redirect_uri=... ─→│
 │              │                     │── 302 to github.com/login/oauth/authorize?... ─→│
 │              │                     │←── consent ─────────│
 │              │←─302 /auth/github/callback?code=...&state=... ─────│
 │              │                     │── exchange code (server-side) ─→│
 │              │                     │←── user info ───────│
 │              │←─302 redirect_uri#token=...&refresh_token=...&user_id=... ─│
 │              │  (fragment 由 BFF 前端解析；不上行)         │
 │              │──后续用 JWT 调其它接口──→│
```

### Token 刷新时序图

```
 │              │──POST /auth/refresh →│
 │              │  {refresh_token}      │
 │              │                    │
 │              │←─新 JWT + refresh ─│ (旧token失效)
 │←──继续使用──│                    │
```

---

## 接口详情

### 公共接口

#### GET /healthz

健康检查（用于负载均衡器或 K8s 探针）。

**响应（200）**：
```json
{"code": 0, "data": {"status": "ok"}}
```

**响应（503）**—— 数据库不可用时：
```json
{"code": 503, "message": "db unavailable"}
```

#### GET /.well-known/jwks.json

获取 RSA 公钥，用于本地验证 JWT 签名。

**响应（200）**：
```json
{
  "keys": [
    {
      "kty": "RSA",
      "kid": "yunhou-users-rsa",
      "alg": "RS256",
      "use": "sig",
      "n": "<base64url-encoded-modulus>",
      "e": "AQAB"
    }
  ]
}
```

> 建议缓存此响应，TTL 建议 1 小时。

#### POST /auth/refresh

刷新 Access Token。

**请求体**：
```json
{
  "refresh_token": "a1b2c3d4e5f6...",
  "app_id": "yundian"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `refresh_token` | 是 | 刷新令牌 |
| `app_id` | 否 | 要访问的 App ID，不填则使用登录时的 App ID |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "access_token": "eyJhbGciOiJSUzI1NiIs...",
    "refresh_token": "b2c3d4e5f6...",
    "user": {...},
    "subscription": {...}
  }
}
```

**`subscription.expires_at` 契约**: 自 2026-07-27 起，`expires_at` 字段在 JSON 响应中**始终存在**（不再 `omitempty`）。新激活的订阅固定为 RFC3339 时间戳（优先 `plan.interval_days` 推算，对 WeChat v3 而言是 webhook 路径上唯一可得的产物，因为 v3 NATIVE 不携带 `sub_expires_at`）。历史 NULL 行（修复前由 WeChat 激活）保持 `null`——它们在 `subscriptions.expires_at` 列上就是 NULL，isExpiredAt 仍按 "never expires" 处理。如果 BFF 想要 UI 上的"永不过期"分支，应仅在 `expires_at: null` 时显示，不再用「字段缺席」做存在性判断。

> **注意**：刷新时旧的 refresh token 会失效，必须使用返回的新 token。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺少 `refresh_token` |
| 401 | `invalid refresh token` | refresh token 不存在、已撤销、已轮换、已过期 |
| 401 | `user is suspended` / `user is deleted` | 用户账号被停用或已删除 |
| 401 | `user not found` | refresh token 对应的 user 行已被删除（理论不应发生；防御性兜底） |
| 401 | `app not found` / `app is inactive` | 解析得到的 `app_id` 不存在或已停用 |

> **Expired / non-active subscriptions are NOT an error.** `POST /auth/refresh` is decoupled from subscription state (login/subscription decouple, Phase 1+). A user with an expired subscription, no subscription, or a subscription that has been swept to `cancelled`/`expired` still receives a fresh token pair. The new JWT carries `scope=[]` and the response's `subscription.has_access=false`; `subscription.plan_id` is preserved as the historical plan id (empty string `""` when there has never been a subscription) so the BFF can render the renewal CTA. Operators must read `subscription.has_access` (not the HTTP status) to decide whether to show protected UI.

#### POST /auth/logout

登出，撤销 refresh token。

**请求体**：
```json
{
  "refresh_token": "a1b2c3d4e5f6..."
}
```

**响应（200）**：
```json
{"code": 0, "message": "logged out"}
```

#### POST /test/login

仅用于开发和测试环境。端点由 `PAYPAL_L3_E2E_MODE=1` 开启；未开启时始终返回 404，生产环境不得启用。

```bash
curl -X POST "https://your-yunhou-domain/test/login?plan_id=monthly" \
  -H "Content-Type: application/json" \
  -d '{"email":"dev@example.com","app_id":"yundian"}'
```

`plan_id` 查询参数、请求体中的 `email` 和 `app_id` 均必填。成功时返回 200 和标准登录 JSON（access token、一次性 refresh token、user、subscription）。状态码：缺少/非法输入或 Plan inactive 为 400；功能未开启或 Plan 不存在为 404；Plan 不接受新订阅为 409 `plan is not accepting new subscriptions`；成功为 200。

#### GET /apps/:id/plans

公共的 Plan 目录接口，**无需鉴权**（无需 `X-App-ID`、无需 JWT）。返回指定 App 当前启用且公开展示的 Plans（`is_active=true AND is_listed=true`），按 `display_order ASC, created_at ASC, id ASC` 排序。响应使用精简的 `PublicPlan` DTO，包含商业展示字段及上游渠道的 plan ID / variant ID。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "monthly",
      "name": "按月订阅",
      "price": 19.9,
      "interval_days": 30,
      "currency": "CNY",
      "trial_days": 0,
      "description": "按月订阅 ¥19.9，自动续费，可随时取消",
      "apps": ["yundian", "yundash"],
      "display_order": 10,
      "is_listed": true,
      "provider_ids": {"paypal": "P-MONTHLY"},
      "cycle": {"trial_days": 0, "billing_cycle_days": 30}
    },
    {
      "id": "yearly",
      "name": "按年订阅",
      "price": 199.9,
      "interval_days": 365,
      "currency": "CNY",
      "trial_days": 0,
      "description": "按年订阅 ¥199.9，自动续费，可随时取消",
      "apps": ["yundian", "yundash"],
      "display_order": 30,
      "is_listed": true,
      "provider_ids": {"paypal": "P-YEARLY"},
      "cycle": {"trial_days": 0, "billing_cycle_days": 365}
    }
  ]
}
```

**响应字段**：

| 字段 | 说明 |
|------|------|
| `currency` | Plan 的结算币种；取值为 `CNY` / `USD` / `EUR`。 |
| `trial_days` | Plan 定义的商业试用天数。 |
| `description` | 可空的营销文案。 |
| `apps` | 该 Plan 授权的 App ID 列表。 |
| `display_order` | 运营配置的展示顺序；数值较小的 Plan 先返回。 |
| `is_listed` | 是否出现在商业目录；当前接口只返回 `is_listed=true` 的行，但字段仍随 DTO 暴露以匹配 `model.PublicPlan` 结构。 |
| `provider_ids` | 该 Plan 在渠道侧对应的上游 plan ID / variant ID 映射。已实现的渠道键：`paypal`（来自 `apps.config.payment_providers.paypal.plan_mapping`）与 `wechat_pay`（来自 `apps.config.payment_providers.wechat_pay.plan_mapping`）；未配置的渠道键不出现。未配置任何渠道时为 `{}`（BFF 即可判定"当前 App 该 Plan 暂无可下单渠道"）。 |
| `cycle` | Provider 侧解析后的试用 + 计费周期；用于营销页核对上游配置。对应 Plan 未配置 PayPal 时为 `null`；业务试用天数仍以顶层 `trial_days`（即 `plan.trial_days`）为准。 |

`PublicPlan` 不返回管理字段 `is_active`、`accepting_new_subscriptions`、`updated_at`（`is_listed` 已作为目录字段随 DTO 暴露）。`quarterly` 当前是 legacy Plan（`accepting_new_subscriptions=false`）；目录出现不代表可以创建新订阅或订单，BFF 下单前必须处理 409。`free` 已由 Phase 2 退役并设置为 `is_active=false`、`accepting_new_subscriptions=false`，不再出现在公开 Plan 目录中。此外还存在 `trial` Plan（`is_active=true`、`is_listed=false`、`accepting_new_subscriptions=false`）——不出现在本目录，仅由服务端在首次登录时自动授予（见快速接入章节）。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 404 | `app not found` | `app_id` 不存在 |

---

### 用户接口

所有用户接口需要携带 JWT Bearer Token：

```
Authorization: Bearer <access_token>
```

#### GET /user/profile

获取当前用户信息。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440002",
    "nickname": "张三",
    "email": "user@example.com",
    "avatar_url": "https://avatars.githubusercontent.com/u/12345",
    "status": "active",
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-06-01T00:00:00Z"
  }
}
```

#### PATCH /user/profile

更新用户资料。所有字段均为可选；只更新提供的字段。

**请求体**：
```json
{
  "nickname": "新昵称",
  "avatar_url": "https://example.com/new-avatar.png"
}
```

| 字段 | 必填 | 校验 |
|------|------|------|
| `nickname` | 否 | trim 后字节长度必须在 1–100 之间（非字符数；CJK 等多字节字符按 3 字节计） |
| `avatar_url` | 否 | 必须是 HTTPS URL，**不能**带 userinfo（`user:pass@host`）或 fragment（`#...`） |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440002",
    "nickname": "新昵称",
    "email": "user@example.com",
    "avatar_url": "https://example.com/new-avatar.png",
    "status": "active",
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-06-23T08:30:00Z"
  }
}
```

**响应（400）**—— 字段不合法：
```json
{"code": 400, "message": "nickname must be 1-100 characters"}
{"code": 400, "message": "avatar_url must be a valid HTTPS URL without userinfo or fragment"}
```

**响应（404）**—— 用户不存在（理论上不应发生，签发 token 必然对应已存在用户；如发生，JWT 主题无法解析到用户行）：
```json
{"code": 404, "message": "user not found"}
```

#### GET /user/identities

查看已绑定的社交账号。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440003",
      "user_id": "550e8400-e29b-41d4-a716-446655440002",
      "provider": "github",
      "provider_uid": "12345",
      "email": "user@example.com",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

#### DELETE /user/identities/:id

解绑社交账号。

> 用户必须至少保留一个社交账号绑定。

**响应（200）**：
```json
{"code": 0, "message": "unbound"}
```

**响应（400）**—— 最后一个身份无法解绑：
```json
{"code": 400, "message": "must keep at least one social account"}
```

#### GET /user/subscriptions

查看用户的订阅历史（按 `created_at` 升序，包含全部状态：`active` / `expired` / `cancelled`）。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440010",
      "user_id": "550e8400-e29b-41d4-a716-446655440002",
      "plan_id": "monthly",
      "status": "active",
      "started_at": "2026-01-01T00:00:00Z",
      "expires_at": "2026-07-01T00:00:00Z",
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

> 该接口只返回订阅本身的字段；`plan_name` 等 Plan 维度的字段需通过 `/admin/plans/:id` 关联查询。

#### POST /user/subscriptions

创建订阅。仅同时满足 `price == 0`、`is_active=true`、`accepting_new_subscriptions=true` 的 Plan 允许用户自助创建；付费 Plan 必须通过支付流程创建（参见 §支付接口）。`expires_at` 字段被服务层忽略，过期时间由 `plan.interval_days` 推导（`interval_days == 0` 表示永不过期）。种子 Plan `free` 已退役且不接受新订阅，因此不能再用作自助订阅目标。

**请求体**：
```json
{
  "plan_id": "promo",
  "expires_at": "2026-07-19T00:00:00Z"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `plan_id` | 是 | 订阅的 Plan ID；只接受可用、接受新订阅且 `price == 0` 的 Plan |
| `expires_at` | 否 | **忽略字段**（保留仅为向后兼容）。过期时间由 `plan.interval_days` 决定 |

**响应（201）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440010",
    "user_id": "550e8400-e29b-41d4-a716-446655440002",
    "plan_id": "promo",
    "status": "active",
    "started_at": "2026-06-23T08:30:00Z",
    "expires_at": null,
    "created_at": "2026-06-23T08:30:00Z",
    "updated_at": "2026-06-23T08:30:00Z"
  }
}
```

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 400 | `invalid expires_at format, use RFC3339` | `expires_at` 字段存在但格式不合法 |
| 400 | `plan not found` | `plan_id` 不存在 |
| 400 | `plan is inactive` | Plan 已停用 |
| 403 | `paid plans require payment, cannot self-subscribe` | 试图自助订阅付费 Plan |
| 409 | `plan is not accepting new subscriptions` (`ErrPlanNotAcceptingNew`) | Plan 是 legacy/停售状态，不接受新订阅 |
| 409 | `user already has an active subscription` | 用户已有活跃订阅 |

#### DELETE /user/subscriptions/:id

取消订阅。只能取消自己的订阅；不是本人或 ID 不存在都返回 404（避免枚举）。

**响应（200）**：
```json
{"code": 0, "message": "cancelled"}
```

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `already cancelled` | 订阅已经处于 `cancelled` 状态 |
| 404 | `subscription not found` | ID 不存在或不属于当前用户 |

---

### Chat 接口

Chat 代理接口让消费端（如 kaya）**无需配置任何 LLM Key** 即可获得对话能力：客户端携带用户 JWT 调用，服务端用自己持有的 DeepSeek API Key 代为调用模型，并把流式响应原样转发给客户端。**每个请求都消耗 yunhou 侧的模型额度**，因此本接口：

- 要求 JWT 认证（与 `/user/*` 同级，引擎级挂载在 `POST /chat`）；
- 要求有效订阅：订阅未过期、Plan 处于激活态且 `plan.apps` 包含 JWT 的 `app_id`（与登录时 `has_access` 同一判定矩阵），否则 403；
- 独立限流桶 10 次/秒、突发 20（按 IP，与其他接口桶隔离）；
- 服务端未配置 `DEEPSEEK_API_KEY` 时整体返回 404（未启用）。

#### POST /chat

**请求体**：

```json
{
  "session_id": "sess-uuid-or-any-opaque-id",
  "messages": [
    {"role": "system", "content": "你是一个简洁的助手"},
    {"role": "user", "content": "你好"}
  ]
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `messages` | 是 | 对话消息数组，OpenAI 兼容格式；`role` 取值 `system` / `user` / `assistant`。kaya 自行维护会话历史，每次请求携带完整上下文（服务端无状态代理） |
| `session_id` | 否 | 会话标识（≤64 字符），仅用于服务端访问日志按会话分组，不参与任何业务逻辑 |

**限制**（超出返回 400）：1–20 条消息；每条 `content` 非空且 ≤8000 字节（`len()` 字节数，CJK 每字约 3 字节）；全部消息总字节数 ≤32000。

**响应（200）**：`Content-Type: text/event-stream`，逐块透传 DeepSeek 的 OpenAI 兼容 SSE 事件：

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"delta":{"content":"你"}}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"delta":{"content":"好"}}]}

data: [DONE]
```

kaya 端按 `data: ` 前缀解析 JSON，拼接 `choices[0].delta.content` 即得完整回复；`data: [DONE]` 表示流结束。

**错误响应**（流开始前返回标准 JSON；流开始后的错误取决于成因——DeepSeek 自身发出的错误事件会原样透传，而上游连接中断（含 5 分钟超时）时流直接结束：客户端收不到 `data: [DONE]`，应把"缺少 `[DONE]` 的结束"视为失败并重试）：

| HTTP | message | 触发条件 |
|---|---|---|
| 400 | `messages is required` / `too many messages` / `invalid message role` / `message content is required` / `message content too long` / `total message content too long` / `session_id too long` | 请求体或消息不符合限制 |
| 401 | `missing or invalid authorization header` / `invalid or expired token` | 未携带或无效 JWT（由 `JWTAuth` 中间件统一返回） |
| 403 | `active subscription with access to this app is required` | 无有效订阅，或 Plan 未激活，或 `plan.apps` 不含 JWT `app_id` |
| 404 | `chat is not enabled` | 服务端未配置 `DEEPSEEK_API_KEY` |
| 429 | `chat upstream rate limit exceeded` | DeepSeek 上游限流 |
| 502 | `chat upstream error` | 上游非 2xx 或网络错误 |

**超时与审计**：本接口豁免全局 20s 请求超时（流式回答可能超过）；服务端上游超时上限 5 分钟，订阅门禁的 DB 查询另有 10s 上限。配置 `CHAT_LOG_PATH` 后，每次请求（成功、失败、客户端中途断开、上游中断）都会在该文件追加一行 JSON 审计日志，含 `user_id`、`app_id`、`session_id`、输入 `messages`、解析后的 `output` 文本、`status`（`ok` / `error` / `disconnected` / `upstream_error`）、输入输出字节数（`input_bytes` / `output_bytes`，均为截断前的真实长度）与耗时。错误行的输入按每条消息 1 KiB 截断（`input_truncated` 标记），`output` 封顶 64 KiB（`output_truncated` 标记）。文件以 0o600 权限打开（对话内容属 PII），需部署侧配置轮转。

**部署注意**：SSE 需要反代放行长连接且不缓冲——参考 `deploy/nginx.conf` 的 `location = /chat`（`proxy_buffering off`、`proxy_read_timeout 360s`）；服务端也会随响应下发 `X-Accel-Buffering: no`。

---

### App 接口

App 相关接口分散在三种鉴权风格下，BFF 接入时务必看清楚：

| 路径 | 鉴权 | 用途 |
|------|------|------|
| `GET /apps/:id/plans` | **无需鉴权**（公共） | 营销页拉取 Plan 目录 |
| `GET /apps/:id/provider-token/:channel` | **`X-App-ID` + `X-App-Secret`**（内部服务） | BFF 拉取 PayPal 凭据再调用上游 |
| `POST /apps/:id/quote` | **JWT Bearer**（终端用户） | 给定 (app, plan) 给出下单报价 |

> 同样的 `X-App-ID` + `X-App-Secret` 头对适用于所有 `/admin/*` 路由（plan 管理 + app 管理 + rotate-secret）。详见下文 §"内部服务鉴权"。

#### apps.config JSONB 结构

`apps.config` 是 JSONB 列，下游 schema 演进不会触发 DB migration。本节给出当前形态；新增可选 provider 块时，旧配置行缺该字段即视为"未配置"，无需回填。

```json
{
  "brand": { "name": "云店" },
  "payment_providers": {
    "paypal": {
      "client_id": "...",
      "client_secret": "...",
      "webhook_id": "W-...",
      "mode": "live",
      "plans": {
        "monthly-usd": {
          "plan_id": "P-MONTHLY-USD-7D",
          "trial_days": 7,
          "billing_cycle_days": 30
        }
      }
    }
  }
}
```

要点：

- `paypal.plans` 是 `plan_id -> 配置对象` 的 map；`plan_id` 与业务 `plans.id` 同名（运营侧负责对齐）。
- `trial_days` 仍会被解析，供 provider 配置兼容与 `PublicPlan.cycle` 核对使用，但已废弃为业务试用期来源；`POST /apps/:id/quote` 只读取 `plan.trial_days`。`billing_cycle_days` 仅描述 PayPal 上游周期，缺省时回退到 `plans.interval_days`。
- `brand.name` 缺省回退到 `apps.name`，对应 PayPal `application_context.brand_name`。
- `apps.config` 中 PayPal 段当前 schema 是嵌套对象形：`{ plan_id, trial_days, billing_cycle_days }`。配置行若缺少 `payment_providers.paypal.plans` 或对应 plan_id 的条目，catalog 的 `provider_ids` 为空且 `cycle` 为 `null`；quote 的币种、试用期和账单周期仍分别来自 `plan.currency`、`plan.trial_days`、`plan.interval_days`。

**字段校验**（`POST /admin/apps` 与 `PATCH /admin/apps/:id` 时执行；违反会得到 400 + 具体字段 message）：

- `payment_providers.paypal`：`client_id` / `client_secret` / `webhook_id` 必填；`mode` 必须 `live` 或 `sandbox`
- `oauth_providers.github`：`client_id` / `client_secret` 必填；`callback_urls` 必须非空且全部为 `https://`，或 `http://localhost` / `http://127.0.0.1` / `http://[::1]`（含 `::ffff:127.0.0.1` 映射形式）；不允许重复项
- `oauth_providers.wechat`：`app_id` 必须匹配正则 `^wx[0-9a-fA-F]{16}$`（`wx` 前缀严格小写，16 位十六进制区分大小写）；`app_secret` 必须恰好 32 字符；`callback_urls` 校验规则与 GitHub 一致（`https://` 或 loopback，不允许重复项）
- 整个 `config` 在保存前会被 canonicalize（按 struct 字段顺序重写），所以 PATCH 后 JSONB 字节布局可能与入参不完全一致——语义不变但 byte-equal diff 会失真。运营侧如果做"配置变更审计"应比对语义而非字节

#### GET /apps

查看所有已注册的应用。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "app_id": "yundian",
      "name": "云店",
      "description": "电商应用",
      "config": {},
      "is_active": true,
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

#### GET /apps/:id

查看指定应用详情。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "app_id": "yundian",
    "name": "云店",
    "description": "电商应用",
    "config": {},
    "is_active": true,
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-01-01T00:00:00Z"
  }
}
```

**响应（404）**：
```json
{"code": 404, "message": "app not found"}
```

#### GET /apps/:id/provider-token/:channel

为 BFF 拉取 PayPal 上游凭据，避免敏感凭据下沉到消费方代码。**鉴权为 `X-App-ID` + `X-App-Secret` 内部服务头对**——BFF 后端用其内部服务身份调用，绝不下发给终端用户；`X-App-Secret` 是每个 app 创建时一次性返回的 64 位十六进制串，仅 bcrypt 哈希存库，丢失后必须通过 `POST /admin/apps/:id/rotate-secret` 重新生成。

**路径参数**：

| 字段 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | App ID |
| `channel` | 是 | `paypal` |

**响应（200）**—— PayPal：
```json
{
  "code": 0,
  "data": {
    "channel": "paypal",
    "access_token": "A21AAG.xxx",
    "expires_in": 3600
  }
}
```

行为说明：

- yunhou-users 真正去 PayPal OAuth `client_credentials` 接口拿 access token，并在进程内缓存 `expires_in − 60s`（即 PayPal 实际返回的剩余有效期减去 60 秒安全余量；典型 ~9 小时；若上游返回的 `expires_in` 异常小，最短回退到 30 秒；若异常大则封顶 1 小时，防止代理重放把 token 钉死数天）。并发去重（同一 `client_id` 同时只有一次上游调用）；单 Yunhou 实例维度缓存，多实例各自刷新（PayPal 的 `client_credentials` 对相同凭据幂等）。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `unsupported channel` | `channel` 不是 `paypal` |
| 400 | `provider not configured for app` | App 未配置 PayPal provider 块 |
| 403 | `app is disabled` | App 已停用 |
| 404 | `app not found` | App 不存在 |
| 500 | `provider token service unavailable` | 服务依赖未注入（理论上不会发生；防御性兜底） |
| 502 | `provider upstream error` | PayPal OAuth 调用失败（网络、认证、配额） |

#### POST /apps/:id/quote

下单前的"取报价"接口。BFF 拿到 `data` 后直接把 `provider_data` 透传给 PayPal 创建 checkout session；`sub_expires_at` 是 yunhou-users 在 webhook 确认订阅时填进 `subscriptions.expires_at` 的值。**鉴权为 JWT Bearer**（终端用户身份触发；调用方必须先完成 GitHub 或 WeChat OAuth 登录并拿到 Yunhou JWT），handler 读 JWT 上下文中的 `user_id`。**注意**：当前**不强制 `subscription.has_access` gating**——任何已登录用户都可以对包含目标 app 的 Plan 发起 quote，前端必须自己根据 `subscription.has_access` 决定是否展示下单按钮。

**路径参数**：`id` 是 App ID。

**请求体**：
```json
{ "plan_id": "monthly-usd" }
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `plan_id` | 是 | 计划 ID。必须是该 App 所属的 Plan（`plan.apps` 包含 `app_id`） |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "plan_id": "monthly-usd",
    "amount": 4.99,
    "currency": "USD",
    "sub_expires_at": "2026-08-04T00:00:00Z",
    "cycle_config": {
      "trial_days": 7,
      "billing_cycle_days": 30,
      "base": "now + trial + cycle"
    },
    "provider_data": {
      "paypal": {
        "plan_id": "P-MONTHLY-USD-7D",
        "application_context": {
          "brand_name": "云店",
          "shipping_preference": "NO_SHIPPING",
          "user_action": "SUBSCRIBE_NOW"
        }
      }
    }
  }
}
```

**响应字段**：

| 字段 | 说明 |
|------|------|
| `amount` | 透传 `plans.price` |
| `currency` | 来自 `plan.currency`（`CNY` / `USD` / `EUR`），不再硬编码，也不接受 caller 覆盖 |
| `sub_expires_at` | **服务端计算**：`now + plan.trial_days + plan.interval_days`（服务器时间，见 `internal/service/quote.go`）。BFF 在创建 PayPal checkout 时将其写入 `metadata.sub_expires_at`；PayPal 自己的 renewal webhook 会通过 `resource.billing_info.next_billing_time` 回传，yunhou 不参与续费的周期计算。注意：`sub_expires_at` 在 channel webhook 路径上由 **BFF 嵌入 → channel 回传 → yunhou 信任并写入**，yunhou-users 不二次推导 webhook payload 里的值。注：yunhou-users webhook / Confirm 路径在 webhook payload / BFF 入参均不含 `sub_expires_at` 时，会回退到 `plan.interval_days`（`time.Now() + interval_days*24h`）作为 contract 兜底。PayPal 续费路径是例外——`next_billing_time` 缺失会被审计并拒绝延期，`sub_expires_at` 不参与。 |
| `cycle_config.base` | 恒为 `"now + trial + cycle"`，给审计/排查时一眼看出计算方式 |
| `provider_data` | 每个已配置的渠道一段 payload；BFF 创建 checkout 时按需透传给对应渠道 SDK |

**Cycle 解析规则**：`cycle_config.trial_days` 来自 `plan.trial_days`，`cycle_config.billing_cycle_days` 来自 `plan.interval_days`；不再读取 `apps.config.payment_providers.paypal.plans[plan_id].trial_days`，且 `plan.trial_days == 0` 时不做 fallback。`sub_expires_at` 按这两个 Plan 字段计算。PayPal 的 provider 配置仍负责提供上游 `plan_id`，运营侧必须确保该 product 的实际周期与 Plan 一致，否则报价跟实际结算对不上。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 400 | `plan is inactive` | Plan 已停用 |
| 400 | `plan does not include this app` | `plan.apps` 不包含该 `app_id` |
| 403 | `app is disabled` | App 已停用 |
| 404 | `plan not found` | `plan_id` 不存在 |
| 404 | `app not found` | App 不存在 |
| 500 | `failed to compute quote` | 服务端异常（DB 或 `apps.config` JSON 解析失败） |

---

### 管理接口

管理接口需要 `X-App-ID` + `X-App-Secret` 头（内部服务调用；详见下文 §"内部服务鉴权"）。

#### GET /admin/plans

查看所有 Plan。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "monthly",
      "name": "按月订阅",
      "price": 19.9,
      "interval_days": 30,
      "apps": ["yundian", "yundash"],
      "is_active": true,
      "is_listed": true,
      "accepting_new_subscriptions": true,
      "currency": "CNY",
      "trial_days": 0,
      "description": "按月订阅 ¥19.9，自动续费，可随时取消",
      "display_order": 10,
      "updated_at": "2026-07-24T08:30:00Z",
      "created_at": "2026-01-01T00:00:00Z"
    },
    {
      "id": "quarterly",
      "name": "按季订阅",
      "price": 79.9,
      "interval_days": 90,
      "apps": ["yundian", "yundash"],
      "is_active": false,
      "is_listed": false,
      "accepting_new_subscriptions": false,
      "currency": "CNY",
      "trial_days": 0,
      "description": "按季订阅 ¥79.9（已下线）",
      "display_order": 20,
      "updated_at": "2026-07-24T08:30:00Z",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

`is_default` 字段已随 Phase 2（迁移 014）从 `plans` 表中移除，管理端读写响应都不再返回；如 BFF 因历史数据继续携带该字段，POST/PATCH 仍会以 400 显式拒绝，便于故障排查。`updated_at` 为只读字段，由数据库 trigger 在每次 UPDATE 时维护。

#### GET /admin/plans/:id

查询单个 Plan 详情。需要 `X-App-ID` + `X-App-Secret`。响应结构与 `GET /admin/plans` 数组元素一致（含全部管理字段）。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "monthly",
    "name": "按月订阅",
    "price": 19.9,
    "interval_days": 30,
    "apps": ["yundian", "yundash"],
    "is_active": true,
    "is_listed": true,
    "accepting_new_subscriptions": true,
    "currency": "CNY",
    "trial_days": 0,
    "description": "按月订阅 ¥19.9，自动续费，可随时取消",
    "display_order": 10,
    "updated_at": "2026-07-24T08:30:00Z",
    "created_at": "2026-01-01T00:00:00Z"
  }
}
```

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 404 | `plan not found` | Plan ID 不存在 |

#### POST /admin/plans

创建 Plan。

**请求体**：
```json
{
  "id": "quarterly-usd",
  "name": "按季订阅（USD）",
  "price": 11.99,
  "interval_days": 90,
  "apps": ["yundian", "yundash"],
  "is_active": true,
  "is_listed": true,
  "accepting_new_subscriptions": true,
  "currency": "USD",
  "trial_days": 7,
  "description": "按季订阅，含 7 天试用",
  "display_order": 25
}
```

| 字段 | 必填 | 校验 |
|------|------|------|
| `id` | 是 | Plan 唯一标识 |
| `name` | 是 | Plan 显示名称 |
| `price` | 否 | 价格，默认 0；必须 `>= 0` |
| `interval_days` | 否 | 订阅周期（天），默认 0（永久）；必须 `>= 0` |
| `apps` | 否 | 可访问的 App 列表，默认空；每项必须是存在且启用的 App，否则返回 400 `ErrInvalidAppID` |
| `is_active` | 否 | 是否启用，默认 `true` |
| `is_listed` | 否 | 是否出现在商业目录，默认 `true` |
| `accepting_new_subscriptions` | 否 | 是否允许创建新订阅/订单，默认 `true` |
| `currency` | 否 | 默认 `CNY`；只能是 `CNY` / `USD` / `EUR` |
| `trial_days` | 否 | 试用天数，默认 0；必须 `>= 0` |
| `description` | 否 | 可空的营销文案 |
| `display_order` | 否 | 目录排序值，默认 0；数值越小越靠前 |
| `is_default` | 禁止 | 已废弃；只要请求体包含该字段就返回 400 |

`updated_at` 不接受 caller 输入；它由数据库默认值和 UPDATE trigger 管理。

**响应（201）**：
```json
{
  "code": 0,
  "data": {
    "id": "quarterly-usd",
    "name": "按季订阅（USD）",
    "price": 11.99,
    "interval_days": 90,
    "apps": ["yundian", "yundash"],
    "is_active": true,
    "is_listed": true,
    "accepting_new_subscriptions": true,
    "currency": "USD",
    "trial_days": 7,
    "description": "按季订阅，含 7 天试用",
    "display_order": 25,
    "updated_at": "2026-07-24T08:30:00Z",
    "created_at": "2026-07-24T08:30:00Z"
  }
}
```

**响应（400）**—— 字段不合法：
```json
{"code": 400, "message": "price must be non-negative"}
{"code": 400, "message": "interval_days must be non-negative"}
{"code": 400, "message": "trial_days must be non-negative"}
{"code": 400, "message": "currency must be one of CNY/USD/EUR"}
{"code": 400, "message": "is_default is no longer supported; use plan selection logic in BFF"}
{"code": 400, "message": "plan apps contains unknown or inactive app_id: missing-app"}
```

#### PATCH /admin/plans/:id

更新 Plan。所有字段均为可选，仅更新提供的字段。

**请求体**：
```json
{
  "name": "新名称",
  "price": 39.9,
  "interval_days": 30,
  "apps": ["yundian", "yundash", "yundown"],
  "is_active": true,
  "is_listed": true,
  "accepting_new_subscriptions": false,
  "currency": "CNY",
  "trial_days": 0,
  "description": "仅保留已有订阅续费",
  "display_order": 20
}
```

| 字段 | 必填 | 校验 |
|------|------|------|
| `name` | 否 | 显示名称 |
| `price` | 否 | 必须 `>= 0` |
| `interval_days` | 否 | 必须 `>= 0` |
| `apps` | 否 | 可访问的 App 列表（数组；传 `[]` 清空）；每项必须存在且启用 |
| `is_active` | 否 | 启用状态 |
| `is_listed` | 否 | 目录可见性 |
| `accepting_new_subscriptions` | 否 | 是否接受新订阅/订单；设为 `false` 不影响既有订阅续费 |
| `currency` | 否 | 只能是 `CNY` / `USD` / `EUR` |
| `trial_days` | 否 | 必须 `>= 0` |
| `description` | 否 | 营销文案 |
| `display_order` | 否 | 目录排序值 |
| `is_default` | 禁止 | 已废弃；只要请求体包含该字段就返回 400 |

`updated_at` 为只读、DB 管理字段，PATCH 成功后由 trigger 自动刷新。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "quarterly",
    "name": "新名称",
    "price": 39.9,
    "interval_days": 30,
    "apps": ["yundian", "yundash", "yundown"],
    "is_active": true,
    "is_listed": true,
    "accepting_new_subscriptions": false,
    "currency": "CNY",
    "trial_days": 0,
    "description": "仅保留已有订阅续费",
    "display_order": 20,
    "updated_at": "2026-07-24T09:00:00Z",
    "created_at": "2026-06-23T08:30:00Z"
  }
}
```

**响应（400）**—— 与创建接口执行相同的非负数、币种、App ID 和 `is_default` 校验。

#### DELETE /admin/plans/:id

删除 Plan。若 Plan 仍被任何订阅引用，数据库外键约束触发，返回 409。

**响应（200）**：
```json
{"code": 0, "message": "deleted"}
```

**响应（409）**—— Plan 仍被订阅引用：
```json
{"code": 409, "message": "plan is in use by existing subscriptions"}
```

#### POST /admin/apps

创建 App。

**请求体**：
```json
{
  "app_id": "yundash",
  "name": "云dash",
  "description": "Dashboard 应用"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `app_id` | 是 | App 唯一标识 |
| `name` | 是 | App 显示名称 |
| `description` | 否 | App 描述 |

**响应（201）**：
```json
{
  "code": 0,
  "data": {
    "app": {
      "app_id": "yundash",
      "name": "云dash",
      "description": "Dashboard 应用",
      "config": null,
      "is_active": true,
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z"
    },
    "secret": "a3f9...64-hex-chars..."
  }
}
```

> 如果请求体未传 `config`，POST 响应里 `data.app.config` 为 `null`（handler 返回的是 in-memory 入参对象，未读 DB 默认值）；如果传了 `config`，则返回 canonicalize 后的值。`GET /admin/apps/:id` 读取时若 DB 列实际为 NULL，会回填为 `{}`。
>
> `data.secret` 是 64 位十六进制随机串，**仅本次响应返回**——服务端只存 bcrypt 哈希，无法再次读出。客户端必须立即把 `secret` 配置到 BFF 环境变量里，下一次 admin 调用时携带 `X-App-Secret` 头才能通过 `InternalAppAuth`。丢失后只能走 `POST /admin/apps/:id/rotate-secret` 重新生成。

#### PATCH /admin/apps/:id

更新 App。所有字段均为可选；`name` 不可为空。**不能通过此接口改 `secret`——secret 走专用 rotation endpoint。**

**请求体**：
```json
{
  "name": "新名称",
  "description": "新描述",
  "is_active": false
}
```

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "app_id": "yundash",
    "name": "新名称",
    "description": "新描述",
    "config": {},
    "is_active": false,
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-06-23T08:30:00Z"
  }
}
```

**响应（404）**：
```json
{"code": 404, "message": "app not found"}
```

**响应（400）**—— name 为空：
```json
{"code": 400, "message": "name must not be empty"}
```

#### POST /admin/apps/:id/rotate-secret

轮换 App 的内部服务密钥。**调用前必须先用现有 secret 通过 `InternalAppAuth` 才能进入此 endpoint**（admin group 上的 middleware 校验）。轮换后旧 secret 立即失效，下次 admin 调用必须改用新返回的 secret。

**路径参数**：

| 字段 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | App ID |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "secret": "b7e2...新的 64 位十六进制串..."
  }
}
```

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 401 | `missing X-App-Secret header` | 调用方没带 secret |
| 401 | `invalid app_secret` | 旧 secret 错误 |
| 404 | `app not found` | app_id 不存在 |

**轮换后必做**：

1. 把新 `secret` 写入 BFF 环境变量（替换旧值）
2. BFF 重启 / 热加载，从下一次请求开始带新 `X-App-Secret`
3. 旧 secret 立即失效，中间没有 grace period

如果怀疑旧 secret 泄漏（例如 BFF 容器镜像被 pull 过），立即 rotate 即可，无需改 `app_id`。

---

### GitHub OAuth 授权码流程

> **设计原则**：所有 OAuth provider 凭据（client_secret、access_token）由 yunhou 持有并使用，BFF 不接触任何长期秘密。BFF 端只持有 `client_id`（明文）+ 一次性 redirect_uri 白名单条目。

适用场景：消费 app 需要让终端用户用 GitHub 账号登录。Yunhou 仅支持 GitHub OAuth 重定向流程——**`POST /auth/login` 已下线**（任何请求会得到 404），所有 GitHub 登录必须走下方流程。

#### 配置（运营侧）

1. 在 GitHub 后台（https://github.com/settings/developers）注册一个 OAuth App。
2. 把 `client_id` 和 `client_secret` 写入 `apps.config.oauth_providers.github`：

   ```json
   {
     "oauth_providers": {
       "github": {
         "client_id": "Iv1.xxxxxxxxxxxxxxx",
         "client_secret": "<plaintext — server-side only>",
         "callback_urls": [
           "https://yundian.com/auth/callback",
           "https://yundian.com/mobile/auth/callback"
         ]
       }
     }
   }
   ```

   - `client_secret` 仅 yunhou-users 使用，永不下发到 BFF
   - `callback_urls` 必须全部是 `https://`，或 `http://localhost` / `http://127.0.0.1` / `http://[::1]`（本地开发用）
   - 一份 OAuth App 多个 callback URL 是合法的（如 web / iOS / Android 共用）

3. 设置环境变量 `OAUTH_STATE_SECRET`（必需，至少 32 个字符的随机串，建议 `openssl rand -hex 32` 生成）—— 用于 state token 的 HMAC 签名。state token 由 `/auth/{github,wechat}/*` 两个 provider 共用（provider-agnostic），多实例部署必须共享同一个值。启动时 `Validate()` 会拒绝过短的值。

#### 流程

```
终端用户           BFF               yunhou-users            GitHub
   │                │                    │                    │
   │ 点登录按钮      │                    │                    │
   │ ────────────→ │                    │                    │
   │                │ GET /auth/github/redirect?app_id=yundian  │
   │                │   &redirect_uri=... │                    │
   │                │ ───────────────→   │                    │
   │                │                    │ 验证 app_id +      │
   │                │                    │ redirect_uri 白名单│
   │                │                    │ 签 HMAC state      │
   │                │ ← 302 ─────────────│                    │
   │                │     Location:     │                    │
   │                │     https://github.com/login/oauth/authorize?│
   │                │       client_id=Iv1.x&redirect_uri=...&    │
   │                │       state=HMAC(...)|                   │
   │ ← 浏览器跳 ──→│                    │                    │
   │                │                    │                    │
   │ 用户在 GitHub 授权                       │                    │
   │                │                    │                    │
   │                │ ← GitHub 回调 ─────────────────────→│
   │                │   /auth/github/callback              │
   │                │   ?code=...&state=...&app_id=yundian │
   │                │                    │                    │
   │                │                    │ 验证 state HMAC   │
   │                │                    │ client_secret 换   │
   │                │                    │ GitHub access_token│
   │                │                    │ 调 /user + /user/   │
   │                │                    │   emails            │
   │                │                    │ 丢弃 access_token │
   │                │                    │ 签发 yunhou JWT    │
   │ ← 302 跳 BFF ──│                    │                    │
   │   #token=...   │                    │                    │
   │ ←──────────────│                    │                    │
   │                │                    │                    │
   │ BFF 从 URL     │                    │                    │
   │ fragment 读    │                    │                    │
   │ yunhou JWT     │                    │                    │
```

#### GET /auth/github/redirect

发起登录的入口。**不需要鉴权**。

**查询参数**：

| 字段 | 必填 | 说明 |
|---|---|---|
| `app_id` | 是 | Yunhou app 标识 |
| `redirect_uri` | 是 | GitHub 授权完成后的回调 URL，必须命中 `apps.config.oauth_providers.github.callback_urls` 中的某一项 |

**响应（302）**：跳转 `https://github.com/login/oauth/authorize?...&state=<HMAC>`。

**错误响应**：

| HTTP | message | 触发条件 |
|---|---|---|
| 400 | `missing app_id` / `missing redirect_uri` | `app_id` 或 `redirect_uri` 缺失 |
| 400 | `redirect_uri not in callback_urls whitelist` | `redirect_uri` 不在 callback_urls 白名单 |
| 403 | `app is disabled` | app 已停用 |
| 404 | `app not found` | `app_id` 不存在 |
| 404 | `github login not configured` | app 未配置 GitHub OAuth |
| 500 | `failed to read app config` / `failed to build authorize url` | 内部错误 |

#### GET /auth/github/callback

GitHub 回调入口。**不需要鉴权**，但 `state` 必须有效。

**查询参数**：

| 字段 | 必填 | 说明 |
|---|---|---|
| `code` | 是 | GitHub 授权码 |
| `state` | 是 | `/redirect` 签发的 HMAC state |
| `app_id` | 是 | 与 `/redirect` 时一致 |

yunhou 完成 code 换 token、拉用户信息、签发 yunhou JWT，然后 302 跳回 BFF 的 callback URL，yunhou JWT 放在 URL fragment 里：

```
https://yundian.com/auth/callback#token=<yunhou_access>&refresh_token=<yunhou_refresh>&user_id=<uuid>&has_access=<bool>
```

BFF 在前端读 `window.location.hash` 解析参数。**fragment 不会被浏览器发送到服务器或 referer 头**，所以 access_token 不会泄漏。

**错误响应**：

| HTTP | message | 触发条件 |
|---|---|---|
| 400 | `missing app_id` / `missing code or state` | `code` / `state` / `app_id` 缺失 |
| 400 | `invalid state` | state 无效或过期（5 分钟） |
| 400 | `invalid callback index` | callback 索引越界 |
| 400 | `github login not configured` | 罕有：config JSON 解析失败的 fallback（message 与下方 404 相同，但触发条件不同——通常是 `apps.config.oauth_providers.github` 结构异常，需要运营侧修复） |
| 404 | `app not found` | `app_id` 不存在 |
| 404 | `github login not configured` | app 未配置 GitHub OAuth |
| 500 | `login failed` | 内部错误（auth service 未预期异常） |
| 502 | `github upstream error` | GitHub 上游调用失败（网络、过期、配额） |

> GitHub `?error=access_denied` 等授权失败参数走另一条路径：state + app 校验通过后会**直接 302 跳回 BFF**，在 URL fragment 里塞 `error=...&error_description=...`（不返回 JSON）。BFF 端需在 `redirect_uri` 落地的回调页同时处理 fragment 里的 `token` 和 `error`。

#### 边界总结

| 信息 | yunhou 持有？ | BFF 能拿到？ |
|---|---|---|
| GitHub `client_id` | ✓ | ✓（明文） |
| GitHub `client_secret` | ✓ | ✗ |
| `callback_urls` 白名单 | ✓ | ✗ |
| GitHub `access_token`（回调后） | ✓（用即丢） | ✗ |
| yunhou `access_token` | yunhou 签发 | ✓（fragment 里） |

---

### WeChat OAuth 扫码登录流程

> **设计原则**：与 GitHub 流程同构——所有 OAuth provider 凭据（app_secret、access_token、refresh_token）由 yunhou 持有并使用，BFF 不接触任何长期秘密。BFF 端只持有 `app_id`（明文）+ 一次性 redirect_uri 白名单条目。

适用场景：消费 app 需要让终端用户用微信扫码登录（PC 浏览器场景）。Yunhou 通过 `open.weixin.qq.com/connect/qrconnect` 渲染二维码，用户用手机微信扫码并在手机上确认授权后，微信回调 yunhou 完成登录。所有 WeChat 登录必须走下方流程。

> **与 GitHub 的差异**：
> - 用户授权方式不同——GitHub 是浏览器内点同意，WeChat 是 PC 浏览器展示二维码 + 手机微信扫码
> - WeChat 多了一次 `/sns/userinfo` 调用，且**必须**返回 `unionid`，否则拒绝登录（`reason=wechat_no_unionid`）
> - state token 完全共用 GitHub 那套——`(app_id, callback_index)` 绑定，HMAC 签名，5 分钟过期

#### 配置（运营侧）

1. 在[微信开放平台](https://open.weixin.qq.com)注册一个**网站应用**（注意：不是公众号 / 小程序 / 移动应用；只有「网站应用」走 `qrconnect` 扫码登录流程）。审核通过后获得 `AppID` 和 `AppSecret`。
2. 把 `app_id` 和 `app_secret` 写入 `apps.config.oauth_providers.wechat`：

   ```json
   {
     "oauth_providers": {
       "wechat": {
         "app_id": "wx0123456789abcdef",
         "app_secret": "<32 chars plaintext — server-side only>",
         "callback_urls": [
           "https://yundian.com/auth/wechat-callback",
           "https://yundian.com/mobile/auth/wechat-callback"
         ]
       }
     }
   }
   ```

   - `app_id` 必须匹配正则 `^wx[0-9a-fA-F]{16}$`——`wx` 前缀严格小写，16 位十六进制区分大小写（Tencent 偶发分配大写 A-F）
   - `app_secret` 必须是恰好 32 字符
   - `app_secret` 仅 yunhou-users 使用，永不下发到 BFF
   - `callback_urls` 必须全部是 `https://`，或 `http://localhost` / `http://127.0.0.1` / `http://[::1]`（本地开发用）
   - 一份网站应用多个 callback URL 是合法的（如 web / iOS / Android 共用）

3. **跨 app unionid 统一**：所有 Yunhou 消费 app 的「网站应用」必须注册在**同一个微信开放平台账号**下。这是 Tencent 侧要求，不在代码层校验。运营侧在 app 上线手册里说明。

4. 设置环境变量 `OAUTH_STATE_SECRET`（必需，至少 32 个字符的随机串，建议 `openssl rand -hex 32` 生成）—— 与 GitHub 流程共用同一个变量。启动时 `Validate()` 会拒绝过短的值。

#### 流程

```
终端用户        BFF           yunhou-users                  微信
   │             │                 │                        │
   │ 点登录      │                 │                        │
   │ ──────────→│                 │                        │
   │             │ GET /auth/wechat/redirect?app_id=yundian │
   │             │   &redirect_uri=... │                   │
   │             │ ─────────────→   │                        │
   │             │                 │ 验证 app_id +           │
   │             │                 │ redirect_uri 白名单     │
   │             │                 │ 签 HMAC state           │
   │             │ ← 302 ──────────│                        │
   │             │   Location:     │                        │
   │             │   https://open.weixin.qq.com/connect/qrconnect?│
   │             │     appid=wx...&redirect_uri=...&         │
   │             │     state=HMAC(...)#wechat_redirect       │
   │             │                 │                        │
   │ 浏览器展示     │                 │                        │
   │ 二维码        │                 │                        │
   │             │                 │                        │
   │ 用手机微信                       │                        │
   │ 扫码 + 确认 ─────────────────→│                        │
   │             │                 │                        │
   │             │ ← 微信回调 ──────────────────────────────│
   │             │   /auth/wechat/callback                   │
   │             │   ?code=...&state=...&app_id=yundian    │
   │             │                 │                        │
   │             │                 │ 验证 state HMAC        │
   │             │                 │ 拿 app_secret 换        │
   │             │                 │   WeChat access_token  │
   │             │                 │   (GET /sns/oauth2/access_token)
   │             │                 │ 拿 access_token+openid │
   │             │                 │   拉用户信息            │
   │             │                 │   (GET /sns/userinfo)   │
   │             │                 │ 校验 unionid 必非空 ──→│
   │             │                 │ 丢弃 access_token       │
   │             │                 │ 丢弃 WeChat refresh_token │
   │             │                 │ 签发 yunhou JWT         │
   │ ← 302 跳 BFF ─│                 │                        │
   │   #token=... │                 │                        │
   │ ←──────────│                 │                        │
   │             │                 │                        │
   │ BFF 从 URL  │                 │                        │
   │ fragment 读 │                 │                        │
   │ yunhou JWT  │                 │                        │
```

> **WeChat `?error=access_denied` 等授权失败参数走另一条路径**：state + app 校验通过后会**直接 302 跳回 BFF**，在 URL fragment 里塞 `error=...&error_description=...`（不返回 JSON）。BFF 端需在 `redirect_uri` 落地的回调页同时处理 fragment 里的 `token` 和 `error`。
>
> **缺失 unionid 的拒绝路径**：`/sns/userinfo` 未返回 `unionid`（通常说明该网站应用没有绑定到用于跨 App 身份统一的同一微信开放平台账号；网站应用流程只请求 `snsapi_login`）→ yunhou 不会签发 yunhou JWT，而是直接 302 跳回 BFF，`#error=auth_failed&reason=wechat_no_unionid`。BFF 必须在回调页识别这个 reason 并展示对应文案。

#### GET /auth/wechat/redirect

发起扫码登录的入口。**不需要鉴权**。

**查询参数**：

| 字段 | 必填 | 说明 |
|---|---|---|
| `app_id` | 是 | Yunhou app 标识 |
| `redirect_uri` | 是 | 微信回调完成后跳转的 BFF URL，必须命中 `apps.config.oauth_providers.wechat.callback_urls` 中的某一项 |

**响应（302）**：跳转 `https://open.weixin.qq.com/connect/qrconnect?appid=...&redirect_uri=...&response_type=code&scope=snsapi_login&state=<HMAC>#wechat_redirect`。`#wechat_redirect` 片段是腾讯侧要求的，缺失会导致「该链接无法访问」。

**错误响应**：

| HTTP | message | 触发条件 |
|---|---|---|
| 400 | `app_id and redirect_uri are required` | `app_id` 或 `redirect_uri` 缺失 |
| 400 | `redirect_uri not in callback_urls whitelist` | `redirect_uri` 不在 callback_urls 白名单 |
| 403 | `app is inactive` | app 已停用 |
| 404 | `app not found` | `app_id` 不存在 |
| 404 | `wechat oauth not configured for app` | app 未配置 WeChat OAuth 块 |
| 500 | `invalid app config` | config JSON 解析失败或块结构异常 |
| 500 | `build authorize url` | state 签发等内部异常 |

#### GET /auth/wechat/callback

微信扫码授权完成后的回调入口。**不需要鉴权**，但 `state` 必须有效。

**查询参数**：

| 字段 | 必填 | 说明 |
|---|---|---|
| `code` | 是 | 微信授权码 |
| `state` | 是 | `/redirect` 签发的 HMAC state |
| `app_id` | 是 | 与 `/redirect` 时一致 |

yunhou 完成 code 换 token、调用 `/sns/userinfo` 拿 unionid、签发 yunhou JWT，然后 302 跳回 BFF 的 callback URL，yunhou JWT 放在 URL fragment 里：

```
https://yundian.com/auth/wechat-callback#token=<yunhou_access>&refresh_token=<yunhou_refresh>&user_id=<uuid>&has_access=<bool>
```

BFF 在前端读 `window.location.hash` 解析参数。**fragment 不会被浏览器发送到服务器或 referer 头**，所以 access_token 不会泄漏。

**错误响应（JSON 路径）**：

| HTTP | message | 触发条件 |
|---|---|---|
| 400 | `app_id, code, state are required` | 必填参数缺失 |
| 400 | `invalid state` | state 无效或过期（5 分钟） |
| 400 | `invalid callback index` | state 里绑定的 callback 索引越界 |
| 400 | `<error>: <error_description>` | 微信侧授权失败（state 验证失败的 JSON fallback） |
| 404 | `app not found` | `app_id` 不存在 |
| 404 | `wechat oauth not configured for app` | app 未配置 WeChat OAuth |
| 500 | `invalid app config` | config JSON 解析失败 |
| 500 | `exchange code` | code 换 token 失败（非上游错误的兜底） |
| 500 | `fetch profile` | `/sns/userinfo` 失败（非上游错误的兜底） |
| 500 | `login` | auth service 内部异常 |

**错误响应（BFF fragment 路径，302 跳转）**：

> 以下错误**不返回 JSON**，而是 302 跳回 BFF 的 `redirect_uri` 并在 URL fragment 里塞 `error=auth_failed&reason=<reason>`：

| reason | 触发条件 |
|---|---|
| `wechat_upstream` | 微信上游调用失败（`/sns/oauth2/access_token` 或 `/sns/userinfo` 返回非 2xx / errcode ≠ 0 / 网络错误 / 解码失败 / 空 openid 等） |
| `wechat_no_unionid` | `/sns/userinfo` 响应中 `unionid` 字段为空；通常说明网站应用未绑定在要求的同一微信开放平台账号下 |
| `user_not_found` | 防御性：JWT 主题无法解析到用户行（理论上不应发生）；`ErrUserDeleted` 也归并到此 reason |
| `user_suspended` | 用户账号被停用 |
| ~~`subscription_expired`~~ | **已弃用（2026-07-23）**：身份层与能力层解耦后，`findUsableSubscription`/`ErrSubscriptionExpired` 不再在 OAuth callback 触发登录失败；过期订阅表现为登录/refresh 输出或 OAuth callback fragment 中 `has_access=false`，浏览器可落在 `/console` + 续费 banner。该 reason 保留在 BFF 上但不再由 Yunhou 发出 — 若再次出现，说明别处出现了回归。 |
| `app_not_found` / `app_disabled` | `app_id` 不存在或已停用 |

#### 边界总结

| 信息 | yunhou 持有？ | BFF 能拿到？ |
|---|---|---|
| WeChat `app_id` | ✓ | ✓（明文，会出现在 `qrconnect` URL 里） |
| WeChat `app_secret` | ✓ | ✗ |
| `callback_urls` 白名单 | ✓ | ✗ |
| WeChat `access_token`（回调后） | ✓（用即丢——仅调一次 `/sns/userinfo`） | ✗ |
| WeChat `refresh_token`（回调后） | ✓（立即丢弃，无后续用途） | ✗ |
| `unionid`（用户身份锚） | ✓（DB 持久化：`social_identities.provider_uid = "wechat_" + unionid`） | ✗ |
| yunhou `access_token` | yunhou 签发 | ✓（fragment 里） |

> **跨 app unionid 统一**：yunhou 要求所有消费 app 的微信「网站应用」必须注册在**同一个微信开放平台账号**下。这样同一微信用户在不同 Yunhou 消费 app 上登录时，`unionid` 相同 → `provider_uid` 相同 → 落到同一个 Yunhou 用户行。如果违反这条（运营侧把不同 app 注册在不同的开放平台账号下），同一个微信用户会在 Yunhou 产生多个独立账号，跨 app 身份统一这条产品原则会失效。这是 Tencent 侧要求，代码层不强制。

---

### 支付接口

支付接口需要 JWT Bearer Token。所有订单、支付、退款只能由本人访问；所有权由服务端强制校验。

#### POST /payments/orders

创建订单（用户选择 plan 后发起支付）。

**请求体**：
```json
{"plan_id": "monthly", "channel": "wechat_pay"}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `plan_id` | 是 | 要购买的 Plan ID；Plan 必须启用且接受新订阅 |
| `channel` | 是 | `stripe` / `wechat_pay` / `alipay` / `paypal`；PayPal 要求 `plan.currency=USD`，WeChat Pay 要求 `plan.currency=CNY` |

**响应（201）**：
```json
{
  "code": 0,
  "data": {
    "id": "order-uuid",
    "user_id": "user-uuid",
    "plan_id": "monthly",
    "amount": 29.9,
    "currency": "CNY",
    "status": "pending",
    "expires_at": "2026-06-23T08:30:00Z",
    "created_at": "2026-06-23T08:00:00Z",
    "updated_at": "2026-06-23T08:00:00Z"
  }
}
```

订单金额和币种分别快照自 `plan.price` 与 `plan.currency`，caller 不能覆盖。PayPal 订单仅接受 USD Plan，WeChat Pay 订单仅接受 CNY Plan；不匹配返回 400 `ErrPlanCurrencyMismatch`。Stripe / Alipay 没有额外的单币种限制，沿用 Plan 币种。

订单默认 30 分钟后过期，过期后由 sweeper 翻转为 `expired`。如果此后 webhook 到达，仍会"honor the payment"激活订阅（参见 webhook 文档 §8）。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 400 | `plan not found` | `plan_id` 不存在 |
| 400 | `plan is inactive` | Plan 已停用 |
| 400 | `plan currency does not match order currency` (`ErrPlanCurrencyMismatch`) | Plan 币种不满足渠道要求（PayPal→USD，WeChat Pay→CNY） |
| 409 | `plan is not accepting new subscriptions` (`ErrPlanNotAcceptingNew`) | Plan 已停售新订阅（例如 legacy `quarterly`） |
| 409 | `user already has an active subscription` | 用户已有活跃订阅 |

#### GET /payments/orders

列出当前用户的订单（按 `created_at DESC`，最新在前）。只能看到自己的订单。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "order-uuid-2",
      "user_id": "user-uuid",
      "plan_id": "monthly",
      "amount": 29.9,
      "currency": "CNY",
      "status": "paid",
      "expires_at": "2026-07-23T08:30:00Z",
      "created_at": "2026-06-23T08:00:00Z",
      "updated_at": "2026-06-23T08:05:00Z"
    }
  ]
}
```

`data` 为订单数组（可能为空 `[]`），元素结构与 `GET /payments/orders/:id` 一致。

#### GET /payments/orders/:id

查询订单详情。只能查自己的订单；不是本人或 ID 不存在都返回 404（避免枚举）。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "order-uuid",
    "user_id": "user-uuid",
    "plan_id": "monthly",
    "amount": 29.9,
    "currency": "CNY",
    "status": "pending",
    "expires_at": "2026-06-23T08:30:00Z",
    "created_at": "2026-06-23T08:00:00Z",
    "updated_at": "2026-06-23T08:00:00Z"
  }
}
```

`status` 取值：`pending` / `paid` / `failed` / `refunded` / `cancelled` / `expired`。

**响应（404）**：
```json
{"code": 404, "message": "not found"}
```

#### DELETE /payments/orders/:id

取消未支付订单。只有 `pending` 状态可以取消；其他状态返回 409。

**响应（200）**：
```json
{"code": 0, "message": "cancelled"}
```

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 404 | `not found` | 订单不存在或不属于当前用户 |
| 409 | `order is not in pending status` | 订单已支付/已取消/已过期/已失败/已退款 |

#### POST /payments/orders/:order_id/confirm

前端 SDK 在收到 channel 回调后调用此接口做"快通道确认"。后端会与随后到达的 webhook 互为幂等。

**请求体**：
```json
{
  "channel": "stripe",
  "external_txn_id": "pi_xxx",
  "expires_at": "2026-07-23T00:00:00Z"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `channel` | 是 | `stripe` / `wechat_pay` / `alipay` / `paypal` |
| `external_txn_id` | 是 | 渠道侧交易 ID |
| `expires_at` | 否 | RFC3339 时间，订阅过期时刻。**前端必须从 `plan.interval_days` + 业务规则（rollover/grace/trial）计算**；yunhou-users 不做服务端推导。省略 = 永不过期。 |

`amount` / `currency` **不接受 caller 输入**；订单行是权威来源。caller 输入金额会让用户把 $1 订单声称 $100。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "payment_id": "payment-uuid",
    "order_id": "order-uuid",
    "status": "paid",
    "activated_subscription": true,
    "was_late_payment": false
  }
}
```

`was_late_payment` 为 `true` 表示该订单已过期才确认成功（系统会 honor 该支付并激活订阅，对应 `audit_log` 记录 `late_payment_post_expiry`）。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid channel` | `channel` 取值不在 `stripe` / `wechat_pay` / `alipay` / `paypal` 之内 |
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 404 | `not found` | 订单不存在或不属于当前用户 |
| 409 | `order is in a non-recoverable terminal state` | 订单已是 `failed` / `refunded` 终态 |
| 409 | `order already has a paid payment on a different channel` | 该订单已有其他渠道的 paid 记录 |

#### GET /payments

列出当前用户的所有支付记录（无分页，按 `created_at` **降序**）。

> 注意：`paid_at`、`failed_reason`、`disputed_at`、`external_txn_id` 等字段带 `omitempty`——仅在适用状态出现，**缺席不等于 `null`**。例如 `paid` 状态没有 `failed_reason` 字段；BFF 解析时应做"字段是否存在"判断，不要假设值为 `null`。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440020",
      "order_id": "550e8400-e29b-41d4-a716-446655440015",
      "channel": "stripe",
      "external_txn_id": "pi_xxx",
      "amount": 29.9,
      "currency": "CNY",
      "status": "paid",
      "paid_at": "2026-06-23T08:00:30Z",
      "failed_reason": null,
      "disputed": false,
      "disputed_at": null,
      "raw_payload": "{\"id\":\"evt_xxx\",...}",
      "created_at": "2026-06-23T08:00:30Z",
      "updated_at": "2026-06-23T08:00:30Z"
    }
  ]
}
```

> `id` 是系统内部 UUID；渠道侧的交易 ID 是 `external_txn_id`（例如 Stripe 的 `pi_xxx`）。

`status` 取值：`pending` / `paid` / `failed` / `refunded`。

#### GET /payments/:id

查询支付详情。只能查自己的支付（通过 order → user_id 关联校验）。字段缺席语义与 `GET /payments` 相同（`omitempty`：`paid_at`/`failed_reason`/`disputed_at` 等仅在适用状态出现）。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440020",
    "order_id": "550e8400-e29b-41d4-a716-446655440015",
    "channel": "stripe",
    "external_txn_id": "pi_xxx",
    "amount": 29.9,
    "currency": "CNY",
    "status": "paid",
    "paid_at": "2026-06-23T08:00:30Z",
    "disputed": false,
    "raw_payload": "{\"id\":\"evt_xxx\",...}",
    "created_at": "2026-06-23T08:00:30Z",
    "updated_at": "2026-06-23T08:00:30Z"
  }
}
```

**响应（404）**：
```json
{"code": 404, "message": "not found"}
```

#### GET /payments/:id/refunds

列出某支付的所有退款记录。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440030",
      "payment_id": "550e8400-e29b-41d4-a716-446655440020",
      "channel": "stripe",
      "user_id": "550e8400-e29b-41d4-a716-446655440002",
      "amount": 29.9,
      "reason": "用户申请",
      "idempotency_key": "user-req-001",
      "external_refund_id": "re_xxx",
      "status": "paid",
      "created_at": "2026-06-23T09:00:00Z",
      "updated_at": "2026-06-23T09:01:00Z"
    }
  ]
}
```

#### POST /refunds

发起退款。需要 `Idempotency-Key` 头（8-128 字符，`[A-Za-z0-9_.:-]+`）做 caller-retry 保护。

**请求体**：
```json
{"payment_id": "payment-uuid", "amount": 10.0, "reason": "用户申请"}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `payment_id` | 是 | 支付 ID |
| `amount` | 是 | 退款金额，> 0 且 ≤ 该支付已 paid 的剩余可退额度 |
| `reason` | 否 | 退款原因 |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440030",
    "payment_id": "550e8400-e29b-41d4-a716-446655440020",
    "channel": "stripe",
    "user_id": "550e8400-e29b-41d4-a716-446655440002",
    "amount": 10.0,
    "reason": "用户申请",
    "idempotency_key": "user-req-001",
    "external_refund_id": "re_xxx",
    "status": "pending",
    "created_at": "2026-06-23T09:00:00Z",
    "updated_at": "2026-06-23T09:00:00Z"
  }
}
```

退款 `status` 流转：`pending → paid`（渠道 webhook 确认）。**v1 不会产生 `failed` 状态**：渠道侧拒绝会直接以 `502 channel refund API call failed` 返回，发生在 INSERT 之前，不会留下 `failed` 记录。

> **不要把 `POST /refunds` 当成同步接口**——handler 调用 channel 退款 API 成功后立刻返回 `status: pending`，**不**会同步翻 `payment.status` 也不会立刻取消订阅。完整退款的 `payment.status → refunded` 与"取消该订单 plan 的活跃订阅"（不影响其他 plan 的订阅）由**随后的 channel webhook** 异步完成；部分退款不影响订阅。集成方需要通过 `GET /payments/:id` 或订阅查询接口跟踪最终结果，不要在 `POST /refunds` 返回 200 后立刻假设订阅已停。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 400 | `missing Idempotency-Key header` | 缺少 `Idempotency-Key` 请求头 |
| 400 | `Idempotency-Key must be 8-128 characters` | key 长度不在 8–128 |
| 400 | `Idempotency-Key must match [A-Za-z0-9_.:-]+` | key 含非法字符 |
| 400 | `refund amount must be > 0 and <= payment amount` | `amount` 非法 |
| 400 | `sum of refunds would exceed payment amount` | 已退 + 本次超出支付金额 |
| 404 | `not found` | payment 不存在或不属于当前用户 |
| 409 | `payment is not in paid status` | payment 非 `paid` 状态 |
| 502 | `channel refund API call failed` | 渠道侧 API 调用失败 |

#### GET /refunds/:id

查询退款详情。只能查自己的退款（payment → order → user_id 关联校验）。

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440030",
    "payment_id": "550e8400-e29b-41d4-a716-446655440020",
    "channel": "stripe",
    "user_id": "550e8400-e29b-41d4-a716-446655440002",
    "amount": 10.0,
    "reason": "用户申请",
    "idempotency_key": "user-req-001",
    "external_refund_id": "re_xxx",
    "status": "paid",
    "created_at": "2026-06-23T09:00:00Z",
    "updated_at": "2026-06-23T09:01:00Z"
  }
}
```

**响应（404）**：
```json
{"code": 404, "message": "not found"}
```

---

### 渠道 Webhook 回调

POST `/webhooks/payment/:channel`，由渠道方调用，**不需要 JWT**，走签名校验。响应永远在事务提交后返回。错误响应分类：

| HTTP | message | 触发条件 | 渠道是否会重试 |
|------|---------|----------|---------------|
| 400 | `invalid signature` | 签名 / 时间戳 / replay window 校验失败 | 否（重试也是同样结果） |
| 404 | `unknown channel` | 该 channel 对应的 webhook secret/key 未配置（如 `STRIPE_WEBHOOK_SECRET` 空时 Stripe 收 404；`WECHAT_PAY_API_V3_KEY` 空时 WeChat 收 404；`ALIPAY_PUBLIC_KEY_PATH` 空时 Alipay 收 404；`PAYPAL_WEBHOOK_ID_SANDBOX` / `PAYPAL_WEBHOOK_ID_LIVE` 空时 PayPal 收 404）。这是"channel 没启用"语义，运营侧需检查对应 env 是否漏配 | 否（重试同样 404） |
| 500 | `signature verification failed` / `handler error` | 临时错误（DB 抖动、PayPal 上游 verify 接口超时等） | 是（渠道按其重试策略） |

成功响应统一格式（标准 envelope）：

```json
{
  "code": 0,
  "data": {
    "received": true,
    "duplicate": false,
    "domain_action": "payment_paid"
  }
}
```

`domain_action` 取值：`payment_paid` / `payment_failed` / `refund_paid` / `payment_disputed` / `payment_dispute_closed` / `none`。其中 `none` 表示"事件类型不在我们关心的范围内"（被记账但不触发业务动作）。

**重要**：
- **判别 dedupe 请用 `duplicate: true`**，不要用 `domain_action == "none"`——后者只是"事件被记录但未触发业务动作"，不代表已处理。
- **dedupe 命中时 `domain_action` 字段为空字符串**（**不是** `none` 也不是某个已知值）——handler 在 dedupe 命中路径上不会经过 switch 分支直接返回。所以重复事件的真实 payload 是 `{"received": true, "domain_action": "", "duplicate": true}`。做 retry 决策时只信 `duplicate` 字段。

订阅过期时间通过 channel metadata 传入（RFC3339）：Stripe `data.object.metadata.sub_expires_at`、WeChat 解密后的 `resource.sub_expires_at`、Alipay form 字段 `sub_expires_at`、PayPal `resource.billing_info.next_billing_time`（renewal `PAYMENT.SALE.COMPLETED` 事件携带；其他事件若无则忽略）。**BFF 应把服务端 `/apps/:id/quote` 返回的 `sub_expires_at` 写入 checkout metadata**；webhook handler 不会重新计算收到的 metadata，而是按渠道回传值处理。

### Quote 路径 vs Confirm 路径：sub_expires_at 来源冲突

订单可能通过两条路径被标记为已支付——生产环境更常见的场景是 **race**（同一笔订单的 webhook 与前端 `/confirm` 几乎同时到达），而不是"业务上选哪条都行"：

- **Quote 路径**：`/quote` 返回 `sub_expires_at`，BFF 把它嵌入 channel checkout metadata（PayPal `metadata.sub_expires_at`，Stripe/WeChat/Alipay 等价字段）；channel webhook 到达后 yunhou 直接写入 `subscriptions.expires_at`
- **Confirm 路径**：`POST /payments/orders/:order_id/confirm` 由 BFF 在前端检测到 channel 支付成功时主动调用，`expires_at` 由 BFF 自己算后透传

两条路径在 `subscriptions.expires_at` 这一列是**最后写入胜出**（`activateSubscriptionOnTx` 是一个盲 UPSERT，没有"哪个值更权威"的判断逻辑；见 `internal/service/payment.go`）。如果两条路径在同一订单上前后到达且 `expires_at` 计算口径不一致，**先到的被覆盖，结果不可预测**。

**推荐契约**（避免 last-write-wins 模糊性）：

1. **有 channel webhook 携带 `sub_expires_at` 的渠道**（Stripe / WeChat / Alipay / PayPal）：webhook 是权威源。`/confirm` 调用时**不要传 `expires_at`**——保持 `nil`，让 webhook 的值留下
2. **没有 channel webhook 的渠道**（理论上不存在；当前四个渠道都支持）：BFF 必须在 `/confirm` 里提供 `expires_at`
3. **`/quote` 的输出仅作为 webhook metadata 的来源**，不作为最终值——BFF 在 webhook 到达前可以展示它给用户，但订阅激活一律以 webhook payload 为准

如果你们的 Stripe / WeChat / Alipay / PayPal 流程会**并发触发** webhook 与 `/confirm`（前端轮询订单状态 + channel 同时回调的常见 race），请务必遵守契约 (1)，否则会出现"前端显示 7 天试用，但实际订阅 30 天"之类的对账漂移。

---

## 内部服务鉴权

`/apps/*`（除 `/apps/:id/plans` 与 `/apps/:id/quote` 外）和所有 `/admin/*` 路径走 `InternalAppAuth` 中间件，要求 BFF 调用方带两个头：

| Header | 说明 |
|---|---|
| `X-App-ID` | 调用的目标 app 的 `app_id`（必须存在、`is_active = true`） |
| `X-App-Secret` | 该 app 的 64 位十六进制共享密钥。**仅 `POST /admin/apps` 创建响应或 `POST /admin/apps/:id/rotate-secret` 响应里的 `data.secret` 一次性返回**——服务端只存 bcrypt 哈希，无法再读出 |

错误响应：

缺失 `X-App-ID` 会返回独立错误，便于调用方修正请求。提供 `X-App-ID` 后，其余失败路径使用统一的 `invalid app_secret`，避免通过差异化响应枚举 app 状态或 secret 配置。

| HTTP | message | 触发条件 |
|------|---------|----------|
| 401 | `missing X-App-ID header` | 缺少 `X-App-ID` |
| 401 | `invalid app_secret` | 其余鉴权失败：app 不存在/停用、缺少或不匹配 `X-App-Secret`、`secret_hash` 未初始化 |

**Rotation 流程**：怀疑 `X-App-Secret` 泄漏（例如 BFF 容器镜像被 pull 过、CI 缓存里出现过）时，立即调：

```bash
curl -X POST https://<YOUR_YUNHOU_HOST>/admin/apps/yundian/rotate-secret \
  -H "X-App-ID: yundian" \
  -H "X-App-Secret: <当前 secret>"
```

响应里 `data.secret` 是新的 64 位 hex，旧 secret 立即失效（**无 grace period**）。把新值部署到 BFF 后，下一次调用即生效。

**部署侧建议**：除了 `X-App-Secret` 服务端校验，部署侧也建议对所有走 `InternalAppAuth` 的端点做 nginx IP 白名单 / VPC 限制：
- `GET /apps`、`GET /apps/:id`
- `GET /apps/:id/provider-token/:channel`
- `GET /admin/plans`、`GET /admin/plans/:id`、`PATCH/DELETE /admin/plans/:id`、`POST /admin/plans`
- `POST /admin/apps`、`PATCH /admin/apps/:id`、`POST /admin/apps/:id/rotate-secret`

把 BFF 出口段固定下来。两层防御互不替代——服务端 secret 防的是凭据泄漏，IP 白名单防的是 endpoint 暴露面。

---

## JWT 验证

应用可以在本地验证 Access Token，无需每次都调用 Yunhou Users 服务端。

### 获取公钥

```
GET /.well-known/jwks.json
```

响应：
```json
{
  "keys": [
    {
      "kty": "RSA",
      "kid": "yunhou-users-rsa",
      "alg": "RS256",
      "use": "sig",
      "n": "<base64url-encoded-modulus>",
      "e": "AQAB"
    }
  ]
}
```

### JWT Claims

| Claim | 说明 |
|-------|------|
| `sub` | 用户 ID（UUID） |
| `iss` | 固定值 `"yunhou-users"`（服务端会校验） |
| `aud` | 登录时请求的 App ID（数组；服务端会校验是否包含 `app_id`） |
| `app_id` | 登录时请求的 App ID（与 `aud[0]` 一致） |
| `scope` | 有效订阅 Plan 的 Apps 列表（`[]string`）；无订阅、订阅过期或 Plan 停用时为 `[]` |
| `exp` | 过期时间（Unix 秒） |
| `iat` | 签发时间（Unix 秒） |

### 验证步骤

1. 获取 JWKS（建议缓存，TTL 建议 1 小时）
2. 使用 `kid=yunhou-users-rsa` 匹配公钥
3. 使用 RS256 算法验证 JWT 签名
4. 检查 `iss` 是否为 `"yunhou-users"`
5. 检查 `exp` 确认 Token 未过期
6. 检查 `aud` 包含目标 App ID

---

## 数据模型

### Plan

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | Plan ID（如 `monthly`, `quarterly`） |
| `name` | string | 显示名称 |
| `price` | decimal | 价格；必须 `>= 0` |
| `interval_days` | int | 订阅周期（天），0 表示永久；必须 `>= 0` |
| `apps` | string[] | 该 Plan 可访问的 App 列表；管理端写入时每个 App 必须存在且启用 |
| `is_active` | bool | 是否启用；停用后不授予访问 scope |
| `is_listed` | bool | 是否在商业目录中展示；与是否接受新订阅相互独立 |
| `accepting_new_subscriptions` | bool | 是否允许创建新订阅/订单；不影响既有订阅续费 |
| `currency` | string | 结算币种；只能是 `CNY` / `USD` / `EUR` |
| `trial_days` | int | 试用天数；必须 `>= 0`，quote 的权威来源 |
| `description` | string? | 可空的营销文案 |
| `display_order` | int | 目录排序值；数值越小越靠前 |
| `updated_at` | datetime | 更新时间；只读，由数据库 trigger 管理 |
| `created_at` | datetime | 创建时间 |

### App

| 字段 | 类型 | 说明 |
|------|------|------|
| `app_id` | string | App ID（如 `yundian`） |
| `name` | string | App 名称 |
| `description` | string | 描述 |
| `config` | jsonb | 扩展配置（可选，默认 `{}`） |
| `is_active` | bool | 是否启用 |
| `created_at` | datetime | 创建时间 |
| `updated_at` | datetime | 更新时间 |

### Subscription

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 订阅 ID |
| `user_id` | string | 用户 ID |
| `plan_id` | string | 订阅的 Plan ID |
| `status` | string | 状态：`active` / `expired` / `cancelled` |
| `started_at` | datetime | 开始时间 |
| `expires_at` | datetime? | 过期时间，null 表示永不过期 |
| `external_subscription_id` | string? | 渠道侧订阅 ID（当前仅 PayPal 的 `I-...`）；非 PayPal 渠道该字段**缺席**（`omitempty`，不是 `null`）。用于 webhook 续费事件反查订阅 |
| `created_at` | datetime | 创建时间 |
| `updated_at` | datetime | 更新时间 |

### User

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 用户 ID（UUID） |
| `nickname` | string? | 昵称 |
| `email` | string? | 邮箱 |
| `avatar_url` | string? | 头像 URL |
| `status` | string | 状态：`active` / `suspended` / `deleted` |
| `created_at` | datetime | 创建时间 |
| `updated_at` | datetime | 更新时间 |

### SocialIdentity

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 身份 ID |
| `user_id` | string | 所属用户 |
| `provider` | string | 提供方：`github` / `wechat`（DB CHECK 约束还允许 `google`，但当前代码未启用） |
| `provider_uid` | string | 提供方用户 ID |
| `email` | string? | 关联邮箱 |
| `created_at` | datetime | 创建时间 |

### Session

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 会话 ID |
| `user_id` | string | 用户 ID |
| `app_id` | string | App ID |
| `session_type` | string | 类型：`refresh` |
| `scope` | string[] | 权限范围（Plan 的 apps） |
| `revoked` | bool | 是否已撤销 |
| `expires_at` | datetime | 过期时间 |
| `created_at` | datetime | 创建时间 |

---

## 错误码

| HTTP 状态码 | code | 说明 |
|------------|------|------|
| 200 | 0 | 请求成功 |
| 201 | 0 | 创建成功 |
| 400 | 400 | 请求参数错误（含签名/Idempotency-Key 不合法） |
| 401 | 401 | 未认证（Token 无效或过期、Provider 验证失败、未提供 `X-App-ID` / `X-App-Secret`、内部服务 secret 不匹配） |
| 403 | 403 | 无权限（如试图自助订阅付费 Plan、App 已停用） |
| 404 | 404 | 资源不存在 |
| 409 | 409 | 资源冲突（已存在活跃订阅、订单非 pending 等） |
| 429 | 429 | 请求过于频繁 |
| 500 | 500 | 服务器内部错误 |
| 502 | 502 | 渠道上游调用失败（如渠道侧退款 API 拒绝） |
| 503 | 503 | 服务暂不可用（如 DB 不可达） |

---

## 频率限制

> 不同接口挂的鉴权中间件不一样，**先看清鉴权列再对照本表**——「App 接口」一节有完整对照（路径 ↔ 鉴权 ↔ 用途）。本表只列限频策略；鉴权要求以接口章节为准。

| 接口类别 | 限制 | 说明 |
|---------|------|------|
| 公共接口（`/healthz`, `/.well-known/jwks.json`, `/auth/refresh`, `/auth/logout`, `/auth/github/*`, `/auth/wechat/*`, `/test/login`, `/apps/:id/plans`） | 10 次/秒，突发 20 | 按客户端 IP 限制；`/healthz` 不在 limiter 路径内（最早期注册，绕过 limiter）；`/apps/:id/plans` 公共可访问（无需鉴权）；`/auth/wechat/redirect` 与 `/auth/wechat/callback` 与 `/auth/github/*` 共用同一 limiter |
| 内部服务接口（`/apps`, `/apps/:id`, `/apps/:id/provider-token/:channel`, `/admin/*`） | 30 次/秒，突发 60 | 按客户端 IP 限制；要求 `X-App-ID` 头 + `X-App-Secret` 头 |
| 用户态接口（`POST /apps/:id/quote`, `POST /chat`, `/payments/*`, `/refunds/*`） | 30 次/秒，突发 60 | 按客户端 IP 限制；要求 JWT（终端用户身份）。`POST /chat` 为独立桶：10 次/秒，突发 20（每请求消耗模型额度，桶更紧且与其他接口隔离） |
| 用户接口（`/user/*`） | 无显式限制 | 仅要求 JWT |
| 渠道 Webhook（`/webhooks/payment/*`） | 200 次/秒，突发 400 | 走签名校验，不限 IP 业务速率 |

---

## 快速接入清单

- [ ] 获取应用的 `app_id` + `app_secret`（`POST /admin/apps` 响应里 `data.secret`，仅一次性返回，需立即落地）
- [ ] BFF 仅在调用 `GET /apps`、`GET /apps/:id`、`GET /apps/:id/provider-token/:channel` 和所有 `/admin/*` 时带 `X-App-ID` + `X-App-Secret`；`/apps/:id/plans` 与 `/apps/:id/quote` 是公共/用户态接口，无需内部服务鉴权
- [ ] 实现 GitHub OAuth 重定向登录：BFF 302 到 `/auth/github/redirect?app_id=<app_id>&redirect_uri=<回调 URL>`，让浏览器走完 GitHub 授权；回调页从 URL fragment 解析 `token` / `refresh_token` / `user_id` / `has_access`（注意 fragment 不会上行到服务器，BFF 必须前端 JS 解析）
- [ ] （可选）实现 WeChat OAuth 扫码登录：BFF 302 到 `/auth/wechat/redirect?app_id=<app_id>&redirect_uri=<回调 URL>`，PC 浏览器展示二维码；用户手机微信扫码后在回调页同样从 URL fragment 解析 token；需要识别 fragment 里的 `error=auth_failed&reason=wechat_no_unionid` 并展示对应文案；所有 Yunhou 消费 app 的「网站应用」必须注册在**同一个微信开放平台账号**下才能跨 app unionid 统一
- [ ] 解析响应中的 `subscription.has_access` 字段，判断用户是否有权限访问
- [ ] 如果 `subscription.has_access` 为 `false`，提示用户订阅/升级；过期订阅用保留的 `subscription.plan_id` 渲染续费 CTA
- [ ] 使用 `access_token` 调用用户接口
- [ ] 实现 Token 刷新逻辑，处理 Refresh Token 轮转（每次 refresh 必须使用返回的新 refresh_token，旧 token 立即失效）
- [ ] 获取 JWKS 配置本地 JWT 验证（**必须**，不要每次请求都把 token 回传给 yunhou-users 校验）
- [ ] （可选）接入 Chat：`POST /chat` 携带用户 JWT + `{"messages":[...]}`，按 SSE 解析回复；无订阅用户收到 403，未启用收到 404；需要按会话分组审计时传 `session_id`（详见「Chat 接口」章节）
- [ ] 怀疑 `app_secret` 泄漏时调 `POST /admin/apps/:id/rotate-secret`，旧 secret 立即失效
