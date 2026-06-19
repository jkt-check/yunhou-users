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
      "email": "user@example.com"
    },
    "subscription": {
      "plan_id": "monthly",
      "plan_name": "按月订阅",
      "has_access": true,
      "expires_at": "2025-07-19T00:00:00Z"
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
      "expires_at": "2025-07-19T00:00:00Z"
    }
  }
}
```

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
    "avatar_url": "https://avatars.githubusercontent.com/u/12345",
    "status": "active",
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-06-01T00:00:00Z"
  }
}
```

#### PATCH /user/profile

更新用户资料。

**请求体**：
```json
{
  "nickname": "新昵称",
  "avatar_url": "https://example.com/new-avatar.png"
}
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

查看用户的订阅历史。

**响应（200）**：
```json
{
  "code": 0,
  "data": [
    {
      "id": "sub-001",
      "plan_id": "monthly",
      "plan_name": "按月订阅",
      "status": "active",
      "started_at": "2026-01-01T00:00:00Z",
      "expires_at": "2026-07-01T00:00:00Z"
    }
  ]
}
```

#### POST /user/subscriptions

创建订阅（用户主动订阅）。

**请求体**：
```json
{
  "plan_id": "monthly",
  "expires_at": "2025-07-19T00:00:00Z"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `plan_id` | 是 | 订阅的 Plan ID |
| `expires_at` | 否 | 订阅过期时间（RFC3339 格式），用于测试或管理员代操作 |
```

#### DELETE /user/subscriptions/:id

取消订阅。

**响应（200）**：
```json
{"code": 0, "message": "cancelled"}
```

---

### App 接口

App 列表接口，查询可用的应用（公开，限流）。

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
      "is_active": true,
      "created_at": "2026-01-01T00:00:00Z"
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
    "is_active": true,
    "created_at": "2026-01-01T00:00:00Z"
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
      "is_default": true
    },
    {
      "id": "monthly",
      "name": "按月订阅",
      "price": 29.9,
      "interval_days": 30,
      "apps": ["yundian", "yundash"],
      "is_active": true,
      "is_default": false
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

| 字段 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | Plan 唯一标识 |
| `name` | 是 | Plan 显示名称 |
| `price` | 否 | 价格，默认 0 |
| `interval_days` | 否 | 订阅周期（天），默认 0（永久） |
| `apps` | 否 | 可访问的 App 列表，默认空 |
| `is_default` | 否 | 是否为默认 Plan，默认 false |

> `is_active` 在创建时默认设为 `true`。

#### PATCH /admin/plans/:id

更新 Plan。

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

所有字段均为可选，仅更新提供的字段。

#### DELETE /admin/plans/:id

删除 Plan（仅当无用户订阅时）。

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
    "is_active": true,
    "created_at": "2026-01-01T00:00:00Z"
  }
}
```

#### PATCH /admin/apps/:id

更新 App。

**请求体**：
```json
{
  "name": "新名称",
  "description": "新描述",
  "is_active": false
}
```

所有字段均为可选。

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
| `iss` | 固定值 `"yunhou-users"` |
| `app_id` | 登录时请求的 App ID |
| `scope` | Plan 中定义的 Apps 列表 |
| `exp` | 过期时间 |
| `iat` | 签发时间 |

### 验证步骤

1. 获取 JWKS（建议缓存，TTL 建议 1 小时）
2. 使用 `kid=yunhou-users-rsa` 匹配公钥
3. 使用 RS256 算法验证 JWT 签名
4. 检查 `iss` 是否为 `"yunhou-users"`
5. 检查 `exp` 确认 Token 未过期

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
| `config` | jsonb | 扩展配置（可选） |
| `is_active` | bool | 是否启用 |
| `created_at` | datetime | 创建时间 |

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
| 400 | 400 | 请求参数错误 |
| 401 | 401 | 未认证（Token 无效或过期） |
| 403 | 403 | 无权限 |
| 404 | 404 | 资源不存在 |
| 429 | 429 | 请求过于频繁 |
| 500 | 500 | 服务器内部错误 |

---

## 频率限制

| 接口类别 | 限制 | 说明 |
|---------|------|------|
| 公共接口（登录、刷新） | 10 次/秒，突发 20 | 按客户端 IP 限制 |
| 管理接口 | 30 次/秒，突发 60 | 按客户端 IP 限制 |
| 用户接口 | 无显式限制 | 需 JWT 认证 |

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
