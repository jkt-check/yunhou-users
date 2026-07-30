-- 2026-07-30: kaya desktop OAuth (yunhou-terminal round-137,
-- docs/superpowers/specs/2026-07-30-kaya-yunhou-oauth-integration-design.md).
--
-- kaya(桌面端)复用 app_id=yunhou-website 登录,走「系统浏览器 + 微信扫码 +
-- https bridge 页 + OS scheme kaya:// 唤起」。本文件做两件运营配置的固化:
--
-- 1. plans.apps 追加 'yunhou-website'(仅全功能 plan 集合)
--    has_access = plan.IsActive && plan.Apps.contains(appID)
--    (internal/service/auth.go::resolvePlanForTokenIssuanceWithPlan)。
--    kaya 用 app_id=yunhou-website 调 /auth/refresh,plan.apps 缺
--    'yunhou-website' 时用户登录成功但 has_access=false,kaya 拒服务。
--    限定 'yundash' = ANY(apps) 的集合(monthly/quarterly/yearly/trial),
--    不动 free 计划(apps={yundian})。幂等:已包含则跳过。
--    注:prod 若已通过 plan admin API 加过,本语句是 no-op。
--
-- 2. apps(yunhou-website) 的 wechat callback_urls 追加 bridge 页 URL
--    微信网站应用只允许 redirect 到已注册授权回调域下的 http(s) URL,
--    kaya:// 进不了微信跳转链(且 validateWeChatOAuthConfig 只收
--    https/loopback),所以微信 → kaya 之间由 yunhou-website 托管的
--    bridge 静态页中转。lookupWeChatConfig 是 exact match,URL 必须与
--    部署完全一致(无尾斜杠)。
--    staging / prod 都写:两个环境跑同一份 migration,白名单只是
--    allow-list,bridge 页会把 token 导向同一个 kaya app,交叉收录无
--    额外暴露面。
--    幂等:已包含则跳过;apps 行不存在(fresh DB 未建 app)时 WHERE
--    不命中,no-op —— 建新 app 行后重跑本文件即可(或走 admin PATCH)。

-- 1. plans.apps 追加 'yunhou-website'(全功能集合)
UPDATE plans
SET apps = array_append(apps, 'yunhou-website')
WHERE 'yundash' = ANY(apps)
  AND NOT ('yunhou-website' = ANY(apps));

-- 2. wechat callback_urls 追加 bridge 页 URL(staging + prod 两条)
UPDATE apps
SET config = jsonb_set(
    config,
    '{oauth_providers,wechat,callback_urls}',
    (config #> '{oauth_providers,wechat,callback_urls}')
        || COALESCE(
            (
                SELECT jsonb_agg(u) FROM (
                    SELECT 'https://staging.yunhouai.com/auth/kaya-bridge' AS u
                    UNION ALL
                    SELECT 'https://yunhouai.com/auth/kaya-bridge'
                ) AS new_urls
                WHERE NOT (config #> '{oauth_providers,wechat,callback_urls}') @> to_jsonb(u)
            ),
            '[]'::jsonb
        )
)
WHERE app_id = 'yunhou-website'
  AND config #> '{oauth_providers,wechat,callback_urls}' IS NOT NULL;
