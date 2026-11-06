package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/middleware"
)

// apikey_auth_edges_test.go — Task 16 覆盖率补强：facade 用户会话主体中
// 间件（JWT → principal）的三分支：无账户 403 / 正常解析 / 账户停用 403。

func TestUserSessionPrincipal_Branches(t *testing.T) {
	f := newAccessFixture(t)
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

// T6：/v1 鉴权链的错误同样按**实际入口协议**渲染 —— OpenAI 入口是
// error 对象，Anthropic 入口是原生结构；两者都不得带管理 envelope 的
// 顶层 code（设计 §9.1）。
func TestV1AuthChain_NativeErrorShapePerEntrypoint(t *testing.T) {
	f := newProtoFixture(t)
	cases := []struct {
		name      string
		path      string
		body      map[string]any
		anthropic bool
	}{
		{"chat_completions", "/v1/chat/completions", map[string]any{
			"model":    "glm-4.6",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, false},
		{"messages", "/v1/messages", map[string]any{
			"model":      "glm-4.6",
			"max_tokens": 64,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		}, true},
		{"responses", "/v1/responses", map[string]any{"model": "glm-4.6", "input": "hi"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.call(t, http.MethodPost, tc.path, "", tc.body)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			var doc map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if _, leaked := doc["code"]; leaked {
				t.Errorf("management envelope leaked into %s: %s", tc.path, w.Body.String())
			}
			errObj, ok := doc["error"].(map[string]any)
			if !ok {
				t.Fatalf("native error object missing: %s", w.Body.String())
			}
			if tc.anthropic {
				if doc["type"] != "error" || errObj["type"] != "authentication_error" {
					t.Errorf("anthropic auth error = %v/%v: %s", doc["type"], errObj["type"], w.Body.String())
				}
				return
			}
			if errObj["type"] != "authentication_error" || errObj["code"] != "invalid_api_key" {
				t.Errorf("auth error = %v: %s", errObj, w.Body.String())
			}
		})
	}
}
