package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// repo_edges_test.go — Task 16 覆盖率补强：订单/支付/权益配置读取面的
// 低频路径（真实库；主链路已在 payments_repos_test.go/repo_test.go）。

func TestOrderRepo_FindByProviderOutTradeNo_AndCancelPending(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	uid := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatal(err)
	}
	orderRepo := NewOrderRepo(db)

	// 未找到 → sql.ErrNoRows 语义（调用方据此创建新订单）。
	if _, err := orderRepo.FindByProviderOutTradeNo(ctx, "missing-otn"); err == nil {
		t.Fatal("missing out_trade_no must error")
	}
	id := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders (id, user_id, plan_id, amount, status, provider_intent)
		VALUES ($1, $2, 'monthly', 19.9, 'pending', $3)`,
		id, uid, `{"out_trade_no":"otn-1"}`); err != nil {
		t.Fatal(err)
	}
	found, err := orderRepo.FindByProviderOutTradeNo(ctx, "otn-1")
	if err != nil || found.ID != id {
		t.Fatalf("find by out_trade_no = %+v/%v", found, err)
	}

	// CancelPending：所有者取消 pending → true；重复取消 → false；他人 → false。
	ok, err := orderRepo.CancelPending(ctx, id, uid)
	if err != nil || !ok {
		t.Fatalf("cancel pending = %v/%v", ok, err)
	}
	ok, err = orderRepo.CancelPending(ctx, id, uid)
	if err != nil || ok {
		t.Fatalf("re-cancel = %v/%v, want false (终态守卫)", ok, err)
	}
	ok, err = orderRepo.CancelPending(ctx, id, uuid.NewString())
	if err != nil || ok {
		t.Fatalf("foreign cancel = %v/%v, want false", ok, err)
	}
}

func TestPaymentRepo_FindByChannelTxnIDTx(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	uid := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders (id, user_id, plan_id, amount, status) VALUES ($1, $2, 'monthly', 19.9, 'pending')`,
		orderID, uid); err != nil {
		t.Fatal(err)
	}
	paymentRepo := NewPaymentRepo(db)

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paymentRepo.FindByChannelTxnIDTx(ctx, tx, "wechat_pay", "wx-1"); err == nil {
		t.Fatal("missing txn must error")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status)
		VALUES ($1, 'wechat_pay', 'wx-1', 19.9, 'CNY', 'paid')`, orderID); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := paymentRepo.FindByChannelTxnIDTx(ctx, tx, "wechat_pay", "wx-1")
	if err != nil || p.OrderID != orderID || p.Status != "paid" {
		t.Fatalf("find by txn = %+v/%v", p, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanBenefitRepo_ReadFaces(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	benefitRepo := NewPlanBenefitRepo(db)

	// 未配置 → NotFound 语义。
	if _, err := benefitRepo.FindByPlanID(ctx, "monthly"); err == nil {
		t.Fatal("unconfigured plan must error")
	}
	// 配置后两变体可读；升级规则有/无。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inference_policy_versions (name, revision, model_ids, status)
		VALUES ('cp-pol', 1, '{glm-4.6}', 'published') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var polID string
	if err := db.GetContext(ctx, &polID,
		`SELECT id::text FROM inference_policy_versions WHERE name = 'cp-pol'`); err != nil {
		t.Fatal(err)
	}
	for _, pl := range [][2]string{{"cp_x", "CP X"}, {"cp_y", "CP Y"}} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO plans (id, name, price, interval_days, product_code, currency)
			VALUES ($1, $2, 9.9, 30, 'coding-plan', 'CNY') ON CONFLICT (id) DO NOTHING`, pl[0], pl[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
		VALUES ('cp_x', $1, '{glm-4.6}', 'subscription')`, polID); err != nil {
		t.Fatal(err)
	}
	cfg, err := benefitRepo.FindByPlanID(ctx, "cp_x")
	if err != nil || cfg.PolicyVersionID != polID {
		t.Fatalf("benefit config = %+v/%v", cfg, err)
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := benefitRepo.FindByPlanIDTx(ctx, tx, "cp_x")
	if err != nil || cfg2.PolicyVersionID != polID {
		t.Fatalf("tx benefit config = %+v/%v", cfg2, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan_upgrade_rules (from_plan_id, to_plan_id) VALUES ('cp_x', 'cp_y')`); err != nil {
		t.Fatal(err)
	}
	has, err := benefitRepo.HasUpgradeRule(ctx, "cp_x", "cp_y")
	if err != nil || !has {
		t.Fatalf("upgrade rule = %v/%v", has, err)
	}
	has, err = benefitRepo.HasUpgradeRule(ctx, "cp_y", "cp_x")
	if err != nil || has {
		t.Fatalf("reverse rule = %v/%v, want false", has, err)
	}
}
