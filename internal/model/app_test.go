package model

import (
	"encoding/json"
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
	if cfg.PaymentProviders.Lemonsqueezy != nil {
		t.Error("lemonsqueezy should be nil when not in JSON")
	}
}

func TestAppConfig_UnmarshalJSON_LemonSqueezyOnly(t *testing.T) {
	raw := []byte(`{
		"payment_providers": {
			"lemonsqueezy": {
				"api_key": "lsq_abc",
				"store_id": "12345",
				"plans": {
					"monthly": {
						"variant_id": "var_xyz",
						"trial_days": 0,
						"billing_cycle_days": 30
					}
				}
			}
		}
	}`)
	var cfg AppConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.PaymentProviders == nil || cfg.PaymentProviders.Lemonsqueezy == nil {
		t.Fatal("lemonsqueezy config nil")
	}
	if cfg.PaymentProviders.Lemonsqueezy.APIKey != "lsq_abc" {
		t.Errorf("api_key = %q", cfg.PaymentProviders.Lemonsqueezy.APIKey)
	}
	plan := cfg.PaymentProviders.Lemonsqueezy.Plans["monthly"]
	if plan.VariantID != "var_xyz" || plan.TrialDays != 0 || plan.BillingCycleDays != 30 {
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