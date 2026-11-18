package router

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/service"
)

// 路由清单权威来源：本文件通过**真实调用 router.Setup()** 装配引擎，再用
// gin 的 engine.Routes() 自省出全量 method × path。禁止复制一份静态 Go
// 路由表冒充自省 —— 那样 Users 改了路由而清单不动，漂移门禁就成了摆设。
//
// 装配时把所有条件挂载面（adminOps / accessOps / relay / usageTokenSvc …）
// 都填成非 nil 实例：Setup 只把指针存进 handler 并注册路由，不解引用依赖，
// 因此 nil 依赖是安全的。这样 engine.Routes() 拿到的是**生产可能出现的
// 全集**，而不是某个子集的缩水版。
//
// 环境变量：
//   - ROUTE_INVENTORY_OUT=<path>     输出 {"routes":[{"method","path"}]} 快照
//   - DASHBOARD_ROUTE_CATALOG=<path> 读取 dashboard 端点清单并比较覆盖，
//     发现未覆盖 / 陈旧条目时非零退出
type routeEntry struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type routeInventoryDoc struct {
	Routes []routeEntry `json:"routes"`
}

// catalogEntry 是 dashboard 端点清单里与 Users 路由绑定的一条声明。
// 字段与 dashboard/config/endpoint-catalog.json 保持一致（只取本文件需要的
// 子集；多余字段忽略）。
type catalogEntry struct {
	ID         string `json:"id"`
	Service    string `json:"service"`
	UsersRoute *struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"users_route"`
}

// catalogIgnore 是清单里显式声明「不探测」的 Users 路由，必须带原因。
type catalogIgnore struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type catalogDoc struct {
	Endpoints         []catalogEntry  `json:"endpoints"`
	UsersRouteIgnores []catalogIgnore `json:"users_route_ignores"`
}

func routeKey(method, path string) string {
	return method + " " + path
}

// buildFullRouteInventory 用真实的 Setup 装配一个引擎，并返回 engine.Routes()
// 的全量 method × path。所有条件挂载面都显式填上非 nil handler。
func buildFullRouteInventory(t *testing.T) []routeEntry {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	noopAuth := func(c *gin.Context) { c.Status(200) }

	adminOps := &httpapi.AdminOps{
		RequireModels:      noopAuth,
		RequireCredentials: noopAuth,
		RequireAdmin:       noopAuth,
		RequireBilling:     noopAuth,
		RequireUsage:       noopAuth,
		Models:             httpapi.NewAdminModelsHandler(nil),
		Credentials:        httpapi.NewAdminCredentialsHandler(nil),
		Auth:               httpapi.NewAdminAuthHandler(nil, nil),
		OAuth:              httpapi.NewAdminOAuthHandler(nil, nil, nil),
		Accounts:           httpapi.NewAdminAccountsHandler(nil),
		Adjustments:        httpapi.NewAdminAdjustmentsHandler(nil, nil, nil, nil),
		Corrections:        httpapi.NewAdminCorrectionsHandler(nil, nil, nil),
		Bulk:               httpapi.NewAdminBulkHandler(nil),
		Usage:              httpapi.NewAdminUsageHandler(nil, nil),
		PriceVersions:      httpapi.NewAdminPriceVersionsHandler(nil),
		QuotaPolicies:      httpapi.NewAdminQuotaPoliciesHandler(nil),
		Entitlements:       httpapi.NewAdminEntitlementsHandler(nil),
	}

	accessOps := &httpapi.AccessOps{
		UserAPIKeys:       httpapi.NewUserAPIKeysHandler(nil),
		V1Auth:            noopAuth,
		V1Models:          httpapi.NewModelsHandler(nil, nil),
		V1ChatCompletions: httpapi.NewChatCompletionsHandler(nil),
		V1Messages:        httpapi.NewMessagesHandler(nil),
		V1Responses:       httpapi.NewResponsesHandler(nil, nil, nil),
		UserQuotas:        httpapi.NewUserQuotasHandler(nil),
		UserUsage:         httpapi.NewUserUsageHandler(nil),
		UserSubscriptions: httpapi.NewUserSubscriptionsHandler(nil),
		UserWallet:        httpapi.NewUserWalletHandler(nil, nil),
		KayaChatModels:    httpapi.NewKayaModelsHandler(nil, nil, nil, ""),
	}

	Setup(t.Context(), engine,
		nil,                               // healthPinger
		nil, nil, nil, nil, nil, nil, nil, // repos (incl. auditLogRepo)
		nil,           // tokenSvc
		nil,           // authSvc
		nil, nil, nil, // subSvc, planSvc, paymentSvc
		nil,      // webhookVerifier
		nil,      // wechatAPIv3Key
		nil, nil, // providerTokenSvc, quoteSvc
		nil,      // chatSvc
		nil,      // chatAccessLog
		nil, nil, // githubOAuthSvc, wechatOAuthSvc
		false, // wechatOAuthMock
		false, // wechatPayMock
		"dev", // appEnv — 非生产，保证 /test/login 这类条件路由的装配面被覆盖
		nil,   // usageSvc
		service.NewUsageTokenService(nil, "day", 30, 1000), // usageTokenSvc
		httpapi.NewAdminModelsHandler(nil),                 // adminModelsHandler
		adminOps,
		accessOps,
		nil,                          // llmUsageSvc
		handler.NewRelayHandler(nil), // relayHandler
		nil,                          // adminOpsSvc
		nil,                          // adminUsersSvc
		[]string{"yundash"},
	)

	routes := engine.Routes()
	out := make([]routeEntry, 0, len(routes))
	for _, r := range routes {
		out = append(out, routeEntry{Method: r.Method, Path: r.Path})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// TestRouteInventory_Emit 在 ROUTE_INVENTORY_OUT 指向文件时输出路由快照。
// 未设置该变量时本用例直接跳过（常规 `go test ./internal/router` 不落盘）。
func TestRouteInventory_Emit(t *testing.T) {
	out := os.Getenv("ROUTE_INVENTORY_OUT")
	if out == "" {
		t.Skip("ROUTE_INVENTORY_OUT 未设置，跳过路由快照输出")
	}
	inventory := buildFullRouteInventory(t)
	doc := routeInventoryDoc{Routes: inventory}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("序列化路由快照失败: %v", err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("写入路由快照失败: %v", err)
	}
	t.Logf("已输出 %d 条路由到 %s", len(inventory), out)
}

// TestRouteInventory_CatalogCoverage 用 dashboard 端点清单检查 Users 路由
// 覆盖：DASHBOARD_ROUTE_CATALOG 指向 dashboard/config/endpoint-catalog.json。
// 未覆盖（真实路由无清单条目）与陈旧（清单条目对应路由已不存在）都必须
// 打印可读清单并让测试失败 —— 这正是漂移门禁。
func TestRouteInventory_CatalogCoverage(t *testing.T) {
	catalogPath := os.Getenv("DASHBOARD_ROUTE_CATALOG")
	if catalogPath == "" {
		t.Skip("DASHBOARD_ROUTE_CATALOG 未设置，跳过端点清单覆盖检查")
	}
	raw, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("读取端点清单失败: %v", err)
	}
	var catalog catalogDoc
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("解析端点清单失败: %v", err)
	}

	inventory := buildFullRouteInventory(t)
	actual := make(map[string]bool, len(inventory))
	for _, r := range inventory {
		actual[routeKey(r.Method, r.Path)] = true
	}

	covered := make(map[string]string) // routeKey -> catalog id
	for _, e := range catalog.Endpoints {
		if e.UsersRoute == nil {
			continue
		}
		key := routeKey(e.UsersRoute.Method, e.UsersRoute.Path)
		if prev, dup := covered[key]; dup {
			t.Errorf("端点清单重复登记 Users 路由 %s（%s 与 %s）", key, prev, e.ID)
		}
		covered[key] = e.ID
	}

	ignored := make(map[string]string) // routeKey -> reason
	for _, ig := range catalog.UsersRouteIgnores {
		key := routeKey(ig.Method, ig.Path)
		if strings.TrimSpace(ig.Reason) == "" {
			t.Errorf("清单忽略项 %s 缺少 reason（忽略规则必须逐条给出原因）", key)
		}
		ignored[key] = ig.Reason
	}

	var uncovered, stale []string
	for _, r := range inventory {
		key := routeKey(r.Method, r.Path)
		if _, ok := covered[key]; ok {
			continue
		}
		if _, ok := ignored[key]; ok {
			continue
		}
		uncovered = append(uncovered, key)
	}
	for key, id := range covered {
		if !actual[key] {
			stale = append(stale, fmt.Sprintf("%s (清单条目 %s)", key, id))
		}
	}
	sort.Strings(uncovered)
	sort.Strings(stale)

	if len(uncovered) > 0 {
		t.Errorf("以下 Users 路由未被端点清单覆盖（%d 条）:\n  %s",
			len(uncovered), strings.Join(uncovered, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("以下端点清单条目对应的 Users 路由已不存在（%d 条）:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
	for key, reason := range ignored {
		if !actual[key] {
			t.Errorf("清单忽略项 %s 在实际路由中不存在（忽略规则会变成永久豁免）", key)
		}
		_ = reason
	}
}
