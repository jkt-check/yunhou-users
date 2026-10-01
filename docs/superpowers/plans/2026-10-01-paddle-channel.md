# Paddle 支付渠道接入实施计划(kaya-membership 订阅)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 yunhou-users 接入 Paddle Billing 作为第 5 个支付渠道,支持 kaya-membership 月付/年付订阅(与 PayPal 并存),覆盖下单 → Paddle checkout → webhook 结算 → 渠道侧自动续费全生命周期。

**Architecture:** 复用现有渠道三触点架构(验签中间件 `ChannelSignatureVerifier` / handler `parseEvent` / service 分发+结算),不改路由、不改表结构。下单走服务端创建 Paddle checkout transaction(`custom_data.order_id` 绑定订单),provider_intent 返回 checkout_url + transaction_id + client_token;结算复用 `onPaymentSucceeded` 主流程;续费泛化 `onPaypalRenewalSucceeded`,Paddle 续费按 `transaction.billed` 触发、经 API 查 `next_billed_at` 延期。幂等完全复用 webhook_events / payments 唯一键。

**Tech Stack:** Go 1.25、官方 SDK `github.com/PaddleHQ/paddle-go-sdk/v5`、Gin、PostgreSQL(迁移 042)、现有 e2e 套件(`make e2e`)。

**Spec:** `docs/plans/2026-10-01-paddle-integration-research.md`(§3 方案、§4 Dashboard 实况、已实测验证)。

## Global Constraints

- 渠道名固定为 `"paddle"`;事件类型原样使用 Paddle Billing 字符串(`transaction.completed` 等,小写点分)。
- Paddle 金额在报文中是**次要单位字符串**(`totals.total`,如 `"999"` = $9.99),解析时必须 `/100` 转主单位。
- Webhook 签名头:`Paddle-Signature: ts=<unix>;h1=<hex>`(**分号**分隔,轮换期多个 `h1=`);signed payload = `ts + ":" + rawBody`,HMAC-SHA256,hex。
- 下单创建 transaction 时必须显式传 `currency_code = order.Currency`(锁币种,否则 Paddle 按买家地区币种扣款,金额校验必挂)。
- 活跃订阅守卫:paypal/paddle 渠道侧自动续费,同产品已有未过期 active 订阅时拒绝新单(409,`ErrUserHasActiveSub`),trial 豁免。
- mock 开关 `PADDLE_MOCK=1` 只允许非生产 APP_ENV,且不得与 `PADDLE_ENV=live` 或真实 `PADDLE_API_KEY` 共存(fail-closed,与 WECHAT_PAY_MOCK 同款语义)。
- Paddle Price ID 权威值(2026-10-01 经 API 核实,调研报告初版有笔误已勘误):
  - 月付 $9.99 `pri_01m36ttg84y5favjtgdjwhy76p`
  - 年付 $99.99 `pri_01m36tvbr7pjg4g70bjhx2mq42`
- Live catalog Product:`pro_01m36trmhgkzmpkp5jbns8h48z`(Kaya AI Coding Terminal)。
- 退款维持现状:不接 Paddle 退款 API,`adjustment.*` webhook 事件 audit-only(ack 200,不动作),运营手工处理。
- 所有现有渠道行为不得变化(PayPal/微信 e2e 必须原样通过);审计 action 名参数化后 paypal 渠道字符串必须与现状逐字一致。

## 已验证的 Paddle 事实(写代码时以此为准)

- `POST /transactions` `{items:[{price_id,quantity:1}], custom_data:{order_id}, currency_code}` → `data.checkout.url = <default payment link>?_ptxn=<txn_id>`;custom_data 原样回显;status=draft。
- **所有账号必须先设 Default payment link**(Dashboard → Checkout → Checkout settings),否则报 `transaction_default_checkout_url_not_set`。Live/Sandbox 均已设为 `https://yunhou.ai/checkout`(yunhou.ai 已批准)。
- webhook 事件包络统一:`{event_id, event_type, occurred_at, data}`;`data` 即 transaction/subscription 对象。
- transaction 对象关键字段:`id`(txn_)、`subscription_id`(订阅首购即有)、`custom_data`、`currency_code`、`totals.total`(次要单位字符串)。
- subscription 对象关键字段:`id`(sub_)、`next_billed_at`(RFC3339)、`status`。
- SDK:`paddle.New(apiKey)` / `paddle.NewSandbox(apiKey)` → `*SDK`;`SDK.CreateTransaction(ctx, *paddle.CreateTransactionRequest) (*paddle.Transaction, error)`;`SDK.GetSubscription(ctx, *paddle.GetSubscriptionRequest{SubscriptionID}) (*paddle.Subscription, error)`,`Subscription.NextBilledAt *string`。
- SDK 创建请求:`paddle.CreateTransactionRequest{Items: []paddle.CreateTransactionItems{*paddle.NewCreateTransactionItemsTransactionItemFromCatalog(&paddle.TransactionItemFromCatalog{Quantity:1, PriceID:...})}, CustomData: paddle.CustomData{...}, CurrencyCode: paddle.PtrTo(paddle.CurrencyCode("USD"))}`;响应 `Transaction.Checkout *paddle.TransactionCheckout`,`Checkout.URL *string`。

---

### Task 1: 迁移 042 — CHECK 约束加 'paddle'

**Files:**
- Create: `migrations/042_paddle_channel.sql`
- Test: 无(幂等 DO 块,`make migrate` + e2e 覆盖)

**Interfaces:**
- Produces: DB 接受 `channel='paddle'` 的 payments/refunds/webhook_events 行。后续所有任务依赖。

- [ ] **Step 1: 写迁移文件**(照 `migrations/005_paypal_channel.sql` + `008_drop_lemonsqueezy.sql` 的幂等 DO 块模式;当前约束列表以 008 为准:`('stripe','wechat_pay','alipay','paypal')` 加 `'paddle'`)

```sql
-- Migration: 042_paddle_channel
-- Description: extend payments/refunds/webhook_events CHECK constraints to allow channel='paddle'.
-- 照 005/008 的 DO 块幂等模式(deploy.sh 会无差别重放;PG 不支持原位修改 CHECK,DROP + ADD)。
DO $$
BEGIN
    ALTER TABLE payments DROP CONSTRAINT payments_channel_check;
EXCEPTION
    WHEN undefined_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE payments ADD CONSTRAINT payments_channel_check
        CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal', 'paddle'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE refunds DROP CONSTRAINT refunds_channel_check;
EXCEPTION
    WHEN undefined_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE refunds ADD CONSTRAINT refunds_channel_check
        CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal', 'paddle'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    ALTER TABLE webhook_events DROP CONSTRAINT webhook_events_channel_check;
EXCEPTION
    WHEN undefined_object THEN NULL;
END $$;
DO $$
BEGIN
    ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_channel_check
        CHECK (channel IN ('stripe', 'wechat_pay', 'alipay', 'paypal', 'paddle'));
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;
```

- [ ] **Step 2: 应用并验证**

Run: `make migrate && make migrate-status`
Expected: `042_paddle_channel.sql` 出现在 applied 列表;重复执行 `make migrate` 不报错(幂等)。

- [ ] **Step 3: Commit**

```bash
git add migrations/042_paddle_channel.sql
git commit -m "feat(migrations): 042 allow channel='paddle' in payments/refunds/webhook_events"
```

---

### Task 2: config — PADDLE_* 环境变量 + Validate

**Files:**
- Modify: `internal/config/config.go`(struct :120-145 区域、Load :306-316 区域、Validate :487-520 区域)
- Modify: `.env.example`(PAYPAL 块 :66-74 之后)
- Test: `internal/config/config_test.go`(追加)

**Interfaces:**
- Produces:
  - `Config.PaddleEnv string` // "" | "sandbox" | "live"
  - `Config.PaddleAPIKey string`
  - `Config.PaddleWebhookSecret string`
  - `Config.PaddleClientToken string`
  - `Config.PaddleMock bool`
  - `Config.PaddlePricesJSON string`(raw)与 `Config.PaddlePrices map[string]string`(Validate 解析填充)
  - 校验错误(新增):PADDLE_ENV 非法、PADDLE_MOCK 与生产信号共存、API key/webhook secret/price 映射缺失或非法。

- [ ] **Step 1: 写失败测试**(`internal/config/config_test.go` 追加)

```go
func TestValidate_PaddleEnvInvalid(t *testing.T) {
	c := validTestConfig()
	c.PaddleEnv = "staging"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PADDLE_ENV") {
		t.Fatalf("expected PADDLE_ENV error, got %v", err)
	}
}

func TestValidate_PaddleMockProductionGate(t *testing.T) {
	c := validTestConfig()
	c.AppEnv = "prod"
	c.PaddleMock = true
	if err := c.Validate(); err == nil {
		t.Fatal("expected PADDLE_MOCK + APP_ENV=prod to fail")
	}
}

func TestValidate_PaddleMockWithLiveEnv(t *testing.T) {
	c := validTestConfig()
	c.AppEnv = "staging"
	c.PaddleMock = true
	c.PaddleEnv = "live"
	if err := c.Validate(); err == nil {
		t.Fatal("expected PADDLE_MOCK + PADDLE_ENV=live to fail")
	}
}

func TestValidate_PaddleMockWithRealKey(t *testing.T) {
	c := validTestConfig()
	c.AppEnv = "staging"
	c.PaddleMock = true
	c.PaddleAPIKey = "pdl_live_apikey_xxx"
	if err := c.Validate(); err == nil {
		t.Fatal("expected PADDLE_MOCK + PADDLE_API_KEY to fail")
	}
}

func TestValidate_PaddleRealModeRequiresTuple(t *testing.T) {
	c := validTestConfig()
	c.AppEnv = "staging"
	c.PaddleEnv = "live"
	// API key missing
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PADDLE_API_KEY") {
		t.Fatalf("expected PADDLE_API_KEY required, got %v", err)
	}
	c.PaddleAPIKey = "pdl_live_apikey_01m3v04av1tyfjjnx6s9cnssg5_xxx"
	// webhook secret missing
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PADDLE_WEBHOOK_SECRET") {
		t.Fatalf("expected PADDLE_WEBHOOK_SECRET required, got %v", err)
	}
	c.PaddleWebhookSecret = "pdl_ntfset_xxx"
	// price map missing
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PADDLE_PRICES_JSON") {
		t.Fatalf("expected PADDLE_PRICES_JSON required, got %v", err)
	}
}

func TestValidate_PaddlePricesParse(t *testing.T) {
	c := validTestConfig()
	c.AppEnv = "staging"
	c.PaddleEnv = "live"
	c.PaddleAPIKey = "pdl_live_apikey_01m3v04av1tyfjjnx6s9cnssg5_xxx"
	c.PaddleWebhookSecret = "pdl_ntfset_xxx"
	c.PaddlePricesJSON = `{"monthly_usd":"pri_01m36ttg84y5favjtgdjwhy76p","yearly_usd":"pri_01m36tvbr7pjg4g70bjhx2mq42"}`
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if c.PaddlePrices["monthly_usd"] != "pri_01m36ttg84y5favjtgdjwhy76p" {
		t.Fatalf("price map not populated: %v", c.PaddlePrices)
	}
}
```

注意:若 `config_test.go` 没有现成的 `validTestConfig()` 助手,先在其中构造一个返回最小合法 Config 的 helper(参照该文件现有 Validate 测试的构造方式)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestValidate_Paddle -v`
Expected: FAIL(字段不存在,编译错误即视为失败)

- [ ] **Step 3: 实现 config**

`Config` struct 在 `PaypalClientSecret`(:144)之后追加:

```go
	// Paddle Billing:sandbox + live 共用一套变量,PaddleEnv 选择环境。
	// 空 PaddleEnv = 渠道未启用(webhook 404、下单拒绝)。PADDLE_MOCK=1
	// 时全部凭据可空(仅 header 存在性 + 时间窗校验,客户端返回 canned 值)。
	PaddleEnv              string // "" | "sandbox" | "live"
	PaddleAPIKey           string
	PaddleWebhookSecret    string
	PaddleClientToken      string // Paddle.js client-side token(可安全下发 BFF,provider_intent 用)
	PaddleMock             bool
	PaddlePricesJSON       string            // raw env;Validate 解析进 PaddlePrices
	PaddlePrices           map[string]string // plan_id → Paddle price_id(pri_...)
```

`Load()` 在 `PaypalClientSecret` 行(:316)之后追加:

```go
		PaddleEnv:           envOr("PADDLE_ENV", ""),
		PaddleAPIKey:        os.Getenv("PADDLE_API_KEY"),
		PaddleWebhookSecret: os.Getenv("PADDLE_WEBHOOK_SECRET"),
		PaddleClientToken:   os.Getenv("PADDLE_CLIENT_TOKEN"),
		PaddleMock:          os.Getenv("PADDLE_MOCK") == "1",
		PaddlePricesJSON:    os.Getenv("PADDLE_PRICES_JSON"),
```

`Validate()` 在 PayPal 专属 mock 门(`c.PaypalEnv == "live"` 块 :491-501)之后追加:

```go
	// Paddle Billing:PaddleEnv 为空 = 渠道未启用,全部校验跳过。
	switch c.PaddleEnv {
	case "", "sandbox", "live":
	default:
		return fmt.Errorf("PADDLE_ENV must be empty, sandbox, or live, got %q", c.PaddleEnv)
	}
	if c.PaddleMock {
		if IsProductionEnv(c.AppEnv) {
			return errors.New("PADDLE_MOCK must not be enabled when APP_ENV is production (set APP_ENV to a non-production value like dev/staging to use mock switches)")
		}
		if c.PaddleEnv == "live" {
			return errors.New("PADDLE_MOCK must not be enabled when PADDLE_ENV=live")
		}
		if c.PaddleAPIKey != "" {
			return errors.New("PADDLE_MOCK must not be enabled when real PADDLE_API_KEY is configured")
		}
	}
	if c.PaddleEnv != "" && !c.PaddleMock {
		if c.PaddleAPIKey == "" {
			return errors.New("PADDLE_API_KEY is required when PADDLE_ENV is set (or enable PADDLE_MOCK for dev/e2e)")
		}
		if c.PaddleWebhookSecret == "" {
			return errors.New("PADDLE_WEBHOOK_SECRET is required when PADDLE_ENV is set")
		}
		if c.PaddlePricesJSON == "" {
			return errors.New("PADDLE_PRICES_JSON is required when PADDLE_ENV is set (plan_id → price_id map)")
		}
	}
	if c.PaddlePricesJSON != "" {
		m := map[string]string{}
		if err := json.Unmarshal([]byte(c.PaddlePricesJSON), &m); err != nil {
			return fmt.Errorf("PADDLE_PRICES_JSON must be a JSON object of plan_id → price_id: %v", err)
		}
		for planID, priceID := range m {
			if planID == "" || !strings.HasPrefix(priceID, "pri_") {
				return fmt.Errorf("PADDLE_PRICES_JSON entry %q must map to a price_id (pri_...), got %q", planID, priceID)
			}
		}
		if c.PaddleEnv != "" && !c.PaddleMock && len(m) == 0 {
			return errors.New("PADDLE_PRICES_JSON must not be empty when PADDLE_ENV is set")
		}
		c.PaddlePrices = m
	}
```

(config.go 需引入 `encoding/json`。)

`.env.example` 在 PAYPAL_API_BASE_LIVE 行(:74)之后追加:

```bash
# Paddle Billing(PADDLE_ENV 为空 = 未启用;sandbox|live 二选一)
# 与 PayPal 并存,仅承接 kaya-membership 订阅。真实模式四件套必填;
# PADDLE_MOCK=1 时全部可空(仅非生产 APP_ENV 允许)。
PADDLE_ENV=
PADDLE_API_KEY=
PADDLE_WEBHOOK_SECRET=
PADDLE_CLIENT_TOKEN=
PADDLE_PRICES_JSON={"monthly_usd":"pri_01m36ttg84y5favjtgdjwhy76p","yearly_usd":"pri_01m36tvbr7pjg4g70bjhx2mq42"}
PADDLE_MOCK=
```

- [ ] **Step 4: 跑测试确认通过 + 全量 config 测试**

Run: `go test ./internal/config/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go .env.example
git commit -m "feat(config): PADDLE_* env vars with mock/production fail-closed validation"
```

---

### Task 3: internal/billing/paddle — SDK 客户端封装 + mock

**Files:**
- Create: `internal/billing/paddle/client.go`
- Create: `internal/billing/paddle/client_test.go`
- Modify: `go.mod` / `go.sum`(`go get github.com/PaddleHQ/paddle-go-sdk/v5`)

**Interfaces:**
- Consumes: 无(独立包)。
- Produces:
  - `type Client struct`(字段:`SDK *paddle.SDK`、`MockMode bool`;私有 `clientToken string`,方法 `SetClientToken(string)` / `ClientToken() string`)
  - `var ErrNotConfigured = errors.New("paddle not configured on this deployment")`
  - `func NewClient(apiKey, env string) (*Client, error)` // env: "sandbox"|"live"
  - `type CheckoutTransaction struct { TransactionID string; CheckoutURL string }`
  - `func (c *Client) CreateCheckoutTransaction(ctx context.Context, priceID string, customData map[string]any, currency string) (*CheckoutTransaction, error)`
  - `func (c *Client) GetSubscriptionNextBilledAt(ctx context.Context, subscriptionID string) (*time.Time, error)`
  - Mock 形态:`CreateCheckoutTransaction` 返回 `txn_mock_<uuid>` + `https://checkout.paddle.com/mock?_ptxn=<id>`;`GetSubscriptionNextBilledAt` 返回 `now()+1month`(UTC)。

- [ ] **Step 1: 加依赖**

Run: `go get github.com/PaddleHQ/paddle-go-sdk/v5 && go mod tidy`
Expected: go.mod 新增 `github.com/PaddleHQ/paddle-go-sdk/v5 v5.x.x`

- [ ] **Step 2: 写失败测试**(`internal/billing/paddle/client_test.go`)

```go
package paddle

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewClient_UnknownEnv(t *testing.T) {
	if _, err := NewClient("key", "staging"); err == nil {
		t.Fatal("expected error for unknown env")
	}
}

func TestMock_CreateCheckoutTransaction(t *testing.T) {
	c := &Client{MockMode: true}
	res, err := c.CreateCheckoutTransaction(context.Background(), "pri_x", map[string]any{"order_id": "o1"}, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.TransactionID, "txn_mock_") {
		t.Fatalf("unexpected txn id %q", res.TransactionID)
	}
	if !strings.Contains(res.CheckoutURL, "_ptxn="+res.TransactionID) {
		t.Fatalf("checkout url missing _ptxn: %q", res.CheckoutURL)
	}
}

func TestMock_GetSubscriptionNextBilledAt(t *testing.T) {
	c := &Client{MockMode: true}
	at, err := c.GetSubscriptionNextBilledAt(context.Background(), "sub_x")
	if err != nil {
		t.Fatal(err)
	}
	if at == nil || !at.After(time.Now()) {
		t.Fatalf("expected future next_billed_at, got %v", at)
	}
}
```

- [ ] **Step 3: 跑测试确认失败**(编译错误:Client/CreateCheckoutTransaction 未定义)

Run: `go test ./internal/billing/paddle/ -v`
Expected: FAIL(编译错误)

- [ ] **Step 4: 实现 client.go**

```go
// Package paddle provides Paddle Billing-specific billing primitives:
// checkout-transaction creation at order time and subscription state reads
// at renewal time. Real mode wraps the official Go SDK; MockMode serves
// canned responses so dev/e2e suites can drive the full flow without a
// Paddle account.
package paddle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	paddle "github.com/PaddleHQ/paddle-go-sdk/v5"
)

// ErrNotConfigured is returned by NewClient for an unknown environment.
var ErrNotConfigured = errors.New("paddle not configured on this deployment")

// Client wraps the Paddle Billing SDK for the two operations
// yunhou-users needs. See docs/plans/2026-10-01-paddle-integration-research.md.
type Client struct {
	SDK       *paddle.SDK
	MockMode  bool
	clientToken string
}

func (c *Client) SetClientToken(tok string) { c.clientToken = tok }
func (c *Client) ClientToken() string       { return c.clientToken }

// NewClient builds a real-mode client. env selects the API base:
// "sandbox" → sandbox-api.paddle.com, "live" → api.paddle.com.
func NewClient(apiKey, env string) (*Client, error) {
	var (
		sdk *paddle.SDK
		err error
	)
	switch env {
	case "sandbox":
		sdk, err = paddle.NewSandbox(apiKey)
	case "live":
		sdk, err = paddle.New(apiKey)
	default:
		return nil, fmt.Errorf("%w: unknown PADDLE_ENV %q", ErrNotConfigured, env)
	}
	if err != nil {
		return nil, fmt.Errorf("paddle new client: %w", err)
	}
	return &Client{SDK: sdk}, nil
}

// CheckoutTransaction is the order-time artifact: the Paddle transaction
// id and its hosted checkout URL (default payment link + ?_ptxn=<id>).
type CheckoutTransaction struct {
	TransactionID string
	CheckoutURL   string
}

// CreateCheckoutTransaction creates an automatically-collected Paddle
// transaction for a catalog price. custom_data rides the transaction and
// is echoed verbatim on transaction.* webhooks — the order binding anchor.
// currency pins the charge currency: without it Paddle bills buyers in
// their local currency and the order-snapshot amount check would reject
// genuine payments.
func (c *Client) CreateCheckoutTransaction(ctx context.Context, priceID string, customData map[string]any, currency string) (*CheckoutTransaction, error) {
	if c.MockMode {
		id := "txn_mock_" + uuid.NewString()
		return &CheckoutTransaction{
			TransactionID: id,
			CheckoutURL:   "https://checkout.paddle.com/mock?_ptxn=" + id,
		}, nil
	}
	if c.SDK == nil {
		return nil, errors.New("paddle client: SDK not wired")
	}
	items := []paddle.CreateTransactionItems{
		*paddle.NewCreateTransactionItemsTransactionItemFromCatalog(&paddle.TransactionItemFromCatalog{
			Quantity: 1,
			PriceID:  priceID,
		}),
	}
	req := &paddle.CreateTransactionRequest{
		Items:      items,
		CustomData: paddle.CustomData(customData),
	}
	if currency != "" {
		req.CurrencyCode = paddle.PtrTo(paddle.CurrencyCode(currency))
	}
	res, err := c.SDK.CreateTransaction(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("paddle create transaction: %w", err)
	}
	if res.Checkout == nil || res.Checkout.URL == nil || *res.Checkout.URL == "" {
		return nil, errors.New("paddle create transaction: response missing checkout.url (set the account default payment link in the Paddle dashboard)")
	}
	return &CheckoutTransaction{TransactionID: res.ID, CheckoutURL: *res.Checkout.URL}, nil
}

// GetSubscriptionNextBilledAt returns the subscription's next_billed_at.
// transaction.billed webhooks don't carry the next billing date (it lives
// on the subscription object), so the renewal path resolves it here.
// nil = subscription has no next billing date (e.g. canceled).
func (c *Client) GetSubscriptionNextBilledAt(ctx context.Context, subscriptionID string) (*time.Time, error) {
	if c.MockMode {
		t := time.Now().AddDate(0, 1, 0).UTC()
		return &t, nil
	}
	if c.SDK == nil {
		return nil, errors.New("paddle client: SDK not wired")
	}
	res, err := c.SDK.GetSubscription(ctx, &paddle.GetSubscriptionRequest{SubscriptionID: subscriptionID})
	if err != nil {
		return nil, fmt.Errorf("paddle get subscription: %w", err)
	}
	if res.NextBilledAt == nil || *res.NextBilledAt == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *res.NextBilledAt)
	if err != nil {
		return nil, fmt.Errorf("paddle next_billed_at %q: %w", *res.NextBilledAt, err)
	}
	return &t, nil
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/billing/paddle/ -v && go build ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/billing/paddle/ go.mod go.sum
git commit -m "feat(billing/paddle): SDK client wrapper with mock mode"
```

---

### Task 4: webhook_sig.go — PaddleVerifier + MultiChannelVerifier 接线

**Files:**
- Modify: `internal/middleware/webhook_sig.go`(Stripe 段之后 :220 前插入;MultiChannelVerifier :744-771)
- Test: `internal/middleware/webhook_sig_test.go`(追加)

**Interfaces:**
- Produces:
  - `type PaddleVerifier struct { Secret []byte; ReplayWindow time.Duration; MockMode bool }`,实现 `ChannelSignatureVerifier`。
  - `parsePaddleSignatureHeader(h string) (ts int64, hmacs [][]byte, err error)`(包内私有)。
  - `MultiChannelVerifier` 新增字段 `Paddle ChannelSignatureVerifier`,`case "paddle"` 分发。
- Consumes: Task 2 无依赖;仅依赖中间件既有约定(sentinel errors)。

- [ ] **Step 1: 写失败测试**(`internal/middleware/webhook_sig_test.go` 追加,与 StripeVerifier 测试同风格)

```go
func signPaddle(secret string, body []byte, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d:%s", ts, body)))
	return fmt.Sprintf("ts=%d;h1=%x", ts, mac.Sum(nil))
}

func TestPaddleVerifier_OK(t *testing.T) {
	v := &PaddleVerifier{Secret: []byte("whsec_test")}
	body := []byte(`{"event_id":"evt_1"}`)
	hdr := signPaddle("whsec_test", body, time.Now().Unix())
	if err := v.VerifySignature("paddle", body, map[string]string{"Paddle-Signature": hdr}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestPaddleVerifier_BadSignature(t *testing.T) {
	v := &PaddleVerifier{Secret: []byte("whsec_test")}
	body := []byte(`{"event_id":"evt_1"}`)
	hdr := signPaddle("whsec_OTHER", body, time.Now().Unix())
	if err := v.VerifySignature("paddle", body, map[string]string{"Paddle-Signature": hdr}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestPaddleVerifier_MissingHeader(t *testing.T) {
	v := &PaddleVerifier{Secret: []byte("whsec_test")}
	if err := v.VerifySignature("paddle", []byte(`{}`), map[string]string{}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestPaddleVerifier_StaleTimestamp(t *testing.T) {
	v := &PaddleVerifier{Secret: []byte("whsec_test")}
	body := []byte(`{}`)
	hdr := signPaddle("whsec_test", body, time.Now().Add(-10*time.Minute).Unix())
	if err := v.VerifySignature("paddle", body, map[string]string{"Paddle-Signature": hdr}); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("expected ErrTimestampOutOfRange, got %v", err)
	}
}

func TestPaddleVerifier_MockModeHeaderPresence(t *testing.T) {
	v := &PaddleVerifier{MockMode: true}
	body := []byte(`{}`)
	hdr := signPaddle("any", body, time.Now().Unix())
	if err := v.VerifySignature("paddle", body, map[string]string{"Paddle-Signature": hdr}); err != nil {
		t.Fatalf("mock mode should accept, got %v", err)
	}
	// mock 仍要求 header 存在
	if err := v.VerifySignature("paddle", body, map[string]string{}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("mock mode must still require the header, got %v", err)
	}
}

func TestMultiChannelVerifier_PaddleRouting(t *testing.T) {
	mv := &MultiChannelVerifier{Paddle: &PaddleVerifier{Secret: []byte("s")}}
	body := []byte(`{}`)
	if err := mv.VerifySignature("paddle", body, map[string]string{"Paddle-Signature": "ts=1;h1=aa"}); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("expected paddle routing + timestamp rejection, got %v", err)
	}
	if err := mv.VerifySignature("paddle", body, map[string]string{}); !errors.Is(err, ErrUnsupportedChannel) && !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("unexpected error for nil-secret paddle: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**(编译错误:PaddleVerifier 未定义)

Run: `go test ./internal/middleware/ -run Paddle -v`
Expected: FAIL(编译错误)

- [ ] **Step 3: 实现 PaddleVerifier**(插在 webhook_sig.go 的 Stripe 段与 WeChat 段之间)

```go
// ============================================================================
// Paddle Billing — HMAC-SHA256 over `ts:body`, replay window enforced
// ============================================================================

// PaddleVerifier verifies Paddle Billing webhooks. Paddle signs the raw
// body with HMAC-SHA256 keyed on the notification destination secret:
//
//	signed payload = "<ts>:" + rawBody        (timestamp + colon + body)
//	Paddle-Signature: ts=<unix>;h1=<hex>      (SEMICOLON-separated kv pairs)
//
// Multiple h1= values may be present during secret rotation — accept any
// that matches (same scheme as StripeVerifier's multi-v1 handling).
type PaddleVerifier struct {
	Secret       []byte
	ReplayWindow time.Duration // default 5 min if zero
	// MockMode (PADDLE_MOCK=1) bypasses the HMAC match while still
	// requiring the Paddle-Signature header and an in-window timestamp.
	// Production MUST leave this false.
	MockMode bool
}

func (v *PaddleVerifier) VerifySignature(channel string, body []byte, headers map[string]string) error {
	// MultiChannelVerifier already routes by channel before calling us;
	// the per-channel channel-name guard is defensive scaffolding.
	_ = channel
	sigHeader := headers["Paddle-Signature"]
	if sigHeader == "" {
		return ErrInvalidSignature
	}
	ts, expectedHMACs, err := parsePaddleSignatureHeader(sigHeader)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	window := v.ReplayWindow
	if window == 0 {
		window = 5 * time.Minute
	}
	if delta := time.Since(time.Unix(ts, 0)); delta > window || delta < -window {
		return ErrTimestampOutOfRange
	}
	if v.MockMode {
		return nil
	}
	mac := hmac.New(sha256.New, v.Secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte(":"))
	mac.Write(body)
	sum := mac.Sum(nil)
	for _, expected := range expectedHMACs {
		if hmac.Equal(expected, sum) {
			return nil
		}
	}
	return ErrInvalidSignature
}

// parsePaddleSignatureHeader parses `ts=<unix>;h1=<hex>[;h1=<hex>...]`.
// Paddle uses SEMICOLON separators (Stripe uses commas) and may send more
// than one h1 during secret rotation.
func parsePaddleSignatureHeader(h string) (int64, [][]byte, error) {
	var ts int64
	var sigHexes []string
	for _, kv := range strings.Split(h, ";") {
		kv = strings.TrimSpace(kv)
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		switch kv[:eq] {
		case "ts":
			n, err := strconv.ParseInt(kv[eq+1:], 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("bad timestamp")
			}
			ts = n
		case "h1":
			sigHexes = append(sigHexes, kv[eq+1:])
		}
	}
	if ts == 0 || len(sigHexes) == 0 {
		return 0, nil, fmt.Errorf("missing ts or h1")
	}
	decoded := make([][]byte, 0, len(sigHexes))
	for _, sh := range sigHexes {
		b, err := hex.DecodeString(sh)
		if err != nil {
			return 0, nil, fmt.Errorf("bad hex: %w", err)
		}
		decoded = append(decoded, b)
	}
	return ts, decoded, nil
}
```

`MultiChannelVerifier` struct 与 switch 追加:

```go
type MultiChannelVerifier struct {
	Stripe ChannelSignatureVerifier
	WeChat ChannelSignatureVerifier
	Alipay ChannelSignatureVerifier
	Paypal ChannelSignatureVerifier
	Paddle ChannelSignatureVerifier
}
```

```go
	case "paddle":
		v = m.Paddle
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/middleware/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/middleware/webhook_sig.go internal/middleware/webhook_sig_test.go
git commit -m "feat(middleware): PaddleVerifier (HMAC-SHA256 ts:body) + multichannel routing"
```

---

### Task 5: handler — parsePaddle

**Files:**
- Modify: `internal/handler/webhook.go`(`parseEvent` switch :113-126 加 case;parsePaypal 之后加 parsePaddle 与 isPaddle 辅助)
- Test: `internal/handler/webhook_test.go`(追加)

**Interfaces:**
- Consumes: `service.WebhookEvent` 现有字段(Channel/EventID/EventType/TransactionID/OrderID/Amount/Currency/ExternalSubscriptionID/SubExpiresAt)。
- Produces: `(*WebhookHandler).parsePaddle(raw []byte) (*service.WebhookEvent, error)`;`parseEvent` 支持 `case "paddle"`。
- 事件映射(与 Task 6 的分发表逐字一致):
  - `transaction.completed` → OrderID(必填,custom_data.order_id)、TransactionID、Amount/100、Currency、ExternalSubscriptionID(data.subscription_id)
  - `transaction.billed` → TransactionID、Amount/100、Currency、ExternalSubscriptionID(必填,空则报错)
  - `transaction.payment_failed` → TransactionID、ExternalSubscriptionID、Amount=0
  - `subscription.*` → ExternalSubscriptionID(data.id)、SubExpiresAt(data.next_billed_at)

- [ ] **Step 1: 写失败测试**(`internal/handler/webhook_test.go` 追加)

```go
func TestParsePaddle_TransactionCompleted(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_01m3x",
	  "event_type": "transaction.completed",
	  "occurred_at": "2026-10-01T08:00:00Z",
	  "data": {
	    "id": "txn_01m3x",
	    "status": "completed",
	    "subscription_id": "sub_01m3x",
	    "currency_code": "USD",
	    "custom_data": {"order_id": "order-uuid-1"},
	    "totals": {"total": "999", "subtotal": "999", "tax": "0", "grand_total": "999"}
	  }
	}`)
	we, err := h.parsePaddle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if we.Channel != "paddle" || we.EventID != "evt_01m3x" || we.EventType != "transaction.completed" {
		t.Fatalf("bad envelope: %+v", we)
	}
	if we.OrderID != "order-uuid-1" || we.TransactionID != "txn_01m3x" {
		t.Fatalf("bad ids: %+v", we)
	}
	if we.Amount != 9.99 || we.Currency != "USD" {
		t.Fatalf("amount/currency: %v %s", we.Amount, we.Currency)
	}
	if we.ExternalSubscriptionID != "sub_01m3x" {
		t.Fatalf("external sub id: %q", we.ExternalSubscriptionID)
	}
}

func TestParsePaddle_TransactionBilled(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_01m3y",
	  "event_type": "transaction.billed",
	  "data": {
	    "id": "txn_01m3y",
	    "subscription_id": "sub_01m3x",
	    "currency_code": "USD",
	    "totals": {"total": "999"}
	  }
	}`)
	we, err := h.parsePaddle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if we.Amount != 9.99 || we.ExternalSubscriptionID != "sub_01m3x" {
		t.Fatalf("bad renewal event: %+v", we)
	}
}

func TestParsePaddle_SubscriptionUpdated(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_01m3z",
	  "event_type": "subscription.updated",
	  "data": {
	    "id": "sub_01m3x",
	    "status": "active",
	    "next_billed_at": "2026-11-01T00:00:00Z"
	  }
	}`)
	we, err := h.parsePaddle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if we.ExternalSubscriptionID != "sub_01m3x" {
		t.Fatalf("bad sub id: %+v", we)
	}
	if we.SubExpiresAt == nil || we.SubExpiresAt.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
		t.Fatalf("bad next_billed_at: %v", we.SubExpiresAt)
	}
}

func TestParsePaddle_CompletedMissingOrderID(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{"event_id":"evt_1","event_type":"transaction.completed","data":{"id":"txn_1","currency_code":"USD","totals":{"total":"999"}}}`)
	if _, err := h.parsePaddle(raw); err == nil {
		t.Fatal("expected error for missing custom_data.order_id")
	}
}

func TestParseEvent_RoutesPaddle(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{"event_id":"evt_1","event_type":"transaction.payment_failed","data":{"id":"txn_1","subscription_id":"sub_1"}}`)
	we, err := h.parseEvent("paddle", raw)
	if err != nil {
		t.Fatal(err)
	}
	if we.Channel != "paddle" || we.TransactionID != "txn_1" {
		t.Fatalf("bad route: %+v", we)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**(编译错误:parsePaddle 未定义)

Run: `go test ./internal/handler/ -run Paddle -v`
Expected: FAIL(编译错误)

- [ ] **Step 3: 实现 parsePaddle**(webhook.go,parsePaypal 辅助函数之后、`wrapRawPayload` 之前)

`parseEvent` switch 追加:

```go
	case "paddle":
		return h.parsePaddle(raw)
```

parsePaddle 本体:

```go
// parsePaddle extracts fields from a Paddle Billing webhook. Paddle's
// event envelope is uniform across event types:
//
//	{
//	  "event_id":    "evt_...",
//	  "event_type":  "transaction.completed" | "transaction.billed" | ...,
//	  "occurred_at": "...",
//	  "data":        { ...transaction or subscription object... }
//	}
//
// transaction.* data: id (txn_...), subscription_id, custom_data,
// currency_code, totals.total — the total is a MINOR-unit string
// ("999" = $9.99), normalize to major units like the Stripe cents path.
// subscription.* data: id (sub_...), next_billed_at (RFC3339), status.
// subscription.* events carry no transaction: they ride the audit-only
// default branch in the service (renewals are driven by
// transaction.billed), we only lift the identifiers for visibility.
func (h *WebhookHandler) parsePaddle(raw []byte) (*service.WebhookEvent, error) {
	var evt struct {
		EventID   string          `json:"event_id"`
		EventType string          `json:"event_type"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("paddle body: %w", err)
	}
	if evt.EventID == "" || evt.EventType == "" {
		return nil, fmt.Errorf("paddle missing event_id or event_type")
	}

	we := &service.WebhookEvent{
		Channel:   "paddle",
		EventID:   evt.EventID,
		EventType: evt.EventType,
	}

	switch {
	case strings.HasPrefix(evt.EventType, "transaction."):
		var txn struct {
			ID             string         `json:"id"`
			SubscriptionID string         `json:"subscription_id"`
			CurrencyCode   string         `json:"currency_code"`
			CustomData     map[string]any `json:"custom_data"`
			Totals         struct {
				Total string `json:"total"` // minor units, e.g. "999"
			} `json:"totals"`
		}
		if err := json.Unmarshal(evt.Data, &txn); err != nil {
			return nil, fmt.Errorf("paddle transaction data: %w", err)
		}
		if txn.ID == "" {
			// data.id is the channel-side transaction id; empty would
			// collapse payments.(channel, external_txn_id) dedupe.
			return nil, fmt.Errorf("paddle missing data.id")
		}
		we.TransactionID = txn.ID
		we.Currency = strings.ToUpper(txn.CurrencyCode)
		we.ExternalSubscriptionID = txn.SubscriptionID
		if orderID, _ := txn.CustomData["order_id"].(string); orderID != "" {
			we.OrderID = orderID
		}

		switch evt.EventType {
		case "transaction.completed":
			// Initial settlement: order binding + amount are mandatory.
			if we.OrderID == "" {
				return nil, fmt.Errorf("paddle missing custom_data.order_id for %s", evt.EventType)
			}
			if txn.Totals.Total == "" {
				return nil, fmt.Errorf("paddle missing totals.total")
			}
			v, err := strconv.ParseFloat(txn.Totals.Total, 64)
			if err != nil {
				return nil, fmt.Errorf("paddle totals.total %q: %w", txn.Totals.Total, err)
			}
			we.Amount = v / 100 // minor units → major units
			if we.Currency == "" {
				return nil, fmt.Errorf("paddle missing currency_code for %s", evt.EventType)
			}
		case "transaction.billed":
			// Renewal settlement: same strictness — money moved.
			if txn.Totals.Total == "" {
				return nil, fmt.Errorf("paddle missing totals.total for %s", evt.EventType)
			}
			v, err := strconv.ParseFloat(txn.Totals.Total, 64)
			if err != nil {
				return nil, fmt.Errorf("paddle totals.total %q: %w", txn.Totals.Total, err)
			}
			we.Amount = v / 100
			if we.Currency == "" {
				return nil, fmt.Errorf("paddle missing currency_code for %s", evt.EventType)
			}
			if we.ExternalSubscriptionID == "" {
				// The renewal handler finds the subscription by this id;
				// without it the charge can never be matched.
				return nil, fmt.Errorf("paddle missing data.subscription_id for %s", evt.EventType)
			}
		case "transaction.payment_failed":
			// No settlement: amount stays 0; the failed branch keys on
			// (channel, external_txn_id) and never INSERTs an amount.
		}

	case strings.HasPrefix(evt.EventType, "subscription."):
		var sub struct {
			ID           string `json:"id"`
			NextBilledAt string `json:"next_billed_at"`
		}
		if err := json.Unmarshal(evt.Data, &sub); err != nil {
			return nil, fmt.Errorf("paddle subscription data: %w", err)
		}
		if sub.ID == "" {
			return nil, fmt.Errorf("paddle missing data.id")
		}
		we.ExternalSubscriptionID = sub.ID
		if sub.NextBilledAt != "" {
			if t, err := time.Parse(time.RFC3339, sub.NextBilledAt); err == nil {
				we.SubExpiresAt = &t
			} else {
				log.Printf("paddle: invalid next_billed_at %q for event %s: %v", sub.NextBilledAt, evt.EventID, err)
			}
		}
	}
	return we, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/handler/ -run Paddle -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/handler/webhook.go internal/handler/webhook_test.go
git commit -m "feat(handler): parsePaddle for transaction.*/subscription.* events"
```

---

### Task 6: service — 渠道名单、分发重构、下单、续费泛化

**Files:**
- Modify: `internal/service/payment.go`(:30-36 wechatClient 旁、:94-102 channelRequiredCurrency、:537-745 CreateOrder、:1616-1751 WebhookEvent/OnWebhook、:2692-2946 onPaypalRenewalSucceeded、:3118-3342 validateChannel/predicates)
- Test: `internal/service/payment_unit_extra_test.go` 或新建 `internal/service/payment_paddle_test.go`

**Interfaces:**
- Consumes: Task 3 的 `billing/paddle` 类型(`*paddle.CheckoutTransaction`)。
- Produces:
  - `var ErrPaddleNotConfigured = errors.New("paddle not configured on this deployment")`
  - `var ErrPaddlePriceNotConfigured = errors.New("paddle price not configured for this plan")`
  - `type paddleClient interface { IsMockMode() bool; ClientToken() string; CreateCheckoutTransaction(ctx, priceID string, customData map[string]any, currency string) (*paddle.CheckoutTransaction, error); GetSubscriptionNextBilledAt(ctx, subscriptionID string) (*time.Time, error) }`
  - `func (s *PaymentService) SetPaddleClient(c paddleClient)` / `SetPaddlePrices(map[string]string)`
  - `func channelAutoRenews(channel string) bool` // paypal、paddle → true
  - OnWebhook 分发改经 `channelWebhookBranches` 表(paypal 各事件→分支映射与现状逐字一致)。
  - `onPaypalRenewalSucceeded` 改名 `onRenewalSucceeded`,审计 action 名按 e.Channel 参数化(paypal 渠道字符串与现状逐字一致:`fmt.Sprintf("%s_renewal_unknown_subscription", e.Channel)` 等)。

- [ ] **Step 1: 写失败测试**(`internal/service/payment_paddle_test.go` 新建)

```go
package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	billingpaddle "github.com/yunhou/users/internal/billing/paddle"
)

type stubPaddle struct {
	mockMode bool
	txnID    string
	checkout string
	next     *time.Time
}

func (s *stubPaddle) IsMockMode() bool { return s.mockMode }
func (s *stubPaddle) ClientToken() string { return "test_xxx" }
func (s *stubPaddle) CreateCheckoutTransaction(_ context.Context, priceID string, _ map[string]any, _ string) (*billingpaddle.CheckoutTransaction, error) {
	return &billingpaddle.CheckoutTransaction{TransactionID: s.txnID, CheckoutURL: s.checkout}, nil
}
func (s *stubPaddle) GetSubscriptionNextBilledAt(_ context.Context, _ string) (*time.Time, error) {
	return s.next, nil
}

func TestValidateChannel_Paddle(t *testing.T) {
	if err := validateChannel("paddle"); err != nil {
		t.Fatalf("paddle must be a valid channel: %v", err)
	}
}

func TestChannelAutoRenews(t *testing.T) {
	for _, ch := range []string{"paypal", "paddle"} {
		if !channelAutoRenews(ch) {
			t.Fatalf("%s should auto-renew", ch)
		}
	}
	if channelAutoRenews("wechat_pay") || channelAutoRenews("stripe") {
		t.Fatal("stripe/wechat must not auto-renew")
	}
}

func TestDispatchBranch_Paddle(t *testing.T) {
	if got := dispatchBranch("paddle", "transaction.completed"); got != branchPaymentSuccess {
		t.Fatalf("transaction.completed → %v", got)
	}
	if got := dispatchBranch("paddle", "transaction.billed"); got != branchRenewal {
		t.Fatalf("transaction.billed → %v", got)
	}
	if got := dispatchBranch("paddle", "transaction.payment_failed"); got != branchPaymentFailed {
		t.Fatalf("transaction.payment_failed → %v", got)
	}
	if got := dispatchBranch("paddle", "subscription.canceled"); got != branchNone {
		t.Fatalf("subscription.canceled must be audit-only, got %v", got)
	}
	// paypal 现有映射不可变
	if got := dispatchBranch("paypal", "PAYMENT.SALE.COMPLETED"); got != branchRenewal {
		t.Fatalf("paypal renewal mapping changed: %v", got)
	}
	if got := dispatchBranch("paypal", "BILLING.SUBSCRIPTION.ACTIVATED"); got != branchPaymentSuccess {
		t.Fatalf("paypal activated mapping changed: %v", got)
	}
}

func TestProviderPreAuth_PaddleRequiresClient(t *testing.T) {
	s := &PaymentService{}
	if err := s.providerPreAuth("paddle"); !errors.Is(err, ErrPaddleNotConfigured) {
		t.Fatalf("expected ErrPaddleNotConfigured, got %v", err)
	}
	s.SetPaddleClient(&stubPaddle{})
	if err := s.providerPreAuth("paddle"); err != nil {
		t.Fatalf("expected nil with client wired, got %v", err)
	}
}
```

(注:`newPaymentServiceForCreateOrder` 之类现有测试助手若已存在,CreateOrder 的 paddle 分支测试复用之;否则参照 `payment_unit_extra_test.go` 里 `TestCreateOrder_WeChat_Real_PersistsIntent` 的构造方式写 `TestCreateOrder_Paddle_PersistsIntent`:wire stubPaddle + SetPaddlePrices,断言 provider_intent 的 `checkout_url`/`transaction_id`/`client_token`;再写 `TestCreateOrder_Paddle_MissingPrice`:price 未配置时返回 `ErrPaddlePriceNotConfigured`。)

- [ ] **Step 2: 跑测试确认失败**(编译错误)

Run: `go test ./internal/service/ -run "Paddle|ChannelAutoRenews|DispatchBranch" -v`
Expected: FAIL(编译错误)

- [ ] **Step 3: 实现 service 改动**

3a. `payment.go` 头部(:36 后)追加:

```go
// paddleClient is the narrow Paddle Billing surface PaymentService needs.
// Production wires *billing/paddle.Client; tests/e2e inject stubs. nil =
// paddle not accepted on this deployment.
type paddleClient interface {
	IsMockMode() bool
	ClientToken() string
	CreateCheckoutTransaction(ctx context.Context, priceID string, customData map[string]any, currency string) (*billingpaddle.CheckoutTransaction, error)
	GetSubscriptionNextBilledAt(ctx context.Context, subscriptionID string) (*time.Time, error)
}

// ErrPaddleNotConfigured is returned by CreateOrder when channel="paddle"
// but no paddle client is wired (same shape as ErrWechatPayNotConfigured).
var ErrPaddleNotConfigured = errors.New("paddle not configured on this deployment")

// ErrPaddlePriceNotConfigured is returned by CreateOrder when the operator
// hasn't mapped the plan to a Paddle price_id in PADDLE_PRICES_JSON.
var ErrPaddlePriceNotConfigured = errors.New("paddle price not configured for this plan")

// channelAutoRenews reports whether the channel bills the buyer
// automatically on the channel side (subscriptions). For those channels an
// active subscription must block new orders — each new order would mint a
// fresh channel-side subscription and double-charge (intl-staging 2026-08-17
// PayPal incident). WeChat has no auto-renewal: manual renewal with
// rollover is correct there.
func channelAutoRenews(channel string) bool {
	return channel == "paypal" || channel == "paddle"
}
```

import 增加 `billingpaddle "github.com/yunhou/users/internal/billing/paddle"`。

3b. `PaymentService` struct(:137 wechat 字段旁)追加:

```go
	// paddle is optional; nil deployments refuse paddle orders at
	// providerPreAuth, mirroring the wechat client contract.
	paddle       paddleClient
	paddlePrices map[string]string // plan_id → Paddle price_id (PADDLE_PRICES_JSON)
```

setter(放在 SetBenefitSync :180 之后):

```go
// SetPaddleClient wires the Paddle Billing client (Task: paddle channel).
// Passing a typed-nil *paddle.Client is a wiring bug — only call this when
// a client was actually constructed (cmd/server gates on cfg).
func (s *PaymentService) SetPaddleClient(c paddleClient) { s.paddle = c }

// SetPaddlePrices wires the plan_id → price_id map parsed from
// PADDLE_PRICES_JSON.
func (s *PaymentService) SetPaddlePrices(m map[string]string) { s.paddlePrices = m }
```

3c. `channelRequiredCurrency`(:99-102)加 `"paddle": "USD"`(intl 订阅计划以 USD 为准;下单时再显式把 currency 传给 Paddle 锁币种)。

3d. `providerPreAuth`(:3139-3150)加分支:

```go
	if channel == "paddle" {
		if s.paddle == nil {
			return ErrPaddleNotConfigured
		}
	}
```

3e. CreateOrder 活跃订阅守卫(:627):把

```go
			if channel == "paypal" && existing.PlanID != "trial" {
```

改为

```go
			if channelAutoRenews(channel) && existing.PlanID != "trial" {
```

并把该处注释中 "PayPal 订阅制与 WeChat 的根本差异" 段落补一句 paddle 同理(渠道侧自动续费,签名/延期机制不同但双重扣费风险相同)。

3f. CreateOrder 在 wechat 块(:679-742)之后、`return order, nil` 之前加 paddle 块:

```go
	// Paddle Billing: mint a checkout transaction server-side so the
	// order binding (custom_data.order_id) and the charge currency are
	// pinned by us, not by the frontend. provider_intent carries the
	// hosted checkout URL (default payment link + ?_ptxn=<id>), the
	// transaction id, and the Paddle.js client-side token for overlay
	// checkouts. The BFF either redirects to checkout_url or opens
	// Paddle.js with transaction_id + client_token.
	if channel == "paddle" {
		priceID := s.paddlePrices[planID]
		if priceID == "" {
			// Order row already exists; returning it with the error
			// mirrors the wechat UnifiedOrder failure shape (caller may
			// cancel/retry; the sweeper expires it otherwise).
			return order, ErrPaddlePriceNotConfigured
		}
		res, err := s.paddle.CreateCheckoutTransaction(ctx, priceID,
			map[string]any{"order_id": order.ID}, order.Currency)
		if err != nil {
			return order, fmt.Errorf("paddle checkout transaction: %w", err)
		}
		intentBytes, _ := json.Marshal(map[string]string{
			"transaction_id": res.TransactionID,
			"checkout_url":   res.CheckoutURL,
			"client_token":   s.paddle.ClientToken(),
		})
		if err := s.orderRepo.UpdateProviderIntent(ctx, order.ID, intentBytes); err != nil {
			return order, fmt.Errorf("persist provider intent: %w", err)
		}
		intent := json.RawMessage(intentBytes)
		order.ProviderIntent = &intent
	}
```

3g. 分发重构(:3274-3342 整块替换)。删除 `isPaymentSuccess` / `isPaymentFailed` / `isRefundEvent` / `isRefundFailedEvent` / `isDisputeCreated` / `isDisputeClosed` / `isPaypalRenewal` 七个函数,替换为:

```go
// webhookBranch identifies the domain action a webhook event maps to.
type webhookBranch int

const (
	branchNone webhookBranch = iota
	branchPaymentSuccess
	branchPaymentFailed
	branchRefund
	branchRefundFailed
	branchDisputeCreated
	branchDisputeClosed
	branchRenewal
)

// domainAction is the OnWebhookResult.DomainAction value for a branch.
func (b webhookBranch) domainAction() string {
	switch b {
	case branchPaymentSuccess, branchRenewal:
		return "payment_paid"
	case branchPaymentFailed:
		return "payment_failed"
	case branchRefund:
		return "refund_paid"
	case branchRefundFailed:
		return "refund_failed"
	case branchDisputeCreated:
		return "payment_disputed"
	case branchDisputeClosed:
		return "payment_dispute_closed"
	default:
		return "none"
	}
}

// channelWebhookBranches is the per-channel dispatch table (the :3274
// TODO refactor — paddle is the 5th channel). Event-type strings are
// globally unique across channels, but scoping each channel's vocabulary
// to its own map keeps the table auditable per channel.
var channelWebhookBranches = map[string]map[string]webhookBranch{
	"stripe": {
		"payment_intent.succeeded":       branchPaymentSuccess,
		"payment_intent.payment_failed":  branchPaymentFailed,
		"payment_intent.canceled":        branchPaymentFailed,
		"charge.refunded":                branchRefund,
		"charge.dispute.created":         branchDisputeCreated,
		"charge.dispute.closed":          branchDisputeClosed,
	},
	"wechat_pay": {
		"TRANSACTION.SUCCESS":  branchPaymentSuccess,
		"TRANSACTION.PAY_FAILED": branchPaymentFailed,
		"TRANSACTION.REVOKED":  branchPaymentFailed,
		"TRANSACTION.REFUND":   branchRefund,
		"REFUND.SUCCESS":       branchRefund,
		"REFUND.ABNORMAL":      branchRefundFailed,
		"REFUND.CLOSED":        branchRefundFailed,
	},
	"alipay": {
		"TRADE_SUCCESS":     branchPaymentSuccess,
		"trade_status_sync": branchPaymentSuccess,
		"trade_closed":      branchRefund,
		"trade_refund":      branchRefund,
	},
	"paypal": {
		// ACTIVATED is the subscription activation trigger: its resource
		// carries custom_id (the order UUID the BFF set at creation),
		// status=ACTIVE (the buyer actually approved) and
		// billing_info.next_billing_time (expiry hint). CREATED fires
		// pre-approval with status=APPROVAL_PENDING and must NOT activate.
		"PAYMENT.CAPTURE.COMPLETED":      branchPaymentSuccess,
		"BILLING.SUBSCRIPTION.ACTIVATED": branchPaymentSuccess,
		"PAYMENT.CAPTURE.DENIED":         branchPaymentFailed,
		"PAYMENT.CAPTURE.FAILED":         branchPaymentFailed,
		"PAYMENT.CAPTURE.REFUNDED":       branchRefund,
		"PAYMENT.SALE.REFUNDED":          branchRefund,
		"PAYMENT.SALE.COMPLETED":         branchRenewal,
	},
	"paddle": {
		// Initial subscription settlement (checkout completed).
		"transaction.completed": branchPaymentSuccess,
		// Renewal charge fired by Paddle's channel-side auto-billing.
		"transaction.billed": branchRenewal,
		// Renewal failure (dunning). subscription.created/activated/
		// updated/canceled/past_due and adjustment.* stay audit-only:
		// settlement is anchored on transaction.* money events only.
		"transaction.payment_failed": branchPaymentFailed,
	},
}

// dispatchBranch resolves (channel, event_type) → branch. Unknown channels
// and unknown event types land on branchNone (audit-only ack 200).
func dispatchBranch(channel, eventType string) webhookBranch {
	if m, ok := channelWebhookBranches[channel]; ok {
		if b, ok := m[eventType]; ok {
			return b
		}
	}
	return branchNone
}
```

`OnWebhook`(:1696-1741)的 switch 替换为:

```go
	switch branch := dispatchBranch(e.Channel, e.EventType); branch {
	case branchPaymentSuccess:
		domainAction = branch.domainAction()
		if err := s.onPaymentSucceeded(ctx, e); err != nil {
			return nil, err
		}
	case branchRenewal:
		domainAction = branch.domainAction()
		if err := s.onRenewalSucceeded(ctx, e); err != nil {
			return nil, err
		}
	case branchPaymentFailed:
		domainAction = branch.domainAction()
		if err := s.onPaymentFailed(ctx, e); err != nil {
			return nil, err
		}
	case branchRefund:
		domainAction = branch.domainAction()
		if err := s.onRefundSucceeded(ctx, e); err != nil {
			return nil, err
		}
	case branchRefundFailed:
		domainAction = branch.domainAction()
		if err := s.onRefundFailed(ctx, e); err != nil {
			return nil, err
		}
	case branchDisputeCreated:
		domainAction = branch.domainAction()
		if err := s.onDisputeCreated(ctx, e); err != nil {
			return nil, err
		}
	case branchDisputeClosed:
		// v1 only reacts when the merchant wins (clear disputed=true).
		// Loss path is handled via the chargeback's charge.refunded event
		// — see webhook doc §7.
		domainAction = branch.domainAction()
		if err := s.onDisputeClosed(ctx, e); err != nil {
			return nil, err
		}
	default:
		domainAction = branch.domainAction()
	}
```

注意:`domainAction` 变量在 switch 前保持声明;`branch.domainAction()` 对 branchNone 返回 "none",与现状一致。

同时检查并更新引用旧函数的注释(webhook.go :665-675 isPaypalSubscriptionEvent 注释提到 "The renewal predicate `isPaypalRenewal` lives in service/payment.go because it's part of the OnWebhook dispatch table" — 改为指向 channelWebhookBranches)。

3h. 续费泛化:`onPaypalRenewalSucceeded`(:2692)改名 `onRenewalSucceeded`,函数体内:
- 所有审计 action 字面量改为 `fmt.Sprintf("%s_renewal_...", e.Channel, ...)` 形式。对照表(paypal 渠道输出必须与现状逐字相同):
  - `"paypal_renewal_missing_external_sub_id"` → `fmt.Sprintf("%s_renewal_missing_external_sub_id", e.Channel)`
  - `"paypal_renewal_unknown_subscription"` → 同上模式
  - `"paypal_renewal_payment_already_exists"` → 同上
  - `"paypal_renewal_plan_lookup_failed"` → 同上
  - `"paypal_renewal_currency_mismatch"` → 同上
  - `"paypal_renewal_amount_below_plan"` → 同上
  - `"paypal_renewal_sub_not_active"` → 同上
  - `"paypal_renewal_no_expiry_hint"` → 同上
  - `"paypal_subscription_renewed"` → `fmt.Sprintf("%s_subscription_renewed", e.Channel)`
- 到期解析:函数开头(external sub id 校验之后、BEGIN tx 之前)加:

```go
	// Resolve the post-renewal expiry. PayPal ships next_billing_time in
	// the webhook; Paddle's transaction.billed does not — the hint lives
	// on the subscription object, so fetch it through the billing client.
	// A fetch failure is transient (500): Paddle retries per its schedule.
	nextBilled := e.SubExpiresAt
	if nextBilled == nil && e.Channel == "paddle" {
		if s.paddle == nil {
			return errors.New("paddle renewal: paddle client not wired")
		}
		t, err := s.paddle.GetSubscriptionNextBilledAt(ctx, e.ExternalSubscriptionID)
		if err != nil {
			return fmt.Errorf("paddle renewal fetch next_billed_at: %w", err)
		}
		nextBilled = t
	}
```

  函数体内后续 `e.SubExpiresAt` 的两处使用(orderExpiresAt 选择 :2774、UPDATE subscriptions :2867 与 no_expiry_hint 分支 :2893)全部改用 `nextBilled`。PAYPAL 行为不变(其 webhook 已带 hint,`nextBilled == e.SubExpiresAt`)。

- [ ] **Step 4: 跑测试确认通过 + 既有单测不回归**

Run: `go test ./internal/service/ ./internal/handler/ ./internal/middleware/ && go build ./...`
Expected: PASS(若 dispatch 表漏掉旧 switch 中的某个事件字符串,相关旧测试会红——对照 :3276-3342 原列表逐一核对补齐,尤其 alipay 的 trade_pending 不存在于旧 isPaymentSuccess——trade_pending 走 default,表中没有即正确)

- [ ] **Step 5: Commit**

```bash
git add internal/service/
git commit -m "feat(service): paddle channel — order pre-auth, dispatch table, generalized renewal"
```

---

### Task 7: main.go 装配

**Files:**
- Modify: `cmd/server/main.go`(wechatClient 块 :100-114 之后;paymentSvc 构造 :163-171 之后;buildWebhookVerifier :664-737)
- Test: `go build ./...` + `make test`

**Interfaces:**
- Consumes: Task 2(config 字段)、Task 3(paddle.NewClient)、Task 4(middleware.PaddleVerifier)、Task 6(SetPaddleClient/SetPaddlePrices)。
- Produces: 运行时装配;PADDLE_ENV 非法时启动响亮失败。

- [ ] **Step 1: 实现装配**

main.go 在 wechatClient else 块(:113)之后加:

```go
	// Paddle Billing client. Mock mode (PADDLE_MOCK=1) needs no
	// credentials and serves canned responses; real mode requires
	// PADDLE_API_KEY + PADDLE_ENV (validated in config.Validate).
	var paddleClient *paddle.Client
	if cfg.PaddleMock {
		paddleClient = &paddle.Client{MockMode: true}
	} else if cfg.PaddleEnv != "" {
		pc, err := paddle.NewClient(cfg.PaddleAPIKey, cfg.PaddleEnv)
		if err != nil {
			log.Fatalf("paddle: %v", err)
		}
		pc.SetClientToken(cfg.PaddleClientToken)
		paddleClient = pc
	}
```

paymentSvc 构造(:171)之后加(注意 typed-nil 陷阱:interface 里装 nil *Client 非 nil,必须 gate):

```go
	if paddleClient != nil {
		paymentSvc.SetPaddleClient(paddleClient)
	}
	if len(cfg.PaddlePrices) > 0 {
		paymentSvc.SetPaddlePrices(cfg.PaddlePrices)
	}
```

buildWebhookVerifier 在 PayPal 块(:707-735)之后加:

```go
	if cfg.PaddleWebhookSecret != "" || cfg.PaddleMock {
		// 真实模式凭 webhook secret 验签;mock 模式只查 header 存在性
		// + 时间窗(与 WeChat mock 同款语义)。任一满足即接线,否则
		// paddle 渠道 404(未启用)。
		mv.Paddle = &middleware.PaddleVerifier{
			Secret:   []byte(cfg.PaddleWebhookSecret),
			MockMode: cfg.PaddleMock,
		}
	}
```

import 增加 `"github.com/yunhou/users/internal/billing/paddle"`(注意与 SDK 包名冲突:main.go 里 SDK 不出现,直接 `paddle` 即可;若有冲突则别名 `billingpaddle`)。

- [ ] **Step 2: 构建 + 全量单测**

Run: `go build ./... && make test`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add cmd/server/main.go
git commit -m "feat(server): wire paddle client, prices, and webhook verifier"
```

---

### Task 8: handler 错误映射(ErrPaddleNotConfigured / ErrPaddlePriceNotConfigured → 4xx)

**Files:**
- Modify: `internal/handler/payment.go`(找 `ErrWechatPayNotConfigured` 的 errors.Is 映射处)
- Test: `internal/handler/payment_test.go` 或 payment_error_edges_test.go(追加,参照现有 ErrWechatPayNotConfigured 的用例)

**Interfaces:**
- Consumes: Task 6 的两个 sentinel。
- Produces: 下单返回与 wechat 未配置同款 4xx(422 或现有映射的状态码,以现有代码为准)。

- [ ] **Step 1: 找到现有映射并写失败测试**

Run: `grep -n "ErrWechatPayNotConfigured" internal/handler/*.go`
照抄其用例结构,把 channel 换成 paddle、错误换成两个新 sentinel。

- [ ] **Step 2: 实现映射**(在现有 ErrWechatPayNotConfigured 分支处并列加两个 errors.Is 分支,状态码与 wechat 未配置一致)

- [ ] **Step 3: 测试 + Commit**

```bash
go test ./internal/handler/ -run Paddle && git add internal/handler/ && git commit -m "feat(handler): map paddle not-configured errors to 4xx"
```

---

### Task 9: e2e — paddle_test.go + testhelpers 接线

**Files:**
- Modify: `tests/e2e/testhelpers.go`(e2ePaddleSecret 常量;setupE2EServerWithVerifierOpts :693-716 mv 构造处;:666-674 paymentSvc 构造后)
- Create: `tests/e2e/paddle_test.go`

**Interfaces:**
- Consumes: Task 4(PaddleVerifier)、Task 6(SetPaddleClient/SetPaddlePrices)。
- Produces:
  - `func paddleSignatureHeaders(secret string, body []byte) map[string]string`(testhelpers,与 signWeChat 同风格)
  - `func paddleTransactionBody(eventID, eventType, txnID, subID, orderID, total, currency string) []byte`(paddle_test.go 内)
  - e2e 用例:下单 intent、首购结算、续费延期、缺 header 400、活跃订阅守卫 409。

- [ ] **Step 1: testhelpers 接线**

`setupE2EServerWithVerifierOpts` 中,paymentSvc 构造(:666-674)之后加:

```go
	paymentSvc.SetPaddleClient(&billingpaddle.Client{MockMode: true})
	paymentSvc.SetPaddlePrices(map[string]string{
		"monthly_usd": "pri_e2e_monthly_usd",
		"yearly_usd":  "pri_e2e_yearly_usd",
	})
```

mv 构造(:693-716)加:

```go
		Paddle: &middleware.PaddleVerifier{Secret: []byte(e2ePaddleSecret)},
```

常量区(e2eStripeSecret 定义处附近)加:

```go
	e2ePaddleSecret = "e2e-paddle-webhook-secret"
```

签名 helper(e2eWeChatSign 风格)加:

```go
// paddleSignatureHeaders builds a real Paddle-Signature header
// (ts=<unix>;h1=HMAC-SHA256(secret, "ts:"+body)) so the e2e suite drives
// the paddle channel through the production verifier.
func paddleSignatureHeaders(secret string, body []byte) map[string]string {
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d:%s", ts, body)
	return map[string]string{
		"Paddle-Signature": fmt.Sprintf("ts=%d;h1=%x", ts, mac.Sum(nil)),
	}
}
```

import 增加 `billingpaddle "github.com/yunhou/users/internal/billing/paddle"`。

- [ ] **Step 2: 写 e2e 用例**(`tests/e2e/paddle_test.go`)

```go
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// paddleTransactionBody builds a Paddle webhook envelope. total is in
// minor units ("999" = $9.99), mirroring the real payload shape.
func paddleTransactionBody(eventID, eventType, txnID, subID, orderID, total, currency string) []byte {
	custom := ""
	if orderID != "" {
		custom = `"custom_data":{"order_id":"` + orderID + `"},`
	}
	totals := ""
	if total != "" {
		totals = `"totals":{"total":"` + total + `","subtotal":"` + total + `","tax":"0","grand_total":"` + total + `"},`
	}
	return []byte(`{
	  "event_id": "` + eventID + `",
	  "event_type": "` + eventType + `",
	  "occurred_at": "` + time.Now().UTC().Format(time.RFC3339) + `",
	  "data": {
	    "id": "` + txnID + `",
	    "status": "completed",
	    "subscription_id": "` + subID + `",
	    "currency_code": "` + currency + `",
	    ` + custom + totals + `
	    "origin": "web"
	  }
	}`)
}

func TestE2E_Paddle_CreateOrder_ProviderIntent(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-intent", "yundian").AccessToken

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID             string          `json:"id"`
			ProviderIntent json.RawMessage `json:"provider_intent"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	var intent map[string]string
	if err := json.Unmarshal(r.Data.ProviderIntent, &intent); err != nil {
		t.Fatalf("provider_intent: %v", err)
	}
	if !strings.HasPrefix(intent["transaction_id"], "txn_mock_") {
		t.Fatalf("transaction_id: %v", intent)
	}
	if !strings.Contains(intent["checkout_url"], "_ptxn=") {
		t.Fatalf("checkout_url: %v", intent)
	}
	if intent["client_token"] == "" {
		t.Fatalf("client_token missing: %v", intent)
	}
}

func TestE2E_Paddle_TransactionCompleted_HappyPath(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-happy", "yundian").AccessToken

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &r)
	orderID := r.Data.ID

	subID := "sub_e2e_" + uuid.NewString()
	body := paddleTransactionBody(
		"evt-e2e-paddle-"+uuid.NewString(),
		"transaction.completed",
		"txn-e2e-"+uuid.NewString(),
		subID, orderID, "999", "USD",
	)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(body),
		paddleSignatureHeaders("e2e-paddle-webhook-secret", body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Fatalf("order status: %s", status)
	}

	var subStatus, extSubID string
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT status, external_subscription_id FROM subscriptions
		 WHERE user_id = (SELECT user_id FROM orders WHERE id = $1)
		 ORDER BY created_at DESC LIMIT 1`, orderID,
	).Scan(&subStatus, &extSubID); err != nil {
		t.Fatal(err)
	}
	if subStatus != "active" {
		t.Fatalf("sub status: %s", subStatus)
	}
	if extSubID != subID {
		t.Fatalf("external_subscription_id not stamped: %q", extSubID)
	}

	// 续费:transaction.billed(mock 客户端返回 now+1mo)
	renewBody := paddleTransactionBody(
		"evt-e2e-paddle-renew-"+uuid.NewString(),
		"transaction.billed",
		"txn-e2e-renew-"+uuid.NewString(),
		subID, "", "999", "USD",
	)
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(renewBody),
		paddleSignatureHeaders("e2e-paddle-webhook-secret", renewBody))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renewal webhook: %d %s", resp.StatusCode, string(resp.Body))
	}

	var expires *time.Time
	if err := srv.DB.QueryRowxContext(context.Background(),
		`SELECT expires_at FROM subscriptions WHERE external_subscription_id = $1`, subID,
	).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires == nil || !expires.After(time.Now().Add(20*24*time.Hour)) {
		t.Fatalf("expires_at not extended by renewal: %v", expires)
	}

	var renewCount int
	if err := srv.DB.GetContext(context.Background(), &renewCount,
		`SELECT COUNT(*) FROM payments WHERE channel = 'paddle' AND external_txn_id LIKE 'txn-e2e-renew-%'`); err != nil {
		t.Fatal(err)
	}
	if renewCount != 1 {
		t.Fatalf("renewal payment rows: %d", renewCount)
	}
}

func TestE2E_Paddle_MissingSignature_400(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	body := paddleTransactionBody("evt-x", "transaction.completed", "txn-x", "sub-x", "order-x", "999", "USD")
	resp := doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(body), map[string]string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestE2E_Paddle_ActiveSubBlocksNewOrder(t *testing.T) {
	srv := setupE2EServerWithVerifier(t)
	token := loginAndGetTokens(t, srv.Engine, "paddle-guard", "yundian").AccessToken

	resp := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first order: %d %s", resp.StatusCode, string(resp.Body))
	}
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &r)

	subID := "sub_e2e_" + uuid.NewString()
	body := paddleTransactionBody("evt-g-"+uuid.NewString(), "transaction.completed",
		"txn-g-"+uuid.NewString(), subID, r.Data.ID, "999", "USD")
	resp = doRequest(t, srv.Engine, http.MethodPost,
		"/webhooks/payment/paddle", string(body),
		paddleSignatureHeaders("e2e-paddle-webhook-secret", body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settle: %d", resp.StatusCode)
	}

	// 已有未过期 active 订阅 → 第二单 409(渠道侧自动续费防双重扣费)
	resp = doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly_usd","channel":"paddle"}`, authHeader(token))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", resp.StatusCode, string(resp.Body))
	}
}
```

- [ ] **Step 3: 跑 e2e(需本地 Postgres)**

Run: `make migrate && make e2e`
Expected: 新增 4 个 paddle 用例 PASS,且 paypal/wechat/alipay 用例全部不回归。

- [ ] **Step 4: Commit**

```bash
git add tests/e2e/
git commit -m "test(e2e): paddle channel — order intent, settlement, renewal, guard"
```

---

### Task 10: 文档收尾 + 全量验证

**Files:**
- Modify: `docs/plans/2026-10-01-paddle-integration-research.md`(§4 补:default payment link 已设、下单链路已实测;新增"实施状态"一节)
- Modify: `CLAUDE.md` / `PROGRESS.md`(若其中枚举了支付渠道清单,grep `stripe.*wechat.*alipay.*paypal` 找到并补 paddle)
- Modify: `.env.example`(Task 2 已做)

**Interfaces:**
- Consumes: Task 1-9 全部。

- [ ] **Step 1: 渠道清单文档扫尾**

Run: `grep -rn "wechat_pay.*alipay.*paypal\|stripe.*wechat" CLAUDE.md PROGRESS.md docs/api-integration-guide.md internal/middleware/webhook_sig.go docs/ --include="*.md" -l | sort -u`
凡枚举四渠道的地方补 paddle(代码注释里的渠道名单同样更新,如 webhook.go 头注释 :27-36、ChannelSignatureVerifier doc :34)。

- [ ] **Step 2: 调研报告补实施状态**

在报告 §4 后追加:

```markdown
### 实施状态(2026-10-01)

- ✅ 迁移 042 / config PADDLE_* / billing/paddle 客户端(mock)/ PaddleVerifier / parsePaddle / service 分发+下单+续费 / main.go 装配 / e2e 用例
- ⚠️ 已知限制:Paddle 退款(`adjustment.*`)audit-only,运营手工处理;`coding-plan` 未接入 Paddle(仅 kaya-membership);沙箱待新版 Billing sandbox 账号(见 §4 待办)。
- 前端待办(其他仓库):yunhou.ai/checkout 页托管 Paddle.js,读 `?_ptxn=` 调 `Paddle.Checkout.open`(overlay 可用 provider_intent.client_token + transaction_id)。
```

- [ ] **Step 3: 全量验证**

Run: `go build ./... && make test && make migrate && make e2e`
Expected: 全绿。

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "docs: paddle channel implementation status and channel-list sweep"
```

---

## Self-Review 记录

- **Spec 覆盖**:决策 1-6 全部落任务:并存(:3118+CreateOrder 守卫)、订阅优先(transaction.completed/billed 生命周期)、Price ID 一致(PADDLE_PRICES_JSON 运营维护)、退款现状(adjustment.* 不进分发表)、mock e2e(PADDLE_MOCK + e2e 接线)。风险 1(webhook 5s)未在本计划内压测,记录为上线前待办;风险 4(多币种)用下单锁 currency_code 消解。
- **类型一致性**:paddleClient 接口两处消费(CreateOrder/onRenewalSucceeded)签名一致;dispatchBranch/branch* 常量名在 Task 6 测试与实现间一致;CheckoutTransaction 字段名在 Task 3 与 Task 6 间一致。
- **遗留占位**:Task 8 依赖现有 handler 映射的具体状态码(执行时 grep 照抄,不是占位);Task 9 依赖 testhelpers 现有 helper 名称(loginAndGetTokens/doRequest/authHeader/resp.JSON,均已在 paypal_test.go 使用,存在)。
