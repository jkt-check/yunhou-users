package catalog_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

func TestListPublishedModelsFromSnapshot_PinnedAndFailClosed(t *testing.T) {
	ctx := context.Background()
	// No store: listing a supplied snapshot must never reload the active revision.
	svc := catalog.NewService(nil)
	snap := &catalog.Snapshot{
		Models: map[string]domain.Model{
			"z":        {ID: "z", DisplayName: "Pinned Z", Lifecycle: domain.LifecycleActive},
			"a":        {ID: "a", DisplayName: "Pinned A", Lifecycle: domain.LifecycleActive},
			"draft":    {ID: "draft", Lifecycle: domain.LifecycleDraft},
			"unrouted": {ID: "unrouted", Lifecycle: domain.LifecycleActive},
			"unpriced": {ID: "unpriced", Lifecycle: domain.LifecycleActive},
			"denied":   {ID: "denied", Lifecycle: domain.LifecycleActive},
		},
		Providers: map[string]domain.Provider{"p": {ID: "p", Status: "active"}},
		Deployments: map[string]domain.Deployment{
			"d": {ID: "d", ProviderID: "p", Status: domain.DeploymentActive},
		},
		RoutesByModel: make(map[string][]domain.ModelRoute),
	}
	for _, id := range []string{"z", "a", "draft", "unpriced", "denied"} {
		snap.RoutesByModel[id] = []domain.ModelRoute{{
			ModelID: id, DeploymentID: "d", Enabled: true, PoolStrategy: domain.PoolRoundRobin,
		}}
	}
	allow := func(context.Context, string) (bool, error) { return true, nil }
	if _, err := svc.ListPublishedModelsFromSnapshot(ctx, nil, allow); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("nil snapshot: %v", err)
	}
	if _, err := svc.ListPublishedModelsFromSnapshot(ctx, snap, nil); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("nil access hook: %v", err)
	}
	models, err := svc.ListPublishedModelsFromSnapshot(ctx, snap, allow)
	if err != nil || len(models) != 0 {
		t.Fatalf("missing price hook must deny: models=%+v err=%v", models, err)
	}

	var priced, authorized []string
	svc.SetPriceCheck(func(_ context.Context, id string) (bool, error) {
		priced = append(priced, id)
		return id != "unpriced", nil
	})
	models, err = svc.ListPublishedModelsFromSnapshot(ctx, snap,
		func(_ context.Context, id string) (bool, error) {
			authorized = append(authorized, id)
			return id != "denied", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "a" || models[0].DisplayName != "Pinned A" ||
		models[1].ID != "z" || models[1].DisplayName != "Pinned Z" {
		t.Fatalf("pinned, filtered, sorted list: %+v", models)
	}
	if !reflect.DeepEqual(priced, []string{"a", "denied", "unpriced", "z"}) ||
		!reflect.DeepEqual(authorized, []string{"a", "denied", "z"}) {
		t.Fatalf("sellability/price/access gates: price=%v access=%v", priced, authorized)
	}
	hookErr := errors.New("hook unavailable")
	svc.SetPriceCheck(func(context.Context, string) (bool, error) { return false, hookErr })
	if models, err := svc.ListPublishedModelsFromSnapshot(ctx, snap, allow); !errors.Is(err, hookErr) || len(models) != 0 {
		t.Fatalf("price error must fail closed: models=%+v err=%v", models, err)
	}
	svc.SetPriceCheck(allow)
	if models, err := svc.ListPublishedModelsFromSnapshot(ctx, snap,
		func(context.Context, string) (bool, error) { return false, hookErr }); !errors.Is(err, hookErr) || len(models) != 0 {
		t.Fatalf("access error must fail closed: models=%+v err=%v", models, err)
	}
}
