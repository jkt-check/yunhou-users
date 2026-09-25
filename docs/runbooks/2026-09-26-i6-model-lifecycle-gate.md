# I-6 非 active 模型直达治理 — 存量排查与切换 Runbook

安全审查 I-6（PR 见 git log）。网关生命周期门两阶段上线：

- **观察模式（默认）**：非 active 模型被点名直连时输出 `LIFECYCLE_GATE_HIT`
  结构化日志并经 `AuditAlertHook` 告警，**不拦截**。
- **强制模式**（`INFERENCE_LIFECYCLE_ENFORCE=1`）：retired/draft 直达 → 403
  `model_not_allowed`；deprecated 放行存量（拒新 grant 属 entitlement 层另案）。

**只有完成下列排查、确认影响面可控后才允许设 `INFERENCE_LIFECYCLE_ENFORCE=1`。**

## 1. 存量排查 SQL

在目标库（staging/prod 的 users 库）执行：

```sql
-- 1) 非 active 模型清单
SELECT id, display_name, lifecycle, updated_at
FROM inference_models
WHERE lifecycle <> 'active'
ORDER BY lifecycle, id;

-- 2) 非 active 模型的历史直达流量（请求事实表 + 调用方 API key）
SELECT r.model_id, m.lifecycle, r.api_key_id, COUNT(*) AS requests,
       MIN(r.created_at) AS first_seen, MAX(r.created_at) AS last_seen
FROM inference_requests r
JOIN inference_models m ON m.id = r.model_id
WHERE m.lifecycle <> 'active'
GROUP BY 1,2,3
ORDER BY requests DESC;

-- 3) 引用非 active 模型的存量权益（这些调用方在强制模式下会被 403）
SELECT m.id, m.lifecycle, e.source_type, e.status, COUNT(*) AS n,
       BOOL_OR(e.effective_to IS NULL OR e.effective_to > now()) AS has_live_window
FROM inference_entitlements e
JOIN inference_models m ON m.id = ANY(e.model_ids)
WHERE m.lifecycle <> 'active'
GROUP BY 1,2,3,4
ORDER BY 1;
```

判定标准（全部满足才可切强制）：

1. 查询 2 近 30 天零命中，或命中方已确认可迁移/停用；
2. 查询 3 无 `has_live_window = true` 的行，或相关权益已显式撤销/过期。

## 2. 观察模式巡检

网关日志按 `LIFECYCLE_GATE_HIT` 命中（字段：`model` `lifecycle`
`blocked` `principal_kind` `billing_account` `api_key_id` `user_id`
`operator`）。示例：

```bash
docker logs <users-container> 2>&1 | grep LIFECYCLE_GATE_HIT
```

告警通道：事件经 `management.AuditAlertHook` 上报（与审计补写失败同级），
生产应已接到 on-call/webhook。

## 3. 切换强制模式

1. 确认第 1 节排查结论已记录（影响面、调用方、处置）。
2. 部署时带 `INFERENCE_LIFECYCLE_ENFORCE=1`（或追加进 env 后重启）。
3. 切换后验证：
   - 对 retired/draft 模型发直达请求 → HTTP 403、`model_not_allowed`；
   - deprecated 模型直达不受影响；
   - `LIFECYCLE_GATE_HIT ... blocked=true` 日志持续产生。
4. 回滚：去掉该环境变量重启即回到观察模式（代码无需变更）。

## 4. 已完成的排查记录

- **cn-staging（2026-09-26）**：非 active 模型 2 个
  （`dashboard-lc-smoke` deprecated、`deepseek-flash` draft）；
  历史直达流量 **0 条**（132 条请求全部命中 active 的 `deepseek-chat`）；
  引用非 active 模型的权益 **0 条**。影响面为零，满足切强制判据。
- cn-prod / intl-prod：上线前由运维在同一库结构执行第 1 节 SQL 并记录结果。
