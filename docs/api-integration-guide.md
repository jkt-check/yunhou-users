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

1. 你的前端通过 OAuth 获取用户的 GitHub/Google access token
2. 你的后端调用 `/auth/login`，传入 provider token
3. 系统返回 JWT access token + refresh token，以及用户的订阅信息

```bash
curl -X POST https://your-yunhou-domain/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "github",
    "provider_token": "gho_xxxxxxxxxxxx",
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

登录接口。

**请求体**：
```json
{
  "provider": "github",
  "provider_token": "gho_xxxx",
  "app_id": "yundian"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `provider` | 是 | 登录方式：`github` / `google` |
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

App 列表接口，查询可用的应用（**需 `X-App-ID` 内部服务头**，限流）。

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

---

### 管理接口

管理接口需要 `X-App-ID` 头（内部服务调用）。

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
    "app_id": "yundash",
    "name": "云dash",
    "description": "Dashboard 应用",
    "config": null,
    "is_active": true,
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-01-01T00:00:00Z"
  }
}
```

> 注意：POST 创建时 `config` 字段为 `null`（in-memory 写入未读取 DB 默认值）。通过 GET 读取时会回填为 DB 默认 `{}`。

#### PATCH /admin/apps/:id

更新 App。所有字段均为可选；`name` 不可为空。

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
| `channel` | 是 | `stripe` / `wechat_pay` / `alipay` / `lemonsqueezy` |
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
| 400 | `invalid channel` | `channel` 取值不在 `stripe` / `wechat_pay` / `alipay` / `lemonsqueezy` 之内 |
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

`domain_action` 取值（事件被处理时填）：`payment_paid` / `payment_failed` / `refund_paid` / `payment_disputed` / `payment_dispute_closed` / `none`。**dedupe 命中时为空字符串**——判别 dedupe 请用 `duplicate: true`，不要用 `domain_action == "none"`（`"none"` 仅表示"事件类型不在我们关心的范围内"，不代表已处理）。

订阅过期时间通过 channel metadata 传入（RFC3339）：Stripe `data.object.metadata.sub_expires_at`、WeChat 解密后的 `resource.sub_expires_at`、Alipay form 字段 `sub_expires_at`、LemonSqueezy `meta.custom_data.sub_expires_at`（在 LS checkout 创建时由前端嵌入；`subscription_payment_*` 事件缺省时不携带此字段）。**前端必须从 `plan.interval_days` + 业务规则计算后写入**；yunhou-users 不做服务端推导。

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
| 401 | 401 | 未认证（Token 无效或过期、Provider 验证失败、未提供 `X-App-ID`） |
| 403 | 403 | 无权限（如试图自助订阅付费 Plan、App 已停用） |
| 404 | 404 | 资源不存在 |
| 409 | 409 | 资源冲突（已存在活跃订阅、订单非 pending 等） |
| 429 | 429 | 请求过于频繁 |
| 500 | 500 | 服务器内部错误 |
| 502 | 502 | 渠道上游调用失败（如渠道侧退款 API 拒绝） |
| 503 | 503 | 服务暂不可用（如 DB 不可达） |

---

## 频率限制

| 接口类别 | 限制 | 说明 |
|---------|------|------|
| 公共接口（`/healthz`, `/.well-known/jwks.json`, `/auth/login`, `/auth/refresh`, `/auth/logout`） | 10 次/秒，突发 20 | 按客户端 IP 限制；`/healthz` 不在 limiter 路径内 |
| App/Admin 接口（`/apps/*`, `/admin/*`） | 30 次/秒，突发 60 | 按客户端 IP 限制；要求 `X-App-ID` 头 |
| 支付/退款接口（`/payments/*`, `/refunds/*`） | 30 次/秒，突发 60 | 按客户端 IP 限制；要求 JWT |
| 用户接口（`/user/*`） | 无显式限制 | 仅要求 JWT |
| 渠道 Webhook（`/webhooks/payment/*`） | 200 次/秒，突发 400 | 走签名校验，不限 IP 业务速率 |

---

## 快速接入清单

- [ ] 获取应用的 `app_id`
- [ ] 实现 OAuth 登录，获取用户的 provider token
- [ ] 调用 `POST /auth/login` 登录
- [ ] 解析响应中的 `has_access` 字段，判断用户是否有权限访问
- [ ] 如果 `has_access` 为 `false`，提示用户订阅/升级
- [ ] 使用 `access_token` 调用用户接口
- [ ] 实现 Token 刷新逻辑，处理 Refresh Token 轮转
- [ ] 获取 JWKS 配置本地 JWT 验证（可选）
