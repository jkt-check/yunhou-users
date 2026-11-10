package gateway

// rate_anomaly_test.go — 终审 Finding 1（网关侧）：网关因为账务异常
// （缺预占 / 预占漂移）把请求停放进专门核对时，该请求的在途速率预占必须被
// **显式处置**（离开账户的 TPM 在途求和）——
//
//	否则一笔异常就永久吃掉账户的 TPM 额度：账户被永久 429（rate_limited）
//	拦住，而且没有任何自动或人工路径能把它取回来。
//
// 处置**不动金额**（窗口/Key 预占仍在、账本没有 charge），**不动证据**
//（核对任务的原因与 request.last_error 不变），**保留 RPM 事实**（一次逻辑
// 请求仍然计 1 次）。
import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// rateReservationStates reads the state of the request's rate rows.
func (f *fixture) rateReservationStates(t *testing.T, requestID string) []string {
	t.Helper()
	var states []string
	if err := f.db.SelectContext(context.Background(), &states,
		`SELECT state FROM inference_rate_reservations WHERE request_id = $1 ORDER BY scope`,
		requestID); err != nil {
		t.Fatalf("read rate reservations: %v", err)
	}
	return states
}

// accountInflightTokens sums the account's in-flight TPM reservations.
func (f *fixture) accountInflightTokens(t *testing.T, accountID string) int64 {
	t.Helper()
	var v int64
	if err := f.db.Get(&v,
		`SELECT COALESCE(SUM(reserved_tokens),0) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account' AND state = 'reserved'`,
		accountID); err != nil {
		t.Fatalf("read in-flight tokens: %v", err)
	}
	return v
}

// TestGateway_ParkedAnomaly_DisposesTheRateReservation — 缺预占被停放的请求：
// 速率预占状态必须是 anomaly（不得停在 reserved），账户的 TPM 在途求和必须
// 归零，RPM 计数仍然保留 1 次，金额与账本完全不动。
func TestGateway_ParkedAnomaly_DisposesTheRateReservation(t *testing.T) {
	var f *fixture
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		// 注入故障：本应存在的金额预占在结算前消失（预占丢失/被篡改）。
		var reqID string
		if err := f.db.QueryRow(
			`SELECT id FROM inference_requests WHERE status = 'dispatching' ORDER BY created_at DESC LIMIT 1`).
			Scan(&reqID); err != nil {
			t.Errorf("find dispatching request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, err := f.db.Exec(
			`DELETE FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, reqID); err != nil {
			t.Errorf("delete held reservations: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-anomaly","object":"chat.completion","created":1,"model":"glm-4.6-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	})
	f = newFixture(t, up)

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}

	status, usage, _, settled := f.requestRow(t, out.RequestID)
	if status != "reconciliation_required" {
		t.Errorf("request status = %q, want reconciliation_required (缺预占必须进入待核对)", status)
	}
	if usage != "unknown" {
		t.Errorf("usage status = %q, want unknown (未知用量不得记成 0)", usage)
	}
	if settled != 0 {
		t.Errorf("settled_micros = %d, want 0 (缺预占时结算不得成功)", settled)
	}
	if n := f.chargeRows(t, out.RequestID); n != 0 {
		t.Errorf("ledger charge rows = %d, want 0 (缺预占时不得落 charge)", n)
	}

	// 速率预占被显式处置。
	states := f.rateReservationStates(t, out.RequestID)
	if len(states) == 0 {
		t.Fatal("no rate reservation rows: the admission must have created one")
	}
	for _, st := range states {
		if st != "anomaly" {
			t.Errorf("rate reservation state = %q, want anomaly (停放的异常不得永久占用 TPM)", st)
		}
	}
	if got := f.accountInflightTokens(t, f.accountID); got != 0 {
		t.Errorf("in-flight tokens = %d, want 0 (停放的异常不得永久占用 TPM)", got)
	}
	// RPM 事实保留：一次逻辑请求仍然计 1 次。
	var counted int
	if err := f.db.Get(&counted,
		`SELECT COUNT(*) FROM inference_rate_reservations
		 WHERE billing_account_id = $1 AND scope = 'account'
		   AND counted_at > now() - interval '60 seconds'`, f.accountID); err != nil {
		t.Fatal(err)
	}
	if counted != 1 {
		t.Errorf("account rate count = %d, want 1 (RPM 事实必须保留)", counted)
	}
	// 异常证据保留：核对任务的原因不变。
	reason, jobStatus, found := f.reconciliationJob(t, out.RequestID)
	if !found {
		t.Fatal("no reconciliation job: a missing expected reservation must enter the reconciliation queue")
	}
	if reason != "missing_reservation" {
		t.Errorf("reconciliation reason = %q, want missing_reservation", reason)
	}
	if jobStatus != "pending" {
		t.Errorf("reconciliation status = %q, want pending", jobStatus)
	}
}
