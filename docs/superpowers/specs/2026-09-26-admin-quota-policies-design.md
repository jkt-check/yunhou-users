# 配额策略(quota policies)管理端点设计 — yunhou-users

日期:2026-09-26 · 状态:已实现
上游需求文件:《yunhou-users-quota-policies-api-requirements.md》(dashboard 运营侧,2026-09-26)
关联先例:`2026-09-25-admin-price-versions-design.md`(实现结构直接镜像)

## 1. 范围

补齐 `inference_policy_versions` 的完整生命周期 HTTP 管理面,替代 SQL 直插:

| 端点 | 说明 |
|---|---|
| GET /admin/quota-policies | 列表(`?name=` 精确、`?status=`、`?limit/offset`;排序 name ASC, revision DESC) |
| GET /admin/quota-policies/:id | 详情 + `referenced_by`(引用该版本的 active 权益条数,只读统计) |
| POST /admin/quota-policies | 新建/出新版(revision 服务端派生 = max+1,draft) |
| PATCH /admin/quota-policies/:id | 仅 draft 可改(model_ids/limits/overage_policy);published/superseded/retired → 409;空修改 → 400 |
| POST /admin/quota-policies/:id/publish | draft → published;同名旧 published 同事务转 superseded |
| POST /admin/quota-policies/:id/retire | → retired(保护规则见 Q1) |

全部挂 `models:manage` 组(Q2);写端点 reason 必填(400 文案与 M-6 一致
"reason is required");写 + 审计同事务(AuditTxRecorder.RecordTx,price-versions
同款,fail-closed);严格 JSON 绑定(strictBindJSON,未知字段 400)。

非目标:不改请求期计量/闸门语义;不动 entitlement 结构;不动 PAYG 端点
(已有 published-only 校验,见 §5-A12)。

## 2. §6 拍板结论(kaya 已代用户拍板,2026-09-26)

- **Q1**:retire 遇 active 权益引用**默认 409** + `referenced_by` 计数;
  `?force=true` 强制放行(已引用权益继续按 pinned 版本执行,仅不再允许
  新发放引用)。draft → retired 直接允许(等同废弃草稿)。
- **Q2**:权限过渡期沿用 `models:manage`,未来收敛到 `quota:manage`
  (本文件标注;dashboard 侧无需变动)。
- **Q3**:同名 published 唯一性 = 写侧同事务显式将旧 published 转
  `superseded`、读侧(新发放引用)只认 published,与 catalog revision
  语义对齐(需求 §4.5 选项 a)。
- **Q4**:不提供物理删除;retire 即审计可回滚的终态(软终态)。不实现
  DELETE。

**迁移例外(重要,与"零迁移"表述的唯一偏离)**:Q3(a) 选定 `superseded`
语义,而现状 status CHECK 只有 ('draft','published','retired')——需求文件 §4.5
明确授权"如缺约束请在 migration 中补齐并在交付说明中列出"。故补最小迁移
`040_policy_superseded.sql`:

1. status CHECK 扩为 ('draft','published','superseded','retired');
2. 部分唯一索引 `UNIQUE(name) WHERE status='published'`(每 name 至多一条
   published 的不变量在 DB 层兜底,写侧同事务转 superseded 是常态路径)。

**部署预检(各环境应用 040 前必跑,重复 name 必须先处置)**:

```sql
SELECT name FROM inference_policy_versions
WHERE status='published' GROUP BY name HAVING COUNT(*) > 1;
```

cn-staging 已核验零重复(2026-09-26);cn-prod / intl-prod 部署前由运维执行。

除此之外零表结构变更。

## 3. 语义细节

- **revision 服务端派生**:create 不带 revision;事务内 MAX(revision)+1;
  并发同 name 创建撞 UNIQUE(name,revision) → 409(不得跳号/重号)。
- **name 校验**:`^[a-z0-9][a-z0-9-]{0,62}$`。
- **model_ids**:非空,每个 id 必须存在于 inference_models,否则 400 指明
  哪个 id(策略 model_ids 仍是统计/口径字段,鉴权语义不变)。
- **limits**:每项为正整数或 null(>0;0 拒绝),至少一项非 null,否则 400
  「策略无任何限制项」。DB CHECK 对窗口限额是 >=0,服务侧收紧为 >0。
- **overage_policy**:枚举 reject | clamp_if_declared | allow_overage,
  缺省 reject。
- **幂等**(需求 §5.4):publish/retire 对同 id 重复调用 → 200 返回当前
  状态,不产生第二条审计;create 不幂等(409)。
- **retire 状态机**:draft → retired(直接);retired → 200(幂等);
  published/superseded → retired 走 referenced_by 保护(默认 409,
  ?force=true 放行)。superseded 版本仍可能被存量权益 pin,故同样走
  保护(超出需求字面、属保守扩展,已在此注明)。
- **pin 不变性**(需求 §5.1,验收 A13):权益持有 policy_version_id 不变;
  发布新 revision 不影响存量权益的配额口径。网关按 id 读取,superseded/
  retired 版本照常服务存量 pin;仅新发放(PAYG config、人工发放)不得
  引用非 published 版本。

## 4. 审计

action:quota_policy.create / .update / .publish / .retire;
detail:name、revision、前后 status、reason(及 retire 的 referenced_by/
force)。写 + 审计同事务。

## 5. 验收矩阵 → 测试映射

| # | 用例 | 测试 |
|---|---|---|
| A1 | 新建全新 name → 201,revision=1,draft,审计落库 | TestAdminQuotaPolicies_Create |
| A2 | 同 name 再建 → revision=max+1,draft | TestAdminQuotaPolicies_CreateNextRevision |
| A3 | model_ids 含不存在模型 → 400 指明 id | TestAdminQuotaPolicies_CreateUnknownModel |
| A4 | 全部 limit 为 null → 400 | TestAdminQuotaPolicies_CreateNoLimits |
| A5 | PATCH published → 409 | TestAdminQuotaPolicies_PatchPublished409 |
| A6 | publish 后同名旧 published 转 superseded | TestAdminQuotaPolicies_PublishSupersedes |
| A7 | 重复 publish 同 id → 200 幂等,审计仅一条 | TestAdminQuotaPolicies_PublishIdempotent |
| A8 | retire 有 active 权益引用(默认) → 409 + referenced_by | TestAdminQuotaPolicies_RetireReferenced409 |
| A9 | retire draft → 200 | TestAdminQuotaPolicies_RetireDraft |
| A10 | 缺 reason 的任何写 → 400 | TestAdminQuotaPolicies_ReasonRequired |
| A11 | 未授权 → 401/403 | TestAdminQuotaPolicies_Authz |
| A12 | PAYG 引用非 published 版本被拒 | TestPutPAYGConfig_RejectsNonPublished(管理包) |
| A13 | 发布新 revision 后存量权益口径不变 | TestQuotaPolicy_PinStability(管理包) |

HTTP 级测试走 opsFixture(真实库 + 真实 OperatorAuthz,/adminm 组),
镜像 admin_price_versions_test.go。
