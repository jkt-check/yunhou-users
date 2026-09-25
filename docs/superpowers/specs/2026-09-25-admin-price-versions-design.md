# 售价版本（inference_price_versions）Admin 管理端点 — 需求与设计

日期：2026-09-25
状态：已实现待评审
关联：总设计 `2026-09-08-kaya-coding-plan-design.md` §7.1（价格不可变口径）、§9.2（运营权限矩阵的 `/admin/model-prices` 空缺）；对接契约 `docs/api/kaya-coding-plan-website-handoff.md` §3

## 1. 背景与缺口

`inference_price_versions` 表（migrations/026）是计量计费的售价事实来源：
套餐额度消耗（`kind=sale_credit, unit=microcredit`，无币种）与现金口径
（`sale_money`/`upstream_cost, unit=micromoney` + ISO 币种）都从该表解析。
目前只有 repo 层 `InsertPriceVersion`（internal/inference/postgres/pricing_repo.go），
dashboard 无录入入口，新模型上架/调价只能手工 SQL（staging 现有
`deepseek-chat` 行：入 1,000,000 / 出 2,000,000 micros per Mtok 即为手工插入）。

本需求补上管理面缺口：dashboard 可查询、可录入价格版本。价格版本按设计
§7.1 **不可变、只追加**——不提供 update/delete，纠正错误的方式是发布新
revision；已入场请求钉住入场时的 price_version_id（migrations/030/031），
历史计费永远不受新版本影响。

## 2. 端点签名

两个端点，全部挂在 `/admin` 下、走 inference operator 鉴权链
（`InternalAppAuth`（X-App-ID/X-App-Secret）→ `JWTAuth` → 权限中间件，
见 internal/router/router.go 与 internal/inference/httpapi/admin_auth.go）：

| 方法 | 路径 | 权限 | 用途 |
|---|---|---|---|
| POST | `/admin/price-versions` | `models:manage` | 创建价格版本（只追加） |
| GET  | `/admin/price-versions` | `models:manage` | 列表查询（过滤 + 分页） |

**权限决策**：挂 `models:manage` 而非 `billing:adjust`。理由：价格版本是
目录/定价规则写面的一部分（设计 §9.2 把 `/admin/model-prices` 与目录、
quota-policies 并列），且配套的只读影响预览 `POST /admin/model-prices/preview`
已在 `models:manage` 组；operator 角色（有 models:manage、无 billing:adjust）
正是录入价格的人。`billing:adjust` 保留给钱包调整/冲正/PAYG 配置。

### 2.1 POST /admin/price-versions — 创建

请求体（JSON，`strictBindJSON`：未知字段拒绝、拒绝尾随垃圾）：

| 字段 | 类型 | 必填 | 校验 |
|---|---|---|---|
| `model_id` | string | 是 | 必须存在于 `inference_models`，否则 404 |
| `kind` | string | 是 | `sale_credit` \| `sale_money` \| `upstream_cost` |
| `currency` | string | 条件 | `sale_credit` 时**必须缺席/空**；其余 kind 必填，`^[A-Z]{3}$` |
| `input_micros_per_mtok` | int64 | 否 | ≥0，缺省 0 |
| `cache_read_micros_per_mtok` | int64 | 否 | ≥0，缺省 0 |
| `cache_write_micros_per_mtok` | int64 | 否 | ≥0，缺省 0 |
| `output_micros_per_mtok` | int64 | 否 | ≥0，缺省 0 |
| `extra_rates` | object | 否 | JSON 对象；缺省落 `{"schema_version":1}` |
| `revision` | int | 是 | >0；约定 = 该 (model_id, kind) 当前最大 revision + 1 |
| `effective_from` | RFC3339 | 否 | 缺省 = 服务器当前时刻（与 preview 同语义） |
| `effective_to` | RFC3339 | 否 | 给了必须 > effective_from |
| `reason` | string | 是 | 审计必填，非空 |

**`unit` 不入请求**：由服务端按 kind 派生（`sale_credit`→`microcredit`，
其余→`micromoney`），从协议上消除 unit/currency 失配（表 CHECK
`(unit='microcredit') = (currency IS NULL)` 永远成立）。

响应 201：

```json
{
  "code": 0,
  "data": {
    "price_version_id": "uuid",
    "model_id": "deepseek-chat",
    "kind": "sale_credit",
    "unit": "microcredit",
    "input_micros_per_mtok": "1000000",
    "cache_read_micros_per_mtok": "0",
    "cache_write_micros_per_mtok": "0",
    "output_micros_per_mtok": "2000000",
    "extra_rates": {"schema_version": 1},
    "revision": 1,
    "effective_from": "2026-09-25T08:00:00Z",
    "created_at": "2026-09-25T08:00:01Z"
  }
}
```

micros 字段渲染为 **string**（int64 JSON 精度安全，与 preview 的
`PriceVersionView` 同约定）；`currency` 为 sale_credit 时省略（omitempty）；
`effective_to` 为 null 时省略。

### 2.2 GET /admin/price-versions — 列表

查询参数：

| 参数 | 必填 | 说明 |
|---|---|---|
| `model_id` | 否 | 精确过滤 |
| `kind` | 否 | `sale_credit` \| `sale_money` \| `upstream_cost`，非法值 400 |
| `limit` | 否 | 默认 100，上限 500（超上限钳到 500，对齐 handoff §5 运营列表约定） |
| `offset` | 否 | 默认 0，<0 时 400 |

排序固定 `(model_id, kind, revision DESC)`（稳定、可分页）。响应 200：

```json
{
  "code": 0,
  "data": {
    "items": [ <同 2.1 的 data 形状> ],
    "limit": 100,
    "offset": 0
  }
}
```

`items` 空时为 `[]` 而非 null。

## 3. 鉴权

与 upstream-accounts 面完全同链（handoff §3「服务身份 + 运营 JWT + 权限」）：

1. `InternalAppAuth`：`X-App-ID` + `X-App-Secret`（bcrypt），失败一律 401；
2. `JWTAuth`：运营人员 JWT；
3. `OperatorAuthz(store, "models:manage")`：JWT app claim 与服务身份一致 +
   `operator_roles` 角色映射。角色矩阵：admin/operator 可达，auditor 403。

无新 env、无新迁移。

## 4. 幂等

自然键幂等，复用表上 `UNIQUE(model_id, kind, revision)`（accounts 面
`UNIQUE(provider_id, credential_id)` 同款模式，无 Idempotency-Key 头）：

- dashboard 重放同一 revision 的同一创建请求 → **409**，body 带
  `data` = 已存在版本视图，message 标明 duplicate——调用方可安全重试
  （网络重试/页面刷新不产生重复版本）；
- 同一 revision 但**内容不同**（费率/生效区间/币种任一不一致）→ **409**，
  message 标明 conflict，data 仍带已存在视图——这是操作错误信号
  （revision 取错），不是重放；
- 服务端创建前按 (model_id, kind, revision) 先查：命中即比较后按上两条
  返回，未命中才插入。插入与唯一约束竞态兜底：constraint 违例映射 409。

## 5. 审计

创建走「写 + 审计同事务」（accounts 先例：store.Begin + AuditTxRecorder，
审计失败即回滚）：

- `action` = `price_version.create`，`object_type` = `price_version`，
  `object_id` = 新版本 id；
- `reason` = 请求 reason；
- 双重归因：`actor_user`（JWT 用户）+ `actor_app`（服务身份），取自
  `OperatorOf(c)`；
- `detail` = 完整创建负载（model_id/kind/currency/四档费率/revision/
  生效区间），过 `SanitizeDetail` 后落 `inference_audit_log`。

列表为只读，不落审计（与 GET /admin/upstream-accounts 等只读面一致）。

## 6. 负面矩阵

envelope 约定：错误 `{code: <HTTP 状态码>, message}`，message 只输出
domain.Error.Message（不泄露 cause 链）。

| # | 场景 | HTTP | code | message 方向 |
|---|---|---|---|---|
| 1 | 缺必填字段 / JSON 未知字段 / 尾随垃圾 | 400 | 400 | invalid request body |
| 2 | `kind` 非法 | 400 | 400 | unknown kind |
| 3 | `model_id` 不存在 | 404 | 404 | model not found |
| 4 | sale_credit 带了 currency | 400 | 400 | currency must be empty for sale_credit |
| 5 | sale_money/upstream_cost 缺 currency 或非 `^[A-Z]{3}$` | 400 | 400 | currency required / invalid |
| 6 | 任一费率 < 0 | 400 | 400 | rates must be >= 0 |
| 7 | `revision` ≤ 0 或缺席 | 400 | 400 | revision must be > 0 |
| 8 | `effective_to` ≤ `effective_from` | 400 | 400 | effective_to must be after effective_from |
| 9 | `extra_rates` 非对象 | 400 | 400 | invalid extra_rates |
| 10 | (model_id,kind,revision) 重放同内容 | 409 | 409 | duplicate + data=已存在视图 |
| 11 | 同 revision 不同内容 | 409 | 409 | conflict + data=已存在视图 |
| 12 | 缺/错 X-App-ID、X-App-Secret | 401 | 401 | unauthenticated |
| 13 | 缺/过期 JWT | 401 | 401 | unauthenticated |
| 14 | auditor 角色（无 models:manage） | 403 | 403 | forbidden |
| 15 | GET `kind` 非法 / `offset` < 0 | 400 | 400 | invalid query |
| 16 | 速率超限（/admin 组 30 req/min） | 429 | 429 | rate_limited |

非目标（显式排除）：不提供 update/delete（不可变设计）；不在创建时自动
闭合前一版本的 `effective_to`（区间重叠允许，`LatestPriceVersion` 按
[from,to) + revision DESC 解析，语义已在 pricing_repo 固定并被测试锁定）；
不做跨模型批量录入（复用 catalog bulk-import 是另一条面）。

## 7. Dashboard 对接契约

1. **字段名**以本文档 §2 为准（micros 一律 `_micros_per_mtok` 后缀、
   string 渲染；`revision` 由 dashboard 侧取「当前最大 +1」——可先 GET
   列表取最新 revision）。字段名在本 PR 评审通过后冻结。
2. **错误码**按 §6 矩阵：dashboard 只需按 HTTP 状态码分流，409 时读
   `data` 展示已存在版本；message 可直接展示（服务端已脱敏）。
3. **录入前预览**：建议 dashboard 在提交前调
   `POST /admin/model-prices/preview`（已上线，同权限组）展示影响面。
4. handoff 文档 §3 运营接口清单表新增一行：
   `models:manage | POST/GET /admin/price-versions | 售价版本：创建（只追加、revision 自然键幂等 409+已存在视图）与列表`。
5. 联调环境：staging；首个验证用例 = 为 deepseek-chat 录入 revision 2
   （入 100 万 / 出 200 万 micros per Mtok 即 revision 1 的 SQL 手工行）。

## 8. 实现落点

- repo：`internal/inference/postgres/pricing_repo.go` 新增
  `GetPriceVersionByRevision`（幂等检查）与 `ListPriceVersions`（过滤+分页）。
- service：`internal/inference/management/` 新增 price-version 服务
  （校验、unit 派生、同事务写+审计），Store 接口由 postgres.Store 满足。
- handler：`internal/inference/httpapi/admin_price_versions.go`
  （strictBindJSON + fail/ok envelope），挂 `AdminOps.Mount` 的
  models:manage 组；`cmd/server/main.go` 装配；`internal/router/router_test.go`
  加 nil-不挂载回归。
- 测试：httpapi 层 HTTP 验收（newOpsFixture，对齐 admin_accounts_test.go：
  鉴权 401/403、负面矩阵全覆盖、审计行数断言、409 重放）+ repo 层
  ListPriceVersions/GetPriceVersionByRevision 测试。
- 文档：handoff §3 表加行。
