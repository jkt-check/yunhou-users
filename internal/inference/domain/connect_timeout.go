package domain

import (
	"context"
	"time"
)

// connect_timeout.go — 建连阶段预算的跨包载体（T7）。
//
// 口径（设计 §5 的部署配置语义）：
//   - deployment.connect_timeout 只管「建连」：DNS 解析 + TCP 建连。它是
//     一个上限，不是请求总时长。
//   - deployment.request_timeout 才是请求总时长（整条响应/流的上限）。
//   - 已经建立的连接（SSE 长流）不被 connect_timeout 截断 —— 它在连接
//     建立之后就不再受建连预算约束。
//
// 为什么用 context VALUE 而不是 deadline：net/http 在拨号前会把请求
// context 的 deadline 剥掉（context.WithoutCancel，明确「保留 values」），
// 所以只有 value 能同时活过 HTTP/1 与 HTTP/2 的拨号路径。真正的截止时间
// 在拨号函数内部按 value 现算（见 providers.NewHTTPClient）。

// connectTimeoutKey 是私有 context key —— 外部包无法伪造建连预算。
type connectTimeoutKey struct{}

// DefaultConnectTimeout is the connect budget used when the caller
// declares NO budget at all. It equals the historical hard-coded dial
// timeout of the shared dispatch client (10s) — but it is only a
// fallback: every deployment written through a supported path carries its
// own value, and the stored default is 5s
// (inference_deployments.connect_timeout_ms DEFAULT 5000). Honouring the
// stored value therefore shrinks the effective budget of the deployments
// that never declared one from 10s to 5s.
const DefaultConnectTimeout = 10 * time.Second

// WithConnectTimeout 把建连预算挂到 ctx 上。非正值表示「未声明」——ctx 原样
// 返回（调用方随后落到 DefaultConnectTimeout）。
func WithConnectTimeout(ctx context.Context, d time.Duration) context.Context {
	if ctx == nil || d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, connectTimeoutKey{}, d)
}

// ConnectTimeoutFrom 读回 ctx 上的建连预算；未声明时返回 0。
func ConnectTimeoutFrom(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	d, _ := ctx.Value(connectTimeoutKey{}).(time.Duration)
	return d
}
