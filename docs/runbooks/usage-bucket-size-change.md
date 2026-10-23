# kaya token 用量统计：bucket_size（hour ↔ day）切换 runbook

契约：yunhou-terminal `docs/api/usage-stats.md` v1.2 §6 第 1 条（防双计三条
MUST 的第 3 条：配置变更必须执行一次性清桶/迁移 runbook，切换后旧桶不得
参与任何查询）。本文件是该约束的操作步骤落点；契约只留约束，不复制步骤。

## 为什么必须清桶

`usage_token_buckets` 的唯一键包含 `bucket_size`，唯一键只保证同一行不被
重复写入，**不防查询侧双计**：hour 与 day 两种粒度的桶覆盖同一时间窗时
会重复计数。读查询侧固定 `WHERE bucket_size = 当前部署配置`
（`internal/repo/usage_token_repo.go` 的 `usageTokenBaseWhere`），摄入侧硬
拒绝不匹配（`bucket_size_mismatch`），所以切换后旧粒度桶天然不再参与
任何查询——但它们占存储、且一旦有人把配置切回去就会复活成双计。切换
必须清桶，不是可选优化。

**水位/墓碑不需要清**：`usage_token_watermarks.cleared_before`、
`usage_devices.cleared_before`、`usage_token_tombstones` 都是
`timestamptz` 语义（"某时刻之前/某区间内的桶被删过"），与桶粒度无关，
切换到 day 后依然正确（摄入丢弃判定是 `bucket_start` 时间比较）。清掉
它们反而会让已删除数据在客户端重传时复活——**保留，不要动**。

## 前置检查

```bash
# 当前生效粒度与保留配置
curl -sf -H "Authorization: Bearer <任一测试用户 JWT>" \
  "$API_BASE/user/usage/tokens/coverage" | jq '.data | {bucket_size, retention_days, export_max_rows, server_time}'
```

1. 公告/维护窗：切换期间在网客户端会因 `bucket_size` 与本地缓存配置
   不一致而收到 `bucket_size_mismatch`。客户端的正常行为是先拉
   coverage 自发现——发现不一致会**暂停上传并提示**，coverage 里的新值
   生效后自动恢复。无需逐客户端操作，但要预告"用量上传会短暂暂停"。
2. 确认没有进行中的 backfill 任务（客户端 `max_days` 回填会在切换窗口
   内全部 400，属预期，重传由客户端自行恢复）。
3. 记录切换前的行数与覆盖范围，供切换后对比：

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -Atqc \
  "SELECT bucket_size, COUNT(*), MIN(bucket_start), MAX(bucket_start) FROM usage_token_buckets GROUP BY bucket_size"
```

## 切换步骤

以下以 hour → day 为例；反向操作相同，把目标值换成 `hour`。

```bash
set -euo pipefail
: "${DATABASE_URL:?export DATABASE_URL first}"
```

1. **改配置**：部署环境的 `USAGE_TOKEN_BUCKET_SIZE=day`（`.env` /
   部署 secret）。`Config.Validate` 只接受 `hour|day`，拼错会拒启。
2. **清桶**（在新 binary 上线前或同一维护窗内执行均可，顺序不敏感——
   读查询的 bucket_size 过滤保证任何时候都不会双计）：

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
TRUNCATE usage_token_buckets;
-- 水位/墓碑是 timestamptz 语义，跨粒度仍有效：保留（见上文）。
COMMIT;
SQL
```

   注意：**不要** TRUNCATE `usage_devices` / `usage_token_watermarks` /
   `usage_token_tombstones`。
3. **重启服务**让新配置生效（配置只在启动时读取；sweeper 与 handler
   共用同一份值）。滚动重启期间新旧实例并存是安全的：旧实例按 hour
   摄入/查询、新实例按 day，二者各查各的桶，不双计；客户端最终会收敛
   到新实例的 coverage 值。
4. **验证**：
   - coverage 返回新粒度：`curl ... /user/usage/tokens/coverage | jq .data.bucket_size` → `"day"`。
   - 表内无旧粒度行：步骤前置检查里的 SQL 重跑，应只剩 `day`（或空表）。
   - 用测试设备上传一个 day 粒度桶（`bucket_start` 必须 UTC 00:00），
     再查 summary 确认可读；上传 hour 桶应收到 `bucket_size_mismatch`。

## 回滚

回滚 = 再执行一次本 runbook（目标值换回 `hour`）。被清掉的旧粒度桶不可
恢复（云端不存原始事件）；客户端本地 `~/.kaya/usage` 有 180 天原始事件，
恢复后客户端会按新粒度重新聚合、经 backfill 补传（上限
`USAGE_TOKEN_RETENTION_DAYS` 天）。

## 关联

- 表结构与边界：`migrations/047_kaya_usage_token_stats.sql` 头部注释。
- 防双计三条 MUST 的代码落点：摄入校验 `internal/handler/usage_tokens.go`
  （`bucket_size_mismatch`）、读侧固定过滤 `internal/repo/usage_token_repo.go`
  （`usageTokenBaseWhere`）、本文档（MUST-3）。
