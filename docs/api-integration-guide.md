# Yunhou Users API 接入文档

本文档面向内部应用开发者，介绍如何接入 Yunhou Users 共享用户管理系统。

## 概述

Yunhou Users 是一个共享用户管理 API，所有接入的应用共享同一套用户身份——每个用户只需一个账号即可使用所有接入应用。系统支持社交账号 OAuth 登录（GitHub、Google）。

核心概念：
- **Plan（订阅计划）**：定义可访问的 App 列表（free/monthly/quarterly/yearly）
- **App（应用）**：接入的系统，如 yundian、yundash
- **Subscription（订阅）**：用户订阅某个 Plan

用户登录时，系统根据其订阅的 Plan 判断可以访问哪些 App。

---

## 快速接入

### 1. 配置 App

管理员在后台创建 App 后，应用获得 `app_id`（如 `yundian`）。

### 2. 用户登录

用户在你的应用中点击登录后：

1. 你的前端通过 OAuth 获取用户的 Google access token（GitHub 登录请使用 §"GitHub OAuth 授权码流程"，本接口不接受 `provider=github`）
2. 你的后端调用 `/auth/login`，传入 provider token
3. 系统返回 JWT access token + refresh token，以及用户的订阅信息

```bash
curl -X POST https://your-yunhou-domain/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "google",
    "provider_token": "ya29.xxxxxxxxxxxx",
    "app_id": "yundian"
  }'
```

响应：
```json
{
  "code": 0,
  "data": {
    "access_token": "eyJhbGciOiJSUzI1NiIs...",
    "refresh_token": "a1b2c3d4e5f6...",
    "user": {
      "id": "550e8400-e29b-41d4-a716-446655440002",
      "nickname": "张三",
      "email": "user@example.com",
      "avatar_url": "https://avatars.githubusercontent.com/u/12345"
    },
    "subscription": {
      "plan_id": "monthly",
      "plan_name": "按月订阅",
      "has_access": true,
      "expires_at": "2026-12-19T00:00:00Z"
    }
  }
}
```

**关键字段**：`has_access` 表示用户是否可以访问当前 App。如果为 `false`，用户可能需要升级订阅。

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
用户          你的应用           Yunhou Users         OAuth 提供方
 │              │                     │                    │
 │──登录───→│                     │                    │
 │              │──获取 provider ───→│                    │
 │              │   token            │                    │
 │←──token──│                     │                    │
 │              │──POST /auth/login →│                    │
 │              │  {provider_token}  │──验证 token ─────→│
 │              │                    │←──用户信息─────────│
 │              │←─JWT + refresh ──│                    │
 │←──完成───│                     │                    │
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

#### POST /auth/login

> **GitHub 登录已迁移到 redirect 流程（见 §"GitHub OAuth 授权码流程"）。本接口不再接受 `provider=github`**——直接传 `provider_token` 的设计违反了"凭据由 yunhou 持有"的边界（详见 CLAUDE.md §"GitHub OAuth Boundary"）。Google 登录仍可走直传路径，未来会同步迁移。

登录接口（仅 Google provider；GitHub 请用 redirect 流程）。

**请求体**：
```json
{
  "provider": "google",
  "provider_token": "ya29.xxx",
  "app_id": "yundian"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `provider` | 是 | 登录方式：当前仅支持 `google`（`github` 见下方 redirect 流程） |
| `provider_token` | 是 | OAuth provider 的 access token |
| `app_id` | 是 | 要访问的应用 ID |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "access_token": "eyJhbGciOiJSUzI1NiIs...",
    "refresh_token": "a1b2c3d4...",
    "user": {
      "id": "uuid",
      "nickname": "张三",
      "email": "user@example.com",
      "avatar_url": "https://..."
    },
    "subscription": {
      "plan_id": "monthly",
      "plan_name": "按月订阅",
      "has_access": true,
      "expires_at": "2026-12-19T00:00:00Z"
    }
  }
}
```

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `unsupported provider: <name>` | `provider` 取值不在 `github` / `google` 之内 |
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 401 | `invalid provider token` | OAuth provider 拒绝该 token |
| 401 | `app not found` / `app is inactive` | `app_id` 不存在或已停用 |
| 401 | `user is suspended` / `user is deleted` | 用户账号被停用或已删除 |
| 401 | `subscription expired` | 用户有订阅但已过期 |

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

> **注意**：刷新时旧的 refresh token 会失效，必须使用返回的新 token。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺少 `refresh_token` |
| 401 | `invalid refresh token` | refresh token 不存在、已撤销、已轮换、已过期 |
| 401 | `user is suspended` / `user is deleted` | 用户账号被停用或已删除 |
| 401 | `app not found` / `app is inactive` | 解析得到的 `app_id` 不存在或已停用 |
| 401 | `subscription expired` | 用户有订阅但已过期 |

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

#### GET /apps/:id/plans

公共的 Plan 目录接口，**无需鉴权**（无需 `X-App-ID`、无需 JWT）。返回指定 App 当前启用的 Plans，每个 Plan 附带上游渠道的 plan_id / variant_id 与解析后的 trial / billing cycle，方便营销页直接渲染价格 + "前 X 天免费，之后每 Y 天 $Z"。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "free",
      "name": "免费",
      "price": 0,
      "interval_days": 0,
      "is_default": true,
      "provider_ids": {},
      "cycle": null
    },
    {
      "id": "monthly",
      "name": "按月订阅",
      "price": 29.9,
      "interval_days": 30,
      "is_default": false,
      "provider_ids": {"paypal": "P-MONTHLY-7D", "lemonsqueezy": "var-MONTHLY"},
      "cycle": {"trial_days": 7, "billing_cycle_days": 30}
    }
  ]
}
```

**响应字段**：

| 字段 | 说明 |
|------|------|
| `provider_ids` | 该 Plan 在每个已配置渠道下对应的 provider plan/variant ID。未配置渠道不出现在 map 中。无任何渠道配置时为 `{}`（BFF 即可判定"当前 App 该 Plan 暂无可下单渠道"）。 |
| `cycle` | 解析后的试用 + 计费周期；用于营销页文案。未配置任何渠道时为 `null`（BFF 应回退到 `interval_days`）。 |

**cycle 解析规则**：当 PayPal 和 LemonSqueezy 都为同一 `plan_id` 配置了 plan 记录时，**PayPal 的 `trial_days` + `billing_cycle_days` 胜出**。运营侧需保证 PayPal 控制台上的账单周期与这里写下的值一致，否则营销页展示的 "X 天免费 / Y 天周期" 跟实际结算会不一致。

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

创建订阅。仅免费 Plan（`price == 0`）允许用户自助创建；付费 Plan 必须通过支付流程创建（参见 §支付接口）。`expires_at` 字段被服务层忽略，过期时间由 `plan.interval_days` 推导（`interval_days == 0` 表示永不过期）。

**请求体**：
```json
{
  "plan_id": "free",
  "expires_at": "2026-07-19T00:00:00Z"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `plan_id` | 是 | 订阅的 Plan ID；只接受 `price == 0` 的 Plan |
| `expires_at` | 否 | **忽略字段**（保留仅为向后兼容）。过期时间由 `plan.interval_days` 决定 |

**响应（201）**：
```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440010",
    "user_id": "550e8400-e29b-41d4-a716-446655440002",
    "plan_id": "free",
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

### App 接口

App 相关接口分散在三种鉴权风格下，BFF 接入时务必看清楚：

| 路径 | 鉴权 | 用途 |
|------|------|------|
| `GET /apps/:id/plans` | **无需鉴权**（公共） | 营销页拉取 Plan 目录 |
| `GET /apps/:id/provider-token/:channel` | **`X-App-ID` + `X-App-Secret`**（内部服务） | BFF 拉取 PayPal/LS 凭据再调用上游 |
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
        "monthly": {
          "plan_id": "P-MONTHLY-7D",
          "trial_days": 7,
          "billing_cycle_days": 30
        }
      }
    },
    "lemonsqueezy": {
      "api_key": "lsq_...",
      "store_id": "12345",
      "plans": {
        "monthly": {
          "variant_id": "var-MONTHLY",
          "trial_days": 0,
          "billing_cycle_days": 30
        }
      }
    }
  }
}
```

要点：

- 每个 `<channel>.plans` 是 `plan_id -> 配置对象` 的 map；`plan_id` 与业务 `plans.id` 同名（运营侧负责对齐）。
- `trial_days` / `billing_cycle_days` 决定 `sub_expires_at = now + trial + cycle` 的计算结果；务必与渠道控制台账单周期同步。`billing_cycle_days` 缺省时回退到 `plans.interval_days`。
- `brand.name` 缺省回退到 `apps.name`，对应 PayPal `application_context.brand_name` 与 LS `checkout_data.custom.brand`。
- v2 schema 把早期"扁平的 `{plan_id: "P-…"}` map" 改成了嵌套对象形；现存配置行如果有旧形态，需通过 `PATCH /admin/apps/:id` 重写为新形态，否则该 plan 在 quote / catalog 接口里查不到。

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

为 BFF 拉取 PayPal / LemonSqueezy 上游凭据，避免敏感凭据下沉到消费方代码（`BFF fetch upstream with short-lived PayPal token` / `BFF sign LS requests with api_key`，不需要在 BFF 端做长期凭据托管）。**鉴权为 `X-App-ID` + `X-App-Secret` 内部服务头对**——BFF 后端用其内部服务身份调用，绝不下发给终端用户；`X-App-Secret` 是每个 app 创建时一次性返回的 64 位十六进制串，仅 bcrypt 哈希存库，丢失后必须通过 `POST /admin/apps/:id/rotate-secret` 重新生成。

**路径参数**：

| 字段 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | App ID |
| `channel` | 是 | `paypal` 或 `lemonsqueezy` |

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

**响应（200）**—— LemonSqueezy：
```json
{
  "code": 0,
  "data": {
    "channel": "lemonsqueezy",
    "api_key": "lsq_xxxxx"
  }
}
```

行为差异：

- PayPal：yunhou-users 真正去 PayPal OAuth `client_credentials` 接口拿 access token，并在进程内缓存 `expires_in − 60s`（即 PayPal 实际返回的剩余有效期减去 60 秒安全余量；典型 ~9 小时，最短不会低于 60 秒）。并发去重（同一 `client_id` 同时只有一次上游调用）；单 Yunhou 实例维度缓存，多实例各自刷新（PayPal 的 `client_credentials` 对相同凭据幂等）。
- LemonSqueezy：仅返回 `apps.config.payment_providers.lemonsqueezy.api_key`（LS webhook-only，不消耗 access token）。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `unsupported channel` | `channel` 取值不在 `paypal` / `lemonsqueezy` |
| 400 | `provider not configured for app` | App 未配置对应 provider 块 |
| 403 | `app is disabled` | App 已停用 |
| 404 | `app not found` | App 不存在 |
| 500 | `provider token service unavailable` | 服务依赖未注入（理论上不会发生；防御性兜底） |
| 502 | `provider upstream error` | PayPal OAuth 调用失败（网络、认证、配额）；LemonSqueezy 路径目前只返回静态 `api_key`，不产生 502 |

#### POST /apps/:id/quote

下单前的"取报价"接口。BFF 拿到 `data` 后直接把 `provider_data` 透传给 PayPal/LS 创建 checkout session；`sub_expires_at` 是 yunhou-users 在 webhook 确认订阅时填进 `subscriptions.expires_at` 的值。**鉴权为 JWT Bearer**（终端用户身份触发；调用方必须先 `/auth/login`），handler 读 JWT 上下文中的 `user_id`。**注意**：当前**不强制 `has_access` gating**——任何已登录用户都可以对任意 app/plan 发起 quote，前端必须自己根据 `has_access` 决定是否展示下单按钮。

**路径参数**：`id` 是 App ID。

**请求体**：
```json
{ "plan_id": "monthly" }
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `plan_id` | 是 | 计划 ID。必须是该 App 所属的 Plan（`plan.apps` 包含 `app_id`） |

**响应（200）**：
```json
{
  "code": 0,
  "data": {
    "plan_id": "monthly",
    "amount": 29.9,
    "currency": "USD",
    "sub_expires_at": "2026-08-04T00:00:00Z",
    "cycle_config": {
      "trial_days": 7,
      "billing_cycle_days": 30,
      "base": "now + trial + cycle"
    },
    "provider_data": {
      "paypal": {
        "plan_id": "P-MONTHLY-7D",
        "application_context": {
          "brand_name": "云店",
          "shipping_preference": "NO_SHIPPING",
          "user_action": "SUBSCRIBE_NOW"
        }
      },
      "lemonsqueezy": {
        "variant_id": "var-MONTHLY",
        "checkout_data": {
          "custom": {
            "brand": "云店",
            "sub_expires_at": "2026-08-04T00:00:00Z"
          }
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
| `currency` | **v1 硬编码 `"USD"`**，与 `/payments/orders` 的 `currency` 字段无关。多币种尚不支持 |
| `sub_expires_at` | `now + trial_days + billing_cycle_days`（服务器时间）。同时被嵌入 `provider_data.lemonsqueezy.checkout_data.custom.sub_expires_at`（LemonSqueezy 流程下 BFF 可一把 `provider_data.lemonsqueezy` 透传给 LS checkout 创建）；其他渠道由 channel 自己计算 billing cycle，yunhou 不参与 |
| `cycle_config.base` | 恒为 `"now + trial + cycle"`，给审计/排查时一眼看出计算方式 |
| `provider_data` | 每个已配置的渠道一段 payload；BFF 创建 checkout 时按需透传给对应渠道 SDK。LemonSqueezy 的 `custom.sub_expires_at` 已经预先填好，BFF 不必从顶层字段二次组装 |

**Cycle 解析规则**：当同一 `plan_id` 下 PayPal 与 LemonSqueezy 都配置了 plan 记录，**PayPal 的 `trial_days + billing_cycle_days` 胜出**——它既决定顶层 `sub_expires_at`，也决定 LS `provider_data.lemonsqueezy.checkout_data.custom.sub_expires_at`（两个值一致）。LemonSqueezy 配置里的 `trial_days` / `billing_cycle_days` 会被忽略，**只用其 `variant_id` 走 LS 链路**。运营侧必须保证 PayPal 控制台的账单周期跟 `provider_data.paypal.plan_id` 对应的 product 一致，否则报价跟实际结算对不上。

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
      "id": "free",
      "name": "免费",
      "price": 0,
      "interval_days": 0,
      "apps": ["yundian"],
      "is_active": true,
      "is_default": true,
      "created_at": "2026-01-01T00:00:00Z"
    },
    {
      "id": "monthly",
      "name": "按月订阅",
      "price": 29.9,
      "interval_days": 30,
      "apps": ["yundian", "yundash"],
      "is_active": true,
      "is_default": false,
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

#### POST /admin/plans

创建 Plan。

**请求体**：
```json
{
  "id": "quarterly",
  "name": "按季订阅",
  "price": 79.9,
  "interval_days": 90,
  "apps": ["yundian", "yundash"],
  "is_default": false
}
```

| 字段 | 必填 | 校验 |
|------|------|------|
| `id` | 是 | Plan 唯一标识 |
| `name` | 是 | Plan 显示名称 |
| `price` | 否 | 价格，默认 0；必须 `>= 0` |
| `interval_days` | 否 | 订阅周期（天），默认 0（永久）；必须 `>= 0` |
| `apps` | 否 | 可访问的 App 列表，默认空 |
| `is_default` | 否 | 是否为默认 Plan，默认 false |

> `is_active` 在创建时默认设为 `true`。

**响应（201）**：
```json
{
  "code": 0,
  "data": {
    "id": "quarterly",
    "name": "按季订阅",
    "price": 79.9,
    "interval_days": 90,
    "apps": ["yundian", "yundash"],
    "is_active": true,
    "is_default": false,
    "created_at": "2026-06-23T08:30:00Z"
  }
}
```

**响应（400）**—— 字段不合法：
```json
{"code": 400, "message": "price must be >= 0"}
{"code": 400, "message": "interval_days must be >= 0"}
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
  "is_default": false
}
```

| 字段 | 必填 | 校验 |
|------|------|------|
| `name` | 否 | 显示名称 |
| `price` | 否 | 必须 `>= 0` |
| `interval_days` | 否 | 必须 `>= 0` |
| `apps` | 否 | 可访问的 App 列表（数组；传 `[]` 清空） |
| `is_active` | 否 | 启用状态 |
| `is_default` | 否 | 是否为默认 Plan |

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
    "is_default": false,
    "created_at": "2026-06-23T08:30:00Z"
  }
}
```

**响应（400）**—— 字段不合法：
```json
{"code": 400, "message": "price must be >= 0"}
{"code": 400, "message": "interval_days must be >= 0"}
```

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

> 注意：POST 创建时 `config` 字段为 `null`（in-memory 写入未读取 DB 默认值）。通过 GET 读取时会回填为 DB 默认 `{}`。
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

适用场景：消费 app 需要让终端用户用 GitHub 账号登录。`POST /auth/login` 不再支持 `provider=github`（会返回 400），所有 GitHub 登录必须走下方流程。

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

3. 设置环境变量 `OAUTH_STATE_SECRET`（必需，任意 32 字节以上随机串）—— 用于 state token 的 HMAC 签名。多实例部署必须共享同一个值。

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

| HTTP | 触发条件 |
|---|---|
| 400 | `app_id` 或 `redirect_uri` 缺失 |
| 400 | `redirect_uri` 不在 callback_urls 白名单 |
| 404 | `app_id` 不存在 |
| 404 | app 未配置 GitHub OAuth |
| 500 | 其他内部错误 |

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

| HTTP | 触发条件 |
|---|---|
| 400 | `code` / `state` / `app_id` 缺失 |
| 400 | state 无效或过期（5 分钟） |
| 400 | app_id 对应的 app 没有配置 GitHub OAuth |
| 400 | GitHub `?error=access_denied` 等授权失败参数 |
| 404 | app_id 不存在 |
| 502 | GitHub 上游调用失败（网络、过期、配额） |

#### 边界总结

| 信息 | yunhou 持有？ | BFF 能拿到？ |
|---|---|---|
| GitHub `client_id` | ✓ | ✓（明文） |
| GitHub `client_secret` | ✓ | ✗ |
| `callback_urls` 白名单 | ✓ | ✗ |
| GitHub `access_token`（回调后） | ✓（用即丢） | ✗ |
| yunhou `access_token` | yunhou 签发 | ✓（fragment 里） |

---

### 支付接口

支付接口需要 JWT Bearer Token。所有订单、支付、退款只能由本人访问；所有权由服务端强制校验。

#### POST /payments/orders

创建订单（用户选择 plan 后发起支付）。

**请求体**：
```json
{"plan_id": "monthly"}
```

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

订单默认 30 分钟后过期，过期后由 sweeper 翻转为 `expired`。如果此后 webhook 到达，仍会"honor the payment"激活订阅（参见 webhook 文档 §8）。

**错误响应**：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 400 | `plan not found` | `plan_id` 不存在 |
| 400 | `plan is inactive` | Plan 已停用 |
| 409 | `user already has an active subscription` | 用户已有活跃订阅 |

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
| `channel` | 是 | `stripe` / `wechat_pay` / `alipay` / `lemonsqueezy` / `paypal` |
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
| 400 | `invalid channel` | `channel` 取值不在 `stripe` / `wechat_pay` / `alipay` / `lemonsqueezy` / `paypal` 之内 |
| 400 | `invalid request body` | 请求体缺失或字段类型错误 |
| 404 | `not found` | 订单不存在或不属于当前用户 |
| 409 | `order is in a non-recoverable terminal state` | 订单已是 `failed` / `refunded` 终态 |
| 409 | `order already has a paid payment on a different channel` | 该订单已有其他渠道的 paid 记录 |

#### GET /payments

列出当前用户的所有支付记录（无分页，按 `created_at` **降序**）。

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

查询支付详情。只能查自己的支付（通过 order → user_id 关联校验）。

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
    "failed_reason": null,
    "disputed": false,
    "disputed_at": null,
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

退款 `status` 流转：`pending → paid`（渠道 webhook 确认）。**v1 不会产生 `failed` 状态**：渠道侧拒绝会直接以 `502 channel refund API call failed` 返回，发生在 INSERT 之前，不会留下 `failed` 记录。完整退款会同步把 `payment.status` 翻成 `refunded`，并取消该订单 plan 上的活跃订阅（不影响其他 plan 的订阅）；部分退款不影响订阅。

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

POST `/webhooks/payment/:channel`，由渠道方调用，**不需要 JWT**，走签名校验。响应永远在事务提交后返回；签名失败 → 400，临时错误 → 500（渠道自动重试）。

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

`domain_action` 取值（事件被处理时填）：`payment_paid` / `payment_failed` / `refund_paid` / `payment_disputed` / `payment_dispute_closed` / `none`。**判别 dedupe 请用 `duplicate: true`，不要用 `domain_action == "none"`**（`"none"` 仅表示"事件类型不在我们关心的范围内"，不代表已处理；dedupe 命中时 `domain_action` 仍会照常填写——只要看到 `duplicate: true` 就是已处理过的事件）。

订阅过期时间通过 channel metadata 传入（RFC3339）：Stripe `data.object.metadata.sub_expires_at`、WeChat 解密后的 `resource.sub_expires_at`、Alipay form 字段 `sub_expires_at`、LemonSqueezy `meta.custom_data.sub_expires_at`（在 LS checkout 创建时由前端嵌入；`subscription_payment_*` 事件缺省时不携带此字段）、PayPal `resource.billing_info.next_billing_time`（renewal `PAYMENT.SALE.COMPLETED` 事件携带；其他事件若无则忽略）。**前端必须从 `plan.interval_days` + 业务规则计算后写入**；yunhou-users 不做服务端推导。

### Quote 路径 vs Confirm 路径：sub_expires_at 来源冲突

订单可能通过两条独立路径被标记为已支付：

- **Quote 路径**：`/quote` 返回 `sub_expires_at`，BFF 把它嵌入 LS `meta.custom_data.sub_expires_at`（Stripe/WeChat/Alipay 等价字段）；channel webhook 到达后 yunhou 直接写入 `subscriptions.expires_at`
- **Confirm 路径**：`POST /payments/orders/:order_id/confirm` 由 BFF 在前端检测到 channel 支付成功时主动调用，`expires_at` 由 BFF 自己算后透传

两条路径在 `subscriptions.expires_at` 这一列是**最后写入胜出**（`activateSubscriptionOnTx` 是一个盲 UPSERT，没有"哪个值更权威"的判断逻辑；见 `internal/service/payment.go`）。如果两条路径在同一订单上前后到达且 `expires_at` 计算口径不一致，**先到的被覆盖，结果不可预测**。

**推荐契约**（避免 last-write-wins 模糊性）：

1. **有 channel webhook 携带 `sub_expires_at` 的渠道**（Stripe / WeChat / Alipay / LemonSqueezy）：webhook 是权威源。`/confirm` 调用时**不要传 `expires_at`**——保持 `nil`，让 webhook 的值留下
2. **没有 channel webhook 的渠道**（理论上不存在；当前四个渠道都支持）：BFF 必须在 `/confirm` 里提供 `expires_at`
3. **`/quote` 的输出仅作为 webhook metadata 的来源**，不作为最终值——BFF 在 webhook 到达前可以展示它给用户，但订阅激活一律以 webhook payload 为准

如果你们的 LS / Stripe / WeChat / Alipay 流程会**并发触发** webhook 与 `/confirm`（前端轮询订单状态 + channel 同时回调的常见 race），请务必遵守契约 (1)，否则会出现"前端显示 7 天试用，但实际订阅 30 天"之类的对账漂移。

---

## 内部服务鉴权

`/apps/*`（除 `/apps/:id/plans` 与 `/apps/:id/quote` 外）和所有 `/admin/*` 路径走 `InternalAppAuth` 中间件，要求 BFF 调用方带两个头：

| Header | 说明 |
|---|---|
| `X-App-ID` | 调用的目标 app 的 `app_id`（必须存在、`is_active = true`） |
| `X-App-Secret` | 该 app 的 64 位十六进制共享密钥。**仅 `POST /admin/apps` 创建响应或 `POST /admin/apps/:id/rotate-secret` 响应里的 `data.secret` 一次性返回**——服务端只存 bcrypt 哈希，无法再读出 |

错误响应：

| HTTP | message | 触发条件 |
|------|---------|----------|
| 401 | `missing X-App-ID header` | 调用方没带 `X-App-ID` |
| 401 | `invalid app_id` | `X-App-ID` 不存在 |
| 401 | `missing X-App-Secret header` | 调用方没带 `X-App-Secret` |
| 401 | `invalid app_secret` | `X-App-Secret` 不匹配（**不会**区分"app 不存在" vs "secret 错"——避免枚举攻击） |
| 401 | `app secret not initialized` | 该 app 行 `secret_hash` 为空（migration 007 之后未跑 `BackfillAppSecrets` 的过渡态）。建议：跑一次 server 启动 backfill，或者直接调 `POST /admin/apps/:id/rotate-secret` 重新生成 |
| 403 | `app is disabled` | app 已停用 |

**Rotation 流程**：怀疑 `X-App-Secret` 泄漏（例如 BFF 容器镜像被 pull 过、CI 缓存里出现过）时，立即调：

```bash
curl -X POST https://yunhou.ai/api/admin/apps/yundian/rotate-secret \
  -H "X-App-ID: yundian" \
  -H "X-App-Secret: <当前 secret>"
```

响应里 `data.secret` 是新的 64 位 hex，旧 secret 立即失效（**无 grace period**）。把新值部署到 BFF 后，下一次调用即生效。

**部署侧建议**：除了 `X-App-Secret` 服务端校验，部署侧也建议对 `POST /admin/*` 与 `GET /apps/:id/provider-token/:channel` 做 nginx IP 白名单 / VPC 限制，把 BFF 出口段固定下来。两层防御互不替代——服务端 secret 防的是凭据泄漏，IP 白名单防的是 endpoint 暴露面。

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
| `scope` | Plan 中定义的 Apps 列表（`[]string`） |
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
| `id` | string | Plan ID（如 `free`, `monthly`） |
| `name` | string | 显示名称 |
| `price` | decimal | 价格 |
| `interval_days` | int | 订阅周期（天），0 表示永久 |
| `apps` | string[] | 该 Plan 可访问的 App 列表 |
| `is_active` | bool | 是否启用 |
| `is_default` | bool | 是否为默认 Plan |
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
| `provider` | string | 提供方：`github` / `google` |
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
| 公共接口（`/healthz`, `/.well-known/jwks.json`, `/auth/login`, `/auth/refresh`, `/auth/logout`, `/apps/:id/plans`） | 10 次/秒，突发 20 | 按客户端 IP 限制；`/healthz` 不在 limiter 路径内；`/apps/:id/plans` 公共可访问（无需鉴权） |
| 内部服务接口（`/apps`, `/apps/:id`, `/apps/:id/provider-token/:channel`, `/admin/*`） | 30 次/秒，突发 60 | 按客户端 IP 限制；要求 `X-App-ID` 头 + `X-App-Secret` 头 |
| 用户态接口（`POST /apps/:id/quote`, `/payments/*`, `/refunds/*`） | 30 次/秒，突发 60 | 按客户端 IP 限制；要求 JWT（终端用户身份） |
| 用户接口（`/user/*`） | 无显式限制 | 仅要求 JWT |
| 渠道 Webhook（`/webhooks/payment/*`） | 200 次/秒，突发 400 | 走签名校验，不限 IP 业务速率 |

---

## 快速接入清单

- [ ] 获取应用的 `app_id` + `app_secret`（`POST /admin/apps` 响应里 `data.secret`，仅一次性返回，需立即落地）
- [ ] BFF 调 `/apps/:id/plans`、`/apps/:id/provider-token/:channel`、所有 `/admin/*` 时带 `X-App-ID` + `X-App-Secret`
- [ ] 实现 OAuth 登录，获取用户的 provider token
- [ ] 调用 `POST /auth/login` 登录
- [ ] 解析响应中的 `has_access` 字段，判断用户是否有权限访问
- [ ] 如果 `has_access` 为 `false`，提示用户订阅/升级
- [ ] 使用 `access_token` 调用用户接口
- [ ] 实现 Token 刷新逻辑，处理 Refresh Token 轮转
- [ ] 获取 JWKS 配置本地 JWT 验证（可选）
- [ ] 怀疑 `app_secret` 泄漏时调 `POST /admin/apps/:id/rotate-secret`，旧 secret 立即失效
