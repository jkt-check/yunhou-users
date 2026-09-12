package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
)

// fail() 的 envelope 面不得向客户端泄漏内部错误细节：500 一律固定文案，
// 与 /v1 原生面（apikey_auth.go）的 "internal error" 口径一致；非 500 的
// domain 错误只透传 Message（评审轮1 M4：不拼 code 前缀与 cause 链——客
// 户端可操作的部分才进 4xx）。
func TestFail_RedactsInternalErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("internal error is redacted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		fail(c, errors.New("pq: duplicate key value violates unique constraint \"inference_ledger_entries_pkey\""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		want := `{"code":500,"data":null,"message":"internal error"}`
		if got := rec.Body.String(); got != want {
			t.Fatalf("body = %s, want %s", got, want)
		}
	})

	t.Run("wrapped internal error is redacted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		fail(c, domain.NewError(domain.CodeInternal, "settle request: connection reset by peer"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		want := `{"code":500,"data":null,"message":"internal error"}`
		if got := rec.Body.String(); got != want {
			t.Fatalf("body = %s, want %s", got, want)
		}
	})

	t.Run("client-facing errors keep only the domain message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		fail(c, domain.NewError(domain.CodeInvalidInput, "limit must be positive"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		want := `{"code":400,"data":null,"message":"limit must be positive"}`
		if got := rec.Body.String(); got != want {
			t.Fatalf("body = %s, want %s", got, want)
		}
	})

	t.Run("4xx drops the cause chain", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		fail(c, domain.WrapError(domain.CodeInvalidInput, "route references unknown model ghost",
			errors.New("pq: relation detail FKD2V88A7 vendor body {\"error\":\"upstream says no\"}")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		want := `{"code":400,"data":null,"message":"route references unknown model ghost"}`
		if got := rec.Body.String(); got != want {
			t.Fatalf("body = %s, want %s (cause chain must not leak into 4xx)", got, want)
		}
	})
}
