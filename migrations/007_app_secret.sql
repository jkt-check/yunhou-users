-- Migration: 007_app_secret
-- Description: 给 apps 表加 secret_hash 列；引入 X-App-Secret 内部服务鉴权。
-- 设计文档: docs/api-integration-guide.md §"App 接口" + §"频率限制"
--
-- 背景: v1 阶段 apps.secret 字段在 002_simplify_plans 里被删除，依赖网络层
-- 隔离保护 /apps/:id/provider-token/:channel。v2 公网部署没有 VPC / IP 白名单
-- （deploy/nginx.conf 与 docs/deployment.md 都未配置），必须重新引入服务端 secret。
--
-- 注意: 本 migration 只加列并允许 NULL；backfill 由 cmd/server/main.go 启动时调用
-- internal/service.BackfillAppSecrets 完成，原因是 bcrypt 哈希必须在 Go 端用
-- util.HashSecret 生成（PG crypt() 输出格式与 Go bcrypt 不兼容）。
--
-- 中间件 InternalAppAuth 对空 secret_hash 返回 401 "app secret not initialized"，
-- 因此未 backfill 的行不会被错误地放过；backfill 是幂等的（已有 hash 的行跳过）。
--
-- 顺序: 必须先 001 → 002 → 003 → 004 → 本 migration；backfill 由启动脚本触发。

ALTER TABLE apps ADD COLUMN secret_hash TEXT;