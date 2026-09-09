package management

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// usage_view_test.go — 用量视图的纯规则测试：时间范围限制（默认 30 天、
// 最大 92 天）、keyset 游标编解码、分页 hasMore 截断与 next_cursor 生成、
// 无账户空视图。

var uvNow = time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC)

type fakeUsageStore struct {
	accountErr error
	groups     []UsageGroup
	series     []UsageSeriesBucket
	rows       []RequestRow
	gotGroupF  *UsageSummaryFilter
	gotListF   *RequestListFilter
}

func (f *fakeUsageStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	return &domain.BillingAccount{ID: "acct-1", Status: "active"}, nil
}

func (f *fakeUsageStore) SummarizeUsageGroups(_ context.Context, _ string, q UsageSummaryFilter) ([]UsageGroup, error) {
	f.gotGroupF = &q
	return f.groups, nil
}

func (f *fakeUsageStore) SummarizeUsageSeries(_ context.Context, _ string, q UsageSummaryFilter) ([]UsageSeriesBucket, error) {
	return f.series, nil
}

func (f *fakeUsageStore) ListRequestRows(_ context.Context, _ string, q RequestListFilter) ([]RequestRow, error) {
	f.gotListF = &q
	return f.rows, nil
}

func TestResolveUsageRange(t *testing.T) {
	// 默认：to=now，from=now-30d。
	from, to, err := ResolveUsageRange(time.Time{}, time.Time{}, uvNow)
	if err != nil {
		t.Fatal(err)
	}
	if !to.Equal(uvNow) || !from.Equal(uvNow.Add(-DefaultUsageRange)) {
		t.Errorf("defaults: from=%v to=%v", from, to)
	}
	// from >= to → 拒绝。
	if _, _, err := ResolveUsageRange(uvNow, uvNow, uvNow); err == nil {
		t.Error("from == to must fail")
	}
	if _, _, err := ResolveUsageRange(uvNow, uvNow.Add(-time.Hour), uvNow); err == nil {
		t.Error("from > to must fail")
	}
	// 跨度过大（>92 天）→ 拒绝（限制时间范围）。
	if _, _, err := ResolveUsageRange(uvNow.Add(-93*24*time.Hour), uvNow, uvNow); err == nil {
		t.Error("span > 92d must fail")
	}
	// 恰好 92 天允许。
	if _, _, err := ResolveUsageRange(uvNow.Add(-MaxUsageRange), uvNow, uvNow); err != nil {
		t.Errorf("span == 92d must pass: %v", err)
	}
}

func TestRequestCursorCodec(t *testing.T) {
	c := RequestCursor{CreatedAt: uvNow, ID: "req-123"}
	dec, err := DecodeRequestCursor(EncodeRequestCursor(c))
	if err != nil {
		t.Fatal(err)
	}
	if !dec.CreatedAt.Equal(c.CreatedAt) || dec.ID != c.ID {
		t.Errorf("round trip: %+v", dec)
	}
	for _, bad := range []string{"", "not-base64!!", "e30", // "{}" — 字段缺失
	} {
		if _, err := DecodeRequestCursor(bad); err == nil {
			t.Errorf("malformed cursor %q must fail", bad)
		}
	}
}

func TestUsageView_Summary(t *testing.T) {
	t.Run("no account → empty summary with resolved range", func(t *testing.T) {
		svc := NewUsageViewService(&fakeUsageStore{accountErr: notFoundErr()}, domain.FixedClock{T: uvNow})
		v, err := svc.Summary(context.Background(), "user-x", UsageSummaryFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Groups) != 0 || len(v.Series) != 0 {
			t.Errorf("view = %+v", v)
		}
		if v.GroupBy != GroupByModel || !v.CompleteThrough.Equal(uvNow) {
			t.Errorf("group_by=%q complete_through=%v", v.GroupBy, v.CompleteThrough)
		}
		if !v.From.Equal(uvNow.Add(-DefaultUsageRange)) || !v.To.Equal(uvNow) {
			t.Errorf("range = %v..%v", v.From, v.To)
		}
	})

	t.Run("range is resolved before hitting the store", func(t *testing.T) {
		store := &fakeUsageStore{}
		svc := NewUsageViewService(store, domain.FixedClock{T: uvNow})
		if _, err := svc.Summary(context.Background(), "u", UsageSummaryFilter{GroupBy: GroupByKey}); err != nil {
			t.Fatal(err)
		}
		if store.gotGroupF == nil || store.gotGroupF.From.IsZero() || !store.gotGroupF.To.Equal(uvNow) {
			t.Errorf("store filter = %+v", store.gotGroupF)
		}
	})

	t.Run("invalid group_by rejected", func(t *testing.T) {
		svc := NewUsageViewService(&fakeUsageStore{}, domain.FixedClock{T: uvNow})
		if _, err := svc.Summary(context.Background(), "u", UsageSummaryFilter{GroupBy: "day"}); err == nil {
			t.Error("group_by=day must fail")
		}
	})
}

func TestUsageView_ListRequests(t *testing.T) {
	mkRow := func(id string, at time.Time) RequestRow {
		return RequestRow{ID: id, ModelID: "glm-4.6", Status: domain.ReqSettled,
			UsageStatus: domain.UsageReported, CreatedAt: at}
	}

	t.Run("hasMore trimming and next_cursor", func(t *testing.T) {
		// store 返回 limit+1 行；服务截断到 limit 并用最后一行生成游标。
		rows := make([]RequestRow, 0, 4)
		for i, id := range []string{"r1", "r2", "r3", "r4"} {
			rows = append(rows, mkRow(id, uvNow.Add(-time.Duration(i)*time.Minute)))
		}
		store := &fakeUsageStore{rows: rows}
		svc := NewUsageViewService(store, domain.FixedClock{T: uvNow})
		page, err := svc.ListRequests(context.Background(), "u", RequestListFilter{Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 3 || page.NextCursor == "" {
			t.Fatalf("page = %d items, cursor %q", len(page.Items), page.NextCursor)
		}
		cur, err := DecodeRequestCursor(page.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		if cur.ID != "r3" {
			t.Errorf("cursor id = %q, want r3 (last row of the page)", cur.ID)
		}
		// limit+1 传给存储层（hasMore 探测）。
		if store.gotListF == nil || store.gotListF.Limit != 3 {
			t.Errorf("store filter = %+v", store.gotListF)
		}
	})

	t.Run("exact page → no next cursor", func(t *testing.T) {
		store := &fakeUsageStore{rows: []RequestRow{mkRow("r1", uvNow)}}
		svc := NewUsageViewService(store, domain.FixedClock{T: uvNow})
		page, err := svc.ListRequests(context.Background(), "u", RequestListFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.NextCursor != "" {
			t.Fatalf("page = %d items cursor %q", len(page.Items), page.NextCursor)
		}
	})

	t.Run("limit validation and default", func(t *testing.T) {
		svc := NewUsageViewService(&fakeUsageStore{}, domain.FixedClock{T: uvNow})
		if _, err := svc.ListRequests(context.Background(), "u", RequestListFilter{Limit: 101}); err == nil {
			t.Error("limit 101 must fail")
		}
		if _, err := svc.ListRequests(context.Background(), "u", RequestListFilter{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no account → empty page", func(t *testing.T) {
		svc := NewUsageViewService(&fakeUsageStore{accountErr: notFoundErr()}, domain.FixedClock{T: uvNow})
		page, err := svc.ListRequests(context.Background(), "u", RequestListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 0 || page.NextCursor != "" || page.Limit != 50 {
			t.Errorf("page = %+v", page)
		}
	})

	t.Run("cursor is opaque base64url (no offset math leaks)", func(t *testing.T) {
		tok := EncodeRequestCursor(RequestCursor{CreatedAt: uvNow, ID: "r9"})
		if strings.Contains(tok, "r9") || strings.ContainsAny(tok, "+/=") {
			t.Errorf("cursor must be opaque base64url: %q", tok)
		}
	})
}

func TestUsageGroup_NetMicros(t *testing.T) {
	g := UsageGroup{ChargeMicros: 1000, ReversedMicros: 250}
	if g.NetMicros() != 750 {
		t.Errorf("net = %d", g.NetMicros())
	}
}
