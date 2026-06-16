# Yunhou Users API 接入文档

本文档面向第三方应用开发者，介绍如何接入 Yunhou Users 共享用户管理系统。

## 目录

- [概述](#概述)
- [接入前准备](#接入前准备)
- [认证流程](#认证流程)
- [Token 管理](#token-管理)
- [JWT 本地验证](#jwt-本地验证)
- [用户信息接口](#用户信息接口)
- [应用管理接口](#应用管理接口)
- [订阅管理接口](#订阅管理接口)
- [错误码](#错误码)
- [频率限制](#频率限制)
- [数据模型](#数据模型)

---

## 概述

Yunhou Users 是一个共享用户管理 API，所有接入的应用共享同一套用户身份——每个用户只需一个账号即可使用所有接入应用。系统支持社交账号 OAuth 登录，目前仅实现了 GitHub，Google 和微信登录为规划中的功能，暂不可用。不支持邮箱密码注册。

核心流程：

1. 应用将用户重定向到 Yunhou Users 的授权页面
2. 用户在社交平台完成登录
3. Yunhou Users 将用户重定向回应用，并附带授权码
4. 应用后端用授权码换取 Access Token 和 Refresh Token
5. 应用使用 Access Token 调用用户信息接口
6. Access Token 过期后使用 Refresh Token 续期

---

## 接入前准备

### 1. 注册应用

调用应用创建接口获取 `app_id` 和 `app_secret`：

```bash
curl -X POST https://your-yunhou-domain/apps \
  -H "Content-Type: application/json" \
  -H "X-App-ID: <admin_app_id>" \
  -H "X-App-Secret: <admin_app_secret>" \
  -d '{
    "name": "我的应用",
    "redirect_uris": ["https://myapp.com/auth/callback"],
    "providers": ["github", "google"],
    "default_plan": "free"
  }'
```

响应（201）：

```json
{
  "code": 0,
  "data": {
    "app_id": "550e8400-e29b-41d4-a716-446655440001",
    "app_secret": "plain-text-secret-only-shown-once",
    "name": "我的应用"
  }
}
```

> **重要**：`app_secret` 仅在创建时以明文返回一次，请妥善保存。后续接口中 `app_secret` 字段不会再次展示。

### 2. 配置参数说明

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | 是 | 应用名称 |
| `redirect_uris` | 是 | OAuth 回调地址列表，必须使用 HTTPS（本地开发可用 http://localhost） |
| `providers` | 否 | 允许的登录方式，默认 `["github", "google", "wechat"]` |
| `default_plan` | 否 | 用户首次登录自动创建的订阅计划，默认 `"free"`，可选 `free` / `trial` / `paid` |

### 3. 保存凭证

接入完成后你将拥有：

- `app_id` — 应用的唯一标识
- `app_secret` — 应用的密钥（仅创建时可见）
- `redirect_uri` — 已注册的回调地址

---

## 认证流程

### 完整时序图

```
用户          你的应用          Yunhou Users        OAuth 提供方
 │              │                  │                    │
 │──点击登录──→│                  │                    │
 │              │──重定向用户────→│                    │
 │              │  GET /authorize │                    │
 │              │                  │──302 重定向──────→│
 │              │                  │                   │
 │←─────────── 社交平台登录页面 ──────────────────────│
 │              │                  │                    │
 │──授权登录──→│（社交平台）       │                    │
 │              │                  │←──回调带 code─────│
 │              │                  │  GET /callback     │
 │              │                  │                    │
 │              │←─302 重定向────│                    │
 │              │  带授权码和state │                    │
 │              │                  │                    │
 │              │──POST /token───→│                    │
 │              │  {code,app_id,  │                    │
 │              │   app_secret}   │                    │
 │              │                  │                    │
 │              │←──access_token──│                    │
 │              │  +refresh_token │                    │
```

### 第一步：发起授权请求

将用户重定向到 Yunhou Users 的授权端点：

```
GET /authorize?app_id={app_id}&provider={provider}&redirect_uri={redirect_uri}&state={state}
```

**参数说明：**

| 参数 | 必填 | 说明 |
|------|------|------|
| `app_id` | 是 | 你的应用 ID |
| `provider` | 是 | 登录方式：当前仅支持 `github`（`google` / `wechat` 暂未实现） |
| `redirect_uri` | 是 | 必须是在应用中已注册的回调地址 |
| `state` | 推荐 | 用于防止 CSRF 攻击的随机字符串，会在回调时原样返回 |

**示例：**

```
https://your-yunhou-domain/authorize?app_id=550e8400-e29b-41d4-a716-446655440001&provider=github&redirect_uri=https://myapp.com/auth/callback&state=random_csrf_token
```

服务器会验证 `redirect_uri` 和 `provider` 是否在应用配置中，然后 307 重定向到对应社交平台的授权页面。

### 第二步：处理回调

用户在社交平台完成登录后，Yunhou Users 会将用户重定向回你注册的 `redirect_uri`：

```
GET https://myapp.com/auth/callback?code={auth_code}&state={original_state}
```

**回调参数：**

| 参数 | 说明 |
|------|------|
| `code` | 一次性授权码，有效期 10 分钟，只能使用一次 |
| `state` | 你在第一步传入的 state 值，需验证其一致性 |

> **安全提示**：务必验证返回的 `state` 与你发送的值一致，否则可能遭受 CSRF 攻击。

### 第三步：换取 Token

你的后端服务使用授权码换取 Token：

```bash
curl -X POST https://your-yunhou-domain/token \
  -H "Content-Type: application/json" \
  -d '{
    "code": "auth_code_from_callback",
    "app_id": "550e8400-e29b-41d4-a716-446655440001",
    "app_secret": "your_app_secret"
  }'
```

响应（200）：

```json
{
  "code": 0,
  "data": {
    "access_token": "eyJhbGciOiJSUzI1NiIs...",
    "refresh_token": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
    "token_type": "Bearer"
  }
}
```

**Token 说明：**

| 字段 | 说明 |
|------|------|
| `access_token` | RS256 签名的 JWT，默认有效期 15 分钟 |
| `refresh_token` | 不透明令牌，默认有效期 7 天 |
| `token_type` | 固定为 `"Bearer"` |

> **注意**：如果用户没有该应用的有效订阅，Token 交换将失败并返回错误。当应用的 `default_plan` 为 `"free"` 时，用户首次登录会自动创建免费订阅。

---

## Token 管理

### 刷新 Access Token

Access Token 过期后，使用 Refresh Token 获取新的 Token 对：

```bash
curl -X POST https://your-yunhou-domain/token/refresh \
  -H "Content-Type: application/json" \
  -d '{
    "refresh_token": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2",
    "app_id": "550e8400-e29b-41d4-a716-446655440001",
    "app_secret": "your_app_secret"
  }'
```

响应（200）：

```json
{
  "code": 0,
  "data": {
    "access_token": "eyJhbGciOiJSUzI1NiIs...",
    "refresh_token": "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3",
    "token_type": "Bearer"
  }
}
```

**Refresh Token 轮转机制：**

每次刷新 Token 时，旧的 Refresh Token 会立即失效，同时发放新的 Refresh Token。这是一种安全措施：

- 防止 Refresh Token 被重复使用
- 如果检测到已失效的 Refresh Token 被使用，说明 Token 可能已泄露
- 必须保存并使用每次返回的新 Refresh Token

**刷新时的订阅检查：**

刷新 Token 时，系统会重新检查用户的有效订阅。如果订阅已过期或被取消，Token 刷新将失败。这意味着即使 Access Token 还未过期，用户的访问权限也可能因订阅状态变化而被撤销。

### Token 有效期

| Token 类型 | 默认有效期 | 环境变量 |
|-----------|-----------|---------|
| Access Token | 15 分钟 | `JWT_ACCESS_TTL` |
| Refresh Token | 7 天 | `JWT_REFRESH_TTL` |
| 授权码 | 10 分钟 | 不可配置 |

---

## JWT 本地验证

应用可以在本地验证 Access Token，无需每次都调用 Yunhou Users 服务端。

### 获取公钥

```
GET /.well-known/jwks.json
```

响应（200）：

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

### JWT Claims 结构

| Claim | 说明 |
|-------|------|
| `sub` | 用户 ID（UUID） |
| `iss` | 固定值 `"yunhou-users"` |
| `aud` | 应用 ID（数组） |
| `app_id` | 颁发 Token 的应用 ID（自定义 Claim） |
| `scope` | 权限范围，默认 `["app:read", "app:write"]` |
| `exp` | 过期时间 |
| `iat` | 签发时间 |

### 验证步骤

1. 获取 JWKS（建议缓存，TTL 建议 1 小时）
2. 使用 `kid=yunhou-users-rsa` 匹配公钥
3. 使用 RS256 算法验证 JWT 签名
4. 检查 `iss` 是否为 `"yunhou-users"`
5. 检查 `aud` 是否包含你的 `app_id`
6. 检查 `exp` 确认 Token 未过期
7. 从 `sub` 获取用户 ID

### 验证示例（Go）

```go
import (
    "context"

    "github.com/MicahParks/keyfunc/v3"
    "github.com/golang-jwt/jwt/v5"
)

jwksURL := "https://your-yunhou-domain/.well-known/jwks.json"
k, err := keyfunc.NewDefaultCtx(context.Background(), []string{jwksURL})
if err != nil {
    // handle error
}

token, err := jwt.ParseWithClaims(accessToken, &TokenClaims{}, k.Keyfunc)
if err != nil {
    // handle error
}
if claims, ok := token.Claims.(*TokenClaims); ok && token.Valid {
    userID := claims.Subject
    appID := claims.AppID
}
```

---

## 用户信息接口

所有用户接口需携带 JWT Access Token：

```
Authorization: Bearer <access_token>
```

### 获取用户资料

```
GET /user/profile
```

响应（200）：

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

### 更新用户资料

```
PATCH /user/profile
```

请求体（部分更新，只传需要修改的字段）：

- `nickname` 长度限制 1-100 字符，前后空白会被自动去除
- `avatar_url` 必须是有效的 HTTPS URL，不允许包含 fragment（`#`）

```json
{
  "nickname": "新昵称",
  "avatar_url": "https://example.com/new-avatar.png"
}
```

响应（200）：

```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440002",
    "nickname": "新昵称",
    "avatar_url": "https://example.com/new-avatar.png",
    "status": "active",
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-06-16T08:00:00Z"
  }
}
```

### 查看已绑定的社交账号

```
GET /user/identities
```

响应（200）：

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
    },
    {
      "id": "550e8400-e29b-41d4-a716-446655440006",
      "user_id": "550e8400-e29b-41d4-a716-446655440002",
      "provider": "google",
      "provider_uid": "67890",
      "email": "user@gmail.com",
      "created_at": "2026-02-01T00:00:00Z"
    }
  ]
}
```

### 解绑社交账号

```
DELETE /user/identities/:id
```

响应（200）：

```json
{
  "code": 0,
  "message": "unbound"
}
```

> **规则**：用户必须至少保留一个社交账号绑定。如果是最后一个，将返回 400 错误。

### 查看已订阅的应用

```
GET /user/apps
```

响应（200）：

```json
{
  "code": 0,
  "data": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440004",
      "user_id": "550e8400-e29b-41d4-a716-446655440002",
      "app_id": "550e8400-e29b-41d4-a716-446655440001",
      "plan": "free",
      "status": "active",
      "expires_at": null,
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z"
    }
  ]
}
```

---

## 应用管理接口

所有应用管理接口需携带应用凭证：

```
X-App-ID: <app_id>
X-App-Secret: <app_secret>
```

> 应用只能访问和管理自己的数据，跨应用访问返回 403。

### 创建应用

```
POST /apps
```

请求体：

- `redirect_uris` 中每个 URI 必须是有效的 URL，不允许包含 fragment；生产环境必须使用 HTTPS（`http://localhost` 和 `http://127.0.0.1` 仅限开发使用）

```json
{
  "name": "我的应用",
  "redirect_uris": ["https://myapp.com/callback"],
  "providers": ["github", "google"],
  "default_plan": "free"
}
```

响应（201）：见[接入前准备](#1-注册应用)

### 查看应用信息

```
GET /apps/:id
```

响应（200）：

```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440001",
    "name": "我的应用",
    "redirect_uris": ["https://myapp.com/callback"],
    "providers": ["github", "google"],
    "default_plan": "free",
    "created_at": "2026-01-01T00:00:00Z",
    "updated_at": "2026-01-01T00:00:00Z"
  }
}
```

> `secret` 字段不会在查询和更新响应中返回。

### 更新应用信息

```
PATCH /apps/:id
```

请求体（部分更新）：

```json
{
  "name": "新名称",
  "redirect_uris": ["https://myapp.com/callback", "https://myapp.com/callback2"]
}
```

响应（200）：返回更新后的应用数据（同查看接口格式）。

---

## 订阅管理接口

所有订阅接口需携带应用凭证（同应用管理接口）。

### 创建订阅

```
POST /subscriptions
```

请求体：

```json
{
  "user_id": "550e8400-e29b-41d4-a716-446655440002",
  "app_id": "550e8400-e29b-41d4-a716-446655440001",
  "plan": "paid",
  "expires_at": "2027-01-01T00:00:00Z"
}
```

| 字段 | 必填 | 说明 |
|------|------|------|
| `user_id` | 是 | 用户 ID（从 JWT `sub` 获取） |
| `app_id` | 是 | 必须与当前认证的应用一致 |
| `plan` | 是 | 订阅计划：`free` / `trial` / `paid` |
| `expires_at` | 否 | RFC3339 格式的过期时间，`free` 计划通常为 null（永不过期） |

响应（201）：

```json
{
  "code": 0,
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440004",
    "user_id": "550e8400-e29b-41d4-a716-446655440002",
    "app_id": "550e8400-e29b-41d4-a716-446655440001",
    "plan": "paid",
    "status": "active",
    "expires_at": "2027-01-01T00:00:00Z",
    "created_at": "2026-06-16T00:00:00Z",
    "updated_at": "2026-06-16T00:00:00Z"
  }
}
```

> 每个用户在同一应用下只能有一个订阅（`user_id + app_id` 唯一约束）。重复创建返回 409。

### 查看订阅

```
GET /subscriptions/:id
```

响应（200）：返回订阅数据。

### 取消订阅

```
DELETE /subscriptions/:id
```

响应（200）：

```json
{
  "code": 0,
  "message": "cancelled"
}
```

> 已取消的订阅不能再次取消（返回 400），也不能续期。需要重新创建订阅。

---

## 错误码

所有接口使用统一的响应格式：

**成功响应：**

```json
{"code": 0, "data": {...}}
```

**错误响应：**

```json
{"code": <http_status>, "message": "<error_description>"}
```

| HTTP 状态码 | code | 说明 |
|------------|------|------|
| 200 | 0 | 请求成功 |
| 201 | 0 | 创建成功 |
| 400 | 400 | 请求参数错误 / 业务规则冲突（如解绑最后一个社交账号、取消已取消的订阅） |
| 401 | 401 | 未认证（缺少或无效的 Token / 应用凭证） |
| 403 | 403 | 无权限（跨应用访问、app_id 不匹配） |
| 404 | 404 | 资源不存在 |
| 409 | 409 | 资源冲突（如重复创建订阅） |
| 429 | 429 | 请求过于频繁，触发频率限制 |
| 500 | 500 | 服务器内部错误 |

---

## 频率限制

| 接口类别 | 限制 | 说明 |
|---------|------|------|
| 公共接口（授权、回调、Token） | 10 次/秒，突发 20 | 按客户端 IP 限制 |
| 应用管理接口 | 30 次/秒，突发 60 | 按客户端 IP 限制 |
| 用户信息接口 | 无显式限制 | 需 JWT 认证 |

超过限制时返回：

```json
{"code": 429, "message": "too many requests"}
```

---

## 数据模型

### 用户（User）

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 用户唯一 ID（UUID） |
| `nickname` | string? | 昵称 |
| `avatar_url` | string? | 头像 URL |
| `status` | string | 状态：`active` / `suspended` / `deleted` |
| `created_at` | datetime | 创建时间 |
| `updated_at` | datetime | 更新时间 |

### 社交身份（SocialIdentity）

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 身份唯一 ID |
| `user_id` | string | 所属用户 ID |
| `provider` | string | 提供方：`github` / `google` / `wechat` |
| `provider_uid` | string | 提供方用户 ID |
| `email` | string? | 关联邮箱 |
| `created_at` | datetime | 绑定时间 |

### 应用（App）

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 应用唯一 ID（UUID） |
| `name` | string | 应用名称 |
| `redirect_uris` | string[] | 已注册的回调地址 |
| `providers` | string[] | 允许的登录方式 |
| `default_plan` | string | 默认订阅计划 |
| `created_at` | datetime | 创建时间 |
| `updated_at` | datetime | 更新时间 |

### 订阅（Subscription）

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 订阅唯一 ID |
| `user_id` | string | 用户 ID |
| `app_id` | string | 应用 ID |
| `plan` | string | 计划：`free` / `trial` / `paid` |
| `status` | string | 状态：`active` / `expired` / `cancelled` |
| `expires_at` | datetime? | 过期时间，null 表示永不过期 |
| `created_at` | datetime | 创建时间 |
| `updated_at` | datetime | 更新时间 |

### 会话（Session）

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 会话唯一 ID |
| `user_id` | string | 用户 ID |
| `app_id` | string | 应用 ID |
| `session_type` | string | 会话类型：`auth_code`（授权码）或 `refresh`（刷新令牌） |
| `scope` | string[] | 权限范围 |
| `revoked` | bool | 是否已撤销 |
| `expires_at` | datetime | 过期时间 |
| `created_at` | datetime | 创建时间 |

> `refresh_token` 仅在 Token 换取/刷新时以明文返回，不会在会话查询中展示。

---

## 快速接入清单 [ ] 调用 `POST /apps` 创建应用，保存 `app_id` 和 `app_secret`
- [ ] 在应用前端添加「使用 GitHub 登录」按钮（Google / 微信暂未实现）
- [ ] 按钮链接指向 `GET /authorize?app_id=...&provider=...&redirect_uri=...&state=...`
- [ ] 实现回调接口，接收 `code` 和 `state`
- [ ] 验证 `state` 一致性
- [ ] 后端调用 `POST /token` 用 `code` 换取 Token
- [ ] 从 JWT `sub` 获取用户 ID，调用 `GET /user/profile` 获取用户信息
- [ ] 获取 JWKS 配置本地 JWT 验证
- [ ] 实现 Token 刷新逻辑，处理 Refresh Token 轮转
- [ ] 处理订阅状态检查，Token 换取/刷新失败时引导用户订阅
