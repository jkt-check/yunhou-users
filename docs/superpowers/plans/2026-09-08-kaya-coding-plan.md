# Kaya Coding Plan Implementation Plan

日期：2026-09-08  
分支：`kaya-coding-plan`  
状态：计划已编写，尚未开始功能实现  
基线：`master` / `ec255653ffccae7bd0b9f9ef792a2ebb58bfc642`  
设计：[Kaya Coding Plan 设计](../specs/2026-09-08-kaya-coding-plan-design.md)

## 目标

在 `yunhou-users` 中建设独立模型业务模块，支持大量模型的配置与动态发布、官方 API/OAuth/自托管上游、客户 API Key、独立 Coding Plan、5 小时/周/月配额、服务端计量与可核对账本，并向 `../yunhou-Website` 提供客户控制台及运营后台接口。

本次仅交付文档与分支。下面复选框均表示未来实施工作，不表示已经完成。此计划不要求本轮创建数据库表、接入真实供应商、迁移客户、提交代码或部署。

## 执行约束

- 按任务依赖实施，每个任务完成后记录实际修改、验证命令、结果和遗留问题。
- 先模块化单体，同 PostgreSQL；吞吐量数据证明必要后才引入 Redis 或独立网关进程。
- 业务实现在本仓；Website 的实际页面开发属于后续独立工作，本计划交付其 OpenAPI/DTO、权限和联调场景。
- 首期正式价格、可售模型、各窗口额度、赠送范围均由发布配置提供，不将文档示例种成在售商品。
- 上游认证与客户端协议分离；不把所有供应商行为压成一个有损的万能请求结构。
- 计费默认拒绝无授权/无价格/无法可靠约束消费的调用；旧 Kaya 入口在迁移开关之外保留兼容行为。
- 数据库测试使用专用可丢弃测试库。现有测试会清表，禁止对开发者业务库/生产库直接运行。
- 数据库测试包串行 `-p 1`；使用当前 `.github/workflows/pr-ci.yml` 门禁，不继承历史计划中的已知失败豁免或无条件全绿声明。
- 只修改本任务文件；工作区原有未跟踪 `package.json`、`package-lock.json` 不属于本计划，不加入提交。
- 文档所列新文件为目标路径；实际复用时允许减少文件数量，但必须保持接口边界并同步文档。

## 里程碑与依赖

| 阶段 | 任务 | 可验收结果 |
|---|---|---|
| A：边界与兼容基础 | 0–4 | 确定已有网关复用，双产品订阅兼容，目录/凭据可管理 |
| B：Coding Plan API 售卖闭环 | 5–11 | Key → 权益 → 预占 → 模型调用 → 结算 → 客户查询；支付发放可核对 |
| C：OAuth 与多工具接入 | 12–13 | OAuth 账号池及 Messages/Responses 独立协议能力 |
| D：按量与规模化运营 | 14–15 | 预付按量余额、批量管理、价格影响预览与成本分析 |
| E：联调和发布验证 | 16 | Website 契约、迁移演练、故障恢复、灰度与回滚验证 |

依赖顺序：0 → 1 → 2；1 → 3 → 4；2/3 → 5；1/2 → 6 → 7；4/5/7 → 8 → 9；2/6/7 → 10；5/7/9/10 → 11；4/8/9 → 12 → 13；7/9/10 → 14；3/4/11 → 15；各阶段功能进入售卖前执行 16 对应检查。

里程碑 B 可以先进行受控 API 售卖。整体计划包含 OAuth、按量及完整后台能力，不能以 B 完成为由标记全部计划完成。

## Task 0：确认基线与复用已有 multi-model-gateway

**涉及文件/材料**

- Read: `feat/multi-model-gateway`，检查时 HEAD `1d0281d`。
- Read: 该分支 `internal/llm/*`、`internal/service/chat.go`、`internal/handler/chat.go`、migration 022/023。
- Create: `docs/runbooks/kaya-coding-plan-baseline.md`。
- Update: 本计划与设计中的复用结果、最终迁移序列。

- [x] 检查当前分支、工作区与目标分支最新提交，不触碰其他 worktree 的未完成工作。
- [x] 在隔离测试环境运行基线 build/vet/串行测试，记录实际结果；对失败在未修改基线上复现，不凭旧计划认定是预置失败。
- [x] Review 候选协议转换、增量 SSE usage、Key 池、旧 `/chat` 契约和覆盖范围。
- [x] 选择兼容适配器与测试的定向复用；避免直接引入“写账失败忽略、缺失用量记零、NULL 白名单全放行”的计费语义。
- [x] 如候选分支已经合入主线，则基于合入结果做增量改造；如未合入，记录选取的提交/文件及归属，避免后续双份实现。
- [x] 登记本地 `feat/usage-analytics`（检查时 HEAD `3fdab4c`，位于 `.worktrees/usage-analytics`）的统计/管理接口能力，评估与 Task 15 的重叠，决定复用或拒绝继承，避免第三份用量统计实现。
- [ ] 向业务方确认“OOS / SUM TO API”实际含义（OAuth/Sub2API 连接器或 OSS 自托管），结论写入基线报告；Task 12 启动前必须已有结论。
- [x] 检查迁移账本与分支编号。022/023 已在候选分支使用，新迁移按最新序号追加，不能修改曾应用的 SQL。计划中的 `NNN_*` 是占位名，编码前替换为唯一序号。

**验收**：基线报告列明具体 commit、可复用能力、拒绝继承的旧语义、测试结果和迁移顺序；没有未经核验的“已有实现足够计费”结论。

**Task 0 结果（2026-09-08）**：基线报告见 [kaya-coding-plan-baseline.md](../../runbooks/kaya-coding-plan-baseline.md)。基线全绿（vet/build/迁移幂等/-race 套件 83.2% 覆盖/e2e 106.5s 均通过）。`feat/multi-model-gateway` 定向复用协议适配、增量 SSE usage、relay 机制；八条语义差距逐条决定，计费旧语义全部拒绝继承。`feat/usage-analytics` 内容与 master `ec25565` 树一致（已随 PR #17 合入），Task 15 复用其统计查询模式、不继承心跳数据源。迁移 022/023 保留给候选分支，inference 从 024 起。未完成项：仅“OOS / SUM TO API 向业务方确认”——本环境无法求证，当前按 OAuth/Sub2API 假设推进，Task 12 启动前必须闭环。

## Task 1：模块契约、类型与持久化骨架

**文件**

- Create: `internal/inference/domain/{model,principal,usage,money,errors}.go`。
- Create: `internal/inference/postgres/` 按表组的 repo 文件。
- Create: `migrations/NNN_inference_catalog.sql`、`NNN_inference_accounts.sql`、`NNN_inference_accounting.sql`。
- Modify: `migrations/README.md`、`internal/migrate/*_test.go`。
- Test: `internal/inference/domain/*_test.go`、`internal/inference/postgres/*_test.go`。

- [ ] 定义稳定公开模型 ID、部署/账号 ID、调用主体、价格版本、规范化用量与来源状态。
- [ ] 定义 `CatalogReader`、`CredentialResolver`、`EntitlementResolver`、`ProviderAdapter`、`QuotaStore`、`SettlementStore` 和可注入时钟接口。
- [ ] 定义事务边界：预占与结算 repo 必须共享同一 tx；不得隐藏调用另一个数据库连接。
- [ ] 实现整数微额度、Decimal/有理数价格转换、溢出检查、取整与币种校验。
- [ ] 按设计表组创建 schema、外键、请求/尝试/结算唯一键；数据字段及分页索引服务实际查询。
- [ ] 核心字段用类型列和 CHECK；仅协议扩展配置使用带 schema 版本的 JSONB。
- [ ] 迁移遵循仓库幂等约定，不自行写顶层 BEGIN/COMMIT；账本删除不使用用户级联删除。

**测试与验收**：全新数据库完整迁移与二次运行通过；金额舍入、极值、币种不符、重复唯一键和负数约束可验证；domain 不依赖 Gin 或跨域 service。

## Task 2：订阅按产品隔离，保护 Kaya 兼容行为

**文件**

- Create: `migrations/NNN_subscription_product_scope.sql`。
- Modify: `internal/model/{plan,subscription,order}.go`、`internal/repo/{repo,payments_repos}.go`。
- Modify: `internal/service/{auth,subscription,plan,quote,payment}.go`；检查 `token.go` 及所有活跃订阅读取。
- Modify: `internal/handler/{app,user,payment}.go`、相关 mocks、`tests/e2e/testhelpers.go`。
- Test: `internal/service/{auth,subscription,quote,payment}_test.go` 及 DB 测试。

- [ ] 给既有套餐、订阅回填 `kaya-membership`；确保 plan 与 subscription 产品归属一致，有数据库约束或等效事务保证。
- [ ] 新查询显式带 `product_code`；旧方法封装固定原会员产品，不随机返回 Coding Plan。
- [ ] 用按 `(user_id, product_code)` 的 active 唯一约束替换全局约束；迁移前检查异常数据并给出可读诊断。
- [ ] 逐项改造：首次登录试用、JWT/refresh、订阅列表/取消、报价、购买资格、支付确认、续订 webhook、退款、到期与重查。
- [ ] 检查 `external_subscription_id` 的定位是否足以识别商品，不通过“用户当前订阅”推断支付对象。
- [ ] 旧 API 省略 product 时只使用原会员产品；Coding Plan 新入口显式传产品，不允许客户端把付款兑换成其他商品。
- [ ] 阶段内暂不开放 Coding Plan 在售；先部署支持双产品的代码，再允许第二产品订阅落库。

**测试与验收**：同一用户双产品共存；同产品重复激活仍被拒绝；购买/取消/退款 API 套餐不改变 Kaya 有效期；旧 JWT、试用和支付 e2e 保持契约。迁移报告列明所有旧查询调用点已经处理。

## Task 3：数据库模型目录、部署映射与配置发布

**文件**

- Create: `internal/inference/catalog/{service,revision,import}.go`。
- Create: `internal/inference/postgres/catalog_repo.go`。
- Create: `internal/inference/management/catalog.go`、`internal/inference/httpapi/admin_models.go`。
- Modify: `internal/config/config.go`、`cmd/server/main.go`、`internal/router/router.go`。
- Test: catalog 与 HTTP/repo 测试。

- [ ] 实现模型、供应商、部署、模型到部署多对多路由的 CRUD 与分页筛选。
- [ ] 定义模型能力矩阵、最大输出、生命周期和公开 ID/上游名的区别。
- [ ] 实现草稿、乐观锁、校验、原子发布、读取不可变快照与回滚到历史版本。
- [ ] 环境变量目录提供显式、幂等导入，不在每次启动时覆盖数据库运营配置。
- [ ] 新模型未绑定售价/权限/有效部署时不可售；发布后模型列表只展示调用者允许的条目。
- [ ] 配置快照刷新失败继续用已验证版本并告警；不得加载半个版本。
- [ ] 管理 handler 可先实现与测试，完成 Task 4 的运营授权前不挂载可写管理路由。

**验收**：通过管理 API 新增第二个同协议模型并调用，无需服务重启；模型可有多个部署；并发编辑产生版本冲突；回滚不改历史账单。

## Task 4：上游凭据、服务端运营授权与审计

**文件**

- Create: `internal/inference/credentials/{vault,service}.go`。
- Create: `internal/inference/httpapi/{admin_auth,admin_credentials}.go`。
- Create: `internal/inference/management/audit.go`。
- Create: `migrations/NNN_operator_permissions.sql`、`cmd/admin-bootstrap/main.go`。
- Modify: `internal/config/config.go`、`cmd/server/main.go`、`.env.example`。

- [ ] 上游凭据采用 AEAD 加密，关联 credential ID/provider 作为附加认证数据，保存 key version，支持轮换及旧版本解密。
- [ ] 密钥从部署秘密注入；示例配置只写变量名，不写真实凭据；管理 API 仅返回脱敏状态。
- [ ] 运营身份由用户 JWT/受验证的服务身份组合确定；实现角色权限，不接受请求体伪造 actor/role。
- [ ] 提供离线幂等 bootstrap 命令创建首个运营管理员；服务端默认拒绝未授权管理操作。
- [ ] 凭据创建、轮换、测试和禁用写操作者/服务/对象/原因审计；后台读权限与秘密写权限分开。
- [ ] 上游 URL、重定向、DNS 解析和可配置头按设计限制；内网自托管部署使用显式允许范围。
- [ ] 凭据状态影响后续路由；Key/账号紧急禁用传播有明确上界。

**测试与验收**：普通用户/普通 App 凭据无法管理平台模型；越权、密文串换、错误 key version 被拒绝；响应、日志和审计均不含可用秘密；轮换期间有界在途请求正常收尾。

## Task 5：客户 API Key 与计费账户

**文件**

- Create: `internal/inference/access/{apikey,principal}.go`。
- Create: `internal/inference/postgres/{billing_account_repo,apikey_repo}.go`。
- Create: `internal/inference/httpapi/{apikey_auth,user_api_keys}.go`。
- Modify: `internal/router/router.go`。

- [ ] 每用户幂等建立个人计费账户，所有权来自可信用户身份。
- [ ] 生成高熵 API Key；明文只返回一次，数据库保存查找前缀与验证摘要。
- [ ] 支持列举、撤销、过期、模型权限、Key 预算；权限变更不能超出所属账户权益。
- [ ] 标准 `/v1/*` 使用 API Key；Kaya JWT 经 facade 转成同一种内部 principal。
- [ ] 明确调用 principal 与运营 principal 的区别，不复用 `X-App-Secret` 给客户。
- [ ] 按账户/Key 限速，IP 限速只保留为外围保护；并发额度与多实例协调接入 Task 7。

**测试与验收**：客户 A 不能读取/撤销 B 的 Key；第二次读取不能恢复明文；创建多个 Key 不增加账户额度；撤销和过期在后续调用生效。

## Task 6：权益映射、价格版本与三窗口纯规则

**文件**

- Create: `internal/inference/access/entitlement.go`。
- Create: `internal/inference/accounting/pricing.go`。
- Create: `internal/inference/quota/{policy,window}.go`。
- Create: `internal/inference/postgres/{entitlement_repo,pricing_repo}.go`。
- Test: `window_test.go`、`pricing_test.go`、`entitlement_test.go`。

- [ ] 定义套餐版本 → 模型集合/配额策略映射，明确订阅来源、有效期和权益修订。
- [ ] 实现固定 5 小时、锚定 7 天、公历月窗口；时钟可注入，读取不激活。
- [ ] 处理闰年、月末裁剪与恢复原始锚点、边界相等、跨月/跨年、UTC 展示转换。
- [ ] 实现同一权益消费主体在升级时保留 used/reserved，续费不提前刷新额度。
- [ ] 定义显式套餐优先、赠送默认不叠加/不自动兜底规则；既有会员无显式 grant 不自动获全部模型。
- [ ] 按独立价目表计算客户额度、按量售价、上游成本；规范化缓存/推理 token 的包含关系。
- [ ] 拒绝未定价且需要扣费的能力；把 estimated/unknown 与 reported 分开。

**测试与验收**：1 月 31 日锚点经过 2 月后恢复至 3 月 31 日；同一消费计入三窗口但只生成一次客户消费；改变价格不影响历史请求；年度付费仍按月发额度。

## Task 7：PostgreSQL 原子预占与跨实例并发控制

**文件**

- Create: `internal/inference/quota/{service,reservation}.go`。
- Create: `internal/inference/postgres/{quota_repo,reservation_repo,lease_repo}.go`。
- Test: `internal/inference/quota/*_test.go`、`internal/inference/postgres/quota_concurrency_test.go`。

- [ ] 一个事务内按固定顺序锁定账户、窗口和 Key 预算，检查并创建请求/预占；在网络调用前提交。
- [ ] 三窗口同时检查并更新 reserved；唯一键防止同一内部请求重复预占。
- [ ] 实现首次五小时窗口并发初始化及“全部请求确认无消费”时的安全撤销。
- [ ] 绑定 admitted_at、窗口 ID、价格与策略版本，结算不会改绑新周期。
- [ ] 用数据库租约协调账户及上游并发，定义续租、所有权和 fencing token；超时回收须避免与仍活跃请求重叠授权。
- [ ] 实现安全的预占金额计算；强制输出上限，校验高成本工具等额外计费项目上界。
- [ ] 存储不可用时新售卖调用拒绝放行；在途请求进入 Task 9 的持久恢复路径。
- [ ] 跨实例并发测试用两个真实服务进程共享同一可丢弃测试库编排；CI 单 job 暂不支撑时，在验收记录中写明本地编排方式、命令与实测结果，不得以单进程加锁模拟代替。

**测试与验收**：真实 PostgreSQL 上多 goroutine、两个服务实例竞争最后额度，放行量不超过安全预占界限；部分窗口失败全部回滚；重复预占、跨窗口完成、Key 子预算及并发释放均一致。不能只用串行 mock 验证。

## Task 8：网关编排、标准 Chat API 与 Kaya facade

**文件**

- Create: `internal/inference/gateway/service.go`。
- Create/Reuse: `internal/inference/providers/{openai_chat,anthropic,stream_usage}.go` 与候选 `internal/llm` 的适配器/测试。
- Create: `internal/inference/routing/service.go`、`internal/inference/httpapi/{models,chat_completions}.go`。
- Modify: `internal/{service,handler}/chat.go`、`internal/model/chat.go`、`internal/router/router.go`、`cmd/server/main.go`、`deploy/nginx.conf`。

- [ ] 以 principal → 权益 → 配额预占 → 路由 → attempt 持久化 → 上游调用编排标准入口。
- [ ] 支持 Chat Completions 流式/非流式及调用者可见 `/v1/models`；管理 envelope 不进入标准协议。
- [ ] 复用并补齐 SSE 增量解析；用量解析不受内容日志截断影响；按协议结束事件判断完整结束。
- [ ] 适配器在上游支持时显式请求流式 usage（如 OpenAI `stream_options.include_usage`、Anthropic `message_delta`）；上游确实不提供时才允许进入 estimated/unknown 路径。
- [ ] 请求字段按能力校验；不支持的工具/模态明确报错，不能静默丢字段或拼成文本。
- [ ] 上游路由只在能力兼容的部署之间选择；有界重试限于可重试错误且未开始客户端输出。
- [ ] 为每次尝试保存成本来源；失败切换不重复扣客户额度，不用错误重试绕过账号容量。
- [ ] `/chat` 保持旧无 model 默认、工具/思考开关、JWT 和错误 shape；是否启用新权益由迁移开关决定。
- [ ] 更新所有流式入口的 server/nginx 超时与缓冲、客户端断开处理、优雅停机。

**测试与验收**：真实 httptest 上游产生拆包 SSE、usage 位于末尾、非流式、工具调用、畸形事件、429/5xx、中途 EOF、客户端断开和慢上游；标准客户端可消费原生响应，旧 Kaya 回归通过。

## Task 9：幂等结算、未知用量与崩溃恢复

**文件**

- Create: `internal/inference/accounting/{settlement,reconciliation}.go`。
- Create: `internal/inference/postgres/{usage_repo,ledger_repo,reconciliation_repo}.go`。
- Create: `internal/inference/workers/settlement_recovery.go`。
- Modify: `internal/inference/gateway/service.go`、`cmd/server/main.go`。

- [ ] 一次事务完成 usage 保存、账本追加、reserved → used 转换及请求状态更新。
- [ ] 分别定义 logical request、attempt、usage revision、settlement 的唯一性，重复工作投递只产生一次结果。
- [ ] 独立有界 context 结算，不随客户端取消丢失已读取用量；错误落入可恢复状态。
- [ ] 请求发送前持久化 dispatch 意图；恢复任务区分未发送、可能已发送、已知结果及未知费用。
- [ ] 对未知用量以保守估算与核对队列为主、按供应商能力例外地做逐笔上游核对（主流 Chat/Messages 上游无按请求执行查询 API）；实施期限告警；禁止 TTL 到期自动视为零消费，禁止重试未知已执行的调用。
- [ ] 实现冲正/补差分录，修正估算时保留原记录；超出预占的实际费用显示异常并阻止继续透支，不隐藏负差额。
- [ ] 增加队列积压、未知 usage、预占悬挂、结算延迟及账本差异指标。

**测试与验收**：在“预占后、发送后、流尾后、写账前、提交后但响应前”分别注入故障；重启 worker 能恢复或明确进入待核对，没有静默免费、双扣或无审计释放。由账本重建聚合值可与窗口核对。

## Task 10：支付到权益的完整闭环

**文件**

- Create: `internal/inference/access/benefit_grant.go`、`internal/inference/postgres/outbox_repo.go`。
- Create: `internal/inference/workers/entitlement_sync.go`。
- Modify: `internal/service/{payment,quote,subscription,sweeper}.go`、相关 repo/model、`internal/handler/payment.go`。
- Create: `migrations/NNN_order_benefit_snapshot.sql`。
- Test: payment DB/e2e、grant 幂等与 outbox 重放测试。

- [ ] 下单时固定产品、金额币种、套餐/权益版本和升级规则快照；支付回调不能读取已被运营改写的商品来多发权益。
- [ ] 支付成功、续费、取消、退款按准确订阅来源更新权益；发放与支付状态同事务，或同事务 outbox 后幂等消费。
- [ ] 重复/乱序 webhook、主动支付确认与后台补单竞争不重复发额度/延长周期。
- [ ] 退款阻止后续不再具备权益的调用；已消费的账本不删除，金额调整使用明确规则与分录。
- [ ] 新商品没有支付配置时不可购买；用户自助免费订阅接口不能创建付费 Coding Plan。
- [ ] Bundle 规则显式发 grant 并遵守不叠加默认；迁移赠送需独立幂等来源键。
- [ ] 保留原会员购买规则；Coding Plan 用自身档位/周期规则，不能用“周期更长就可升级”的旧逻辑推导全部新商品。

**测试与验收**：从下单到 Key 调用有额度的端到端路径成立；调价/撤售后已支付订单按快照兑现；重复退款/续费不产生重复效果；API 套餐支付不会改变 Kaya 订阅。

## Task 11：客户配额、用量、套餐查询 API

**文件**

- Create: `internal/inference/management/{quota_view,usage_view,subscription_view}.go`。
- Create: `internal/inference/httpapi/{user_quotas,user_usage,user_subscriptions}.go`。
- Create: `docs/api/kaya-coding-plan.openapi.yaml`。
- Modify: `internal/router/router.go`、`docs/api-integration-guide.md`。

- [ ] 提供 `/user/model-quotas` 三窗口 used/reserved/remaining、UTC 时间、未激活状态、权益过期与 blocked_by。
- [ ] 提供模型/Key 分组和明细分页，限制时间范围，游标稳定排序，显示 as_of 与计量完整性。
- [ ] 明确 quota 读是当前权威状态，历史统计可延迟但给出截止时间；不能用延迟统计作为实时放行依据。
- [ ] 提供模型套餐与旧会员的独立视图；记录 grant 来源而不暴露上游账号。
- [ ] 定义 OpenAPI、错误码、整数精度和时间格式；64 位额度/金额采用十进制字符串，统一示例和 DTO。
- [ ] 添加零额度、未激活、耗尽、已过期、预占中、跨月、待核对的响应 fixture。

**测试与验收**：只有本人可查自身 Key/账本；多个窗口阻断返回准确原因；前端无需计算窗口；没有使用心跳表作为模型用量来源。

## Task 12：OAuth 连接器与上游账号池

**文件**

- Create: `docs/runbooks/kaya-coding-plan-connector-evaluation.md`。
- Create: `internal/inference/credentials/{oauth,refresh}.go`。
- Create: `internal/inference/providers/connector/` 选定连接器 client。
- Create: `internal/inference/routing/{account_pool,session_binding}.go`。
- Create: `internal/inference/workers/{credential_refresh,upstream_health}.go`。
- Create: `internal/inference/httpapi/admin_oauth.go`。

- [ ] 落实“OOS”实际接入语义及首个供应商，其他任务继续依照通用接口推进。
- [ ] 固定 Sub2API/CLIProxyAPI 候选版本，验证许可证、管理认证、模型发现、协议字段、流式终止、用量来源、账号切换与状态 API。
- [ ] 根据实测选择外置连接器或 SDK 复用，记录差异；不复制其客户系统/账本作为第二扣费权威。
- [ ] 实现独立于社交登录的 OAuth 授权、state/PKCE、回调一次性校验、凭据密文存储与撤销。
- [ ] 通过分布式刷新锁与 generation CAS 处理轮换；失效进入 reauth_required，停止分配新请求。
- [ ] 实现账号容量、冷却、健康度、上游额度 observed_at，以及必要的会话绑定。
- [ ] 上游额度不可得时显示未知；不得由客户余额推算上游余额，亦不得反向展示账号池总额度给客户。
- [ ] 用独立接入适配器支持自托管服务认证，不强迫 OSS 模型使用 OAuth。

**测试与验收**：模拟授权过期、两个实例同时刷新、旧 token 返回晚于新 token、账号限额耗尽、授权撤销、绑定账号失效和连接器不可用；客户计费只经过 Yunhou 账本。真实上游验收按已配置的测试账号执行并记录消耗。

## Task 13：Messages / Responses 协议与编程工具兼容

**文件**

- Create: `internal/inference/httpapi/{messages,responses}.go`。
- Create: `internal/inference/providers/` 对应原生协议映射文件。
- Modify: `internal/router/router.go`、`cmd/server/main.go`、`deploy/nginx.conf`、OpenAPI 和集成指南。
- Test: `tests/integration/inference_protocols_test.go` 及 provider fixture。

- [ ] 为每种协议独立定义支持字段、错误、流式事件和终止语义，先实现真实目标客户端要求的子集。
- [ ] 保留工具调用 ID、推理相关字段、usage、模型名称映射和多模态内容；无法兼容时明确拒绝。
- [ ] 如实现会话/response ID，持久化账户归属与上游账号绑定，防止跨客户读取或接续。
- [ ] 使用实际目标客户端或官方 SDK 进行契约验证，记录支持版本；WebSocket 仅在目标接入确有需要时单独实现并验收。
- [ ] 各协议仍共用同一 principal/预占/结算链，不绕过闸门。

**验收**：对外能力矩阵准确；不能仅把 Chat Completions 换路径就宣称 Messages/Responses 兼容。多轮工具调用、错误中断和 token 分类账本通过跨协议 fixture 验证。

## Task 14：预付按量余额与显式套餐外消费

**文件**

- Create: `migrations/NNN_inference_wallet.sql`。
- Create: `internal/inference/accounting/{wallet,overage}.go`。
- Create: `internal/inference/postgres/wallet_repo.go`。
- Create: `internal/inference/httpapi/{user_wallet,admin_adjustments}.go`。
- Modify: 支付商品/订单快照、gateway 预占与结算、OpenAPI。

- [ ] 单独定义余额充值商品与现金/赠送来源，不把充值金额当订阅有效期。
- [ ] 用定点、按币种隔离的借贷分录实现充值、冻结、消费、释放、退款及冲正，写入幂等业务键。
- [ ] 客户必须显式开启套餐外消费并设置支出上限；默认只消耗套餐，耗尽停止。
- [ ] 每个请求在入场时固定扣费来源，首版不在请求中途隐式切换套餐/余额；使用完整有界预占。
- [ ] 若账户没有套餐，允许显式 pay-as-you-go 权益下的余额调用；仍执行模型授权、速率与并发限制。
- [ ] 充值支付和重复回调/退款与模型账本对账；赠送余额不得伪装现金退款。

**测试与验收**：并发最后余额不能双花；未开启超额时不动余额；调价不会改冻结请求的价格；充值、消费、退款、调整借贷平衡，客户展示与账本一致。

## Task 15：大量模型运营、价格预览与成本分析

**文件**

- Create: `internal/inference/management/{bulk_import,pricing_preview,operations}.go`。
- Create: `internal/inference/httpapi/{admin_bulk,admin_usage}.go`；`admin_adjustments.go` 归 Task 14 创建，本任务扩展之（若本任务先于 Task 14 实施，则创建该文件并在实施记录中注明归属）。
- Modify: catalog 发布、审计、分页统计及 OpenAPI。

- [ ] 实现批量导入 dry-run、逐项错误、幂等任务 ID、大小上限与可追踪发布结果。
- [ ] 模型自动发现进入草稿，运营补齐授权/价格再发布；停用上游不删除历史路由记录。
- [ ] 价格/额度策略变更展示生效时间、受影响套餐/模型、旧订阅是否保留版本。
- [ ] 查看每模型/供应商/客户请求量、成功率、延迟、客户消费和采购成本，区分 unknown/estimated 成本与币种。
- [ ] 提供异常预占、授权失效和结算积压筛选；补偿必须有原因、权限、幂等键和追加审计。
- [ ] 按真实查询计划增加聚合/分区，不提前要求大型分析数据库；额度闸门继续用权威事务状态。

**测试与验收**：批量导入支持部分错误预览但不会半发布配置；重复提交不重复创建；统计聚合能与账本抽样核对；批量操作和补偿均可定位到人员。

## Task 16：Website 契约交付、完整验证与发布演练

**文件**

- Create: `docs/runbooks/kaya-coding-plan-rollout.md`。
- Create: `docs/api/kaya-coding-plan-website-handoff.md`。
- Modify: `README.md`、`docs/api-integration-guide.md`、必要的 CI 测试命令。
- Test: `tests/e2e/kaya_coding_plan_test.go`、相关迁移/恢复测试。

- [ ] 提供 Website 客户与运营接口、Cookie/BFF 身份传递要求、DTO/fixture、权限矩阵、分页和错误码。
- [ ] 约定三窗口卡片、未激活/耗尽/预占/到期/待核对状态及服务端时间；BFF 不持有上游明文凭据，不重新定价。
- [ ] 页面实现另在 Website 工作项展开；本任务验收这里的后端契约，不把页面已完成写入报告。
- [ ] 测试“购买独立套餐 → 发 Key → 调模型 → 三窗口查询 → 配额耗尽 → 正确重置 → 续费/退款”的链路。
- [ ] 基于合成历史数据库演练升级，包含空 expires_at 历史行、原会员、未完成订单、退款、候选分支 022/023 已应用与未应用路径。
- [ ] 运行协议、并发、断流、进程重启、数据库短暂不可用和上游耗尽场景；核对所有请求有终态或明确待核对状态。
- [ ] 做受控吞吐/长连接压测，记录 DB 锁等待、连接池、SSE 内存、结算延迟；基于实测决定是否另立 Redis/独立网关任务。
- [ ] 明确灰度：仅影子计量 → 有权益内部账户强制配额 → 小范围购买 → 逐步开放。
- [ ] 发布开关覆盖新商品、网关、OAuth 和超额消费。关闭新流量时持续结算既有请求；回滚不能删除账本或恢复全局单订阅假设。
- [ ] 完成商品实际配置、上游来源可售范围及历史用户迁移权益；未配置则保留关闭，不凭文档示例直接售卖。

**验收**：后端与文档契约一致、阶段功能满足验收矩阵、迁移/回滚有实测记录；完整计划所有已承诺阶段完成才标记整体完成。

## 验证命令

编码后使用专用测试数据库，先配置 `DATABASE_URL`、`E2E_DATABASE_URL` 指向同一可丢弃实例（e2e 与其他 DB 测试不并发）。以下命令按顺序执行；当前纯文档阶段不运行：

```bash
go vet ./internal/... ./cmd/...
go build ./...
go run ./cmd/migrate
go run ./cmd/migrate
go test -race -p 1 -coverprofile=coverage.out ./internal/... ./cmd/... ./tests/integration/...
go test -race -count=1 ./tests/e2e/...
go tool cover -func=coverage.out
```

日常任务先运行所改模块的有意义测试；涉及收费、并发、支付或迁移时必须用真实 PostgreSQL 覆盖关键事务。集成门禁遵循当前 CI 的整体覆盖率 >=80%，不为文档或简单字段更名编写镜像测试。禁止将缺少 DATABASE_URL 导致的 skip 记为数据库验收通过。

## 必须保持的验收矩阵

| 场景 | 期望 |
|---|---|
| Kaya 与 Coding Plan 同时有效 | 两个产品互不覆盖，旧登录响应兼容 |
| 多个 Key 并发竞争最后额度 | 账户共同封顶，无部分预占 |
| 五小时满、周/月有余额 | 明确五小时阻断，无默认钱包扣款 |
| 五小时刷新但周/月已满 | 仍阻断，恢复时间考虑所有窗口 |
| 首次使用失败且无消费 | 无残留收费；无人占用时可撤销未使用五小时窗口 |
| 月末/闰年/年度套餐 | 按原始锚点公历月重置，无日期漂移 |
| 跨窗口完成 | 回写入场窗口，旧预占正确消除 |
| 中断/缺失 usage/崩溃 | 已消费不漏记，未知可核对，不重复扣 |
| 同次请求上游重试 | 尝试成本分别记录，客户结算不重复 |
| 发布新模型或新价 | 未授权不可用，历史账单价格不变 |
| 重复支付/续费/退款事件 | 权益与账本幂等 |
| OAuth 并发刷新或授权失效 | 无旧凭据覆盖新凭据，失效账号停止调度 |
| 跨客户/无权限管理操作 | 服务端拒绝，秘密不泄漏 |
| 回滚或关闭新网关 | 停止新流量，存量结算可完成，财务事实保留 |

## 实施记录

- 2026-09-08：创建并切换 `kaya-coding-plan`；完成设计及本计划，记录未合入网关分支的复用候选。未开始 Task 0–16 实施，未运行功能测试，未修改生产代码。
- 2026-09-08：评审后补充——登记 `feat/usage-analytics` 复用候选；“OOS / SUM TO API” 术语澄清前置到 Task 0；未知用量恢复改为估算为主、核对例外；剩余额度不足预占上界时默认拒绝、不静默钳制；流式 usage 显式请求；修正 Task 14/15 的 `admin_adjustments.go` 归属重叠；Task 7 明确双进程并发测试编排要求。
- 2026-09-08：完成 Task 0。实际修改：新增 `docs/runbooks/kaya-coding-plan-baseline.md`；勾选本计划 Task 0 已完成项（“OOS 术语向业务方确认”保持未勾选）；更新设计 §2 复用决定与迁移序列。验证：一次性 PostgreSQL 16.14 实例上 `go vet`/`go build` 通过、`cmd/migrate` 连跑两次幂等（applied=21 → applied=0 skipped=21）、`go test -race -p 1` 全套通过（总覆盖 83.2%）、`go test -race ./tests/e2e/...` 通过（106.5s）；候选分支 `internal/llm` 与 `ChatService` 测试在其 worktree 只读运行通过。遗留问题：OOS / SUM TO API 术语待业务方确认（Task 12 前置）；`feat/multi-model-gateway` 的 022/023 尚未合入主线，inference 迁移固定从 024 起。
