package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// payment_error_edges_test.go — Task 16 覆盖率补强：writePaymentError 的
// 错误→HTTP 映射矩阵（支付 handler 的 envelope 契约）。

func TestWritePaymentError_MappingMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		err  error
		want int
	}{
		{service.ErrPlanNotFound, http.StatusBadRequest},
		{service.ErrPlanInactive, http.StatusBadRequest},
		{service.ErrPlanNotAcceptingNew, http.StatusConflict},
		{service.ErrPlanCurrencyMismatch, http.StatusBadRequest},
		{service.ErrUserHasActiveSub, http.StatusConflict},
		{service.ErrPlanDowngrade, http.StatusConflict},
		{service.ErrPlanNotPurchasable, http.StatusBadRequest},
		{service.ErrPlanUpgradeNotConfigured, http.StatusConflict},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		writePaymentError(c, tc.err)
		if c.Writer.Status() != tc.want {
			t.Errorf("%v → %d, want %d", tc.err, c.Writer.Status(), tc.want)
		}
	}
}
