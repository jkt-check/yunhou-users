package e2e

// chat_zero_price_test.go — R7-N5 全链路钉:0 价模型(sale_credit 价格版本
// rate=0)不得阻断零余额订阅用户(有 billing account + 显式权益、无钱包、
// 从未充值)的 /chat 请求——预占与结算都不触碰钱包/余额。

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/postgres"
)

func TestChatFacade_ZeroPriceModelAdmitsZeroBalanceUser(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up, nil)
	ctx := context.Background()

	// 发布目录的 0 价修订:revision 2 全费率 0(LatestPriceVersion 按
	// revision DESC 取最新生效版本,覆盖 setup 里的非 0 价 revision 1)。
	if err := store.InsertPriceVersion(ctx, &postgres.PriceVersion{
		ModelID: "deepseek-chat", Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 0, OutputPerMtok: 0,
		Revision: 2, EffectiveFrom: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("zero price version: %v", err)
	}

	login := loginAndGetTokens(t, engine, "chatzeroprice", "yundian")
	// 显式权益(grant 来源 → 套餐/quota 准入路径,非 PAYG 钱包路径)。
	grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")

	// 数据面前置断言:该计费账户没有任何钱包行(从未充值/未开启超额)。
	var walletCount int
	if err := db.QueryRow(`SELECT count(*) FROM inference_wallets w
		JOIN inference_billing_accounts a ON a.id = w.billing_account_id
		WHERE a.user_id = $1`, login.User.ID).Scan(&walletCount); err != nil {
		t.Fatal(err)
	}
	if walletCount != 0 {
		t.Fatalf("wallet rows = %d, want 0 (never charged, no wallet)", walletCount)
	}

	// POST /chat 不带 model 字段 → 默认模型;全链路必须 200,链上任何一环
	// 都不得出现 429/403。
	w := chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("zero-price /chat = %d %s", w.Code, w.Body.String())
	}
	// 全程 SSE:content 增量 + 尾部 usage chunk + [DONE]。
	body := w.Body.String()
	for _, want := range []string{
		`"content":"你"`, `"content":"好"`,
		`"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("zero-price SSE missing %s: %s", want, body)
		}
	}
	select {
	case <-up.lastBody:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw the request")
	}

	// 账本事实:0 价结算照常落终态,实扣 0(reserved=0 → used=0)。
	var status string
	var settled int64
	if err := db.QueryRow(`SELECT status, settled_micros FROM inference_requests
		ORDER BY created_at DESC LIMIT 1`).Scan(&status, &settled); err != nil {
		t.Fatal(err)
	}
	if status != "settled" || settled != 0 {
		t.Errorf("request = %s/%d, want settled/0", status, settled)
	}
}
