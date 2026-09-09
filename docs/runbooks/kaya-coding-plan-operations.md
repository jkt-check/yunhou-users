# Kaya Coding Plan 运营面 runbook（Task 15）

对应实现：`internal/inference/management/{bulk_import,pricing_preview,operations}.go`、
`internal/inference/httpapi/{admin_bulk,admin_usage}.go`、`admin_adjustments.go`（Task 14 建、
本任务扩展）。API 契约：`docs/api/kaya-coding-plan.openapi.yaml`（0.13.0）。

## 权限矩阵（server 端强制，自报无效）

| 端点 | 权限 | 角色 |
|---|---|---|
| `POST /admin/catalog/bulk-import`、`/admin/model-prices/preview`、`/admin/quota-policies/preview` | `models:manage` | admin / operator |
| `POST /admin/wallet/adjustments`、`/admin/wallet/reversals`、`GET /admin/wallet*`、`/admin/payg-config` | `billing:adjust` | admin |
| `GET /admin/model-usage/*`、`/admin/upstream-accounts/shared`、`/admin/model-adjustments` | `usage:read` | admin / operator / auditor |

每次批量导入 commit、补偿、冲正、PAYG 发布都追加审计（`inference_audit_log`），
人员及服务双重归因（`user:<uid>@app:<appid>`）；补偿与冲正的审计行与效果同事务
提交或回滚。

## 批量导入（POST /admin/catalog/bulk-import）

- **dry_run=true**：逐项预演（`would_insert` / `would_skip` / `error`），一行不写。
  部分错误的预览只在 dry-run 存在。
- **commit（dry_run=false）**：全部有效才落库；任一 item 出错 → 400 +
  `data.items` 逐项错误，**一行不写（绝不半发布）**。要导入有效子集：删掉
  错误项、换新 `task_id` 重提——不存在隐式的"只发布有效部分"。
- **task_id 幂等**：重复提交（哪怕文档不同）返回 200 + `replayed=true` 与
  首次已记录结果；不重复创建。自然键（provider code / model id /
  provider+upstream_model+base_url / model+deployment）已存在的项跳过不覆盖。
- **大小上限**：条目总数 ≤ 200（另有全局 1 MiB body 上限）。
- 落库即草稿（默认不可售）；补齐价格（sale_credit 版本）与授权后
  `POST /admin/catalog/publish` 发布。停用上游（status=disabled）不删除历史
  路由/尝试记录；有路由或历史尝试的部署不可 DELETE。

## 一账号一部署（显式运营约束）

**一个上游账号只挂一个部署。** 两部署共享同一上游账号时，故障切换会撞并发
租约唯一键（`inference_concurrency_leases` 的 (scope, scope_id, request_id)
UNIQUE，表现为 500）。调度语义不在线变更（Task 13 移交决定）；运营面用检测
视图发现违规：

```
GET /admin/upstream-accounts/shared?from=&to=&limit=
```

观测窗口（默认最近 7 天）内同一上游账号服务过 >1 个部署（attempts 落库事实）
即列出；命中应把账号拆到各自部署。

## 统计与成本口径（GET /admin/model-usage/summary）

- 数据源只有 `inference_requests / inference_attempts / inference_usage_records /
  inference_ledger_entries`；绝不使用 `usage_events` 心跳表。
- 客户消费为账本派生（charge − reversed ± adjustment），与客户面逐字同口径；
  聚合可与账本抽样核对（测试内置断言）。
- 采购成本来自 attempts 落库的 `cost_micros/cost_currency/cost_basis`：按
  **币种 × basis（reported/estimated/allocated）分片**，绝不跨币种合并；
  成本未知的尝试单列 `cost_unknown_attempts`。
- 供应商分组：请求级计数按**最终尝试**归属（重试不双计）；供应商维度不归属
  请求级账本金额（恒 0，金额权威维度是模型/客户）。
- 统计是只读派生路径：额度闸门继续用权威事务状态，统计不引入对写路径的
  共享锁。延迟统计不得用作实时放行依据。

## 异常筛选（GET /admin/model-usage/exceptions?kind=）

- `stuck_reservations`：held 超过 1 小时的异常预占（恢复 worker 的处理对象）。
- `reauth_required_accounts`：授权失效的上游账号（重新走 OAuth 授权后恢复调度）。
- `settlement_backlog`：pending/running/escalated 核对任务；`overdue=true` 是
  超期告警——**超期不等于自动释放**，必须人工/恢复流程核对。

## 补偿（POST /admin/wallet/adjustments 等）

补偿必须有原因、权限（billing:adjust）、幂等键；幂等键重放返回
`applied=false` 不重复补偿。追踪读面：`GET /admin/model-adjustments`
（usage:read）与 `GET /admin/wallet/adjustments`（billing:adjust），同一口径。
