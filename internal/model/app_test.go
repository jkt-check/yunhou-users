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
				"plan_ids": {"monthly": "P-1"}
			}
		}
	}`)
	var cfg AppConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.PaymentProviders == nil || cfg.PaymentProviders.Paypal == nil {
		t.Fatal("paypal config nil")
	}
	if cfg.PaymentProviders.Paypal.ClientID != "cid" {
		t.Errorf("client_id = %q, want cid", cfg.PaymentProviders.Paypal.ClientID)
	}
	if cfg.PaymentProviders.Lemonsqueezy != nil {
		t.Error("lemonsqueezy should be nil when not in JSON")
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
}