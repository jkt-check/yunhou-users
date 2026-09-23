package service

import (
	"context"
	"errors"
	"testing"
)

func TestLLMUsageService_StatsByModelValidation(t *testing.T) {
	svc := NewLLMUsageService(&mockLLMUsageRepo{})
	cases := []struct{ name, from, to string }{
		{"bad from", "2026/01/01", "2026-01-31"},
		{"bad to", "2026-01-01", "yesterday"},
		{"from after to", "2026-02-01", "2026-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.StatsByModel(context.Background(), tc.from, tc.to)
			if !errors.Is(err, ErrUsageInvalidParam) {
				t.Errorf("err = %v, want ErrUsageInvalidParam", err)
			}
		})
	}
}

func TestLLMUsageService_StatsByModelOK(t *testing.T) {
	repo := &mockLLMUsageRepo{}
	svc := NewLLMUsageService(repo)
	rows, err := svc.StatsByModel(context.Background(), "2026-09-01", "2026-09-07")
	if err != nil {
		t.Fatalf("StatsByModel: %v", err)
	}
	if rows == nil {
		t.Error("rows must be non-nil empty slice on no data")
	}
}
