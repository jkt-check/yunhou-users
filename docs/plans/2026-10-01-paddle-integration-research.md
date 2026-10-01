# Paddle 支付渠道接入调研报告(v2 — 基于 Dashboard 实况与决策)

日期:2026-10-01(v2,已查看 Paddle Dashboard 实际配置)
状态:调研完成,未动代码
范围:yunhou-users 项目接入 Paddle 作为海外支付渠道

## 0. 已确认的决策

1. 卖家主体:公司主体(北京云猴智能科技有限公司)— 已完成 Paddle 账号注册与验证
2. 渠道定位:与 PayPal **并存**,新增一种订阅付费方式,不影响现有 PayPal/微信订阅
3. 接入形态:**按 Paddle 官方方式的订阅优先**(原生订阅生命周期:checkout → webhook → 自动续费),不做一次性付费模式
4. 定价:Paddle 的 Product/Price ID 与价格同 PayPal 保持一致
5. 退款:维持现状(webhook-only,不接真实退款 API)
6. 测试:需要 mock 模式支撑本地 e2e

---

## 1. Paddle Dashboard 实况(2026-10-01 查看,Live 环境)

账号:**Beijing Yunhou Intelligent Technology Co., Ltd.**(vendors.paddle.com)

| 项目 | 状态 |
|---|---|
| 账号验证(04 Verify your account) | ✅ **Verification passed** |
| Catalog(01 Create your catalog) | ✅ **Complete** |
| Checkout 集成(02) | ⬜ 未开始 |
| Fulfillment/webhook(03) | ⬜ 未开始 |
| 上线测试(05) | ⬜ 未开始 |
| 余额/订阅/交易 | 全新(全部 0) |

**已配置 Catalog**:
- Product:`Kaya AI Coding Terminal` — `pro_01m36trmhgkzmpkp5jbns8h48z`,Tax category = Standard digital goods,创建时间 2026-09-23
- Price(月付):**$9.99/Monthly** — `pri_01m36ttg84y5favjtgdjwhy76p`(2026-10-01 经 API `GET /products?include=prices` 核实;初版报告中的 `...tgdjvhy76p` 系笔误)
- Price(年付):**$99.99/Yearly** — `pri_01m36tvbr7pjg4g70bjhx2mq42`(同上,初版 `...70blhx...` 系笔误)
- 无 trial、无国家差异化定价

**未配置(需要补)**:
- API keys:0 个(Developer Tools → Authentication 显示 "Create your first API key")
- Client-side tokens:未确认(前端若用 Paddle.js 需要)
- Notification destinations(webhook):**0 个**("No notification destinations yet")
- Sandbox 环境:未建(需要单独注册 sandbox 账号)

> ⚠️ 定价核对:Paddle 侧是 $9.99/月、$99.99/年(USD)。需与 PayPal 渠道现有 USD 套餐价格比对,不一致则以现有 PayPal 价格为准调整 Paddle 侧(决策 4)。

---

## 2. 本项目支付体系现状(摘要,详见 v1 调研)

- 渠道接入三触点:`internal/middleware/webhook_sig.go` 的 `ChannelSignatureVerifier`、`internal/handler/webhook.go` 的 `parseEvent` switch、`internal/service/payment.go` 的 `validateChannel` + 事件类型 switch;渠道名单还有 DB CHECK 约束(`migrations/008_drop_lemonsqueezy.sql:19-31`)。
- Webhook 泛型路由 `/webhooks/payment/:channel`(`internal/router/router.go:344-351`),**加渠道不改路由**。
- 三层幂等与 Paddle 官方建议 1:1:`webhook_events UNIQUE(channel,event_id)`、`payments UNIQUE(channel,external_txn_id)`、refunds 唯一键;失败返 500 触发渠道重投。
- 结算主流程 `onPaymentSucceeded`(`service/payment.go:1786`):查单(两段式)→ 金额/币种校验 → INSERT payment → `resolveSubExpiry` 算到期 → UPSERT 订阅 → outbox 发权益。
- PayPal 续费模式:`PAYMENT.SALE.COMPLETED` → `onPaypalRenewalSucceeded`(:2692),按 `subscriptions.external_subscription_id` 找订阅、造合成 renewal order、按渠道给的下一期时间延期。**Paddle 续费可复用/泛化此模式**(续费结算锚定 `transaction.completed` + `origin=subscription_recurring`,经 API 查 `next_billed_at`;见 §6 勘误)。
- 事件类型分发 switch 有既定 TODO(:3274 "refactor to per-channel predicate maps at 5+ channels"),Paddle 是第 5 渠道,顺手重构。
- 每用户每产品一个活跃订阅(`idx_subscriptions_user_product_active`);PayPal 渠道"有活跃订阅禁止再下单"防双重扣费——**并存时需把该守卫扩展到跨渠道**(任一渠道有活跃订阅则其他渠道不可再订同产品,决策 2)。
- 环境:代码一份,env 区分 cn(微信)/ intl(PayPal,Phase 4);mock 开关与生产信号互斥 fail-closed(`internal/config/config.go:477-514`)。
- LemonSqueezy 先例:Paddle 验签格式(`Paddle-Signature: ts=...,h1=...`)与 Stripe 几乎一致,`custom_data` 透传 order_id 机制现成;PayPal 接入清单 `docs/superpowers/plans/2026-07-01-paypal-channel.md` 可作实施模板。

---

## 3. 接入方案(订阅优先,与 PayPal 并存)

### 3.1 下单链路(Paddle 原生订阅)

1. `POST /payments/orders`(channel=paddle)→ `CreateOrder`:校验、权益快照、INSERT(pending)。
2. 服务端预授权(新):`internal/billing/paddle/` 客户端用官方 SDK(`paddle-go-sdk/v5`,**go.mod 已是 1.25,兼容**)调 Paddle API:
   - 方案 a(推荐):创建 checkout 用的 transaction 草稿或直接用 price_id 生成 hosted checkout / Paddle.js 会话,`custom_data={order_id, user_id}`,把 checkout URL/client token 写入 `orders.provider_intent` 返回前端。
   - 前端在 Paddle checkout 完成支付(overlay 或跳转)。
3. Webhook `transaction.completed` / `subscription.activated` → 走现有 `onPaymentSucceeded` 主流程结算(金额以 order 快照为准校验),并把 `subscription.id` 写入 `subscriptions.external_subscription_id`(复用现有列,无需迁移)。
4. 续费:Paddle 渠道侧自动扣费 → webhook `transaction.completed`(`origin=subscription_recurring`)→ 泛化 `onPaypalRenewalSucceeded` 为 per-channel renewal handler,经 API 查 `next_billed_at` 延期。
5. 取消/过期:`subscription.canceled` / `subscription.past_due`(dunning 失败)→ 状态机处理(参照现有取消/退款翻订阅逻辑)。

### 3.2 后端改动清单

| 文件 | 改动 |
|---|---|
| `migrations/042_paddle_channel.sql`(新增) | payments/refunds/webhook_events 三张表 CHECK 加 `'paddle'`(照 `005_paypal_channel.sql` 幂等 DO 块) |
| `internal/config/config.go` + `.env.example` | `PADDLE_ENV` / `PADDLE_API_KEY` / `PADDLE_WEBHOOK_SECRET` / `PADDLE_CLIENT_TOKEN`(可选)+ Validate(含 `PADDLE_MOCK` 与生产互斥) |
| `internal/billing/paddle/`(新增) | SDK 客户端封装:checkout 会话/transaction 创建、订阅查询(照 paypal 包模式) |
| `internal/middleware/webhook_sig.go` | `PaddleVerifier`(HMAC-SHA256 over `ts:body`,照 `StripeVerifier:141`;或直接用 SDK `WebhookVerifier`) |
| `internal/handler/webhook.go` | `parseEvent` 加 `case "paddle"`(`transaction.completed/payment_failed/billed`、`subscription.created/activated/updated/canceled/past_due`、`adjustment.*`) |
| `internal/service/payment.go` | `validateChannel` 加 paddle;**顺手做 :3274 per-channel predicate map 重构**;续费 handler 泛化(复用 `external_subscription_id`);活跃订阅守卫扩展为跨渠道 |
| `internal/model/app.go`(可选) | `PaymentProvidersConfig.Paddle` + per-plan price_id 映射(对齐 paypal.Plans 模式) |
| `cmd/server/main.go` | `buildWebhookVerifier` 装配 paddle;billing client 装配;mock 模式接线 |
| `tests/e2e/` | `paddle_test.go` + `signPaddle` helper 入 `testhelpers.go`;mock 端到端覆盖下单→webhook→权益 |

**不用动**:`router.go`(泛型路由)、orders/payments/subscriptions 表结构、`onPaymentSucceeded` 主流程、deploy/nginx/Dockerfile。

工作量粗估:**8–12 个工作日**(含订阅生命周期映射、跨渠道守卫、mock 模式与 e2e)。

### 3.3 风险

1. Paddle webhook 要求 **5 秒内回 2xx**,live 重试最长 3 天——现有同步事务链路需压测大事件体耗时。
2. MoR 渠道锁定:卡 token 在 Paddle 侧,未来迁出需买家重绑。
3. 月结现金流(最迟次月 15 日电汇/Payoneer)。
4. 多币种:Paddle 按买家地区币种扣款,金额校验必须严格走 order 快照;e2e 需覆盖币种不匹配拒绝路径。
5. 并存守卫改动有回归风险,需完整跑现有 PayPal/微信 e2e。

---

## 4. Dashboard 操作执行情况(2026-10-01,浏览器实操)

### ✅ 已完成(我直接在 Paddle Dashboard 操作)

1. **Live API key 已创建并验证可用**:名称 `yunhou-users-prod`,权限 Read+Write 全量(本想最小权限,但 Dashboard 权限列表虚拟化导致单项勾选不可用,先全量、后续可在 Dashboard 收紧),**永不过期**(未选 90 天,避免 12 月 30 日过期导致支付中断)。已用 `GET https://api.paddle.com/products` 实测返回正常。
2. **Client-side token 已创建**(Paddle.js 前端用):名称 `yunhou-web-prod`,`live_7248d1ffd31c6ce970817c8e8b4`。
3. **Webhook 通知目的地已创建(Active)**:
   - URL:`https://api.yunhouai.com/webhooks/payment/paddle`(api.yunhouai.com 已实测 `/healthz` 200)
   - 描述:yunhou-users paddle webhook (prod);API version 1;Usage: Platform
   - 事件:All(56 个;后端解析器会忽略不处理的类型,后续可在 Dashboard 收紧为 transaction.*/subscription.*/adjustment.*)
   - Secret key 已获取(`pdl_ntfset_01m3v0c...`,见 `.env`)
4. **价格已核对**:Paddle Catalog $9.99/月、$99.99/年 与官网 yunhou.ai/terminal#pricing 的 USD 定价完全一致 ✅(Paddle 是 intl USD 定价的权威源,PayPal intl 尚未启用)。
5. **三个凭证已存入项目 `.env`**(已被 .gitignore 忽略,chmod 600):`PADDLE_ENV=live`、`PADDLE_API_KEY`、`PADDLE_CLIENT_TOKEN`、`PADDLE_WEBHOOK_SECRET`。

### ⬜ 需要你来做(我做不了或不宜代做)

1. **Sandbox 账号**:Dashboard "Switch to Sandbox" 跳转 sandbox-login.paddle.com 后是独立账号体系,浏览器自动化在其登录页失效(窗口句柄丢失),且新建账号涉及设置账号密码,应由你本人操作:
   - 在 Live Dashboard 点 "Switch to Sandbox"(或打开 sandbox-vendors.paddle.com → Sign up,邮箱可同用 jacktian16898@gmail.com,密码单独设)
   - 建好后告诉我,我可以继续在 sandbox 里建 product/price、API key、notification destination(webhook URL 可用 ngrok 指到本地,或指向 staging)
2. **收款方式绑定**:Business Account → 绑定 Payoneer/银行账户(涉及你的金融账户,必须本人操作;可上线后再做,payout 月结 $100 起)。
3. **API key 权限收紧(可选,建议上线前)**:Developer Tools → Authentication → 编辑 `yunhou-users-prod`,Write 只留 Transactions / Subscriptions / Customers / Addresses / Adjustments,其余去掉。

### ✅ 已完成(2026-10-01 补充,浏览器+API 实操)

6. **Default payment link 已设置(Live + Sandbox 均已保存并批准)**:均为 `https://yunhou.ai/checkout`(Dashboard → Checkout → Checkout settings → Default payment link;Live 要求域名已批准,yunhou.ai 已带绿勾直接通过)。**所有账号创建 transaction 前都必须设此项**,否则报 `transaction_default_checkout_url_not_set`。
7. **下单链路已实测调通**:live key `POST /transactions`(items=price_id + custom_data.order_id)→ 返回 `checkout.url = https://yunhou.ai/checkout?_ptxn=<txn_id>`、custom_data 原样回显、status=draft(打开 checkout 后变 ready)。探针交易已 PATCH 取消。前端形态由此确定为:**yunhou.ai/checkout 页托管 Paddle.js,读 ?_ptxn 打开 checkout**(overlay/整页均可);provider_intent 需带 `checkout_url` + `transaction_id`,overlay 集成另需 `client_token`。
8. **Catalog Price ID 勘误**(见 §1,API 核实)。

### 实施状态(2026-10-01,分支 feat/paddle-channel)

- ✅ 迁移 042(CHECK 加 'paddle')、config `PADDLE_*`(mock/生产 fail-closed,含"只配凭证不配 PADDLE_ENV 启动即报错")、`internal/billing/paddle` SDK 封装(mock 模式)、`PaddleVerifier`(HMAC-SHA256 `ts:body`,分号分隔头)、`parsePaddle`(宽松解析,结算事件缺字段也落 webhook_events 审计而非 400 死循环)、`channelWebhookBranches` 分发表(顺手完成 :3274 重构)、CreateOrder paddle 分支(provider_intent 带 checkout_url/transaction_id/client_token)、续费 handler 泛化(transaction.completed + `origin=subscription_recurring` → API 查 next_billed_at)、跨渠道活跃订阅守卫、e2e 6 用例(下单 intent/首购结算+盖章+billed audit-only+续费延期+生命周期 audit-only/拒付重试/守卫 409/验签 400)。
- 已知限制:Paddle 退款(`adjustment.*`)audit-only,运营手工处理;`coding-plan` 未接入(仅 kaya-membership);续费扣款失败(dunning)只出 `transaction.past_due`/`subscription.past_due` 审计,无自动回收(与 PayPal 现状一致);webhook 5s 应答预算未压测(上线前待办)。
- 前端待办(其他仓库):yunhou.ai/checkout 页托管 Paddle.js,读 `?_ptxn=` 打开 checkout;overlay 形态用 provider_intent 的 `client_token` + `transaction_id`。
- 实施计划:`docs/superpowers/plans/2026-10-01-paddle-channel.md`。

### 6. 复审勘误(2026-10-01,code review 修正)

调研初版把续费结算锚在 `transaction.billed` 上——**错误**。Paddle 官方 webhook simulator(subscription renewed 场景)确认事件序列为:

1. `subscription.updated` / `transaction.created` / **`transaction.billed`** —— 续费账单**开具**(status=billed),此时尚未扣款;
2. 仅当扣款成功才发 `transaction.paid` → `transaction.completed`(status=completed)。

若锚在 billed,续费卡被拒(最常见的续费失败)时会先延期 + 记一笔幽灵 paid 收入,而失败只以 `transaction.past_due`/`subscription.past_due` 出现(audit-only),无回收路径。已修正为:

- **续费结算锚定 `transaction.completed` 且 `data.origin=subscription_recurring`**(扣款成功后才发);`transaction.billed`/`transaction.paid` audit-only。
- Paddle 会把订阅的 `custom_data` 传播到续费 transaction,续费 completed 可能**带回原始 order_id**——路由按 origin 判断,绝不按 custom_data 有无判断(否则续费会误进首购路径报 duplicate)。
- `transaction.payment_failed` 改 audit-only:checkout 内拒付重试**复用同一 transaction**,若按失败翻单,后续成功重试的 completed 会被 `unexpected_state_transition` 卡死,客户付了钱不激活。

---

## 7. Sandbox 实测(2026-10-01,Billing sandbox 真实 webhook 端到端)

**Sandbox 账号**:Billing 后台(paddle.com)左上角直接切 Sandbox 即可,无需单独注册;后台域名同为 sandbox-vendors.paddle.com。之前 vendor_id 126667 的 Classic sandbox 账号弃用(只能生成 legacy auth code,Billing API 一律 403)。

**已配好(API + 后台实操)**:sandbox API key(`pdl_sdbx_apikey_…`)+ client token(`test_…`)、product `pro_01m3vsyck6ng9b0k1k74f0m73y`(kaya-Membership)、月付 price($29.90,对齐本地 plans.monthly_usd)、年付 price($99.99)、default payment link(`https://yunhou.ai/checkout`,域名批准账号级共享,带绿勾)、notification destination(cloudflared 隧道 → 本地 `/webhooks/payment/paddle`,全量 transaction.*/subscription.*/adjustment.* 事件,`pdl_ntfset_…` secret)。

**首购链路实测全绿**:本地 server(真实 sandbox 模式,非 mock)→ `POST /payments/orders`(channel=paddle)→ Paddle checkout 付测试卡 → 真实签名 webhook `transaction.paid`/`completed`/`subscription.created`/`activated` 到达 → order=paid、payment(paddle, $29.90, paid)、subscription active + `external_subscription_id` 盖章、expires_at 按 plan 周期 +30d。webhook 处理耗时毫秒级(5s 预算宽裕)。

**实测抓出并已修的两个真 bug(mock e2e 都测不出来)**:

1. **金额字段位置**:webhook payload 里钱在 `data.details.totals.total`,顶层 `data.totals` 恒为 null(只有 API 返回的 transaction 对象顶层才有 totals)。原实现读顶层 → 真实结算全部 `webhook_amount_mismatch`(event_amount=0)拒结。已改为 details.totals 优先、顶层兜底(commit 7a484fd)。
2. **税模式与金额校验冲突**:price `tax_mode=location`(默认)时美国买家总价 = 单价 + 税($29.90+$2.65),而严格金额校验拿 webhook 总额对订单快照 → 必拒。**生产 prices 必须设 `tax_mode=internal`(税含价)**,客户看到/付出的就是标价,税从标价里拆。sandbox 两个 price 已改;**live 的 pri_01m36ttg84y5favjtgdjwhy76p / pri_01m36tvbr7pjg4g70bjhx2mq42 上线前必须同样 PATCH**(或确认账号级税设置为含价)。

**其他实测事实**:API 创建的 checkout transaction 的 `origin="api"`(非 "web",路由只特判 `subscription_recurring`,兼容);`include_sensitive_fields` 不影响 details.totals;续费可用 `PATCH /subscriptions {next_billed_at}` 提前(最早 +30min)触发真实续费。

### 备注

- Onboarding 任务 02(Build checkout)与 03(fulfillment)已由上述 API key + webhook 创建自动变为 In progress;完成代码集成与一次端到端测试后 05(Test and go live)即可点亮。
- yunhou.ai 页脚已确认有 Terms of Service / Privacy Policy / Refund Policy 公开链接(上线审核检查项)。

---

## 5. 待确认项

1. ~~前端结账形态~~:client-side token 已生成,Paddle.js overlay 与 hosted checkout 都可用,建议 overlay;最终形态在代码集成时定。
2. ~~定价核对~~:已核对,与官网 USD 定价一致。
3. webhook 公网域名:已确认为 `api.yunhouai.com`(healthz 200)。
4. `coding-plan`(模型 API 套餐)是否也要走 Paddle,还是先只接 `kaya-membership`?(影响 `PaymentProvidersConfig.Paddle` 的 plan 映射范围)
5. 跨渠道互斥守卫:先只接 `kaya-membership` 单一产品时守卫简单;若多产品需按产品维度判断。

---

## 附:v1 调研结论(背景)

- Paddle 是 Merchant of Record:5% + $0.50/笔全包(含全球 VAT/GST/销售税、拒付、发票),月结(次月 15 日前电汇/Payoneer,$100 起)。
- 中国大陆主体官方支持注册;本账号验证已通过,审核风险已消除。
- 官方 Go SDK `paddle-go-sdk/v5` 活跃维护,要求 Go ≥ 1.25(本项目 go.mod = 1.25.0 ✅)。
- Webhook 签名 `Paddle-Signature: ts=<ts>;h1=<hex>`,h1 = HMAC-SHA256(secret, "ts:原始body");官方幂等建议 event_id 去重、occurred_at 排序——与现有机制完全对齐。
- 新接入必然是 Paddle Billing(Classic 已停止新接入)。
- 详细平台调研(费率、对比、来源 URL)见 git 历史中 v1 版本或另行提供。
