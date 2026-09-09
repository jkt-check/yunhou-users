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

- [x] 定义稳定公开模型 ID、部署/账号 ID、调用主体、价格版本、规范化用量与来源状态。
- [x] 定义 `CatalogReader`、`CredentialResolver`、`EntitlementResolver`、`ProviderAdapter`、`QuotaStore`、`SettlementStore` 和可注入时钟接口。
- [x] 定义事务边界：预占与结算 repo 必须共享同一 tx；不得隐藏调用另一个数据库连接。
- [x] 实现整数微额度、Decimal/有理数价格转换、溢出检查、取整与币种校验。
- [x] 按设计表组创建 schema、外键、请求/尝试/结算唯一键；数据字段及分页索引服务实际查询。
- [x] 核心字段用类型列和 CHECK；仅协议扩展配置使用带 schema 版本的 JSONB。
- [x] 迁移遵循仓库幂等约定，不自行写顶层 BEGIN/COMMIT；账本删除不使用用户级联删除。

**测试与验收**：全新数据库完整迁移与二次运行通过；金额舍入、极值、币种不符、重复唯一键和负数约束可验证；domain 不依赖 Gin 或跨域 service。

## Task 2：订阅按产品隔离，保护 Kaya 兼容行为

**文件**

- Create: `migrations/NNN_subscription_product_scope.sql`。
- Modify: `internal/model/{plan,subscription,order}.go`、`internal/repo/{repo,payments_repos}.go`。
- Modify: `internal/service/{auth,subscription,plan,quote,payment}.go`；检查 `token.go` 及所有活跃订阅读取。
- Modify: `internal/handler/{app,user,payment}.go`、相关 mocks、`tests/e2e/testhelpers.go`。
- Test: `internal/service/{auth,subscription,quote,payment}_test.go` 及 DB 测试。

- [x] 给既有套餐、订阅回填 `kaya-membership`；确保 plan 与 subscription 产品归属一致，有数据库约束或等效事务保证。
- [x] 新查询显式带 `product_code`；旧方法封装固定原会员产品，不随机返回 Coding Plan。
- [x] 用按 `(user_id, product_code)` 的 active 唯一约束替换全局约束；迁移前检查异常数据并给出可读诊断。
- [x] 逐项改造：首次登录试用、JWT/refresh、订阅列表/取消、报价、购买资格、支付确认、续订 webhook、退款、到期与重查。
- [x] 检查 `external_subscription_id` 的定位是否足以识别商品，不通过“用户当前订阅”推断支付对象。
- [x] 旧 API 省略 product 时只使用原会员产品；Coding Plan 新入口显式传产品，不允许客户端把付款兑换成其他商品。
- [x] 阶段内暂不开放 Coding Plan 在售；先部署支持双产品的代码，再允许第二产品订阅落库。**部署顺序约束（Task 2 审查修复）：迁移 027 必须先于（或与首发同一批次）应用到生产库，双产品代码才能上线——新代码的订阅查询直接引用 `subscriptions.product_code`，二进制先于迁移滚动上线会让登录/聊天鉴权/支付路径因 "column does not exist" 大面积 500；代码上线后才开放第二产品售卖。**

**测试与验收**：同一用户双产品共存；同产品重复激活仍被拒绝；购买/取消/退款 API 套餐不改变 Kaya 有效期；旧 JWT、试用和支付 e2e 保持契约。迁移报告列明所有旧查询调用点已经处理。

## Task 3：数据库模型目录、部署映射与配置发布

**文件**

- Create: `internal/inference/catalog/{service,revision,import}.go`。
- Create: `internal/inference/postgres/catalog_repo.go`。
- Create: `internal/inference/management/catalog.go`、`internal/inference/httpapi/admin_models.go`。
- Modify: `internal/config/config.go`、`cmd/server/main.go`、`internal/router/router.go`。
- Test: catalog 与 HTTP/repo 测试。

- [x] 实现模型、供应商、部署、模型到部署多对多路由的 CRUD 与分页筛选。
- [x] 定义模型能力矩阵、最大输出、生命周期和公开 ID/上游名的区别。
- [x] 实现草稿、乐观锁、校验、原子发布、读取不可变快照与回滚到历史版本。
- [x] 环境变量目录提供显式、幂等导入，不在每次启动时覆盖数据库运营配置。
- [x] 新模型未绑定售价/权限/有效部署时不可售；发布后模型列表只展示调用者允许的条目。
- [x] 配置快照刷新失败继续用已验证版本并告警；不得加载半个版本。
- [x] 管理 handler 可先实现与测试，完成 Task 4 的运营授权前不挂载可写管理路由。

**验收**：通过管理 API 新增第二个同协议模型并调用，无需服务重启；模型可有多个部署；并发编辑产生版本冲突；回滚不改历史账单。

## Task 4：上游凭据、服务端运营授权与审计

**文件**

- Create: `internal/inference/credentials/{vault,service}.go`。
- Create: `internal/inference/httpapi/{admin_auth,admin_credentials}.go`。
- Create: `internal/inference/management/audit.go`。
- Create: `migrations/NNN_operator_permissions.sql`、`cmd/admin-bootstrap/main.go`。
- Modify: `internal/config/config.go`、`cmd/server/main.go`、`.env.example`。

- [x] 上游凭据采用 AEAD 加密，关联 credential ID/provider 作为附加认证数据，保存 key version，支持轮换及旧版本解密。
- [x] 密钥从部署秘密注入；示例配置只写变量名，不写真实凭据；管理 API 仅返回脱敏状态。
- [x] 运营身份由用户 JWT/受验证的服务身份组合确定；实现角色权限，不接受请求体伪造 actor/role。
- [x] 提供离线幂等 bootstrap 命令创建首个运营管理员；服务端默认拒绝未授权管理操作。
- [x] 凭据创建、轮换、测试和禁用写操作者/服务/对象/原因审计；后台读权限与秘密写权限分开。
- [x] 上游 URL、重定向、DNS 解析和可配置头按设计限制；内网自托管部署使用显式允许范围。
- [x] 凭据状态影响后续路由；Key/账号紧急禁用传播有明确上界。

**测试与验收**：普通用户/普通 App 凭据无法管理平台模型；越权、密文串换、错误 key version 被拒绝；响应、日志和审计均不含可用秘密；轮换期间有界在途请求正常收尾。

## Task 5：客户 API Key 与计费账户

**文件**

- Create: `internal/inference/access/{apikey,principal}.go`。
- Create: `internal/inference/postgres/{billing_account_repo,apikey_repo}.go`。
- Create: `internal/inference/httpapi/{apikey_auth,user_api_keys}.go`。
- Modify: `internal/router/router.go`。

- [x] 每用户幂等建立个人计费账户，所有权来自可信用户身份。
- [x] 生成高熵 API Key；明文只返回一次，数据库保存查找前缀与验证摘要。
- [x] 支持列举、撤销、过期、模型权限、Key 预算；权限变更不能超出所属账户权益。
- [x] 标准 `/v1/*` 使用 API Key；Kaya JWT 经 facade 转成同一种内部 principal。
- [x] 明确调用 principal 与运营 principal 的区别，不复用 `X-App-Secret` 给客户。
- [x] 按账户/Key 限速，IP 限速只保留为外围保护；并发额度与多实例协调接入 Task 7。

**测试与验收**：客户 A 不能读取/撤销 B 的 Key；第二次读取不能恢复明文；创建多个 Key 不增加账户额度；撤销和过期在后续调用生效。

## Task 6：权益映射、价格版本与三窗口纯规则

**文件**

- Create: `internal/inference/access/entitlement.go`。
- Create: `internal/inference/accounting/pricing.go`。
- Create: `internal/inference/quota/{policy,window}.go`。
- Create: `internal/inference/postgres/{entitlement_repo,pricing_repo}.go`。
- Test: `window_test.go`、`pricing_test.go`、`entitlement_test.go`。

- [x] 定义套餐版本 → 模型集合/配额策略映射，明确订阅来源、有效期和权益修订。
- [x] 实现固定 5 小时、锚定 7 天、公历月窗口；时钟可注入，读取不激活。
- [x] 处理闰年、月末裁剪与恢复原始锚点、边界相等、跨月/跨年、UTC 展示转换。
- [x] 实现同一权益消费主体在升级时保留 used/reserved，续费不提前刷新额度。
- [x] 定义显式套餐优先、赠送默认不叠加/不自动兜底规则；既有会员无显式 grant 不自动获全部模型。
- [x] 按独立价目表计算客户额度、按量售价、上游成本；规范化缓存/推理 token 的包含关系。
- [x] 拒绝未定价且需要扣费的能力；把 estimated/unknown 与 reported 分开。

**测试与验收**：1 月 31 日锚点经过 2 月后恢复至 3 月 31 日；同一消费计入三窗口但只生成一次客户消费；改变价格不影响历史请求；年度付费仍按月发额度。

## Task 7：PostgreSQL 原子预占与跨实例并发控制

**文件**

- Create: `internal/inference/quota/{service,reservation}.go`。
- Create: `internal/inference/postgres/{quota_repo,reservation_repo,lease_repo}.go`。
- Test: `internal/inference/quota/*_test.go`、`internal/inference/postgres/quota_concurrency_test.go`。

- [x] 一个事务内按固定顺序锁定账户、窗口和 Key 预算，检查并创建请求/预占；在网络调用前提交。
- [x] 三窗口同时检查并更新 reserved；唯一键防止同一内部请求重复预占。
- [x] 实现首次五小时窗口并发初始化及“全部请求确认无消费”时的安全撤销。
- [x] 绑定 admitted_at、窗口 ID、价格与策略版本，结算不会改绑新周期。
- [x] 用数据库租约协调账户及上游并发，定义续租、所有权和 fencing token；超时回收须避免与仍活跃请求重叠授权。
- [x] 实现安全的预占金额计算；强制输出上限，校验高成本工具等额外计费项目上界。
- [x] 存储不可用时新售卖调用拒绝放行；在途请求进入 Task 9 的持久恢复路径。
- [x] 跨实例并发测试用两个真实服务进程共享同一可丢弃测试库编排；CI 单 job 暂不支撑时，在验收记录中写明本地编排方式、命令与实测结果，不得以单进程加锁模拟代替。

**测试与验收**：真实 PostgreSQL 上多 goroutine、两个服务实例竞争最后额度，放行量不超过安全预占界限；部分窗口失败全部回滚；重复预占、跨窗口完成、Key 子预算及并发释放均一致。不能只用串行 mock 验证。

## Task 8：网关编排、标准 Chat API 与 Kaya facade

**文件**

- Create: `internal/inference/gateway/service.go`。
- Create/Reuse: `internal/inference/providers/{openai_chat,anthropic,stream_usage}.go` 与候选 `internal/llm` 的适配器/测试。
- Create: `internal/inference/routing/service.go`、`internal/inference/httpapi/{models,chat_completions}.go`。
- Modify: `internal/{service,handler}/chat.go`、`internal/model/chat.go`、`internal/router/router.go`、`cmd/server/main.go`、`deploy/nginx.conf`。

- [x] 以 principal → 权益 → 配额预占 → 路由 → attempt 持久化 → 上游调用编排标准入口。
- [x] 支持 Chat Completions 流式/非流式及调用者可见 `/v1/models`；管理 envelope 不进入标准协议。
- [x] 复用并补齐 SSE 增量解析；用量解析不受内容日志截断影响；按协议结束事件判断完整结束。
- [x] 适配器在上游支持时显式请求流式 usage（如 OpenAI `stream_options.include_usage`、Anthropic `message_delta`）；上游确实不提供时才允许进入 estimated/unknown 路径。
- [x] 请求字段按能力校验；不支持的工具/模态明确报错，不能静默丢字段或拼成文本。
- [x] 上游路由只在能力兼容的部署之间选择；有界重试限于可重试错误且未开始客户端输出。
- [x] 为每次尝试保存成本来源；失败切换不重复扣客户额度，不用错误重试绕过账号容量。
- [x] `/chat` 保持旧无 model 默认、工具/思考开关、JWT 和错误 shape；是否启用新权益由迁移开关决定。
- [x] 更新所有流式入口的 server/nginx 超时与缓冲、客户端断开处理、优雅停机。

**测试与验收**：真实 httptest 上游产生拆包 SSE、usage 位于末尾、非流式、工具调用、畸形事件、429/5xx、中途 EOF、客户端断开和慢上游；标准客户端可消费原生响应，旧 Kaya 回归通过。

## Task 9：幂等结算、未知用量与崩溃恢复

**文件**

- Create: `internal/inference/accounting/{settlement,reconciliation}.go`。
- Create: `internal/inference/postgres/{usage_repo,ledger_repo,reconciliation_repo}.go`。
- Create: `internal/inference/workers/settlement_recovery.go`。
- Modify: `internal/inference/gateway/service.go`、`cmd/server/main.go`。

- [x] 一次事务完成 usage 保存、账本追加、reserved → used 转换及请求状态更新。
- [x] 分别定义 logical request、attempt、usage revision、settlement 的唯一性，重复工作投递只产生一次结果。
- [x] 独立有界 context 结算，不随客户端取消丢失已读取用量；错误落入可恢复状态。
- [x] 请求发送前持久化 dispatch 意图；恢复任务区分未发送、可能已发送、已知结果及未知费用。
- [x] 对未知用量以保守估算与核对队列为主、按供应商能力例外地做逐笔上游核对（主流 Chat/Messages 上游无按请求执行查询 API）；实施期限告警；禁止 TTL 到期自动视为零消费，禁止重试未知已执行的调用。
- [x] 实现冲正/补差分录，修正估算时保留原记录；超出预占的实际费用显示异常并阻止继续透支，不隐藏负差额。
- [x] 增加队列积压、未知 usage、预占悬挂、结算延迟及账本差异指标。

**测试与验收**：在“预占后、发送后、流尾后、写账前、提交后但响应前”分别注入故障；重启 worker 能恢复或明确进入待核对，没有静默免费、双扣或无审计释放。由账本重建聚合值可与窗口核对。

## Task 10：支付到权益的完整闭环

**文件**

- Create: `internal/inference/access/benefit_grant.go`、`internal/inference/postgres/outbox_repo.go`。
- Create: `internal/inference/workers/entitlement_sync.go`。
- Modify: `internal/service/{payment,quote,subscription,sweeper}.go`、相关 repo/model、`internal/handler/payment.go`。
- Create: `migrations/NNN_order_benefit_snapshot.sql`。
- Test: payment DB/e2e、grant 幂等与 outbox 重放测试。

- [x] 下单时固定产品、金额币种、套餐/权益版本和升级规则快照；支付回调不能读取已被运营改写的商品来多发权益。
- [x] 支付成功、续费、取消、退款按准确订阅来源更新权益；发放与支付状态同事务，或同事务 outbox 后幂等消费。
- [x] 重复/乱序 webhook、主动支付确认与后台补单竞争不重复发额度/延长周期。
- [x] 退款阻止后续不再具备权益的调用；已消费的账本不删除，金额调整使用明确规则与分录。
- [x] 新商品没有支付配置时不可购买；用户自助免费订阅接口不能创建付费 Coding Plan。
- [x] Bundle 规则显式发 grant 并遵守不叠加默认；迁移赠送需独立幂等来源键。
- [x] 保留原会员购买规则；Coding Plan 用自身档位/周期规则，不能用“周期更长就可升级”的旧逻辑推导全部新商品。

**测试与验收**：从下单到 Key 调用有额度的端到端路径成立；调价/撤售后已支付订单按快照兑现；重复退款/续费不产生重复效果；API 套餐支付不会改变 Kaya 订阅。

## Task 11：客户配额、用量、套餐查询 API

**文件**

- Create: `internal/inference/management/{quota_view,usage_view,subscription_view}.go`。
- Create: `internal/inference/httpapi/{user_quotas,user_usage,user_subscriptions}.go`。
- Create: `docs/api/kaya-coding-plan.openapi.yaml`。
- Modify: `internal/router/router.go`、`docs/api-integration-guide.md`。

- [x] 提供 `/user/model-quotas` 三窗口 used/reserved/remaining、UTC 时间、未激活状态、权益过期与 blocked_by。
- [x] 提供模型/Key 分组和明细分页，限制时间范围，游标稳定排序，显示 as_of 与计量完整性。
- [x] 明确 quota 读是当前权威状态，历史统计可延迟但给出截止时间；不能用延迟统计作为实时放行依据。
- [x] 提供模型套餐与旧会员的独立视图；记录 grant 来源而不暴露上游账号。
- [x] 定义 OpenAPI、错误码、整数精度和时间格式；64 位额度/金额采用十进制字符串，统一示例和 DTO。
- [x] 添加零额度、未激活、耗尽、已过期、预占中、跨月、待核对的响应 fixture。

**测试与验收**：只有本人可查自身 Key/账本；多个窗口阻断返回准确原因；前端无需计算窗口；没有使用心跳表作为模型用量来源。

## Task 12：OAuth 连接器与上游账号池

**文件**

- Create: `docs/runbooks/kaya-coding-plan-connector-evaluation.md`。
- Create: `internal/inference/credentials/{oauth,refresh}.go`。
- Create: `internal/inference/providers/connector/` 选定连接器 client。
- Create: `internal/inference/routing/{account_pool,session_binding}.go`。
- Create: `internal/inference/workers/{credential_refresh,upstream_health}.go`。
- Create: `internal/inference/httpapi/admin_oauth.go`。

- [x] 落实“OOS”实际接入语义及首个供应商，其他任务继续依照通用接口推进。
- [x] 固定 Sub2API/CLIProxyAPI 候选版本，验证许可证、管理认证、模型发现、协议字段、流式终止、用量来源、账号切换与状态 API。
- [x] 根据实测选择外置连接器或 SDK 复用，记录差异；不复制其客户系统/账本作为第二扣费权威。
- [x] 实现独立于社交登录的 OAuth 授权、state/PKCE、回调一次性校验、凭据密文存储与撤销。
- [x] 通过分布式刷新锁与 generation CAS 处理轮换；失效进入 reauth_required，停止分配新请求。
- [x] 实现账号容量、冷却、健康度、上游额度 observed_at，以及必要的会话绑定。
- [x] 上游额度不可得时显示未知；不得由客户余额推算上游余额，亦不得反向展示账号池总额度给客户。
- [x] 用独立接入适配器支持自托管服务认证，不强迫 OSS 模型使用 OAuth。

**测试与验收**：模拟授权过期、两个实例同时刷新、旧 token 返回晚于新 token、账号限额耗尽、授权撤销、绑定账号失效和连接器不可用；客户计费只经过 Yunhou 账本。真实上游验收按已配置的测试账号执行并记录消耗。

## Task 13：Messages / Responses 协议与编程工具兼容

**文件**

- Create: `internal/inference/httpapi/{messages,responses}.go`。
- Create: `internal/inference/providers/` 对应原生协议映射文件。
- Modify: `internal/router/router.go`、`cmd/server/main.go`、`deploy/nginx.conf`、OpenAPI 和集成指南。
- Test: `tests/integration/inference_protocols_test.go` 及 provider fixture。

- [x] 为每种协议独立定义支持字段、错误、流式事件和终止语义，先实现真实目标客户端要求的子集。
- [x] 保留工具调用 ID、推理相关字段、usage、模型名称映射和多模态内容；无法兼容时明确拒绝。
- [x] 如实现会话/response ID，持久化账户归属与上游账号绑定，防止跨客户读取或接续。
- [x] 使用实际目标客户端或官方 SDK 进行契约验证，记录支持版本；WebSocket 仅在目标接入确有需要时单独实现并验收。
- [x] 各协议仍共用同一 principal/预占/结算链，不绕过闸门。

**验收**：对外能力矩阵准确；不能仅把 Chat Completions 换路径就宣称 Messages/Responses 兼容。多轮工具调用、错误中断和 token 分类账本通过跨协议 fixture 验证。

## Task 14：预付按量余额与显式套餐外消费

**文件**

- Create: `migrations/NNN_inference_wallet.sql`。
- Create: `internal/inference/accounting/{wallet,overage}.go`。
- Create: `internal/inference/postgres/wallet_repo.go`。
- Create: `internal/inference/httpapi/{user_wallet,admin_adjustments}.go`。
- Modify: 支付商品/订单快照、gateway 预占与结算、OpenAPI。

- [x] 单独定义余额充值商品与现金/赠送来源，不把充值金额当订阅有效期。
- [x] 用定点、按币种隔离的借贷分录实现充值、冻结、消费、释放、退款及冲正，写入幂等业务键。
- [x] 客户必须显式开启套餐外消费并设置支出上限；默认只消耗套餐，耗尽停止。
- [x] 每个请求在入场时固定扣费来源，首版不在请求中途隐式切换套餐/余额；使用完整有界预占。
- [x] 若账户没有套餐，允许显式 pay-as-you-go 权益下的余额调用；仍执行模型授权、速率与并发限制。
- [x] 充值支付和重复回调/退款与模型账本对账；赠送余额不得伪装现金退款。

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
- [ ] 明确灰度：仅影子计量 → 有权益内部账户强制配额 → 小范围购买 → 逐步开放。**每步灰度前先确认目标环境的迁移已全部应用（迁移先于服务流量，尤其是 027 这类新代码直接引用新列的迁移）；027 未应用的库不得滚动双产品代码。**
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
- 2026-09-08：完成 Task 1。实际修改：新增 `internal/inference/domain/`（model/principal/usage/money/errors + 测试）与 `internal/inference/postgres/`（按表组 repo + 共享 UnitOfWork 事务 + 测试）、`migrations/024_inference_catalog.sql`/`025_inference_accounts.sql`/`026_inference_accounting.sql`、`internal/migrate/migrate_inference_test.go`；修改 `migrations/README.md` 与本计划 Task 1 复选框。验证（一次性 PostgreSQL 16.14，端口 55434 可丢弃库，报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-1-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=24 → applied=0 skipped=24）；`go test -race -p 1 ./internal/inference/... ./internal/migrate/...` 全 ok；全量 `./internal/... ./cmd/...` 全 ok；domain 包零跨域依赖（`go list -deps` 无 gin/service/model 等）。遗留问题：`migrate.Apply` 的 `pg_advisory_lock` 实为会话级锁（注释语义有误，现有测试模型不暴露；是否改 `pg_advisory_xact_lock` 待后续任务决定）；Key 预算周期重置语义留给 Task 7；OOS/SUM TO API 术语待业务方确认（Task 12 前置）。
- 2026-09-08：完成 Task 2。实际修改：新增 `migrations/027_subscription_product_scope.sql`（plans/subscriptions 增加 product_code 并回填 kaya-membership、异常数据可读诊断 DO 块、(user_id, product_code) 部分唯一索引替换 002 全局索引、套餐↔订阅产品一致性触发器）、`internal/migrate/migrate_027_test.go`、`internal/service/subscription_product_scope_db_test.go`、`tests/e2e/subscription_product_test.go`；修改 model/repo/service/handler 订阅链路（详见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-2-report.md` 逐项调用点对照表）、`migrations/README.md` 与本计划 Task 2 复选框。验证（一次性 PostgreSQL 16.14，端口 55435 可丢弃库 yunhou_task2，测毕已清理）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=25 → applied=0 skipped=25）；`go test -race -p 1 ./internal/... ./cmd/... ./tests/integration/...` 全 ok；`go test -race -count=1 ./tests/e2e/...` 通过（104.6s）。双产品共存/同产品重复激活拒绝/购买取消退款不影响 Kaya 有效期/迁移异常诊断均有真实库测试钉牢；Coding Plan 本阶段不在售，无客户端路径可落库。遗留问题：plan 改写 product_code 不联动既有订阅（本阶段无此管理入口）；退款/失败级联取消仍按 (user_id, plan_id) 定位（plan_id 经触发器唯一绑定产品）。
- 2026-09-08：Task 1 审查修复（2 Important，commit 897364b）：`requests.reserved_micros` 由三窗口+Key 预算四目标累加改为单次消费量（取各 hold 之 max，口径注释锁定）；`Settle` 对 key_budget 预占做 `budget_used += (charge − hold)` 校正并带防负守卫与 CodeConflict。覆盖测试 `quota_settlement_test.go` 断言单次预占额与结算后实际消费累计。
- 2026-09-08：完成 Task 3（含审查修复，commits ea8001f、2f4563b）。实际修改：新增 `internal/inference/catalog/{service,revision,import}.go`、`internal/inference/management/catalog.go`、`internal/inference/httpapi/admin_models.go`；扩展 `internal/inference/postgres/catalog_repo.go`（写侧 CRUD、updated_at/config_version 乐观锁、revision 历史）；修改 `internal/config/config.go`、`cmd/server/main.go`、`internal/router/router.go`（写管理路由留 `RegisterWrite` 待 Task 4 挂载，router_test 断言 404）。语义：草稿→校验→原子发布（追加不可变 revision + 单事务切换 active）→读取不可变快照（SnapshotCache 刷新失败吃告警继续已验证版本，不得加载半个版本）→回滚=以新 revision 号重发旧内容（账本零触碰，测试断言）；`LLM_PROVIDERS_JSON` 显式幂等导入，不覆盖 DB 运营配置。审查修复（2 Important）：Publish 移除 500 单页上限改为 keyset 分页拉全（20000 防御上限，超限显式失败 "catalog too large to publish"）；`ListPublishedModels` nil AccessCheck 显式 fail-closed。验证（一次性 PostgreSQL 16.14，端口 55436 可丢弃库 yunhou_task3，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-3-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 连跑两次幂等；`go test -race -p 1 ./internal/... ./cmd/... ./tests/integration/...` 全 ok；`go test -race ./tests/e2e/...` 通过（105.3s）。并发编辑 1 胜 9 冲突（409）、管理 API 新增第二模型无需重启均有测试钉牢。
- 2026-09-08：完成 Task 4。实际修改：新增 `migrations/028_operator_permissions.sql`（operator_roles + inference_audit_log）、`internal/inference/credentials/`（vault=XChaCha20-Poly1305+AAD(credential ID+provider)+key version 轮换、egress=SSRF allowlist/拒绝集、service=Create/Rotate/Test/SetStatus/ResolveSecret 脱敏视图）、`internal/inference/management/`（audit.go 递归脱敏、operators.go 角色→四权限映射、catalog.go 全写操作审计+部署 URL egress 校验）、`internal/inference/httpapi/`（admin_auth 双身份中间件 OperatorAuthz/OperatorRequireRole+strictBindJSON 防伪造 actor、admin_credentials、admin_ops 聚合挂载）、`cmd/admin-bootstrap`（离线幂等 bootstrap，email 经 social_identities 解析）；修改 config（INFERENCE_CREDENTIAL_KEYS/INFERENCE_UPSTREAM_ALLOWLIST）、router（Setup 增 adminOps 参数、nil 不挂写路由）、cmd/server 装配、InsertCredential 显式写 id（修 AAD/审计归因不一致）、domain.Credential.LastRotatedAt、`.env.example` 只写变量名。验证（一次性 PostgreSQL 16.14，端口 55437 可丢弃库 yunhou_task4，报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-4-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 连跑两次幂等（applied=0 skipped=26）；`go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...` 20 包全 ok 无 FAIL；`go test -race -count=1 ./tests/e2e/` 通过（285.8s）。越权矩阵/密文串换(AAD)/错误 key version/伪造 actor/审计双重归因/禁用传播均有真实库测试钉牢。遗留问题：共享一次性库的包间残留为预存在模式（不带 -p 1 并发跑会互踩，Makefile 因此用 -p 1），本任务给 setupTask4 补了与 newTestServer 相同的全量 wipe；operator_roles 无自动过期；凭据 test 端点为结构化占位，真实上游拨测归 Task 8；部署层禁用传播到目录快照的最坏窗口为一个刷新周期（Task 8 按请求 pin 后收窄）。
- 2026-09-08：Task 4 审查修复（4 Important，0 open）。①运营身份补绑定校验：JWT app claim（ContextAppID）与受验证服务身份不一致即 403（admin_auth.go loadOperator）。②ParseKeysEnv 错误脱敏：只含 entry 序号或 version 段，hex 解码错误不再回显密钥字节。③凭据写路径原子化：新增 credentials.TxStore/TxRecorder 可选接口，Create/Rotate/SetStatus 的"变更+传播+审计"在同一 domain.UnitOfWork 内提交、审计失败整体回滚（postgres 四个 Tx 变体经 sqlTx 解包，编译期断言）；真实 PG 故障注入测试钉牢三种回滚/传播场景。④ValidateCustomHeaders 接入生产：黑名单单源移至 domain（credentials 委托，避免循环依赖），catalog.ValidateDeployment 在唯一写入 choke point 拒绝黑名单/畸形 headers、放行良性头。验证（同一一次性实例）：`go vet`/`go build` 通过；`go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...` 20 包全 ok 无 FAIL；`go test -race -count=1 ./tests/e2e/` 通过（131.7s）。详见 task-4-report.md 审查修复一节。
- 2026-09-08：完成 Task 5。实际修改：新增 `internal/inference/access/`（apikey=`yk-`+base64url(32B) 高熵生成 + SHA-256 摘要常数时间比对 + KeyService 管理用例；principal=Resolver.Authenticate/facade ResolveUserSession/AuthorizedModelIDs；ratelimit=可注入时钟滑动窗口 RPM）、`internal/inference/postgres/{billing_account_repo,apikey_repo}.go`（ON CONFLICT 幂等建户、管理面不读 key_hash、幂等撤销/惰性过期）、`internal/inference/httpapi/{apikey_auth,user_api_keys}.go`（/v1 原生错误形状 + Key/账户 RPM + Retry-After；/user/api-keys 五端点 envelope + 字段存在性 PATCH + 明文仅创建一次）、`internal/inference/access/*_test.go`、`postgres/billing_account_repo_test.go`、`httpapi/user_api_keys_test.go`（越权矩阵/限速/撤销过期生效，真实库+真实 JWT）；修改 `internal/router/router.go`（accessOps 参数，/user/api-keys 挂 JWT 后、/v1 组固定鉴权链）、`cmd/server/main.go` 装配、`internal/config/config.go`（INFERENCE_ACCOUNT_RPM 默认 120）、`.env.example`、httpapi `fail()` 增加 401/403/429 映射、router_test fail-closed 断言、integration/e2e 六处 Setup 调用点补 nil。无新迁移（025 已含表组）。验证（一次性 PostgreSQL 16.14，端口 55438 可丢弃库 yunhou_task5，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-5-report.md`）：`go vet`/`go build` 通过；`go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...` 全 ok 无 FAIL。遗留问题：/v1 组暂无业务路由（Task 8 挂载；httpapi 探针测试已钉牢鉴权/撤销/过期/限速语义）；facade 中间件生产接线归 Task 8；RPM 为进程内原语（多实例≈配置×实例数，Task 7 租约接管）；last_used_at 写放大与 Key 预算扣减归 Task 7/8。
- 2026-09-09：Task 5 审查修复（1 Important，commit a39a76d）：`ownedKey` 三条 NotFound 路径（未知 ID / 无账户 / 他人所有）统一返回共享 `errAPIKeyNotFound`，不再透传内部操作名；httpapi 越权矩阵断言"不存在 ID 与他人 ID"响应体逐字节相同。
- 2026-09-09：完成 Task 6。实际修改：新增 `internal/inference/quota/{window,policy}.go`（三窗口纯规则：AddMonthsClamped 原始锚点日裁剪闰年感知、weekly 7×24h 锚定、five_hour 消费激活/读取不激活、[start,end) 边界；策略 EvaluateAdmission 全窗口 fail-closed、SettlementPlan 镜像不翻倍）、`internal/inference/accounting/pricing.go`（三套价目独立版本化 ResolvePrice [from,to)、BillableBuckets 缓存/推理规范化、Quote 逐行取整——客户 RoundUp/成本 RoundDown、estimated/unknown 与 reported 分离、CodeUnpricedCapability 拒绝未定价扣费项）、`internal/inference/access/entitlement.go`（PlanRevision→GrantFromPlan 映射、SelectEntitlement 显式优先/赠送抑制/不叠加/无 grant 不自动获模型、Upgrade/Renew/DowngradeEffectiveAt 修订规则）、纯逻辑测试四件（quota/window_test.go+policy_test.go、accounting/pricing_test.go、access/entitlement_test.go，注入时钟/内存 fake 不依赖 DB）、`internal/inference/postgres/{entitlement_repo_test,pricing_repo_test}.go`（真实库）；修改 `internal/inference/domain/{errors,principal}.go`（CodeUnpricedCapability、EntitlementPatch）、`internal/inference/postgres/{entitlement_repo,pricing_repo,ledger_repo}.go`（ReviseEntitlement 乐观锁 in-place 修订、GetPriceVersion/Pure 转换器、Settle 账本 charge 携带请求钉住的 price_version_id）。无新迁移（表组已在 026）。验证（一次性 PostgreSQL 16.14，端口 55439 可丢弃库 yunhou_task6，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-6-report.md`）：`go vet`/`go build` 通过；`go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...` 23 包全 ok 无 FAIL。验收硬项全部有测试钉牢：1/31 锚点→2 月末→3/31 恢复（含闰年与年付 12 期逐月）、同一消费三窗口 used 同增且账本恰一条 charge、钉住版本不受新价影响且账本归因不可改写。遗留问题：窗口并发建行/原子预占编排归 Task 7；Stackable 合并规则待后续明确发布；CodeUnpricedCapability 的 httpapi 映射随 Task 8/9 接线；OOS 术语待业务方确认（Task 12 前置）。
- 2026-09-09：完成 Task 7。实际修改：新增 `internal/inference/quota/{service,reservation}.go`（准入编排：事务内窗口激活+预占+账户并发租约一次提交、admitted_at 按预占成功时绑定且每次重试取新鲜服务端时钟、fail-closed；预占上界纯规则：输入/强制输出/额外计费项目逐行 RoundUp 上界价）、`internal/inference/postgres/{reservation_repo,lease_repo}.go`（释放+五小时窗口事务内安全撤销；租约获取/续租/所有权/fencing/超时回收/CheckLease）、`internal/inference/quota/{reservation_test,service_test}.go`、`internal/inference/postgres/quota_concurrency_test.go`（多 goroutine + re-exec 双真实进程竞争最后额度）；修改 `internal/inference/postgres/quota_repo.go`（锁序改为 账户→窗口→Key 预算、`lockAccountTx`、`ActivateWindowsTx`、配额拒绝带 DeficitMicros 缺口、移除已迁移骨架）、`internal/inference/postgres/ledger_repo.go`（Settle 无条件锁请求行/终态拒绝/空预占拒绝/固定锁序/held 守卫）、`internal/inference/domain/usage.go`（租约类型）、勾选本计划 Task 7 复选框。无新迁移（表组与约束已在 026）。验证（一次性 PostgreSQL，端口 55440 可丢弃库 yunhou_task7，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-7-report.md`）：`go vet`/`go build` 通过；`go test -race -count=1 -p 1 ./internal/... ./cmd/... ./tests/integration/...` 全 ok；双进程实测 proc A 放行 2 + proc B 放行 1 = 容量 3，其余 13 次 quota_exceeded，并发套件 -race 连跑 5 轮全绿。修复的自测发现：admitted_at 跨重试沿用排队前时间戳会与等待期间提交的窗口永久 EXCLUDE 冲突（改为按预占成功时绑定）；CheckLease 误用"token 须为 scope 最大值"会 fence 合法并发持有者（改为状态+所有权+未过期）。遗留问题：上游 scope 租约消费点归 Task 8；崩溃恢复 worker 归 Task 9；clamp_if_declared 待协议字段；advisory 锁哈希碰撞仅性能影响。
- 2026-09-09：Task 7 审查修复（1 Important）：`Reserve` 窗口 hold 失败路径由 fail-open 改为 fail-closed——条件 UPDATE 未命中活跃窗口行即无条件记录 `WindowBlock`（`windowKindOfTarget` 补 kind，行消失时 ResetsAt=nil 不编造恢复时刻），明细重读仅作补充、出真实错误则整个 Reserve 中止；与 Key 预算路径对称。新增真实库测试 `TestReserveWindowHoldFailClosedOnVanishedRow`（同事务删除窗口行 → 阻断 + 零预占零请求行）。验证（一次性 PostgreSQL，端口 55440，测毕清理）：quota/reservation/并发定向 22 用例与全量 `-race -p 1` 23 包全绿。详见 task-7-report.md §8。
- 2026-09-09：完成 Task 8。实际修改：新增 `internal/inference/providers/{adapter,openai_chat,anthropic,anthropic_stream,stream_usage}.go`（适配器+SSE 增量计量：include_usage 恒开、[DONE]/message_stop 终止语义、nil≠0、拆包/畸形事件/中途 EOF 容错、Anthropic 流式与非流式翻译）、`internal/inference/routing/service.go`（能力兼容候选+账号池 round-robin/冷却+上游租约 Acquire/Check/Renew/Release 消费点+fencing 流中止）、`internal/inference/gateway/service.go`（principal→权益→快照 pin→Admit 预占→路由→attempt 同事务落库→上游→detached ctx 结算/释放/核对）、`internal/inference/httpapi/{models,chat_completions,kaya_models}.go`（/v1 原生形状与错误映射、Retry-After 仅可计算时、能力字段明确 400）、`internal/service/chat_facade.go`（/chat facade：ErrChat* 哨兵映射、无 [DONE] EOF→上游中断事件、Close 兜底结算）；修改 postgres（ListActiveUpstreamAccounts、InsertAttemptTx/UpdateRequestStatus/FinishAttempt）、router（/v1 业务路由实际挂载、/chat 按迁移开关选 facade、GET /chat/models）、cmd/server（gateway 装配、catalogCache 按请求 pin、priceCheck、超时跳过 /v1/chat/completions）、config（INFERENCE_KAYA_CHAT_GATEWAY 默认关 + KAYA_CHAT_MODEL 开关开启必填）、deploy/nginx.conf（/v1 SSE 700s/buffering off/1m）、handler/chat.go（导出 ChatStreamer + model 覆盖扩展）、model/chat.go（可选 Model 字段+长度上限）、.env.example。候选分支 `internal/llm` 定向复用 openai/anthropic/anthropic_stream 适配器与测试（改造点见 task-8-report §3：无名工具改显式报错、缺失 usage 永不记零、keypool 仅继承轮询+冷却形状）。无新迁移。验证（一次性 PostgreSQL，端口 55441 可丢弃库 yunhou_task8，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-8-report.md`）：`go vet`/`go build` 通过；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 26 包全 ok 无 FAIL；`go test -race -count=1 ./tests/e2e/` 通过（108.2s）。验收硬项全绿：拆包 SSE/usage 末尾/非流式/工具/畸形事件/429 失败切换恰结算一次/全 5xx 释放+窗口撤销/中途 EOF（有 usage=estimated 已读实际量，无 usage=核对+预占保留）/客户端断开/慢上游超时进核对/Anthropic 翻译结算；标准客户端形状可消费；旧 /chat 回归（无 model 默认、tools/thinking、401/403/400/404 envelope 逐字）两种模式通过。遗留问题：凭据真实拨测归 Task 12；Key scope 租约、TPM、session_sticky 调度未接；clamp_if_declared 待协议字段；进程内冷却不跨实例（数据库租约兜底）。
- 2026-09-09：完成 Task 9。实际修改：新增 `migrations/031_inference_settlement_recovery.sql`（控制者裁决 031 起空号：charge 允许零额——零消费恒落 charge 行使结算唯一键覆盖全部投递且账本层可区分"已结算(0)/未结算"；reconciliation reason 增 settlement_overage；reconciliation_jobs.request_id 可空+窗口级任务按 detail->>'window_id' 部分唯一索引去重；请求行持久化准入上界 input_bound/output_cap/extra_bounds 作恢复估算依据）、`internal/inference/accounting/{settlement,reconciliation}.go`（Decide 归一+计价+超占检测、ConservativeRecord 保守估算事实、CorrectionDelta、RebuildUsedMicros/WindowReconciliation 账本重建纯规则）、`internal/inference/postgres/{usage_repo,reconciliation_repo}.go`（尝试/计量读取面；核对任务 upsert/滞留扫描/期限升级/CorrectSettlement 冲正补差事务/窗口重建核对/RecoveryMetrics）、`internal/inference/workers/settlement_recovery.go`（恢复 worker：未发送/全尝试确认零消费→释放、可能已发送/已知结果未知费用→保守估算=预占额全额入账+crash_recovery 证据窗口、期限升级只叫人不记零不释放、UsageVerifier 供应商能力例外、窗口差异入队不自动修复）；修改 `internal/inference/postgres/{ledger_repo,request_repo,reservation_repo}.go`（恒落 charge 行、停放置 usage_status=unknown、bounds 落库、ReleaseFromReconciliation 证据驱动释放+任务同事务 resolve）、`internal/inference/quota/service.go`（准入上界随请求持久化）、`internal/inference/domain/usage.go`、`internal/inference/gateway/service.go`（settle 改用 accounting.Decide、事务内 3 次重试再停放、超占真实入账+同事务超占任务+告警）、`cmd/server/main.go`（worker 装配启动即跑一轮）、config/.env.example（INFERENCE_RECOVERY_* 四变量）。验证（一次性 PostgreSQL 16.14，端口 55442 可丢弃库 yunhou_task9，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-9-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=27 → skipped=27）；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 28 包全 ok 无 FAIL；五故障注入点（预占后/发送后/流尾后/写账前/提交后响应前）真实库实测全部恢复或明确待核对，无静默免费/双扣/无审计释放，账本重建与窗口核对一致。遗留问题：死信租约靠 TTL+fencing（worker 不代释放）；UsageVerifier 生产无实现（主流上游无查询 API，Task 12 视供应商能力补）；窗口核对全量扫描待规模化分页；人工调整入口归 Task 14/15；031 前存量行无 bounds（按预占额结算、桶不编造）。

- 2026-09-09：完成 Task 10。实际修改：新增 `migrations/029_order_benefit_snapshot.sql`（orders 权益快照七列 + plan_benefit_configs 支付配置 + plan_upgrade_rules 跨档规则）、`internal/inference/access/benefit_grant.go`（DecideSync/Converge 收敛规则、ResolveCodingPlanActivation 档位规则、迁移赠送幂等键）、`internal/inference/workers/entitlement_sync.go`（outbox 消费 worker）、`internal/repo/plan_benefit_repo.go`、`internal/model/plan_benefit.go`、`internal/migrate/migrate_029_test.go`、测试三件（access 纯规则、workers 真实库 11 例、service payment_benefit_db_test 10 例）+ e2e `coding_plan_purchase_test.go`（mock 渠道全链路）；扩展 `internal/inference/postgres/{outbox_repo,entitlement_repo}.go`（EnqueueOutboxSQLTx/按 topic 拉取/支付状态读取面/收敛写路径/MarkExpiredEntitlements）；修改 `internal/service/{payment,quote,subscription,sweeper}.go`（快照下单+发放入队同事务、coding-plan 档位规则、报价/自助兜底、Cancel 同事务、权益到期标记钩子）、`internal/handler/{payment,app}.go` 错误映射、`internal/{model/order.go,repo/payments_repos.go}` 快照列、`cmd/server/main.go` 装配、`internal/config` + `.env.example` 两个新变量、`migrations/README.md`（补登 028/031 缺漏行 + 029）、本计划 Task 10 复选框。发放路径决策：同事务 outbox + worker 收敛消费（理由见 task-10-report §3）。验证（一次性 PostgreSQL 16.14，端口 55443 可丢弃库 yunhou_task10，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-10-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=28 → skipped=28）；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 全 ok 无 FAIL；`go test -race -count=1 ./tests/e2e/` 通过（122.0s）。验收硬项均有测试钉牢：下单→mock 渠道回调→worker→resolver→配额准入端到端（e2e）；调价/撤售/权益配置改写后已支付订单按快照兑现；重复退款/续费/Confirm-webhook 竞争零重复效果；API 套餐支付不动 Kaya 订阅。遗留问题：plan_benefit_configs/plan_upgrade_rules 暂无管理面（运营 SQL 维护，归 Task 15）；永久失败的 outbox 消息无死信态（慢车道重试 + pending 可查）；PayPal 渠道 coding-plan 跨档改签仍受既有 PayPal 双扣守卫整体拒绝。
- 2026-09-09：Task 10 审查修复（1 Important）：Converge 对 nil 目标 EffectiveTo 的空指针 panic + worker 无 recover 崩溃循环 → EntitlementPatch 增 ClearEffectiveTo（有限→开放显式置 NULL，三个 revise/revive 变体 SQL 同步）+ processMessage recover 兜底进有界退避；纯规则/真实库/panic 注入四组新测试钉牢。验证：一次性 PostgreSQL（端口 55443，测毕清理）定向 access/workers/postgres 全 ok，全量 `-race -p 1` 27 包无 FAIL。详见 task-10-report.md §9。
- 2026-09-09：完成 Task 11。实际修改：新增 `internal/inference/management/{quota_view,usage_view,subscription_view}.go`（配额读模型：视图权益选择同调用路径优先序、阻断计算[窗口耗尽 remaining=0 全返回 + 权益过期/吊销/未生效 + 账户暂停]、读取不激活五小时窗口；用量读模型：模型/Key 分组聚合 + UTC 日序列 + keyset 游标明细分页、92 天跨度上限、计量完整性计数、token 桶 NULL≠0；套餐读模型：coding-plan 订阅 + 权益 + grant 来源分类 bundle/migration、kaya_membership 命名空间隔离）、`internal/inference/postgres/customer_read_repo.go`（三个 Store 接口实现；subscriptions/plans 跨域只读走 Task 10 先例）、`internal/inference/httpapi/{user_quotas,user_usage,user_subscriptions}.go`（设计 §9.2 示例形状逐字对齐；十进制整数字符串；RFC3339 UTC）、`docs/api/kaya-coding-plan.openapi.yaml` + `docs/api/fixtures/` 七态响应 fixture（零额度/未激活/耗尽/已过期/预占中/跨月/待核对）、测试五件（management 纯规则三件、postgres 真实库六例[含心跳表隔离断言]、httpapi 真实 JWT 越权/校验/分页/多窗阻断/套餐隔离 + fixture 归一化逐字段比较 + OpenAPI 文本级契约断言）；修改 `internal/router/router.go`（AccessOps 三字段挂载 /user 组）、`cmd/server/main.go`（视图服务装配）、`internal/inference/management/catalog.go`（包文档含客户读视图）、`docs/api-integration-guide.md`（新增 Kaya Coding Plan 客户接口一节）、本计划 Task 11 复选框。无新迁移。验证（一次性 PostgreSQL 16.14，端口 55444 可丢弃库 yunhou_task11，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-11-report.md`）：`go vet`/`go build` 通过；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 全 ok 无 FAIL；`go test -race -count=1 ./tests/e2e/` 回归通过。验收硬项：端点不接 ID 参数（越权结构上不可能，B 用户响应零 A 标识）；多窗阻断 blocked_by 全部返回（各带 resets_at）；前端无需计算窗口（window_start/resets_at/remaining 服务端给出）；usage 只读 inference_requests/usage_records/ledger（usage_events 心跳行注入后汇总分毫不动）。遗留问题：OpenAPI 校验为文本级 + fixture 归一化比较（未引入 yaml/kin-openapi 依赖）；usage summary 的 series 为 UTC 自然日桶（更细粒度由前端多次 range 查询或后续任务扩展）；usage_events 心跳与模型用量的口径分离已在文档与测试钉死。
- 2026-09-09：Task 11 审查修复轮 1（1 Critical + 2 Important + 3 Minor）：C1 net_micros 双重扣减（settled−reversal 在 CorrectSettlement 作废后报负净额）→ 行/组/序列金额全部改从不可变账本派生（charge=Σ原始分录、reversed=Σ冲正、adjusted=Σ请求级调整 debit+/credit−，与 reconciliation 重建不变量逐字一致），新增真实作废路径测试（行级/组级 net=0）与运营补偿计入 net 测试；I1 伪造游标 500 → DecodeRequestCursor 增 uuid.Parse 边界校验（400 invalid_input）；I2 随 C1 自然消解并钉测试；M1 窗口块 remaining 改由 management.Remaining 计算；M2 series 桶加 reversed/adjusted/net 列；M3 BillingAccountStatus 常量替换魔法字符串（quota_view + lockAccountTx）。验证（一次性 PostgreSQL 16.14，端口 55444，测毕清理）：vet/build 通过；migrate 连跑两次幂等（28→28）；`-race -p 1` 27 包无 FAIL；e2e 122.9s 通过。详见 task-11-report.md §8。
- 2026-09-09：完成 Task 12。实际修改：新增 `migrations/032_inference_oauth_sessions.sql`（裁决 4：oauth_grants 一次性 state+PKCE+运营身份绑定、session_bindings 粘性会话[同 (session,model) 至多一条 active 部分唯一索引]、credentials.connector 列）、`internal/inference/credentials/{oauth,refresh}.go`（独立社交登录的授权流：state 绑定运营 user+app+10min 有效期、回调单语句原子消费先消费后验身份、token bundle AEAD 落库、撤销本地为准厂商尽力而为；刷新协议：无锁慢路径调厂商 + pg_advisory_xact_lock 跨实例互斥 + 锁内重读 + generation CAS 写，代次已动则收敛丢弃——旧 token 晚返回不得覆盖新 token；invalid_grant 同事务翻转 reauth_required + 终止绑定 + 审计）、`internal/inference/providers/connector/`（厂商中立 client：authorize/exchange/refresh/revoke/models/quota/health + 错误分类 invalid_grant/401→reauth、5xx/传输→retryable；quota 不可得保持未知；ServiceAuth 自托管静态凭据适配器）、`internal/inference/routing/{account_pool,session_binding}.go`（额度耗尽跳过调度/未知可调度/reset 已过恢复/HealthOf 合成视图；绑定 Bind/Resolve[失效不静默换号]/Migrate[同事务终止旧建新]/End）、`internal/inference/workers/{credential_refresh,upstream_health}.go`（到期 skew 扫描轮换、双实例安全；健康探测 401→即时刷新/reauth、可重试→DB 冷却→到期复测恢复、quota observed_at/source/reset_at 单调守卫落缓存）、`internal/inference/httpapi/admin_oauth.go`（authorizations/callback/revoke/refresh/upstream-accounts 五端点挂 credentials:manage）、`docs/runbooks/kaya-coding-plan-connector-evaluation.md`（OOS=OAuth 工作假设显式标注、Sub2API 0.2.3 LGPL-3.0 与 CLIProxyAPI v7.2.155 MIT 九维核对、选型=自实现不外置不内嵌[扣费权威唯一]+差异记录、真实验收待测试账号清单）；修改 domain（Credential.Connector + OAuthGrant/SessionBinding 类型）、postgres（accounts_repo connector 列 + oauth_repo 持久面）、config/cmd/server/.env.example（INFERENCE_OAUTH_CONNECTORS_JSON 注册表与两 worker 旋钮装配）、migrations/README、三个既有测试 wipe 列表 + db_atomic 清理 bug 修复（provider_id 误用 code 文本比较）。验证（一次性 PostgreSQL 16.14，端口 55444 可丢弃库 kaya，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-12-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=29 → applied=0 skipped=29）；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 29 包全 ok 无 FAIL；connector 单测复跑 ok；`go test -race -count=1 ./tests/e2e/` 通过（128.3s）。验收硬项全有真库测试：双实例同时刷新（两连接池两 worker 并发恰 1 rotated+1 converged）、旧 token 晚返回 CAS 拒写、invalid_grant→reauth_required 停分配+绑定终止、限额耗尽调度跳过、撤销传播、连接器不可用只重试、绑定失效不静默换号。遗留问题：OOS 假设待业务方确认（Task 0 复选框保持未勾选）；session_sticky 未接网关调度（建议随 Task 13 接线）；真实上游验收待测试账号；浏览器重定向型公开回调入口未定（当前运营手工提交 code）。
- 2026-09-09：完成 Task 13。实际修改：新增 `migrations/033_inference_response_chains.sql`（Responses 会话链：规范 items transcript + 账户/上游账号双重归属 + 24h TTL，store:false 不落链）、`internal/inference/providers/{client_surface,messages_client,responses_client}.go`（内部 OpenAI 单一 wire 的增量解析泵 + streamRenderer 管线；Messages 2023-06-01 子集与 Responses 子集双向翻译——工具调用 ID 全链路逐字保留、thinking/effort→ThinkingEnabled+新 ThinkingBudget、usage 按 Inclusion 归一化两种协议口径、多模态/服务端工具/background/include/truncation:auto 等显式 400、中断一律原生 error 事件绝不伪造终止标记）、`internal/inference/httpapi/{messages,responses}.go`（原生错误映射：Anthropic 分类学含 529 overloaded / OpenAI 形状；链持久化 detached ctx）、`internal/inference/postgres/response_chain_repo.go`（账户限定读取，跨客户/过期/不存在无差别 404）、`tests/integration/inference_protocols_test.go`（多轮工具 ID 三向[含 Anthropic 双重翻译]、三协议中断序列与结算归类、跨协议账本一致性[3×charge 17 micros 同桶 reported]、拒绝矩阵零痕迹、绑定归属+钉住对抗 round-robin+跨客户 404）、`internal/inference/httpapi/protocol_surfaces_test.go` + providers 两测试文件；修改 `gateway/service.go`（Outcome 增 AccountID/Inclusion；ChatCompletionsSticky/pinSessionCandidates——失效显式 Migrate 不静默换号，瞬态冷却 503 不重绑；SetSessionBinder/BindSession）、`httpapi/apikey_auth.go`（X-Api-Key 凭据头 + messages 路径原生错误形状）、`httpapi/chat_completions.go`（quotaExceededDetails 共享）、`providers/{anthropic,openai_chat,anthropic_stream,stream_usage}.go`（budget 客户端值、top_k/parallel_tool_calls 映射、passthrough 白名单外显式拒绝、cache 桶经扩展键上 wire）、`workers/upstream_health.go`（链 TTL 清扫挂轮次）、router/cmd/server（装配 + 超时跳过两路径）、nginx.conf（两个 SSE location）、OpenAPI/集成指南增量（能力矩阵 + 版本记录 + WebSocket 不做决定）、12 个既有测试 wipe 列表补 033 表、本计划复选框。**M-4（Task 12 遗留）无迁移解法**：刷新扫描 SQL 增 EXISTS 可调度账号过滤，reauth 传播后凭据移出扫描集（`TestReauthPropagatedCredentialLeavesRefreshScanSet` 钉牢三段：传播前在集/传播后移出/账号恢复后回归），不动 025 枚举。验证（一次性 PostgreSQL 16.14，端口 55446 可丢弃库 kaya，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-13-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=30 → applied=0 skipped=30）；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 全绿无 FAIL；`go test -race -count=1 ./tests/e2e/`（E2E_DATABASE_URL 指向一次性库）通过。验收硬项全有真库测试：三协议共用同一闸门（账本分类一致）、多轮工具调用 ID 保留、错误中断流式序列、无法兼容字段明确拒绝、会话绑定归属与跨客户隔离。遗留问题：真实客户端联调待 Task 16 演练或业务方环境（本环境契约级验证基于官方文档/SDK 形状 fixture）；thinking 历史块不回放（原生 Anthropic 交错 thinking+工具连续性不在本期，联调若命中需在 model.ChatMessage 增回放通道）；预存在边缘——两部署共享同一上游账号的故障切换撞租约唯一键（500），运营面一账号一部署规避，归 Task 15 明确；Anthropic 形状 GET /v1/models 不实现（路径冲突，矩阵已声明）。
- 2026-09-09：完成 Task 14。实际修改：新增 `migrations/030_inference_wallet.sql`（030 为 Task 0 预留号，全 IF NOT EXISTS/duplicate_object 门控幂等：wallets[每账户每币种一行、无缓存余额列、套餐外开关+UTC 自然月支出上限、开启必设上限 CHECK]、wallet_entries[追加+冲正借贷分录、cash/bonus 来源隔离、refund 仅 cash+debit CHECK、business_key 唯一、reverses_entry_id 部分唯一索引]、wallet_holds[每请求一条冻结=幂等业务键、赠送先扣拆分、价格版本钉住]、wallet_audits[开关/上限修改审计]、payg_config[单行]；权益来源扩 'payg'、请求行增 charge_source、预占目标扩 'wallet'）、`internal/inference/accounting/{wallet,overage}.go`（分录不变式/赠送先扣拆分/派生余额/冲正配对/十进制→微金额严格换算；支出门控/月界/审计行计算）、`internal/inference/postgres/wallet_repo.go`（固定锁序 账户→钱包；ReserveWallet 派生余额检查+冻结；settle/release 钱包分支；topup/refund/adjustment/reversal；PAYG 配置与权益幂等建立）、`internal/inference/httpapi/{user_wallet,admin_adjustments}.go`（/user/wallet[总览/流水/套餐外开关/PAYG 开启] + /admin/wallet[调整/冲正/只读] + /admin/payg-config，billing:adjust）、`internal/inference/access/wallet_sync.go` + `internal/inference/workers/wallet_sync.go`（wallet.sync outbox 契约与消费 worker：dedup 键+业务键双兜底，重复回调/重投/乱序退款幂等收敛）、测试六件（accounting 纯规则、postgres 真实库[并发双花/调价钉住/恢复保守入账/审计]、gateway 真实库+httptest[默认关不动余额/回退金额结算/PAYG 并发限制]、httpapi 真实 JWT[展示=账本/越权/校验]、workers 真实库、service payment_wallet_db[充值/退款全链路]）；修改 domain（insufficient_balance/SourcePAYG/TargetWallet/WalletCharge/ChargeSource/ReserveWalletCommand）、access.SelectEntitlement（payg 兜底池：显式套餐>赠送>PAYG，Task 6 语义不变）、quota（AdmitWallet/ReserveAmountMoney 同一有界预占口径）、postgres ledger/reservation/request/quota repo（钱包结算/释放分支、锁序、charge_source 读写）、gateway（PAYG 直走钱包+套餐耗尽且策略 allow_overage 且客户已开启时入场回退钱包、金额结算）、payment.go（wallet-topup 商品：下单免权益配置、已支付不激活订阅而同事务入队钱包充值、退款[全额+部分]入队钱包退款）、model.ProductWalletTopup、router/cmd 装配（wallet sync worker 复用 entitlement-sync 旋钮）、OpenAPI 0.12.0 + 契约测试、migrations/README、本计划复选框。验证（一次性 PostgreSQL 16.14，端口 55447 可丢弃库 yunhou_task14/yunhou_task14_e2e，测毕清理；报告见 `.superpowers/sdd/2026-09-08-kaya-coding-plan/task-14-report.md`）：`go vet`/`go build` 通过；`cmd/migrate` 全新库连跑两次幂等（applied=31 → applied=0 skipped=31，含 030）；`go test -race -p 1 -count=1 ./internal/... ./cmd/... ./tests/integration/...` 全绿无 FAIL；`go test -race -count=1 ./tests/e2e/`（E2E_DATABASE_URL 指向一次性库）通过。验收硬项全有真库/网关级测试：10 并发抢 5 份余额恰 5 成功不超扣、未开启超额余额一分不动（上游零调用）、调价 r2 发布后按钉住 r1 结算、充值/赠送/消费/退款/调整派生余额=分录手工汇总、重复回调/退款幂等、bonus 退款 DB CHECK 拒绝、无 PAYG 记录 model_not_allowed/开启后调用+concurrency=1 第二并发 insufficient_capacity。遗留问题（报告关注点节）：月上限为 UTC 自然月口径；超占超出部分只扣现金可为负（同 Task 9 不隐藏负差额）；退款透支现金可为负；钱包路径不占 Key 预算（额度口径 vs 金额口径不混）；钱包调整不写 inference_ledger_entries（钱包账本自洽，统一报表需显式 join）；UsageVerifier 生产无实现维持 Task 9 遗留；sale_money 价目发布走既有运维通道（Task 15 价格预览覆盖）。
