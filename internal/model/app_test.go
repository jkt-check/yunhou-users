package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAppConfig_UnmarshalJSON_PaypalOnly(t *testing.T) {
	raw := []byte(`{
		"payment_providers": {
			"paypal": {
				"client_id": "cid",
				"client_secret": "cs",
				"webhook_id": "WH-1",
				"mode": "live",
				"plans": {
					"monthly": {
						"plan_id": "P-1",
						"trial_days": 7,
						"billing_cycle_days": 30
					}
				}
			}
		},
		"brand": {"name": "yunhou agentic"}
	}`)
	var cfg AppConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Brand == nil || cfg.Brand.Name != "yunhou agentic" {
		t.Errorf("brand = %+v, want {Name: yunhou agentic}", cfg.Brand)
	}
	if cfg.PaymentProviders == nil || cfg.PaymentProviders.Paypal == nil {
		t.Fatal("paypal config nil")
	}
	if cfg.PaymentProviders.Paypal.ClientID != "cid" {
		t.Errorf("client_id = %q", cfg.PaymentProviders.Paypal.ClientID)
	}
	plan := cfg.PaymentProviders.Paypal.Plans["monthly"]
	if plan.PlanID != "P-1" || plan.TrialDays != 7 || plan.BillingCycleDays != 30 {
		t.Errorf("monthly plan = %+v", plan)
	}
}

func TestAppConfig_UnmarshalJSON_Empty(t *testing.T) {
	var cfg AppConfig
	if err := json.Unmarshal([]byte(`{}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.PaymentProviders != nil {
		t.Error("payment_providers should be nil for empty config")
	}
	if cfg.Brand != nil {
		t.Error("brand should be nil for empty config")
	}
}
func TestResolveCycle(t *testing.T) {
	t.Parallel()

	t.Run("no payment_providers → plan.IntervalDays fallback", func(t *testing.T) {
		t.Parallel()
		cfg := AppConfig{}
		got := ResolveCycle(cfg, "monthly", 30)
		if got.BillingCycleDays != 30 {
			t.Errorf("BillingCycleDays: got %d, want 30", got.BillingCycleDays)
		}
		if got.TrialDays != 0 {
			t.Errorf("TrialDays: got %d, want 0", got.TrialDays)
		}
		if got.Base != CycleBaseFormula {
			t.Errorf("Base: got %q, want %q", got.Base, CycleBaseFormula)
		}
	})

	t.Run("paypal present but no entry for plan → fallback", func(t *testing.T) {
		t.Parallel()
		cfg := AppConfig{
			PaymentProviders: &PaymentProvidersConfig{
				Paypal: &PaypalConfig{
					ClientID: "cid",
					Plans:    map[string]PaypalPlanConfig{}, // empty
				},
			},
		}
		got := ResolveCycle(cfg, "monthly", 30)
		if got.BillingCycleDays != 30 {
			t.Errorf("BillingCycleDays: got %d, want 30", got.BillingCycleDays)
		}
	})

	t.Run("paypal present with plan entry → use entry's billing_cycle_days", func(t *testing.T) {
		t.Parallel()
		cfg := AppConfig{
			PaymentProviders: &PaymentProvidersConfig{
				Paypal: &PaypalConfig{
					ClientID: "cid",
					Plans: map[string]PaypalPlanConfig{
						"monthly": {PlanID: "P-1", TrialDays: 7, BillingCycleDays: 31},
					},
				},
			},
		}
		got := ResolveCycle(cfg, "monthly", 30)
		if got.BillingCycleDays != 31 {
			t.Errorf("BillingCycleDays: got %d, want 31", got.BillingCycleDays)
		}
		if got.TrialDays != 7 {
			t.Errorf("TrialDays: got %d, want 7", got.TrialDays)
		}
	})

	t.Run("paypal entry with non-positive billing → plan.IntervalDays", func(t *testing.T) {
		t.Parallel()
		cfg := AppConfig{
			PaymentProviders: &PaymentProvidersConfig{
				Paypal: &PaypalConfig{
					ClientID: "cid",
					Plans: map[string]PaypalPlanConfig{
						"monthly": {PlanID: "P-1", TrialDays: 7, BillingCycleDays: 0},
					},
				},
			},
		}
		got := ResolveCycle(cfg, "monthly", 30)
		if got.BillingCycleDays != 30 {
			t.Errorf("BillingCycleDays: got %d, want 30 (fallback to plan.IntervalDays)", got.BillingCycleDays)
		}
		if got.TrialDays != 7 {
			t.Errorf("TrialDays: got %d, want 7 (entry value still applies)", got.TrialDays)
		}
	})
}

// TestAppConfig_UnmarshalJSON_WeChatPayOnly covers the payment_providers
// .wechat_pay block introduced by A2.c (production credentials skeleton).
// The block must round-trip without the paypal block being present, and
// must not steal the oauth_providers.wechat block (different schemas).
func TestAppConfig_UnmarshalJSON_WeChatPayOnly(t *testing.T) {
	raw := []byte(`{
		"payment_providers": {
			"wechat_pay": {
				"mch_id": "1900000001",
				"api_v3_key": "01234567890123456789012345678901",
				"cert_path": "/keys/wechatpay/apiclient_cert.pem",
				"key_path": "/keys/wechatpay/apiclient_key.pem",
				"notify_url": "https://api.yunhouai.com/webhooks/payment/wechat_pay",
				"plan_mapping": {"monthly": "MONTHLY_PLAN_CODE"}
			}
		}
	}`)
	var cfg AppConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.PaymentProviders == nil || cfg.PaymentProviders.WeChatPay == nil {
		t.Fatal("WeChatPay block not populated")
	}
	wp := cfg.PaymentProviders.WeChatPay
	if wp.MchID != "1900000001" {
		t.Errorf("MchID = %q, want 1900000001", wp.MchID)
	}
	if wp.APIv3Key != "01234567890123456789012345678901" {
		t.Errorf("APIv3Key not round-tripped")
	}
	if wp.CertPath != "/keys/wechatpay/apiclient_cert.pem" {
		t.Errorf("CertPath = %q", wp.CertPath)
	}
	if wp.PlanMapping["monthly"] != "MONTHLY_PLAN_CODE" {
		t.Errorf("PlanMapping[monthly] = %q", wp.PlanMapping["monthly"])
	}
	// paypal block must NOT be populated (independent fields).
	if cfg.PaymentProviders.Paypal != nil {
		t.Errorf("Paypal block should be nil when only wechat_pay is set")
	}
	// Re-marshal roundtrip preserves the block.
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"wechat_pay"`) {
		t.Errorf("re-marshalled JSON missing wechat_pay key: %s", out)
	}
}
