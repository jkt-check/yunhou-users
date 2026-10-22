package service

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/yunhou/users/internal/repo"
)

// UsageTokenRetentionSweeper 是 kaya token 用量桶的保留期清理（契约
// usage-stats.md §4.9：云端桶保留 retentionDays，默认 180 天，与客户端
// 本地 ~/.kaya/usage 的滚动窗口对齐）。每轮顺带清理 to_bucket 早于保留期
// 的墓碑（墓碑只用于摄入侧丢弃，过期窗口的桶已不可能再合法上传，留着只是
// 垃圾行）。
//
// 与 OrderSweeper 同模式：进程内 ticker，N 实例各自跑同一 DELETE（幂等，
// 重复无害）；日级间隔对 180 天保留窗口足够快。
type UsageTokenRetentionSweeper struct {
	repo          repo.UsageTokenRepo
	retentionDays int
	interval      time.Duration
	stop          chan struct{}
	done          chan struct{}
	once          sync.Once
	// startOnce 与 OrderSweeper 同理：二次 Start 不能再起 goroutine（两个
	// goroutine 都会 close(done)，Stop 双重 close 会 panic）。
	startOnce sync.Once
}

// NewUsageTokenRetentionSweeper 的 interval 为 0 时按日级默认（24h）。
func NewUsageTokenRetentionSweeper(r repo.UsageTokenRepo, retentionDays int, interval time.Duration) *UsageTokenRetentionSweeper {
	if retentionDays <= 0 {
		retentionDays = 180
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &UsageTokenRetentionSweeper{
		repo:          r,
		retentionDays: retentionDays,
		interval:      interval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// Start 启动后台清理循环；幂等。
func (s *UsageTokenRetentionSweeper) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		go s.run(ctx)
	})
}

// Stop 通知退出并打断在跑的一轮（分批删除在批次间响应取消）；等待退出
// 是有界的（M7：一轮最多 1 分钟 + 单批中断余量），兜底超时后不再阻塞
// 停机流程。幂等。
func (s *UsageTokenRetentionSweeper) Stop() {
	s.once.Do(func() {
		close(s.stop)
	})
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		log.Printf("usage token retention: stop wait timed out; sweep aborted in background")
	}
}

func (s *UsageTokenRetentionSweeper) run(ctx context.Context) {
	defer close(s.done)
	// runCtx 把父 ctx 与 Stop 合并：Stop 打断在跑的一轮（M7——分批删除
	// 后批次间取消是安全的，半截进度下轮续跑）。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-runCtx.Done():
		}
	}()

	t := time.NewTicker(s.interval)
	defer t.Stop()

	// 启动即跑一轮：重启后不必等一个 interval 才清理积压。
	s.tick(runCtx)

	for {
		select {
		case <-runCtx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.tick(runCtx)
		}
	}
}

// usageTokenSweepTickTimeout 是单轮清理的超时（PR #61 review M7）：
// 5 分钟会卡住停机（Stop 等当前轮）；分批删除后 1 分钟足够，删不完下轮
// 续跑（DELETE 幂等）。
const usageTokenSweepTickTimeout = time.Minute

// tick 执行一轮删除；错误只记日志不打断循环（瞬时 DB 错误下轮重试）。
func (s *UsageTokenRetentionSweeper) tick(ctx context.Context) {
	if _, _, err := s.sweep(ctx); err != nil {
		// Stop/停机触发的取消不是故障，不打错误日志。
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		log.Printf("usage token retention: tick failed: %v", err)
	}
}

// sweep 是实际删除逻辑（SweepOnce 与 tick 共用）。
func (s *UsageTokenRetentionSweeper) sweep(ctx context.Context) (buckets, tombstones int64, err error) {
	// 单轮 1 分钟上限（M7）；父 ctx 取消（Stop/停机）在批次间中断，删除
	// 按批原子，半截进度下轮重跑，幂等无害。
	tickCtx, cancel := context.WithTimeout(ctx, usageTokenSweepTickTimeout)
	defer cancel()
	cutoff := time.Now().Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
	buckets, tombstones, err = s.repo.SweepRetention(tickCtx, cutoff)
	if err != nil {
		return 0, 0, err
	}
	if buckets > 0 || tombstones > 0 {
		log.Printf("usage token retention: deleted %d buckets, %d tombstones older than %d days",
			buckets, tombstones, s.retentionDays)
	}
	return buckets, tombstones, nil
}

// SweepOnce 同步跑一轮清理（测试与一次性命令用）。
func (s *UsageTokenRetentionSweeper) SweepOnce(ctx context.Context) (buckets, tombstones int64, err error) {
	return s.sweep(ctx)
}
