package repo

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestLLMUsageRepo_InsertAndSum(t *testing.T) {
	db := setupDB(t)
	r := NewLLMUsageRepo(db)
	ctx := context.Background()
	uid := seedUsageUser(t, db)

	events := []model.LLMUsageEvent{
		{UserID: uid, AppID: usageTestApp, Model: "deepseek-flash", Provider: "deepseek", UpstreamModel: "deepseek-chat", Status: "ok", InputTokens: 100, OutputTokens: 50, CostMicros: 600},
		{UserID: uid, AppID: usageTestApp, Model: "deepseek-flash", Provider: "deepseek", UpstreamModel: "deepseek-chat", Status: "disconnected", InputTokens: 100, OutputTokens: 10, CostMicros: 280},
		{UserID: uid, AppID: usageTestApp, Model: "kimi-k3", Provider: "kimi", UpstreamModel: "kimi-k3-latest", Status: "ok", InputTokens: 200, OutputTokens: 80, CostMicros: 1040},
	}
	for _, ev := range events {
		if err := r.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	rows, err := r.SumByModel(ctx, "2000-01-01", "2100-01-01")
	if err != nil {
		t.Fatalf("SumByModel: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("SumByModel rows = %d, want 2: %+v", len(rows), rows)
	}
	byModel := map[string]model.LLMUsageRow{}
	for _, row := range rows {
		byModel[row.Model] = row
	}
	flash := byModel["deepseek-flash"]
	if flash.Requests != 2 || flash.InputTokens != 200 || flash.OutputTokens != 60 || flash.CostMicros != 880 {
		t.Errorf("deepseek-flash row = %+v", flash)
	}
	kimi := byModel["kimi-k3"]
	if kimi.Requests != 1 || kimi.CostMicros != 1040 {
		t.Errorf("kimi-k3 row = %+v", kimi)
	}
}

func TestLLMUsageRepo_SumByModelEmptyRange(t *testing.T) {
	db := setupDB(t)
	r := NewLLMUsageRepo(db)
	rows, err := r.SumByModel(context.Background(), "2000-01-01", "2000-01-02")
	if err != nil {
		t.Fatalf("SumByModel: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want empty", rows)
	}
}
