package gateway

// key_concurrency_test.go — Task 5：Key 级并发租约在网关侧的完整生命周期。
//
// 钉住的事实（全部是数据库行，不是进程内计数器）：
//   - 同一 Key 的第二个并发请求被拒（insufficient_capacity），出站零调用；
//   - 非流式成功、流式结束、客户端断开、失败切换、组装 panic 五条终态路径
//     都不得残留 held 的租约（并发槽不得冻结）；
//   - 请求进行中租约确实是 held 的（否则"没有残留"是空断言）。

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/routing"
)

// heldLeases counts the still-held leases (any scope).
func heldLeases(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state = 'held'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// keyLeaseStates counts the api_key-scope lease rows by state.
func keyLeaseStates(t *testing.T, f *fixture) map[string]int {
	t.Helper()
	rows, err := f.db.Query(`SELECT state FROM inference_concurrency_leases WHERE scope = 'api_key'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			t.Fatal(err)
		}
		out[st]++
	}
	return out
}

// TestGateway_KeyConcurrencyLimit_BlocksConcurrentRequestAndReleasesSlot:
// 同一 Key（上限 1）的第二个并发请求被拒；第一个请求结束后 Key 槽归还
// （没有残留的 held 租约）。
func TestGateway_KeyConcurrencyLimit_BlocksConcurrentRequestAndReleasesSlot(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if calls.Add(1) == 1 {
			// 只有第一个请求卡住：第二个请求若没被 Key 槽位挡住，会立刻
			// 得到一个正常应答（RED 阶段就是这个事实）。
			entered <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-k","object":"chat.completion","created":1,
			"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	})
	f := newFixture(t, up, withKeyConcurrencyLimit(1))

	p, key := f.principal()
	done := make(chan error, 1)
	go func() {
		_, err := f.gateway.ChatCompletions(context.Background(), p, key,
			domain.ProtocolOpenAIChat, chatReq(false, "hi"))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream was never reached")
	}
	// 请求进行中：Key 槽确实是 held 的（否则下面的"无残留"是空断言）。
	if got := keyLeaseStates(t, f)["held"]; got != 1 {
		t.Errorf("held key leases during the request = %d, want 1", got)
	}
	// 第二个并发请求：同一 Key 的槽位已满。
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if domain.CodeOf(err) != domain.CodeInsufficientCapacity {
		close(release)
		<-done
		t.Fatalf("second concurrent request: err = %v, want insufficient_capacity", err)
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1 (the rejected request must not reach the upstream)", up.calls.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first request: %v", err)
	}
	if got := heldLeases(t, f); got != 0 {
		t.Errorf("held leases after the request = %d, want 0 (Key 槽必须归还)", got)
	}
	// 槽位归还后同一 Key 可以再次准入。
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Errorf("the key must be usable again after the slot was released: %v", err)
	}
}

// TestGateway_KeyLeaseReleasedOnStreamCompletion: 流式结束后 Key 租约释放。
func TestGateway_KeyLeaseReleasedOnStreamCompletion(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up, withKeyConcurrencyLimit(1))

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	drainStream(t, out, EndCompleted)
	if got := heldLeases(t, f); got != 0 {
		t.Errorf("held leases after the stream = %d, want 0", got)
	}
	states := keyLeaseStates(t, f)
	if states["released"] != 1 {
		t.Errorf("key lease states = %v, want one released", states)
	}
}

// TestGateway_KeyLeaseReleasedOnClientCancel: 客户端断开（EndClientGone）
// 之后 Key 租约必须释放。
func TestGateway_KeyLeaseReleasedOnClientCancel(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	f := newFixture(t, up, withKeyConcurrencyLimit(1))

	ctx, cancel := context.WithCancel(context.Background())
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	buf := make([]byte, 4096)
	if n, err := out.Stream.Read(buf); err != nil || n == 0 {
		t.Fatalf("first read = %d, %v", n, err)
	}
	cancel()
	for i := 0; i < 16; i++ {
		if _, err := out.Stream.Read(buf); err != nil {
			break
		}
	}
	if err := out.Stream.Finish(EndClientGone); err != nil {
		t.Fatal(err)
	}
	_ = out.Stream.Close()
	if got := heldLeases(t, f); got != 0 {
		t.Errorf("held leases after the client cancel = %d, want 0", got)
	}
	if got := keyLeaseStates(t, f)["released"]; got != 1 {
		t.Errorf("released key leases = %d, want 1 (客户端断开必须归还 Key 槽)", got)
	}
}

// TestGateway_KeyLeaseReleasedOnFailover: 失败切换（上游 5xx，全部候选耗尽）
// 之后 Key 租约必须释放 —— 失败的请求不得占住并发槽。
func TestGateway_KeyLeaseReleasedOnFailover(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	f := newFixture(t, up, withKeyConcurrencyLimit(1))

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	if domain.CodeOf(err) != domain.CodeUpstreamUnavailable {
		t.Fatalf("err = %v, want upstream_unavailable", err)
	}
	if got := heldLeases(t, f); got != 0 {
		t.Errorf("held leases after the failover = %d, want 0", got)
	}
	if got := keyLeaseStates(t, f)["released"]; got != 1 {
		t.Errorf("released key leases = %d, want 1 (失败切换必须归还 Key 槽)", got)
	}
}

// TestGateway_KeyLeaseReleasedOnStreamAssemblyPanic: 流式组装阶段 panic
// （交接前）必须停 keeper 释放租约 —— 否则 Key 并发槽永久冻结。
func TestGateway_KeyLeaseReleasedOnStreamAssemblyPanic(t *testing.T) {
	up := newUpstream(t, sseHandler(chunkA, chunkB, chunkUsage, chunkDone))
	f := newFixture(t, up, withKeyConcurrencyLimit(1))
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       nilStreamAdapter{providers.NewOpenAIChat()},
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
	}
	routingSvc := routing.NewService(f.store, adapters, nil)
	gw := NewService(&staticSnapshot{f.snap}, f.store, f.gateway.entitlements, f.gateway.quotaSvc,
		routingSvc, f.gateway.secrets, f.gateway.client, f.gateway.egress, nil)

	p, key := f.principal()
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_, _ = gw.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(true, "hi"))
	}()
	if !panicked {
		t.Fatal("a nil stream must panic during StreamBody assembly (pre-handoff)")
	}
	if got := heldLeases(t, f); got != 0 {
		t.Errorf("held leases after the assembly panic = %d, want 0 (不得冻结并发槽)", got)
	}
	if got := keyLeaseStates(t, f)["released"]; got != 1 {
		t.Errorf("released key leases = %d, want 1 (panic 守卫必须归还 Key 槽)", got)
	}
}
