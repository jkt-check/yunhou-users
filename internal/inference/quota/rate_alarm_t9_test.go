package quota

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// rate_alarm_t9_test.go — T9 的运行告警之一：**限速存储失败**。
//
// 限速闸门与金额预占在**同一个事务**里（Task 4）。存储不可用时准入
// fail-closed（503 service_unavailable），绝不能回退成单机放行；但「存储
// 故障」与「正常的限速拒绝」是两件事 —— 前者是运行告警，必须响亮。
//
// 断言落在标准日志上（仓库唯一的监控通道：结构化日志行），因此在未改动
// 的生产代码上是行为断言失败（RED），不是编译错误。

// captureLog 捕获标准日志输出（仓库的告警通道就是 log.Printf）。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// TestReserveRateTx_StoreFailureAlarms 钉住：限速存储失败（不是限速拒绝）
// 必须打出 ALARM 行 —— 运维能区分「限速拒绝」与「限速存储故障」。
func TestReserveRateTx_StoreFailureAlarms(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	fs.rateErr = errors.New("pq: relation \"inference_rate_reservations\" does not exist")
	buf := captureLog(t)

	_, err := svc.Admit(context.Background(), cmd)
	if err == nil {
		t.Fatal("a rate-store failure must fail the admission closed")
	}
	if !strings.Contains(buf.String(), "ALARM") {
		t.Fatalf("a rate-store failure must raise an ALARM line, log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "rate") {
		t.Errorf("the alarm must name the rate store, log = %q", buf.String())
	}
}

// TestReserveRateTx_RateRejectionIsNotAnAlarm 钉住反面：**正常的限速拒绝**
// 不是存储故障 —— 不得每次都打 ALARM（否则告警会被噪声淹没）。
func TestReserveRateTx_RateRejectionIsNotAnAlarm(t *testing.T) {
	svc, fs, cmd := serviceFixture(t)
	fs.rateErr = domain.NewError(domain.CodeRateLimited, "rate: account rpm exceeded")
	buf := captureLog(t)

	_, err := svc.Admit(context.Background(), cmd)
	if domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("err = %v, want rate_limited", err)
	}
	if strings.Contains(buf.String(), "ALARM") {
		t.Fatalf("a rate rejection must not raise an ALARM line, log = %q", buf.String())
	}
}
