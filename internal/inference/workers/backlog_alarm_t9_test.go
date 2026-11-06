package workers

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// backlog_alarm_t9_test.go — T9 的运行告警之一：**未知用量积压**。
//
// 恢复 worker 每轮都会把「未知用量」的请求数打进结构化日志，但**没有
// 告警**：积压本身没人看见。运营需要一条明确的 ALARM：消费未知的请求在
// 积压 —— 它们按保守估算结算，但真实用量要靠人核对。
//
// 断言落在标准日志上（仓库唯一的监控通道），因此在未改动的生产代码上是
// 行为断言失败（RED），不是编译错误。

// backlogFakeStore 是最小 RecoveryStore：没有任何滞留请求、没有待办
// 任务，只返回一份「未知用量积压」的指标快照。
type backlogFakeStore struct {
	RecoveryStore
	metrics postgres.RecoveryMetrics
}

func (s *backlogFakeStore) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	return fakeUOW{}, nil
}

func (s *backlogFakeStore) ListStaleOpenRequests(ctx context.Context, cutoff time.Time, limit int) ([]domain.Request, error) {
	return nil, nil
}

func (s *backlogFakeStore) ListPendingReconciliationJobs(ctx context.Context, limit int) ([]postgres.ReconciliationJob, error) {
	return nil, nil
}

func (s *backlogFakeStore) EscalateOverdueReconciliationJobs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return nil, nil
}

func (s *backlogFakeStore) ReconcileWindowAggregates(ctx context.Context) ([]accounting.WindowReconciliation, error) {
	return nil, nil
}

func (s *backlogFakeStore) RecoveryMetrics(ctx context.Context, staleCutoff time.Time) (postgres.RecoveryMetrics, error) {
	return s.metrics, nil
}

// TestRecover_UnknownUsageBacklogAlarms 钉住：未知用量积压超过阈值时必须
// 打出 ALARM 行（积压本身要有人看见）。
func TestRecover_UnknownUsageBacklogAlarms(t *testing.T) {
	store := &backlogFakeStore{metrics: postgres.RecoveryMetrics{UnknownUsageRequests: 11}}
	w := NewSettlementRecovery(store, domain.SystemClock{}, RecoveryConfig{}, nil)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if _, err := w.RunPass(context.Background()); err != nil {
		t.Fatalf("run pass: %v", err)
	}
	if !strings.Contains(buf.String(), "ALARM") {
		t.Fatalf("an unknown-usage backlog must raise an ALARM line, log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "unknown") {
		t.Errorf("the alarm must name the unknown-usage backlog, log = %q", buf.String())
	}
}

// TestRecover_NoUnknownUsageBacklogNoAlarm 钉住反面：没有积压时不打告警
// （告警必须是噪声低的信号）。
func TestRecover_NoUnknownUsageBacklogNoAlarm(t *testing.T) {
	store := &backlogFakeStore{metrics: postgres.RecoveryMetrics{UnknownUsageRequests: 1}}
	w := NewSettlementRecovery(store, domain.SystemClock{}, RecoveryConfig{}, nil)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if _, err := w.RunPass(context.Background()); err != nil {
		t.Fatalf("run pass: %v", err)
	}
	if strings.Contains(buf.String(), "ALARM") {
		t.Fatalf("no backlog must not raise an ALARM line, log = %q", buf.String())
	}
}
