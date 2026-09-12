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
// domain 错误照常透传 message（客户端可操作的输入错误）。
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

	t.Run("client-facing errors keep their message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		fail(c, domain.NewError(domain.CodeInvalidInput, "limit must be positive"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		want := `{"code":400,"data":null,"message":"inference/invalid_input: limit must be positive"}`
		if got := rec.Body.String(); got != want {
			t.Fatalf("body = %s, want %s", got, want)
		}
	})
}
