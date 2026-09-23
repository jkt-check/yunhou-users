package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
)

// review_round1_internal_test.go — 评审轮1 批次5 修复的纯单测（无需 DB）：
// reasoning_content 映射、parseLimit 上限、strictBindJSON 尾部数据、
// 413 超限、/chat/models 与 /user/wallet 的错误不伪装零态、调整幂等键
// 重放的载荷一致性校验。

// --- I-1：入站 assistant 消息的 reasoning_content 不再被丢弃 ---------------

func TestParseChatCompletions_ReasoningContentMapped(t *testing.T) {
	// 多轮 thinking + 工具调用会话：DeepSeek 要求 tool-call assistant 轮
	// 原样回传 reasoning_content，否则上游 400。
	body := []byte(`{
	  "model": "deepseek-reasoner",
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "content": "", "reasoning_content": "chain-of-thought…",
	     "tool_calls": [{"id":"call_1","type":"function","function":{"name":"run","arguments":"{}"}}]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "tool out"},
	    {"role": "assistant", "content": "final answer", "reasoning_content": "second chain…"},
	    {"role": "user", "content": "continue"}
	  ]
	}`)
	req, err := parseChatCompletions(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(req.Messages))
	}
	asst := req.Messages[1]
	if asst.ReasoningContent != "chain-of-thought…" {
		t.Errorf("reasoning_content dropped: %q", asst.ReasoningContent)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Errorf("tool_calls = %+v", asst.ToolCalls)
	}
	if req.Messages[3].ReasoningContent != "second chain…" {
		t.Errorf("second reasoning turn = %q", req.Messages[3].ReasoningContent)
	}
	// 未携带 reasoning_content 的轮次保持零值（上行 omitempty 逐字节兼容）。
	for _, i := range []int{0, 2, 4} {
		if req.Messages[i].ReasoningContent != "" {
			t.Errorf("message %d unexpectedly carries reasoning_content %q",
				i, req.Messages[i].ReasoningContent)
		}
	}
}

// --- m3：parseLimit 钳到硬上限 --------------------------------------------

func TestParseLimit_ClampedToMax(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mk := func(query string) *gin.Context {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/x"+query, nil)
		return c
	}
	if got := parseLimit(mk("?limit=999999999"), 100); got != adminListMaxLimit {
		t.Errorf("limit=999999999 → %d, want clamp %d", got, adminListMaxLimit)
	}
	if got := parseLimit(mk("?limit=501"), 100); got != adminListMaxLimit {
		t.Errorf("limit=501 → %d, want clamp %d", got, adminListMaxLimit)
	}
	if got := parseLimit(mk("?limit=7"), 100); got != 7 {
		t.Errorf("limit=7 → %d, want 7", got)
	}
	if got := parseLimit(mk("?limit=abc"), 100); got != 100 {
		t.Errorf("invalid limit → %d, want default 100", got)
	}
	if got := parseLimit(mk(""), 100); got != 100 {
		t.Errorf("absent limit → %d, want default 100", got)
	}
}

// --- m4：strictBindJSON 拒绝尾部数据 ---------------------------------------

func TestStrictBindJSON_RejectsTrailingData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mk := func(body string) *gin.Context {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		return c
	}
	var dst struct {
		UserID string `json:"user_id"`
	}
	if err := strictBindJSON(mk(`{"user_id":"u"} trailing-garbage`), &dst); err == nil {
		t.Error("trailing garbage must be rejected")
	}
	if err := strictBindJSON(mk(`{"user_id":"u"}{"user_id":"v"}`), &dst); err == nil {
		t.Error("a second JSON value must be rejected")
	}
	if err := strictBindJSON(mk("{\"user_id\":\"u\"} \n\t "), &dst); err != nil {
		t.Errorf("whitespace-only tail is legal: %v", err)
	}
	if err := strictBindJSON(mk(`{"user_id":"u"}`), &dst); err != nil {
		t.Errorf("clean body: %v", err)
	}
}

// --- m7：MaxBytesReader 超限 → 413（三个协议面） ----------------------------

func TestV1Handlers_BodyTooLargeIs413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	big := strings.Repeat("x", v1ChatMaxBodyBytes+1)
	principal := &domain.Principal{Kind: domain.PrincipalAPIKey, BillingAccountID: "acct", APIKeyID: "key"}

	mount := func(h gin.HandlerFunc) *gin.Engine {
		engine := gin.New()
		engine.Use(func(c *gin.Context) {
			c.Set(ContextCallerPrincipal, principal)
			c.Next()
		})
		engine.POST("/v1/x", h)
		return engine
	}
	call := func(engine *gin.Engine) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(big))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	// nil Gateway 是安全的：超限在读体阶段即返回，到不了网关。
	cases := map[string]gin.HandlerFunc{
		"chat/completions": (&ChatCompletionsHandler{}).Create,
		"messages":         (&MessagesHandler{}).Create,
		"responses":        NewResponsesHandler(nil, nil, nil).Create,
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			w := call(mount(h))
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: %s", w.Code, w.Body.String())
			}
		})
	}
}

// --- m5：/chat/models 非 NotFound 错误不再伪装成空列表 ----------------------

// stubResolverStore 实现 access.KeyStore；本测试只走 GetBillingAccountByUser。
type stubResolverStore struct {
	accountErr error
}

func (s stubResolverStore) EnsureBillingAccount(context.Context, string) (*domain.BillingAccount, error) {
	return nil, s.accountErr
}
func (s stubResolverStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	return nil, s.accountErr
}
func (s stubResolverStore) GetBillingAccountByID(context.Context, string) (*domain.BillingAccount, error) {
	return nil, s.accountErr
}
func (s stubResolverStore) ListActiveEntitlements(context.Context, string, time.Time) ([]domain.Entitlement, error) {
	return nil, nil
}
func (s stubResolverStore) InsertAPIKey(context.Context, *domain.APIKey, string) error { return nil }
func (s stubResolverStore) GetAPIKeyByPrefix(context.Context, string) (*domain.APIKey, string, error) {
	return nil, "", s.accountErr
}
func (s stubResolverStore) GetAPIKeyByID(context.Context, string) (*domain.APIKey, error) {
	return nil, s.accountErr
}
func (s stubResolverStore) ListAPIKeysByAccount(context.Context, string, int, int) ([]domain.APIKey, int64, error) {
	return nil, 0, nil
}
func (s stubResolverStore) UpdateAPIKey(context.Context, *domain.APIKey) error { return nil }
func (s stubResolverStore) RevokeAPIKey(context.Context, string, time.Time) (bool, error) {
	return false, nil
}
func (s stubResolverStore) MarkAPIKeyExpired(context.Context, string) error { return nil }
func (s stubResolverStore) TouchAPIKeyLastUsed(context.Context, string, time.Time) error {
	return nil
}

func TestKayaModels_InternalErrorIs500NotEmptyList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	call := func(storeErr error) *httptest.ResponseRecorder {
		engine := gin.New()
		engine.Use(func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "u1")
			c.Next()
		})
		// 解析失败时不会触达 catalog，nil 安全。
		h := NewKayaModelsHandler(nil, access.NewResolver(stubResolverStore{accountErr: storeErr}, nil), "")
		engine.GET("/chat/models", h.List)
		req := httptest.NewRequest(http.MethodGet, "/chat/models", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	// DB 故障 → 500，不得渲染成「用户无模型」。
	if w := call(errors.New("db connection reset")); w.Code != http.StatusInternalServerError {
		t.Fatalf("db failure = %d, want 500: %s", w.Code, w.Body.String())
	}
	// 账户停用（invalid_key）同样不得伪装成空列表。
	if w := call(domain.NewError(domain.CodeInvalidKey, "billing account is not active")); w.Code != http.StatusInternalServerError {
		t.Fatalf("suspended account = %d, want 500: %s", w.Code, w.Body.String())
	}
	// 无计费账户（NotFound）→ 合法的 200 空列表。
	w := call(domain.NewError(domain.CodeNotFound, "no billing account"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":0`) {
		t.Fatalf("no account = %d, want 200 code:0: %s", w.Code, w.Body.String())
	}
}

// --- m6：/user/wallet 的 PAYG 查询错误不再吞成 enabled=false -----------------

type stubUserWalletStore struct {
	acct   *domain.BillingAccount
	ent    *domain.Entitlement
	entErr error
}

func (s *stubUserWalletStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	return s.acct, nil
}
func (s *stubUserWalletStore) EnsureBillingAccount(context.Context, string) (*domain.BillingAccount, error) {
	return s.acct, nil
}
func (s *stubUserWalletStore) Begin(context.Context) (domain.UnitOfWork, error) { return nil, nil }
func (s *stubUserWalletStore) ListWalletBalances(context.Context, string, time.Time) ([]postgres.WalletBalanceView, error) {
	return nil, nil
}
func (s *stubUserWalletStore) WalletBalance(context.Context, string, string, time.Time) (*postgres.WalletBalanceView, error) {
	return nil, nil
}
func (s *stubUserWalletStore) ListWalletEntries(context.Context, string, string, int64, int) ([]postgres.WalletEntry, error) {
	return nil, nil
}
func (s *stubUserWalletStore) SetOverageTx(context.Context, domain.UnitOfWork, string, string, bool, *int64, string) (*postgres.Wallet, error) {
	return nil, nil
}
func (s *stubUserWalletStore) EnsurePAYGEntitlementTx(context.Context, domain.UnitOfWork, string, time.Time) (*domain.Entitlement, error) {
	return nil, nil
}
func (s *stubUserWalletStore) GetLatestEntitlementBySource(context.Context, domain.EntitlementSource, string) (*domain.Entitlement, error) {
	return s.ent, s.entErr
}

func TestUserWallet_PAYGLookupErrorIs500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	call := func(store *stubUserWalletStore) *httptest.ResponseRecorder {
		engine := gin.New()
		engine.Use(func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "u1")
			c.Next()
		})
		NewUserWalletHandler(store, nil).Register(engine.Group("/user"))
		req := httptest.NewRequest(http.MethodGet, "/user/wallet", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}
	acct := &domain.BillingAccount{ID: "acct-1", UserID: "u1", Status: "active"}

	// DB 故障 → 500，不得渲染成 payg.enabled=false。
	if w := call(&stubUserWalletStore{acct: acct, entErr: errors.New("db down")}); w.Code != http.StatusInternalServerError {
		t.Fatalf("db failure = %d, want 500: %s", w.Code, w.Body.String())
	}
	// 无 PAYG 权益（NotFound）→ 200 enabled=false 零态。
	w := call(&stubUserWalletStore{acct: acct, entErr: domain.NewError(domain.CodeNotFound, "none")})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("no entitlement = %d, want 200 enabled=false: %s", w.Code, w.Body.String())
	}
	// 活跃 PAYG 权益 → enabled=true。
	w = call(&stubUserWalletStore{acct: acct, ent: &domain.Entitlement{
		ID: "ent-1", Status: domain.EntitlementActive, ModelIDs: []string{"m1"},
	}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("active entitlement = %d, want enabled=true: %s", w.Code, w.Body.String())
	}
}

// --- m9：调整幂等键重放必须载荷一致，不一致 → 409 ----------------------------

type stubAdjUow struct{ committed, rolledBack bool }

func (u *stubAdjUow) Commit(context.Context) error   { u.committed = true; return nil }
func (u *stubAdjUow) Rollback(context.Context) error { u.rolledBack = true; return nil }

type stubAdminWalletStore struct {
	acct     *domain.BillingAccount
	applyErr error
	stored   *postgres.Adjustment
	uow      *stubAdjUow
}

func (s *stubAdminWalletStore) Begin(context.Context) (domain.UnitOfWork, error) { return s.uow, nil }
func (s *stubAdminWalletStore) GetBillingAccountByUser(context.Context, string) (*domain.BillingAccount, error) {
	return s.acct, nil
}
func (s *stubAdminWalletStore) EnsureBillingAccount(context.Context, string) (*domain.BillingAccount, error) {
	return s.acct, nil
}
func (s *stubAdminWalletStore) GetBillingAccountByID(context.Context, string) (*domain.BillingAccount, error) {
	return s.acct, nil
}
func (s *stubAdminWalletStore) WalletBalance(context.Context, string, string, time.Time) (*postgres.WalletBalanceView, error) {
	return nil, nil
}
func (s *stubAdminWalletStore) ListWalletEntries(context.Context, string, string, int64, int) ([]postgres.WalletEntry, error) {
	return nil, nil
}
func (s *stubAdminWalletStore) ApplyWalletAdjustmentTx(context.Context, domain.UnitOfWork, postgres.WalletAdjustmentCommand) (*postgres.Adjustment, error) {
	return nil, s.applyErr
}
func (s *stubAdminWalletStore) GetAdjustmentByIdempotencyKey(context.Context, string) (*postgres.Adjustment, error) {
	return s.stored, nil
}
func (s *stubAdminWalletStore) ReverseWalletEntryTx(context.Context, domain.UnitOfWork, string, int64, string) error {
	return nil
}
func (s *stubAdminWalletStore) GetPAYGConfig(context.Context) (*postgres.PAYGConfig, error) {
	return nil, nil
}
func (s *stubAdminWalletStore) PutPAYGConfig(context.Context, string, []string, string) (*postgres.PAYGConfig, error) {
	return nil, nil
}

func TestAdminAdjustments_ReplayValidatesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	acct := &domain.BillingAccount{ID: "acct-1", UserID: "u1", Status: "active"}
	store := &stubAdminWalletStore{
		acct:     acct,
		uow:      &stubAdjUow{},
		applyErr: domain.NewError(domain.CodeConflict, "duplicate idempotency key"),
		stored: &postgres.Adjustment{
			ID: "adj-1", BillingAccountID: "acct-1", AmountMicros: 100,
			Direction: "credit", Currency: "CNY", CreatedAt: time.Now(),
		},
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		setOperatorContext(c, "op-1", "app-1", []string{"admin"})
		c.Next()
	})
	NewAdminAdjustmentsHandler(store, nil, nil, nil).Register(engine.Group("/admin"))

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/wallet/adjustments", strings.NewReader(body))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}
	base := `{"billing_account_id":"acct-1","currency":"CNY","source":"bonus","direction":"credit","amount_micros":"100","reason":"r","idempotency_key":"k1"}`

	// 同载荷重放 → 200 applied=false（良性重放）。
	w := call(base)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Fatalf("identical replay = %d, want 200 applied=false: %s", w.Code, w.Body.String())
	}
	// 不同金额复用同键 → 409。
	w = call(strings.Replace(base, `"amount_micros":"100"`, `"amount_micros":"200"`, 1))
	if w.Code != http.StatusConflict {
		t.Fatalf("amount mismatch = %d, want 409: %s", w.Code, w.Body.String())
	}
	// 不同方向复用同键 → 409。
	w = call(strings.Replace(base, `"direction":"credit"`, `"direction":"debit"`, 1))
	if w.Code != http.StatusConflict {
		t.Fatalf("direction mismatch = %d, want 409: %s", w.Code, w.Body.String())
	}
	// 不同账户复用同键 → 409。
	store.stored.BillingAccountID = "acct-other"
	w = call(base)
	if w.Code != http.StatusConflict {
		t.Fatalf("account mismatch = %d, want 409: %s", w.Code, w.Body.String())
	}
}
