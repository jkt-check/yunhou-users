package service

import (
	"context"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// LLMUsageService backs the admin LLM usage stats endpoint. Thin: date
// validation + repo delegation (mirrors UsageService's param handling).
type LLMUsageService struct {
	repo repo.LLMUsageRepo
}

func NewLLMUsageService(r repo.LLMUsageRepo) *LLMUsageService { return &LLMUsageService{repo: r} }

// StatsByModel returns per-model aggregates over [from, to] (YYYY-MM-DD,
// server timezone). Empty result is a non-nil slice so the JSON response is
// [] not null.
func (s *LLMUsageService) StatsByModel(ctx context.Context, from, to string) ([]model.LLMUsageRow, error) {
	fromDate, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("%w: from must be YYYY-MM-DD", ErrUsageInvalidParam)
	}
	toDate, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("%w: to must be YYYY-MM-DD", ErrUsageInvalidParam)
	}
	if toDate.Before(fromDate) {
		return nil, fmt.Errorf("%w: to must be >= from", ErrUsageInvalidParam)
	}
	rows, err := s.repo.SumByModel(ctx, from, to)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []model.LLMUsageRow{}
	}
	return rows, nil
}
