package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/middleware"
)

// apikey_auth_edges_test.go — Task 16 覆盖率补强：facade 用户会话主体中
// 间件（JWT → principal）的三分支：无账户 403 / 正常解析 / 账户停用 403。

func TestUserSessionPrincipal_Branches(t *testing.T) {
	f := newAccessFixture(t, 0)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		c.Next()
	}, httpapi.UserSessionPrincipal(access.NewResolver(f.store, nil)))
	engine.GET("/facade/probe", func(c *gin.Context) {
		p := httpapi.CallerPrincipalOf(c)
		if p == nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{"account": p.BillingAccountID})
	})
	call := func(userID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/facade/probe", nil)
		req.Header.Set("X-Test-User", userID)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	// 无计费账户 → 403（读路径不建户）。
	userID, _ := f.addUser(t)
	if w := call(userID); w.Code != http.StatusForbidden {
		t.Fatalf("no-account = %d %s, want 403", w.Code, w.Body.String())
	}

	// 有账户（含权益）→ 200 且 principal 设置。
	acct, err := f.store.EnsureBillingAccount(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if w := call(userID); w.Code != http.StatusOK {
		t.Fatalf("resolved = %d %s, want 200", w.Code, w.Body.String())
	}
	_ = acct

	// 账户停用 → 403。
	if _, err := f.db.Exec(`UPDATE inference_billing_accounts SET status = 'suspended'`); err != nil {
		t.Fatal(err)
	}
	if w := call(userID); w.Code != http.StatusForbidden {
		t.Fatalf("suspended = %d %s, want 403", w.Code, w.Body.String())
	}
}
