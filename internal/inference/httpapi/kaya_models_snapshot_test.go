package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
)

func TestKayaModels_UsesPinnedSnapshotAcrossPublishAndRefreshFailure(t *testing.T) {
	f := newOpsFixture(t)
	seedPatchRoute(t, f)
	ctx := context.Background()
	cat := catalog.NewService(f.store)
	priceAvailable := true
	cat.SetPriceCheck(func(context.Context, string) (bool, error) { return priceAvailable, nil })
	if _, err := cat.Publish(ctx, "revision A"); err != nil {
		t.Fatal(err)
	}
	cache := catalog.NewSnapshotCache(f.store, func(error) {})
	now := time.Now()
	cache.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if _, err := cache.Current(ctx); err != nil {
			t.Fatal(err)
		}
	}
	m, err := cat.GetModel(ctx, "patch-model")
	if err != nil {
		t.Fatal(err)
	}
	m.DisplayName, m.ContextTokens, m.MaxOutputTokens, m.SupportsTools = "Revision B", 2000, 200, true
	if err := cat.UpdateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Publish(ctx, "revision B"); err != nil {
		t.Fatal(err)
	}
	userID := uuid.NewString()
	f.grantOp(t, userID)
	acct, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{Name: "snapshot", Revision: 1, ModelIDs: []string{"patch-model"},
		FiveHourLimit: microP(1000)}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceGrant, SourceID: uuid.NewString(),
		ModelIDs: []string{"patch-model"}, PolicyVersionID: pol.ID,
		AnchorAt: now.Add(-time.Minute), EffectiveFrom: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.GET("/chat/models", func(c *gin.Context) { c.Set(middleware.ContextUserID, userID) },
		httpapi.NewKayaModelsHandler(cat, access.NewResolver(f.store, nil), cache, "patch-model").List)
	read := func(t *testing.T) []map[string]any {
		t.Helper()
		env := doH(t, engine, http.MethodGet, "/chat/models", nil, http.StatusOK, nil)
		var data struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data.Models
	}
	assertA := func(t *testing.T) {
		t.Helper()
		models := read(t)
		if len(models) != 1 || models[0]["display_name"] != "Patch Model" ||
			models[0]["supports_tools"] != false || models[0]["context_tokens"] != float64(1000) ||
			models[0]["max_output_tokens"] != float64(100) {
			t.Errorf("picker mixed pinned A with fresh B: %+v", models)
		}
	}
	t.Run("within cache TTL", assertA)
	// Make the newer revision unloadable: the verified A snapshot remains usable.
	if _, err := f.db.Exec(`UPDATE inference_config_revisions SET payload = '{}'::jsonb WHERE is_active`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	t.Run("verified fallback", assertA)
	priceAvailable = false
	t.Run("price check still fail closed", func(t *testing.T) {
		if models := read(t); len(models) != 0 {
			t.Fatalf("unpriced model advertised: %+v", models)
		}
	})
}
