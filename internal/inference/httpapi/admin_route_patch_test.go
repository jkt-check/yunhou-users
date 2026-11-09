package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

func seedPatchRoute(t *testing.T, f *opsFixture) *domain.ModelRoute {
	t.Helper()
	ctx := context.Background()
	if err := f.store.InsertModel(ctx, &domain.Model{
		ID: "patch-model", DisplayName: "Patch Model", Lifecycle: domain.LifecycleActive,
		ContextTokens: 1000, MaxOutputTokens: 100,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolKayaChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}); err != nil {
		t.Fatal(err)
	}
	p := &domain.Provider{Code: "patch-provider", DisplayName: "Patch Provider",
		AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := f.store.InsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	d := &domain.Deployment{ProviderID: p.ID, UpstreamModel: "upstream",
		BaseURL: "https://api.example.com", Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: time.Second, RequestTimeout: time.Second, Status: domain.DeploymentActive}
	if err := f.store.InsertDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	r := &domain.ModelRoute{ModelID: "patch-model", DeploymentID: d.ID,
		Priority: 7, Weight: 2, Capabilities: []string{"tools"},
		PoolStrategy: domain.PoolRoundRobin, Enabled: true}
	if err := f.store.InsertRoute(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUpdateRoute_PatchPreservesAbsentFields(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	r := seedPatchRoute(t, f)
	env := doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, map[string]any{
		"weight": 5, "updated_at": r.UpdatedAt, "reason": "adjust weight",
	}, http.StatusOK, f.headers(op))
	var got struct {
		Priority     int      `json:"priority"`
		Weight       int      `json:"weight"`
		Enabled      bool     `json:"enabled"`
		Capabilities []string `json:"capabilities"`
		PoolStrategy string   `json:"pool_strategy"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Priority != 7 || got.Weight != 5 || !got.Enabled ||
		!reflect.DeepEqual(got.Capabilities, []string{"tools"}) || got.PoolStrategy != "round_robin" {
		t.Fatalf("weight-only PATCH lost existing fields: %+v", got)
	}
	saved, err := f.store.GetRoute(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Priority != 7 || !saved.Enabled || !reflect.DeepEqual(saved.Capabilities, []string{"tools"}) {
		t.Fatalf("persisted route lost restrictions: %+v", saved)
	}
	doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, map[string]any{
		"priority": nil, "weight": nil, "enabled": nil, "capabilities": nil, "pool_strategy": nil,
		"updated_at": saved.UpdatedAt, "reason": "null preserves fields",
	}, http.StatusOK, f.headers(op))
	saved, err = f.store.GetRoute(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Priority != 7 || saved.Weight != 5 || !saved.Enabled ||
		!reflect.DeepEqual(saved.Capabilities, []string{"tools"}) || saved.PoolStrategy != domain.PoolRoundRobin {
		t.Fatalf("null PATCH lost existing fields: %+v", saved)
	}
	doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, map[string]any{
		"priority": 0, "enabled": false, "capabilities": []string{},
		"updated_at": saved.UpdatedAt, "reason": "explicit clear",
	}, http.StatusOK, f.headers(op))
	saved, err = f.store.GetRoute(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Priority != 0 || saved.Enabled || len(saved.Capabilities) != 0 || saved.Weight != 5 {
		t.Fatalf("explicit zero/false/empty list not applied: %+v", saved)
	}
}

func TestUpdateRoute_RequiresClientVersion(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	r := seedPatchRoute(t, f)
	for _, token := range []any{nil, time.Time{}} {
		body := map[string]any{"priority": 7, "weight": 5, "enabled": true, "reason": "adjust"}
		if token != nil {
			body["updated_at"] = token
		}
		doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, body,
			http.StatusBadRequest, f.headers(op))
	}
	saved, err := f.store.GetRoute(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := f.db.Get(&audits, `SELECT count(*) FROM inference_audit_log`); err != nil {
		t.Fatal(err)
	}
	if saved.Weight != 2 || !saved.UpdatedAt.Equal(r.UpdatedAt) || audits != 0 {
		t.Fatalf("invalid version mutated route/audit: %+v, audits=%d", saved, audits)
	}
	doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, map[string]any{
		"weight": 5, "updated_at": r.UpdatedAt, "reason": "valid",
	}, http.StatusOK, f.headers(op))
	doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, map[string]any{
		"weight": 9, "updated_at": r.UpdatedAt, "reason": "stale",
	}, http.StatusConflict, f.headers(op))
	saved, err = f.store.GetRoute(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Weight != 5 {
		t.Fatalf("stale write changed weight: %+v", saved)
	}
}

func TestUpdateRoute_RejectsImmutableIdentity(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	r := seedPatchRoute(t, f)
	for _, field := range []string{"deployment_id", "model_id"} {
		doH(t, f.engine, http.MethodPatch, "/adminm/routes/"+r.ID, map[string]any{
			field: uuid.NewString(), "weight": 2, "updated_at": r.UpdatedAt, "reason": "rebind",
		}, http.StatusBadRequest, f.headers(op))
	}
}
