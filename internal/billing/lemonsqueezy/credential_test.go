package lemonsqueezy

import (
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestCredential_APIKey_ReturnsConfigured(t *testing.T) {
	cfg := &model.LemonsqueezyConfig{APIKey: "lsq_abc", StoreID: "12345"}
	c := NewCredential(cfg)
	got, err := c.APIKey()
	if err != nil {
		t.Fatal(err)
	}
	if got != "lsq_abc" {
		t.Errorf("api_key = %q", got)
	}
}

func TestCredential_APIKey_Missing(t *testing.T) {
	c := NewCredential(nil)
	if _, err := c.APIKey(); err == nil {
		t.Fatal("expected error when lemonsqueezy config is nil")
	}
}