package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// messages_internal_test.go — writeAnthropicDomainError 的钱包门控映射：
// plain *domain.Error 携带的 CodeQuotaExceeded / CodeInsufficientBalance
// 必须与 chat 平面同口径答复 429 rate_limit_error（而非 default 500），
// CodeConflict 对齐 409（Task 14 设计意图）。

func TestWriteAnthropicDomainError_WalletGates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{"overage disabled", accounting.ErrOverageDisabled, http.StatusTooManyRequests, "rate_limit_error"},
		{"spend limit exceeded", accounting.ErrSpendLimitExceeded, http.StatusTooManyRequests, "rate_limit_error"},
		{"insufficient balance", accounting.ErrInsufficientBalance, http.StatusTooManyRequests, "rate_limit_error"},
		{"conflict", domain.NewError(domain.CodeConflict, "settle wallet: hold already transitioned"),
			http.StatusConflict, "invalid_request_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			writeAnthropicDomainError(c, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var doc struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if doc.Type != "error" || doc.Error.Type != tc.wantType {
				t.Fatalf("body = %s, want error/%s", rec.Body.String(), tc.wantType)
			}
			if doc.Error.Message == "" || doc.Error.Message == "internal error" {
				t.Fatalf("wallet gate rejections must surface the domain message, got %q", doc.Error.Message)
			}
		})
	}
}
