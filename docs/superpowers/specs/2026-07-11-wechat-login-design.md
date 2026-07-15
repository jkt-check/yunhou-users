# WeChat Login — Design Spec

**Date:** 2026-07-11
**Status:** Draft
**Branch:** `feat/wechat-login`
**Author:** Claude (Yunhou Users)

## 1. Goal

Add WeChat as a second OAuth provider alongside GitHub in yunhou-users. Same shared-user-identity model, same JWT issuance flow, same JWT shape, same BFF-facing redirect-with-fragment contract — only the upstream authorize URL, code exchange endpoint, and identity-key semantics differ.

The flow is the WeChat Open Platform **网站应用 (website app) OAuth2.0 QR-code login**. Mobile-app SDK login and 公众号 网页授权 are explicitly out of scope for v1.

## 2. Scope (decisions locked)

| # | Decision | Rationale |
|---|---|---|
| 1 | **Flow: 开放平台 网站应用 扫码登录** | Desktop-friendly QR scan + Win/Mac WeChat-client fast-login. Mirrors GitHub most closely. Authorize URL: `https://open.weixin.qq.com/connect/qrconnect`. |
| 2 | **Architecture: mirror GitHub verbatim** | Concrete `*service.WeChatOAuthService` paralleling `*service.GitHubOAuthService`. New `internal/handler/auth_wechat.go` paralleling `auth_github.go`. Routes `/auth/wechat/redirect` and `/auth/wechat/callback`. No shared interface, no path-parameter dispatcher. |
| 3 | **Identity key: unionid** | `provider_uid = "wechat_" + unionid`. Stable across all Yunhou consumer apps registered under the SAME 微信开放平台 account. Returning user with WeChat on a different Yunhou app = same identity row. |
| 4 | **Reject login if `unionid` is missing** | If the WeChat response has no `unionid` (because the app didn't request `snsapi_userinfo` or the user didn't grant it), redirect to BFF with `#error=auth_failed&reason=wechat_no_unionid`. We do NOT fall back to `openid` — the shared-identity model requires unionid. |
| 5 | **Per-app WeChat credentials in `apps.config.oauth_providers.wechat`** | Each Yunhou consumer app that wants WeChat login registers its own 网站应用 under the same 微信开放平台 account. Matches the existing per-app GitHub credentials pattern. |
| 6 | **No email-merge fallback for WeChat identities** | WeChat's `/sns/userinfo` does NOT expose email. The `social_identities.email` column for WeChat rows stays `NULL`. The existing email-merge branch in `resolveOrCreateUser` (auth.go:274) is unaffected — WeChat identities won't trigger it. |
| 7 | **No provider-side refresh_token storage** | Yunhou only needs the WeChat `access_token` once (to call `/sns/userinfo`). The WeChat `refresh_token` from the code-exchange response is dropped. Yunhou's own refresh tokens are what matters for session continuity. |
| 8 | **Scope requested: `snsapi_login,snsapi_userinfo`** | `snsapi_login` is required for the QR flow. `snsapi_userinfo` is additionally requested so the response includes `unionid` and the user profile (nickname, headimgurl). The latter is what makes the identity key decision above (§3) possible. |
| 9 | **No DB migration** | `social_identities.provider` CHECK constraint already permits `'wechat'` (`migrations/001_init.sql:17`). Verified. |
| 10 | **Out of scope for v1**: mobile-app WeChat SDK login, 公众号 网页授权 (`/auth/wechat-mp/*`), provider-side `refresh_token` storage, email-merge fallback, `/test/login` equivalent. |

## 3. WeChat OAuth endpoints (verified against official docs)

| Step | Method | URL | Notes |
|---|---|---|---|
| 1. Authorize | GET (browser redirect) | `https://open.weixin.qq.com/connect/qrconnect?appid=...&redirect_uri=...&response_type=code&scope=snsapi_login,snsapi_userinfo&state=...#wechat_redirect` | `redirect_uri` must be URL-encoded. `#wechat_redirect` fragment is REQUIRED — without it WeChat returns "该链接无法访问". |
| 2. Code exchange | GET | `https://api.weixin.qq.com/sns/oauth2/access_token?appid=...&secret=...&code=...&grant_type=authorization_code` | Returns `{access_token, expires_in, refresh_token, openid, scope, unionid?}`. `unionid` only when scope includes `snsapi_userinfo` AND user granted it. |
| 3. User profile | GET | `https://api.weixin.qq.com/sns/userinfo?access_token=...&openid=...&lang=zh_CN` | Requires scope `snsapi_userinfo`. Returns `{openid, nickname, headimgurl, sex, province, city, country, unionid, privilege[]}`. |

**Errors** are returned with HTTP 200 and JSON body `{"errcode": <int>, "errmsg": <string>}`. Common codes:

| errcode | meaning |
|---|---|
| 40001 | `AppSecret` 错误 or `access_token` 无效 |
| 40013 | 不合法的 AppID |
| 40029 | 不合法的 code (already used / expired — code TTL is 5 min, single use) |
| 40125 | 不合法的 AppSecret |
| 42001 | access_token 超时 (用户 access_token, TTL 7200s) |
| 43004 | 需要接收用户信息授权 (scope 缺失) |

All upstream errors → `service.ErrWeChatUpstream`. Malformed JSON / non-200 → `service.ErrWeChatUpstream` (no separate sentinel — same handling as GitHub).

Sources: `developers.weixin.qq.com/doc/oplatform/developers/dev/auth/web.html`, `developers.weixin.qq.com/doc/oplatform/Website_App/WeChat_Login/Wechat_Login.html`.

## 4. Identity semantics

**Provider string:** `"wechat"` (matches `social_identities.provider` CHECK).

**ProviderUID format:** `"wechat_" + unionid`. Example: `wechat_o6_bmasdasdsad6_2sgVt7hMZOPfL`.

**Lookup chain** (`AuthService.LoginWithProfile` with `Profile.Provider = "wechat"`):

1. `identityRepo.FindByProviderUID("wechat", "wechat_<unionid>")` → hit → bind to existing user, refresh identity row, return existing `UserInfo`.
2. Else: `resolveOrCreateUser` — `identityRepo.FindByEmail(profile.Email)` is consulted (existing flow, auth.go:274). For WeChat identities, `profile.Email == ""` always, so this branch returns no hits and we fall through to "create new user + new identity".
3. Insert identity row with `provider="wechat"`, `provider_uid="wechat_<unionid>"`, `email=NULL`. `nickname` / `avatar_url` denormalized into `users.nickname` / `users.avatar_url` via the existing flow (the GitHub path already does this).

**Cross-app identity unification:** Only works when all Yunhou consumer apps register their WeChat 网站应用 under the SAME 微信开放平台 account. This is a Tencent-side requirement, not a code-side one. Onboarding docs note this for operators.

**Cross-provider identity unification:** Not solved by this change. A user who signs up with GitHub then later uses WeChat (and vice versa) gets two Yunhou accounts. Out of scope for v1 — same as today's behavior with two GitHub OAuth Apps across providers.

## 5. Architecture (file map)

| # | File | Change |
|---|---|---|
| 1 | `internal/model/app.go` | Add `WeChat *WeChatOAuthConfig` to `OAuthProvidersConfig`. Define `WeChatOAuthConfig { AppID, AppSecret string; CallbackURLs []string }`. |
| 2 | `internal/service/wechat_oauth.go` | NEW. `WeChatOAuthService` with `BuildAuthorizeURL`, `VerifyCallbackState`, `ExchangeCode`, `FetchWeChatProfile`. Mirrors `github_oauth.go` shape. Package-level var injection for tests (SetAccessTokenURL, SetUserInfoURL, etc.). |
| 3 | `internal/service/interfaces.go` | Unchanged. `AuthServiceInterface` is already keyed by provider string. |
| 4 | `internal/service/oauth_sentinel_errors.go` | NEW (or add to existing file). Sentinels: `ErrWeChatNotConfigured`, `ErrWeChatCallbackURLMismatch`, `ErrWeChatUpstream`, `ErrWeChatNoUnionID`. |
| 5 | `internal/handler/auth_wechat.go` | NEW. `RegisterWeChatOAuthRoutes`, `Redirect`, `Callback`, `lookupWeChatConfig`. Mirrors `auth_github.go`. |
| 6 | `internal/handler/auth_common.go` | NEW. Lift `attachYunhouJWTToURL` from `auth_github.go:316` into a shared handler-package helper `buildCallbackRedirectURL`. Both `auth_github.go` and `auth_wechat.go` call it. The old name stays as a thin wrapper for one release to keep the existing GitHub test imports clean — actually no, simpler: rename in place and update `auth_github_test.go` to use the new name in the same commit. |
| 7 | `internal/handler/auth.go` | Extend `authErrReason` map (line 52) with `wechat_no_unionid`, `wechat_upstream` reasons. |
| 8 | `internal/handler/app.go` | Add `validateWeChatOAuthConfig` and branch in `validateAppConfig` (line 288). |
| 9 | `internal/router/router.go` | Add `wechatOAuthSvc *service.WeChatOAuthService` parameter to `Setup`. Register `/auth/wechat/*` on the same `publicLimiter` as `/auth/github/*` (line 52). |
| 10 | `internal/router/router_test.go` | Pass `nil` for new `wechatOAuthSvc` (test-only path — `Setup` only stores pointers). |
| 11 | `cmd/server/main.go` | Construct `wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)`. Pass into `router.Setup`. |
| 12 | `internal/service/wechat_oauth_test.go` | NEW. ~600 lines, mirrors `github_oauth_test.go`. |
| 13 | `internal/handler/auth_wechat_test.go` | NEW. ~700 lines, mirrors `auth_github_test.go`. |
| 14 | `internal/handler/app_test.go` | Extend `validateAppConfig` tests for the wechat branch. |
| 15 | `internal/handler/auth_common_test.go` | NEW. Unit tests for `buildCallbackRedirectURL` (was `TestAttachYunhouJWTToURL_*` in auth_github_test.go). |
| 16 | `internal/handler/auth_github_test.go` | Update import of `buildCallbackRedirectURL` to point to new package location. Behavior unchanged. |
| 17 | `migrations/` | Unchanged. No DB migration. |
| 18 | `tests/e2e/` | Unchanged. WeChat E2E requires a real Open Platform account; out of scope for v1. |
| 19 | `docs/plans/`, `CLAUDE.md` | Update CLAUDE.md to add WeChat to the GitHub OAuth Boundary table (key ownership unchanged — Yunhou still holds AppSecret). |

## 6. `WeChatOAuthService` design

Struct shape (mirrors `github_oauth.go:60`):

```go
type WeChatOAuthService struct {
    stateSecret      []byte
    authorizeURL     string  // default: https://open.weixin.qq.com/connect/qrconnect
    accessTokenURL   string  // default: https://api.weixin.qq.com/sns/oauth2/access_token
    userInfoURL      string  // default: https://api.weixin.qq.com/sns/userinfo
    httpClient       *http.Client
}
```

Package-level vars (test-injection points):

```go
var (
    wechatAuthorizeURL   = "https://open.weixin.qq.com/connect/qrconnect"
    wechatAccessTokenURL = "https://api.weixin.qq.com/sns/oauth2/access_token"
    wechatUserInfoURL    = "https://api.weixin.qq.com/sns/userinfo"
    wechatOAuthHTTPClient = &http.Client{Timeout: 10 * time.Second}
)
```

### Method contracts

**`BuildAuthorizeURL(appID string, cfg *model.WeChatOAuthConfig, callbackIndex int, now time.Time) string`**

1. Validate non-empty `stateSecret` (panics if not — same as GitHub path).
2. Call `util.IssueOAuthState(s.stateSecret, appID, callbackIndex, now)` — already provider-agnostic.
3. Build query:
   ```
   appid=<cfg.AppID>
   redirect_uri=<url.QueryEscape(cfg.CallbackURLs[callbackIndex])>
   response_type=code
   scope=snsapi_login%2Csnsapi_userinfo   // URL-encoded comma
   state=<signed>
   ```
4. Append `#wechat_redirect` fragment — REQUIRED per docs, otherwise WeChat returns "该链接无法访问".
5. Return full URL.

**`VerifyCallbackState(state, expectedAppID string, now time.Time) (int, error)`** — thin wrapper over `util.VerifyOAuthState`, returns `callbackIndex`. Same as GitHub.

**`ExchangeCode(ctx context.Context, cfg *model.WeChatOAuthConfig, code, redirectURI string) (*wechatAccessToken, error)`**

GET to `wechatAccessTokenURL` with query params `appid`, `secret`, `code`, `grant_type=authorization_code`. Decode JSON:
- Success: returns struct with `AccessToken, OpenID, ExpiresIn, Scope, RefreshToken`. `RefreshToken` and `Scope` are parsed but not used downstream (decision #7: we don't store the WeChat refresh_token).
- Error response (`{"errcode": X, "errmsg": Y}`): returns `ErrWeChatUpstream`.
- Non-200 or malformed JSON: returns `ErrWeChatUpstream`.
- HTTP transport error: returns wrapped error (not `ErrWeChatUpstream` — caller distinguishes).

Note: WeChat's code-exchange endpoint is GET, not POST. The GitHub version uses POST. Doc explicitly says GET.

`unionid` is NOT in the `wechatAccessToken` struct. The handler reads `unionid` exclusively from `FetchWeChatProfile`'s response (step 3) so there's a single source of truth and a single missing-unionid sentinel path.

**`FetchWeChatProfile(ctx context.Context, cfg *model.WeChatOAuthConfig, accessToken, openID string) (*ProviderUserInfo, error)`**

GET to `wechatUserInfoURL` with query `access_token`, `openid`, `lang=zh_CN`. Decode JSON:
- Success: returns `*ProviderUserInfo{Provider: "wechat", ProviderUID: "wechat_" + unionid, Email: "", Nickname: <nickname>, AvatarURL: <headimgurl>}`. `Email` is hardcoded `""` since WeChat doesn't expose it. If `nickname` or `headimgurl` is absent in the JSON, the corresponding field is `""` (not an error — WeChat returns them only when the user has a profile photo, etc.).
- `unionid` missing in response → return `ErrWeChatNoUnionID`. Don't proceed.
- Error response (`errcode`/`errmsg`): `ErrWeChatUpstream`.

The caller (`handler.Callback`) is responsible for checking `ErrWeChatNoUnionID` and redirecting to the BFF with `#error=auth_failed&reason=wechat_no_unionid`.

## 7. Handler design — `internal/handler/auth_wechat.go`

```go
// (parallel to RegisterGitHubOAuthRoutes in auth_github.go:43)
func RegisterWeChatOAuthRoutes(group *gin.RouterGroup,
    svc *service.WeChatOAuthService,
    appRepo appLoader,
    authSvc service.AuthServiceInterface) {
    deps := &wechatOAuthDeps{svc: svc, appRepo: appRepo, authSvc: authSvc}
    group.GET("/redirect", deps.Redirect)
    group.GET("/callback", deps.Callback)
}

type wechatOAuthDeps struct {
    svc     *service.WeChatOAuthService
    appRepo appLoader
    authSvc service.AuthServiceInterface
}

type appLoader interface {  // SAME interface as auth_github.go:25 — re-uses the existing one
    FindByID(ctx context.Context, id string) (*model.App, error)
}
```

**`Redirect`** — mirrors `auth_github.go:54`:
- Parse `app_id`, `redirect_uri` query params.
- `appLoader.FindByID` → 404 if not found, 403 if inactive.
- `lookupWeChatConfig(app, redirectURI)` → 404 `ErrWeChatNotConfigured`, 400 `ErrWeChatCallbackURLMismatch`.
- `svc.BuildAuthorizeURL(app.AppID, cfg, callbackIdx, wechatOAuthClock())`.
- `c.Redirect(302, authorizeURL)`.

**`Callback`** — mirrors `auth_github.go:121`:
1. If query has `?error=...&error_description=...` → redirect to BFF with `#error=<err>&error_description=<desc>` (lines 138–147 GitHub). Same fallback if state/app lookup fails.
2. Validate `app_id`, `code`, `state` (lines 164–174 GitHub).
3. `svc.VerifyCallbackState(state, appID, wechatOAuthClock())`.
4. Load app, `lookupWeChatConfig`.
5. `svc.ExchangeCode(ctx, cfg, code, redirectURI)`.
6. If `errors.Is(err, ErrWeChatUpstream)` → redirect to BFF with `#error=auth_failed&reason=wechat_upstream`.
7. `svc.FetchWeChatProfile(ctx, cfg, accessToken, openID)`.
8. If `errors.Is(err, ErrWeChatNoUnionID)` → redirect to BFF with `#error=auth_failed&reason=wechat_no_unionid`.
9. If `errors.Is(err, ErrWeChatUpstream)` → same as step 6.
10. `authSvc.LoginWithProfile(ctx, LoginWithProfileRequest{Profile: profile, AppID: appID})`.
11. On expected `AuthService` sentinel errors → redirect with mapped `reason` (handler/auth.go:52–67 — extended with new reasons).
12. Success → `c.Redirect(302, buildCallbackRedirectURL(redirectURI, resp))`.

**`lookupWeChatConfig(app *model.App, redirectURI string) (cfg *model.WeChatOAuthConfig, callbackIdx int, err error)`** — mirrors `lookupGitHubConfig` (auth_github.go:262). Returns:
- `nil, -1, ErrWeChatNotConfigured` if `wechat == nil` or empty / `CallbackURLs` empty.
- `nil, -1, ErrWeChatCallbackURLMismatch` if `redirectURI` doesn't match any whitelisted URL after percent-decoding.
- Otherwise `cfg, idx, nil`.

**`buildCallbackRedirectURL`** — extracted from `auth_github.go:316` (see §5 file map #6). Same signature, same output. Lives in a new `internal/handler/auth_common.go` and is unit-tested separately.

### Clock injection

```go
var wechatOAuthClock = time.Now  // override in tests with t.Cleanup
```

Same pattern as `githubOAuthClock` (auth_github.go:20).

## 8. App config validation

`validateAppConfig` (handler/app.go:288) gets a new branch:

```go
if cfg.OAuthProviders != nil && cfg.OAuthProviders.WeChat != nil {
    if err := validateWeChatOAuthConfig(cfg.OAuthProviders.WeChat); err != nil {
        return err
    }
}
```

`validateWeChatOAuthConfig(cfg *model.WeChatOAuthConfig) error`:
- `AppID` must match `^wx[0-9a-f]{16}$` — WeChat's standard pattern, defensive against typos.
- `AppSecret` length must be exactly 32 chars.
- `CallbackURLs` non-empty; each entry must satisfy `isAcceptableCallbackURL` (extracted to shared helper if not already; today it lives at auth_github.go:899).
- Returns `(validationErr, nil)` shape consistent with `validateGitHubOAuthConfig` (handler/app.go:315).

If `OAuthProviders == nil` or both `GitHub == nil` and `WeChat == nil` → existing behavior (no provider required). Adding a provider is strictly opt-in.

## 9. Error handling matrix

| Scenario | Detection | Response |
|---|---|---|
| Missing `app_id` / `redirect_uri` on `/redirect` | query parse | 400 JSON `{"code":400,"message":"..."}` |
| App not found | `appLoader.FindByID` ErrNotFound | 404 JSON |
| App inactive | `app.IsActive == false` | 403 JSON |
| WeChat not configured for app | `lookupWeChatConfig` ErrWeChatNotConfigured | 404 JSON |
| `redirect_uri` not in whitelist | `lookupWeChatConfig` ErrWeChatCallbackURLMismatch | 400 JSON |
| `?error=...` on `/callback` (WeChat-side denial) | query parse | Redirect to BFF `#error=<wechatErr>&error_description=<desc>` (mirror GitHub 138–147) |
| Missing `code` / `state` on `/callback` | query parse | 400 JSON |
| State HMAC fail or expired | `VerifyCallbackState` | 400 JSON |
| Code exchange upstream error | `ExchangeCode` ErrWeChatUpstream | Redirect `#error=auth_failed&reason=wechat_upstream` |
| Code exchange non-200 / malformed | `ExchangeCode` ErrWeChatUpstream | Same as above |
| Userinfo upstream error | `FetchWeChatProfile` ErrWeChatUpstream | Redirect `#error=auth_failed&reason=wechat_upstream` |
| `unionid` missing from userinfo | `FetchWeChatProfile` ErrWeChatNoUnionID | Redirect `#error=auth_failed&reason=wechat_no_unionid` |
| `AuthService.LoginWithProfile` returns sentinel | `errors.Is(err, ...)` | Redirect with mapped `reason` (handler/auth.go:52–67) |

`authErrReason` map (handler/auth.go:52) gets:
- `"wechat_upstream"`: 502 → no, this is a wechat-side issue, not ours. 502 is wrong. → `"wechat_upstream"` returns BFF a marker; BFF shows a localized error.
- `"wechat_no_unionid"`: same.

Both new reasons documented in the map so the BFF can branch on them.

## 10. Testing strategy

Three test files, mirroring GitHub:

1. **`internal/service/wechat_oauth_test.go`** — ~600 lines, mirrors `github_oauth_test.go`. Use the same package-level var + `t.Cleanup` injection pattern.

   Coverage:
   - `BuildAuthorizeURL_*` — happy path asserts URL contains `appid=<cfg.AppID>`, `redirect_uri=<encoded>`, `response_type=code`, `scope=snsapi_login%2Csnsapi_userinfo`, `state=<signed>`, fragment `#wechat_redirect`. Pin URLs by overriding `wechatAuthorizeURL` package var.
   - `VerifyCallbackState_*` — same as GitHub's. Re-uses `util.IssueOAuthState`.
   - `ExchangeCode_*` — `httptest.Server` stubs `/sns/oauth2/access_token`. Cases:
     - Success returns struct with all fields parsed.
     - Error response `{"errcode":40029,"errmsg":"invalid code"}` → `ErrWeChatUpstream`.
     - Non-200 → `ErrWeChatUpstream`.
     - Malformed JSON → `ErrWeChatUpstream`.
     - Network error → wrapped error (not `ErrWeChatUpstream`).
   - `FetchWeChatProfile_*` — stub `/sns/userinfo`. Cases:
     - Success with unionid → `*ProviderUserInfo{Provider:"wechat", ProviderUID:"wechat_<unionid>", ...}`.
     - Missing unionid → `ErrWeChatNoUnionID`.
     - Error response → `ErrWeChatUpstream`.
     - Missing nickname / headimgurl → use empty string (not error).

2. **`internal/handler/auth_wechat_test.go`** — ~700 lines, mirrors `auth_github_test.go`.

   Mocks (function-field, no gomock):
   - `stubAppLoader` (same shape as auth_github_test.go:20).
   - `stubAuthSvc` (same shape, satisfies `service.AuthServiceInterface`).
   - `wechatAppWithOAuth(appID, callbackURLs ...)` helper builds `*model.App` with `oauth_providers.wechat` populated.
   - `callbackURIFor(t, svc, appID, callbackIdx, cbURL)` uses `BuildAuthorizeURL` + `extractStateFromURL` to forge a valid callback URL.

   Coverage:
   - `TestWeChatOAuth_Redirect_HappyPath` — 302 to `open.weixin.qq.com/connect/qrconnect?...#wechat_redirect`.
   - `_MissingAppID`, `_MissingRedirectURI`, `_AppNotFound`, `_InactiveApp`, `_WeChatNotConfigured`, `_CallbackURLMismatch`, `_MalformedConfig`.
   - `TestWeChatOAuth_Callback_HappyPath` — full success, asserts final URL fragment.
   - `_WeChatErrorParam`, `_WeChatErrorParamNoAppID`.
   - `_ExchangeCodeFailure` (upstream error).
   - `_ProfileFetchError` (upstream error).
   - `_NoUnionID` — userinfo returns without unionid → redirect with `reason=wechat_no_unionid`.
   - `_AuthServiceError` (sentinel errors mapped to reasons).
   - `_MissingCodeOrState`, `_MissingAppID`, `_InvalidState`, `_AppNotFound`, `_WeChatNotConfigured`, `_MalformedAppConfig`.
   - `TestBuildCallbackRedirectURL_*` — moved from auth_github_test.go, covers both providers.

3. **`internal/handler/app_test.go`** — extend `validateAppConfig` for the wechat branch:
   - Happy path with all fields populated.
   - Missing `app_id`, missing `app_secret`, missing/empty `callback_urls`.
   - Invalid `app_id` format (not `wx[0-9a-f]{16}`).
   - Invalid `app_secret` length (not 32 chars).
   - Invalid `callback_urls` entries (non-HTTP, malformed).

No DB migration tests, no e2e changes (requires real WeChat Open Platform account, not available in CI).

## 11. Rollout

**No DB migration.** Verified at `migrations/001_init.sql:14–25`: `CHECK (provider IN ('github', 'google', 'wechat'))` — `'wechat'` already allowed.

**Per-app operator workflow:**
1. Register a 网站应用 under the 微信开放平台 account that owns all Yunhou consumer apps.
2. Configure 授权回调域 to match each Yunhou app's BFF callback domain.
3. Receive AppID + AppSecret from the WeChat admin console.
4. `PATCH /admin/apps/:id` with body:
   ```json
   {
     "config": {
       "oauth_providers": {
         "wechat": {
           "app_id": "wx...",
           "app_secret": "...",
           "callback_urls": ["https://bff.example.com/auth/wechat-callback"]
         }
       }
     }
   }
   ```
5. BFF shows "微信登录" button. User scans QR → callback → JWT issued.

**Apps that haven't added a `wechat` block** are unaffected: `/auth/wechat/redirect` returns 404 (`ErrWeChatNotConfigured`) for them, just like `/auth/github/redirect` returns 404 for apps without GitHub config.

**Backwards compatibility:** Existing GitHub-only apps work unchanged. `/auth/github/*` is untouched. `cmd/server/main.go` gets one new service construction; `router.Setup` gets one new parameter (call site update).

**Operational notes** (for the runbook, not code):
- WeChat Open Platform 网站应用 registration requires ICP filing + business domain verification. Out of code scope.
- The same WeChat Open Platform account must own all Yunhou consumer apps that want cross-app unionid unification. Tencent-side requirement.
- Dev / staging: register a "测试网站应用" with placeholder domain. Test against it locally. No WeChat sandbox environment exists.

## 12. Out of scope (deferred)

- Mobile-app WeChat SDK login (uses `SendAuthReq`, not the redirect flow).
- 公众号 网页授权 (`/auth/wechat-mp/*`).
- Provider-side `refresh_token` storage to call provider APIs post-login (not needed; Yunhou has its own refresh tokens).
- Email-merge fallback for WeChat identities (WeChat doesn't expose email).
- Cross-provider account merge (GitHub user later uses WeChat, etc.) — same as today's behavior.
- `/test/login` equivalent for WeChat. The existing dev test-login uses synthetic `l3-e2e-` provider_uids; WeChat would need its own synthetic identity format. Not blocking for v1.
- E2E tests against real WeChat — requires Open Platform app credentials. CI uses service + handler unit tests only.

## 13. Acceptance criteria

A change implementing this spec is complete when:

1. `make build` produces a binary that starts successfully with `OAUTH_STATE_SECRET=<32+ chars>` and registers `/auth/wechat/redirect` + `/auth/wechat/callback` (verified by `make run` + curl).
2. `make test` passes with ≥80% line coverage on `internal/service/wechat_oauth.go` and `internal/handler/auth_wechat.go`.
3. `make lint` passes.
4. A manual end-to-end test against a real WeChat Open Platform 测试网站应用 succeeds: user scans QR → WeChat redirects to BFF → BFF parses `#token=...&refresh_token=...&user_id=...&has_access=true` → Yunhou JWT verifies locally via `/.well-known/jwks.json`.
5. `social_identities` row written for a WeChat login has `provider='wechat'`, `provider_uid='wechat_<unionid>'`, `email IS NULL`.
6. A second WeChat login from the same user on a different Yunhou app (with WeChat registered under the same Open Platform account) hits the existing identity row — no duplicate user created.
7. Existing GitHub login flow continues to work unchanged (smoke test: GitHub OAuth round-trip on the same binary).