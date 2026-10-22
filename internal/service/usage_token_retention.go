package service

import (
	"context"
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

// Stop 通知退出并等待当前一轮跑完；幂等。
func (s *UsageTokenRetentionSweeper) Stop() {
	s.once.Do(func() {
		close(s.stop)
	})
	<-s.done
}

func (s *UsageTokenRetentionSweeper) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()

	// 启动即跑一轮：重启后不必等一个 interval 才清理积压。
	s.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick 执行一轮删除；错误只记日志不打断循环（瞬时 DB 错误下轮重试）。
func (s *UsageTokenRetentionSweeper) tick(ctx context.Context) {
	if _, _, err := s.sweep(ctx); err != nil {
		log.Printf("usage token retention: tick failed: %v", err)
	}
}

// sweep 是实际删除逻辑（SweepOnce 与 tick 共用）。
func (s *UsageTokenRetentionSweeper) sweep(ctx context.Context) (buckets, tombstones int64, err error) {
	// 用 background + 超时而非父 ctx：父 ctx 取消不应让半截删除停在中途
	//（下轮会重跑，DELETE 幂等）。
	tickCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
