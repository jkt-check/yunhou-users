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
--    注:集合含 quarterly(016 已退役,is_active=false)是有意的:
--    has_access 以 is_active 为前置,quarterly 不会实际授权;若 admin
--    日后复活 quarterly,它即为全功能集合一员,无需补数据。
--    注:本迁移是一次性的;后续经 admin API 新建的全功能 plan 需自行
--    在 apps[] 里包含 'yunhou-website'(或在 CreatePlan 后重跑本文件)。
--
-- 2. apps(yunhou-website) 的 wechat callback_urls 追加 bridge 页 URL
--    微信网站应用只允许 redirect 到已注册授权回调域下的 http(s) URL,
--    kaya:// 进不了微信跳转链(且 validateWeChatOAuthConfig 只收
--    https/loopback),所以微信 → kaya 之间由 yunhou-website 托管的
--    bridge 静态页中转。lookupWeChatConfig 是 exact match,URL 必须与
--    部署完全一致(无尾斜杠)。
--    staging / prod 都写:两个环境跑同一份 migration,免 per-env 手动
--    PATCH。这是有意接受的取舍(accepted risk),不是零暴露面:
--    prod 白名单含 staging bridge URL(及反向)意味着 prod 签发的
--    token 可被 302 投递到 staging origin 的页面 JS(fragment 不经
--    网络/服务器日志,staging 为同 org 静态页)。真实暴露面 =
--    「能在 staging.yunhouai.com 上跑 JS 的人」(staging 失陷 /
--    XSS)在社工配合下可拿到 prod token。接受理由:bridge 页是同
--    org 静态页、无第三方脚本、fragment 不进日志;staging 失陷的
--    爆炸半径扩大是已知取舍,优先运营可靠性(免 per-env 手动 PATCH)。
--    mock 模式注意:staging 开 WECHAT_OAUTH_MOCK=1 时,mock redirect
--    不经微信域名闸直接跳白名单 entry —— staging DB 里的 prod entry
--    在 mock 路径下是活的,联调时知悉。
--    幂等:已包含则跳过。两种静默跳过情形:(a) apps 行不存在
--    (fresh DB 未建 app);(b) apps 行存在但 config 未配 wechat
--    callback_urls —— 两种情形 WHERE 都不命中,UPDATE 是 no-op;
--    文件末尾的 DO 块会对情形 (b) RAISE WARNING,给部署日志留信号。
--    配好 wechat OAuth 后重跑本文件即可(或走 admin PATCH)。

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
                SELECT jsonb_agg(u ORDER BY ord) FROM (
                    SELECT 'https://staging.yunhouai.com/auth/kaya-bridge' AS u, 1 AS ord
                    UNION ALL
                    SELECT 'https://yunhouai.com/auth/kaya-bridge', 2
                ) AS new_urls
                WHERE NOT (config #> '{oauth_providers,wechat,callback_urls}') @> to_jsonb(u)
            ),
            '[]'::jsonb
        )
)
WHERE app_id = 'yunhou-website'
  AND config #> '{oauth_providers,wechat,callback_urls}' IS NOT NULL;

-- 可观测性:app 行存在但 wechat 块未配时,上面的 UPDATE 静默跳过,
-- kaya 登录会 404 wechat oauth not configured。给部署日志留一个明确信号。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM apps WHERE app_id = 'yunhou-website')
       AND NOT EXISTS (
           SELECT 1 FROM apps
           WHERE app_id = 'yunhou-website'
             AND config #> '{oauth_providers,wechat,callback_urls}' IS NOT NULL
       ) THEN
        RAISE WARNING '019_kaya_bridge: yunhou-website has no wechat callback_urls configured; bridge URL not added — configure wechat OAuth via admin API, then re-apply THIS FILE manually (psql -f migrations/019_kaya_bridge.sql) or DELETE FROM _migrations WHERE id=''019_kaya_bridge'' and re-run cmd/migrate (the ledger skips already-applied files, so a plain re-run is a no-op)';
    END IF;
END $$;
