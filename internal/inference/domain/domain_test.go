package domain

import (
	"errors"
	"testing"
	"time"
)

func TestCodeOf(t *testing.T) {
	if got := CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q, want empty", got)
	}
	if got := CodeOf(NewError(CodeInvalidKey, "bad key")); got != CodeInvalidKey {
		t.Errorf("CodeOf(Error) = %q, want invalid_key", got)
	}
	cause := errors.New("sql: no rows")
	wrapped := WrapError(CodeNotFound, "model", cause)
	if !errors.Is(wrapped, cause) {
		t.Error("WrapError must preserve the cause chain")
	}
	if got := CodeOf(wrapped); got != CodeNotFound {
		t.Errorf("CodeOf(wrapped) = %q, want not_found", got)
	}
	qe := NewQuotaExceeded([]WindowBlock{{Kind: WindowFiveHour}}, true)
	if got := CodeOf(qe); got != CodeQuotaExceeded {
		t.Errorf("CodeOf(quota) = %q, want quota_exceeded", got)
	}
	if got := CodeOf(errors.New("boom")); got != CodeInternal {
		t.Errorf("CodeOf(foreign) = %q, want internal", got)
	}
}

func TestQuotaExceededErrorListsAllBlocks(t *testing.T) {
	reset := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	err := NewQuotaExceeded([]WindowBlock{
		{Kind: WindowFiveHour, LimitMicros: 100, UsedMicros: 90, ReservedMicros: 10, ResetsAt: &reset},
		{Kind: WindowWeekly, LimitMicros: 1000, UsedMicros: 1000},
	}, false)
	var qe *QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatal("errors.As should find QuotaExceededError")
	}
	if len(qe.BlockedBy) != 2 {
		t.Errorf("BlockedBy = %v, want 2 windows", qe.BlockedBy)
	}
	if qe.BlockedBy[1].ResetsAt != nil {
		t.Error("unknown recovery time must stay nil — 不编造倒计时")
	}
}

func TestQuotaWindowAvailable(t *testing.T) {
	w := QuotaWindow{Limit: 100, Used: 30, Reserved: 20}
	if got := w.Available(); got != 50 {
		t.Errorf("Available = %d, want 50", got)
	}
	// 设计 §6: 可用额度为 max(0, limit - used - reserved) — never negative.
	w2 := QuotaWindow{Limit: 100, Used: 90, Reserved: 30}
	if got := w2.Available(); got != 0 {
		t.Errorf("Available overdrawn = %d, want 0", got)
	}
}

func TestUsageBucketsUnknownIsNotZero(t *testing.T) {
	// nil bucket = not reported; unknown usage must not collapse into 0.
	var unknown UsageBuckets
	if got := unknown.TotalTokens(); got != 0 {
		t.Errorf("TotalTokens(nil buckets) = %d, want 0 (sum of nothing)", got)
	}
	if unknown.InputTokens != nil {
		t.Error("unknown input must stay nil, not zero")
	}
	in, out := int64(10), int64(20)
	b := UsageBuckets{InputTokens: &in, OutputTokens: &out}
	if got := b.TotalTokens(); got != 30 {
		t.Errorf("TotalTokens = %d, want 30", got)
	}
}

func TestEntitlementAllowsModelExplicitSetOnly(t *testing.T) {
	e := &Entitlement{ModelIDs: []string{"glm-4.6"}}
	if !e.AllowsModel("glm-4.6") {
		t.Error("explicitly listed model must be allowed")
	}
	if e.AllowsModel("gpt-5") {
		t.Error("unlisted model must be denied — no NULL-all-allow semantics")
	}
	empty := &Entitlement{}
	if empty.AllowsModel("glm-4.6") {
		t.Error("empty model set allows nothing (新模型默认无授权)")
	}
}

func TestClocks(t *testing.T) {
	fixed := FixedClock{T: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)}
	if !fixed.Now().Equal(fixed.T) {
		t.Error("FixedClock must return the fixed instant")
	}
	sys := SystemClock{}.Now()
	if sys.Location() != time.UTC {
		t.Errorf("SystemClock must be UTC, got %v", sys.Location())
	}
}
