package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// seedProviderModelDeployment inserts the minimal sellable chain:
// provider + active model + active deployment + enabled route.
func seedProviderModelDeployment(t *testing.T, svc *catalog.Service, modelID string) {
	t.Helper()
	ctx := context.Background()
	prov := &domain.Provider{Code: "prov-" + strings.ReplaceAll(modelID, ".", "-"), DisplayName: "Prov", AccessType: domain.AccessOfficialAPI}
	if err := svc.CreateProvider(ctx, prov); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	m := sampleModel(modelID)
	if err := svc.CreateModel(ctx, &m); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := svc.SetModelLifecycle(ctx, modelID, domain.LifecycleActive); err != nil {
		t.Fatalf("activate model: %v", err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "upstream-" + modelID,
		BaseURL:  "https://api." + modelID + ".example.com/v1",
		Protocol: domain.ProtocolOpenAIChat, Status: domain.DeploymentActive,
	}
	if err := svc.CreateDeployment(ctx, dep); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	route := &domain.ModelRoute{ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true}
	if err := svc.CreateRoute(ctx, route); err != nil {
		t.Fatalf("create route: %v", err)
	}
}

func TestModelCRUDAndPagination(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		m := sampleModel(fmt.Sprintf("model-%02d", i))
		if err := svc.CreateModel(ctx, &m); err != nil {
			t.Fatalf("create model %d: %v", i, err)
		}
		if m.Lifecycle != domain.LifecycleDraft {
			t.Errorf("new model lifecycle = %s, want draft (新模型默认不可售)", m.Lifecycle)
		}
	}

	// Pagination by keyset cursor.
	page1, err := svc.ListModels(ctx, domain.ModelFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID != "model-00" || page1[1].ID != "model-01" {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := svc.ListModels(ctx, domain.ModelFilter{AfterID: page1[1].ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID != "model-02" {
		t.Fatalf("page2 = %+v", page2)
	}

	// Lifecycle filter.
	active := domain.LifecycleActive
	filtered, err := svc.ListModels(ctx, domain.ModelFilter{Lifecycle: &active})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Errorf("draft models leaked into active filter: %+v", filtered)
	}

	// Update + read back.
	m, err := svc.GetModel(ctx, "model-00")
	if err != nil {
		t.Fatal(err)
	}
	m.DisplayName = "Renamed"
	if err := svc.UpdateModel(ctx, m); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := svc.GetModel(ctx, "model-00")
	if err != nil {
		t.Fatal(err)
	}
	if again.DisplayName != "Renamed" {
		t.Errorf("display name = %q", again.DisplayName)
	}

	// Validation gate.
	bad := sampleModel("Bad ID!")
	if err := svc.CreateModel(ctx, &bad); err == nil {
		t.Error("invalid model id must be rejected")
	}
}

func TestConcurrentEditsProduceVersionConflict(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	m := sampleModel("conflict-model")
	if err := svc.CreateModel(ctx, &m); err != nil {
		t.Fatal(err)
	}
	base, err := svc.GetModel(ctx, "conflict-model")
	if err != nil {
		t.Fatal(err)
	}

	// Ten editors all read the same base and write concurrently: exactly
	// one may win; the rest must get CodeConflict, never silent overwrite.
	const editors = 10
	var wg sync.WaitGroup
	wins := make(chan error, editors)
	for i := 0; i < editors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			candidate := *base
			candidate.DisplayName = fmt.Sprintf("edit-%d", i)
			wins <- svc.UpdateModel(ctx, &candidate)
		}(i)
	}
	wg.Wait()
	close(wins)
	var won, conflicted int
	for err := range wins {
		if err == nil {
			won++
			continue
		}
		if domain.CodeOf(err) == domain.CodeConflict {
			conflicted++
			continue
		}
		t.Errorf("unexpected error: %v", err)
	}
	if won != 1 || conflicted != editors-1 {
		t.Errorf("won=%d conflicted=%d, want 1/%d", won, conflicted, editors-1)
	}
}

func TestDeploymentOptimisticLockAndRouteGuards(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	prov := &domain.Provider{Code: "dep-prov", DisplayName: "P", AccessType: domain.AccessOfficialAPI}
	if err := svc.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "m", BaseURL: "https://api.example.com/v1",
		Protocol: domain.ProtocolOpenAIChat,
	}
	if err := svc.CreateDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if dep.ConfigVersion != 1 {
		t.Fatalf("initial config_version = %d, want 1", dep.ConfigVersion)
	}

	dep.Region = "us-east-1"
	if err := svc.UpdateDeployment(ctx, dep); err != nil {
		t.Fatalf("update: %v", err)
	}
	if dep.ConfigVersion != 2 {
		t.Errorf("config_version after update = %d, want 2", dep.ConfigVersion)
	}

	// Stale token → conflict.
	stale := *dep
	stale.ConfigVersion = 1
	stale.Region = "eu-west-1"
	if err := svc.UpdateDeployment(ctx, &stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("stale config_version: got %v, want conflict", err)
	}

	// Invalid protocol and metadata address are rejected.
	bad := *dep
	bad.ConfigVersion = dep.ConfigVersion
	bad.Protocol = domain.ProtocolKayaChat
	if err := svc.UpdateDeployment(ctx, &bad); err == nil {
		t.Error("kaya_chat on a deployment must be rejected")
	}
	bad = *dep
	bad.ConfigVersion = dep.ConfigVersion
	bad.BaseURL = "https://169.254.169.254/latest"
	if err := svc.UpdateDeployment(ctx, &bad); err == nil {
		t.Error("metadata address must be rejected")
	}
}

// 对抗评审 C1（管理面写闸）：request_timeout 达到/超过生效 recovery grace
// 的 deployment 写入必须被拒绝——否则超 grace 的活请求会被恢复扫描全额误
// 结算，真实用量随后被终态守卫吞掉。
func TestDeploymentWriteRejectsTimeoutBeyondRecoveryGrace(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	prov := &domain.Provider{Code: "grace-prov", DisplayName: "P", AccessType: domain.AccessOfficialAPI}
	if err := svc.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	over := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "m", BaseURL: "https://api.example.com/v1",
		Protocol: domain.ProtocolOpenAIChat, RequestTimeout: 16 * time.Minute, // > 默认 grace 15m
	}
	if err := svc.CreateDeployment(ctx, over); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("create with timeout above grace: err = %v, want invalid_input", err)
	}

	// 合法超时照常写入；随后向超 grace 方向的 Update 同样被拒。
	ok := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "m", BaseURL: "https://api.example.com/v1",
		Protocol: domain.ProtocolOpenAIChat, RequestTimeout: 5 * time.Minute,
	}
	if err := svc.CreateDeployment(ctx, ok); err != nil {
		t.Fatalf("create within grace: %v", err)
	}
	ok.RequestTimeout = 20 * time.Minute
	if err := svc.UpdateDeployment(ctx, ok); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("update raising timeout above grace: err = %v, want invalid_input", err)
	}
	// 默认 request_timeout（600s）在 grace 地板（12m）之下：默认路径永不被误伤。
	def := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "m2", BaseURL: "https://api2.example.com/v1",
		Protocol: domain.ProtocolOpenAIChat,
	}
	if err := svc.CreateDeployment(ctx, def); err != nil {
		t.Fatalf("default-timeout create rejected: %v", err)
	}
}

func TestPublishPinsImmutableSnapshot(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	seedProviderModelDeployment(t, svc, "glm-4.6")

	rev, err := svc.Publish(ctx, "op-alice")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if rev != 1 {
		t.Errorf("first revision = %d, want 1", rev)
	}

	// One call pins one snapshot: two loads see the same immutable content
	// even after a draft edit lands in the mutable tables.
	snap1, err := svc.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := svc.GetModel(ctx, "glm-4.6")
	if err != nil {
		t.Fatal(err)
	}
	m.DisplayName = "Changed After Publish"
	if err := svc.UpdateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	snap2, err := svc.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap1.Revision != snap2.Revision {
		t.Errorf("snapshot revision changed without publish: %d → %d", snap1.Revision, snap2.Revision)
	}
	if got := snap2.Models["glm-4.6"].DisplayName; got != "GLM 4.6" {
		t.Errorf("snapshot mutated by draft edit: %q", got)
	}

	// Publish again → new revision number, new content visible.
	rev2, err := svc.Publish(ctx, "op-bob")
	if err != nil {
		t.Fatal(err)
	}
	if rev2 != 2 {
		t.Errorf("second revision = %d, want 2", rev2)
	}
	snap3, err := svc.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap3.Models["glm-4.6"].DisplayName; got != "Changed After Publish" {
		t.Errorf("published snapshot still old: %q", got)
	}
}

func TestRollbackPublishesOldContentWithoutRewritingHistory(t *testing.T) {
	db, _, svc := testDB(t)
	ctx := context.Background()
	seedProviderModelDeployment(t, svc, "glm-4.6")

	if _, err := svc.Publish(ctx, "op-1"); err != nil {
		t.Fatal(err)
	}
	// Mutate + publish revision 2.
	m, _ := svc.GetModel(ctx, "glm-4.6")
	m.DisplayName = "V2 Name"
	if err := svc.UpdateModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "op-2"); err != nil {
		t.Fatal(err)
	}

	// A billing fact exists that rollback must never touch (回滚不改历史账单).
	userID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}
	var acctID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO inference_billing_accounts (user_id) VALUES ($1) RETURNING id`, userID).Scan(&acctID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inference_ledger_entries (billing_account_id, entry_type, amount_micros, created_by)
		 VALUES ($1, 'adjustment', 4242, 'test')`, acctID); err != nil {
		t.Fatal(err)
	}

	// Roll back to revision 1: a NEW revision 3 carrying revision 1 content.
	rev3, err := svc.Rollback(ctx, 1, "op-rollback")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rev3 != 3 {
		t.Errorf("rollback revision = %d, want 3 (new revision, not a rewrite)", rev3)
	}

	snap, err := svc.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Models["glm-4.6"].DisplayName; got != "GLM 4.6" {
		t.Errorf("active snapshot after rollback = %q, want revision-1 content", got)
	}

	// History intact: revision 2 still exists with its V2 payload, and is
	// superseded (immutable), revision 3 published+active.
	revs, err := svc.ListRevisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 {
		t.Fatalf("revision history = %d rows, want 3", len(revs))
	}
	r2, err := svc.Store().GetRevision(ctx, domain.ScopeCatalog, 2)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != domain.RevisionSuperseded {
		t.Errorf("revision 2 status = %s, want superseded", r2.Status)
	}
	r3, err := svc.Store().GetRevision(ctx, domain.ScopeCatalog, 3)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != domain.RevisionPublished || !r3.IsActive {
		t.Errorf("revision 3 = %s active=%v, want published+active", r3.Status, r3.IsActive)
	}

	// The billing fact is byte-identical after the rollback.
	var amount int64
	var createdBy string
	if err := db.QueryRowContext(ctx,
		`SELECT amount_micros, created_by FROM inference_ledger_entries WHERE billing_account_id=$1`, acctID).
		Scan(&amount, &createdBy); err != nil {
		t.Fatal(err)
	}
	if amount != 4242 || createdBy != "test" {
		t.Errorf("ledger entry mutated by rollback: amount=%d created_by=%q", amount, createdBy)
	}

	// Rolling back to a never-published draft revision is refused.
	draftRev := &domain.ConfigRevision{
		Scope: domain.ScopeCatalog, Revision: 99, Status: domain.RevisionDraft,
		Payload:   domain.ExtensionConfig{Raw: catalog.BuildCatalogPayload(nil, nil, nil, nil)},
		CreatedBy: "test",
	}
	if err := svc.Store().InsertConfigRevision(ctx, draftRev); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rollback(ctx, 99, "x"); err == nil {
		t.Error("rollback to a never-published draft revision must be refused")
	}
}

func TestListPublishedModelsAppliesSellablePriceAndAccessHooks(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	seedProviderModelDeployment(t, svc, "glm-4.6")
	// A second model, same protocol, still draft.
	draft := sampleModel("glm-draft")
	if err := svc.CreateModel(ctx, &draft); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "op"); err != nil {
		t.Fatal(err)
	}

	allowAll := func(context.Context, string) (bool, error) { return true, nil }
	denyAll := func(context.Context, string) (bool, error) { return false, nil }

	// No price hook wired → nothing is sellable (默认拒绝无售价).
	got, err := svc.ListPublishedModels(ctx, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("without price hook, published models = %+v, want empty", got)
	}

	// Price hook wired but access denies → empty.
	svc.SetPriceCheck(func(context.Context, string) (bool, error) { return true, nil })
	got, err = svc.ListPublishedModels(ctx, denyAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("access-denied listing = %+v, want empty", got)
	}

	// Both allow: only the active model appears, never the draft.
	got, err = svc.ListPublishedModels(ctx, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "glm-4.6" {
		t.Errorf("published listing = %+v, want [glm-4.6]", got)
	}

	// Price hook error fails the listing closed (no silent over-grant).
	svc.SetPriceCheck(func(context.Context, string) (bool, error) {
		return false, errors.New("pricing backend down")
	})
	if _, err := svc.ListPublishedModels(ctx, allowAll); err == nil {
		t.Error("price hook error must propagate (fail-closed)")
	}
}

func TestSnapshotCacheRefreshFailureKeepsVerifiedSnapshot(t *testing.T) {
	db, store, svc := testDB(t)
	ctx := context.Background()
	seedProviderModelDeployment(t, svc, "glm-4.6")
	if _, err := svc.Publish(ctx, "op"); err != nil {
		t.Fatal(err)
	}

	var alerts int
	cache := catalog.NewSnapshotCache(store, func(error) { alerts++ })

	snap, err := cache.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 1 {
		t.Fatalf("first load revision = %d", snap.Revision)
	}

	// Publish v2 through the service, then CORRUPT v2's payload in the DB
	// directly (simulates a half-written/future-schema revision). The cache
	// must keep serving v1 and raise the alert — never a half version.
	if _, err := svc.Publish(ctx, "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE inference_config_revisions SET payload = '{"schema_version":99}' WHERE revision = 2`); err != nil {
		t.Fatal(err)
	}
	snap, err = cache.Current(ctx)
	if err != nil {
		t.Fatalf("refresh failure must be absorbed when a verified snapshot exists: %v", err)
	}
	if snap.Revision != 1 {
		t.Errorf("served revision = %d, want 1 (last verified)", snap.Revision)
	}
	if alerts == 0 {
		t.Error("refresh failure must raise the alert callback")
	}

	// Recovery: fix the payload, next Current picks the fresh revision up.
	if _, err := svc.Publish(ctx, "op"); err != nil {
		t.Fatal(err)
	}
	snap, err = cache.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 3 {
		t.Errorf("after recovery served revision = %d, want 3", snap.Revision)
	}

	// A fresh cache on an empty catalog errors at cold start — there is no
	// verified version to fall back to.
	wipe(t, db)
	cold := catalog.NewSnapshotCache(store, func(error) {})
	if _, err := cold.Current(ctx); err == nil {
		t.Error("cold start without any active revision must error")
	}
}

func TestLifecycleStateMachine(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	m := sampleModel("lc-model")
	if err := svc.CreateModel(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetModelLifecycle(ctx, "lc-model", domain.LifecycleDeprecated); err != nil {
		t.Errorf("draft→deprecated: %v", err)
	}
	if err := svc.SetModelLifecycle(ctx, "lc-model", domain.LifecycleActive); err != nil {
		t.Errorf("deprecated→active: %v", err)
	}
	if err := svc.SetModelLifecycle(ctx, "lc-model", domain.LifecycleRetired); err != nil {
		t.Errorf("active→retired: %v", err)
	}
	if err := svc.SetModelLifecycle(ctx, "lc-model", domain.LifecycleActive); err == nil {
		t.Error("retired→active must be refused (retired is terminal)")
	}
}

// 审查修复 Important #1: publish must drain the whole catalog with keyset
// pagination — the old hardcoded Limit: 500 silently dropped entities 501+
// from the published snapshot.
func TestPublishDrainsMoreThanOnePage(t *testing.T) {
	_, store, svc := testDB(t)
	ctx := context.Background()

	const totalModels = 600 // > publishPageSize (500) forces at least two pages
	store.InsertProvider(ctx, &domain.Provider{Code: "bulk", DisplayName: "Bulk", AccessType: domain.AccessOfficialAPI})
	for i := 0; i < totalModels; i++ {
		m := sampleModel(fmt.Sprintf("bulk-%04d", i))
		if err := store.InsertModel(ctx, &m); err != nil {
			t.Fatalf("seed model %d: %v", i, err)
		}
	}

	rev, err := svc.Publish(ctx, "op-bulk")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if rev != 1 {
		t.Errorf("revision = %d, want 1", rev)
	}
	snap, err := svc.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Models) != totalModels {
		t.Errorf("snapshot has %d models, want %d (entities beyond page 1 must not be silently dropped)",
			len(snap.Models), totalModels)
	}
	for _, id := range []string{"bulk-0000", "bulk-0499", "bulk-0500", "bulk-0599"} {
		if _, ok := snap.Models[id]; !ok {
			t.Errorf("model %s missing from published snapshot", id)
		}
	}
}

// 审查修复 Important #2: a nil AccessCheck must fail closed with an
// explicit error — never a panic, never an implicit allow.
func TestListPublishedModelsNilAccessCheckFailsClosed(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()
	seedProviderModelDeployment(t, svc, "glm-4.6")
	if _, err := svc.Publish(ctx, "op"); err != nil {
		t.Fatal(err)
	}
	svc.SetPriceCheck(func(context.Context, string) (bool, error) { return true, nil })

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil AccessCheck panicked: %v", r)
		}
	}()
	models, err := svc.ListPublishedModels(ctx, nil)
	if err == nil {
		t.Error("nil AccessCheck: want explicit error, got nil")
	}
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("nil AccessCheck: code = %s, want invalid_input", domain.CodeOf(err))
	}
	if len(models) != 0 {
		t.Errorf("nil AccessCheck returned %d models, want none (fail-closed)", len(models))
	}
}
