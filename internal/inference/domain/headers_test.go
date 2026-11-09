package domain

import "testing"

func TestValidateCustomHeaders_ReservedCaseVariants(t *testing.T) {
	for _, name := range []string{
		"AUTHORIZATION", "aUtHoRiZaTiOn", "PROXY-AUTHORIZATION",
		"HOST", "X-API-KEY", "x-aPI-kEY", "X-REQUEST-ID",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCustomHeaders(map[string]string{name: "override"}); err == nil {
				t.Fatalf("reserved header %q accepted", name)
			}
		})
	}
	for _, name := range []string{"X-CUSTOM", "anthropic-version", "X-Request-Id-Extra"} {
		if err := ValidateCustomHeaders(map[string]string{name: "allowed"}); err != nil {
			t.Errorf("custom header %q rejected: %v", name, err)
		}
	}
}
