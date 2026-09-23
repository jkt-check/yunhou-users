package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// user_views_payg_test.go — Task 14 PAYG 态的客户读面用例（评审轮9
// Important-2）：七态 fixture 集在 Task 14 之前，没覆盖 source_type=payg。
// 钉住：/user/model-quotas 渲染 source_type:"payg"；/user/model-subscriptions
// 渲染 source.type:"payg" 且 reference 脱敏为空（"payg:<billing_account_id>"
// 是内部行标识，不得出现在任何客户响应中）。

// seedPAYG seeds account + policy + 一条 active PAYG 权益（与
// EnsurePAYGEntitlementTx 落库的来源形状一致：source_id = "payg:<account>"）。
func (f *viewsFixture) seedPAYG(t *testing.T, userID string) (accountID, entID string) {
	t.Helper()
	ctx := context.Background()
	acct, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertModel(ctx, &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM 4.6", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "payg", Revision: 1, ModelIDs: []string{"glm-4.6"},
		FiveHourLimit: mustMicro(1_000_000), WeeklyLimit: mustMicro(10_000_000),
		MonthlyLimit: mustMicro(100_000_000), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	anchor := time.Now().UTC().Add(-48 * time.Hour)
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourcePAYG,
		SourceID: accounting.PAYGEntitlementSourceID(acct.ID), ModelIDs: []string{"glm-4.6"},
		PolicyVersionID: pol.ID, AnchorAt: anchor, EffectiveFrom: anchor,
		Status: domain.EntitlementActive,
	}
	if err := f.store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	return acct.ID, ent.ID
}

func TestUserViews_PAYGModelQuotas(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	acctID, _ := f.seedPAYG(t, userA)

	w := f.get(t, "/user/model-quotas", tokA)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	ent := data["entitlement"].(map[string]any)
	if ent["source_type"] != "payg" {
		t.Errorf("entitlement.source_type = %v, want payg", ent["source_type"])
	}
	// 可用的 PAYG 权益与套餐权益同一展示语义：无阻断、三窗口可见。
	if len(blocksOf(t, data)) != 0 {
		t.Errorf("payg 可用权益不应阻断: %v", data["blocked_by"])
	}
	if len(data["windows"].([]any)) != 3 {
		t.Errorf("windows = %v, want 3", data["windows"])
	}
	if strings.Contains(w.Body.String(), acctID) {
		t.Errorf("quota 响应泄漏内部账单账户 id: %s", w.Body.String())
	}
}

func TestUserViews_PAYGModelSubscriptions(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	acctID, _ := f.seedPAYG(t, userA)

	w := f.get(t, "/user/model-subscriptions", tokA)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	ents := data["entitlements"].([]any)
	if len(ents) != 1 {
		t.Fatalf("entitlements = %v, want 1 (PAYG 权益)", ents)
	}
	src := ents[0].(map[string]any)["source"].(map[string]any)
	if src["type"] != "payg" {
		t.Errorf("source.type = %v, want payg", src["type"])
	}
	// reference 脱敏：内部 "payg:<billing_account_id>" 不输出。
	if src["reference"] != "" {
		t.Errorf("payg source.reference 必须为空（内部标识脱敏）, got %v", src["reference"])
	}
	if strings.Contains(w.Body.String(), acctID) {
		t.Errorf("subscriptions 响应泄漏内部账单账户 id: %s", w.Body.String())
	}
}
