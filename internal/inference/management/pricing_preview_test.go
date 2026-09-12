package management

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// pricing_preview_test.go — Task 16 覆盖率补强：价格/策略变更预览的纯编
// 排（fake store）：被替换版本、受影响套餐/账户、在途钉住、校验失败。

type fakePreviewStore struct {
	model       *domain.Model
	rejectModel string
	price       *PriceVersionInfo
	priceErr    error
	policy      *PolicyVersionInfo
	covering    int64
	plans       []AffectedPlan
	usingPolicy int64
	inflight    int64
}

func (f *fakePreviewStore) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	if f.model == nil || id == f.rejectModel {
		return nil, domain.NewError(domain.CodeNotFound, "model")
	}
	// 测试 fake：任何被询问的 id 都给一个合法模型（策略预览逐个校验）。
	m := *f.model
	m.ID = id
	return &m, nil
}
func (f *fakePreviewStore) LatestPriceVersionInfo(ctx context.Context, modelID, kind string, at time.Time) (*PriceVersionInfo, error) {
	if f.priceErr != nil {
		return nil, f.priceErr
	}
	return f.price, nil
}
func (f *fakePreviewStore) LatestPolicyVersionByName(ctx context.Context, name string) (*PolicyVersionInfo, error) {
	if f.policy == nil {
		return nil, domain.NewError(domain.CodeNotFound, "policy")
	}
	return f.policy, nil
}
func (f *fakePreviewStore) CountActiveEntitlementsCoveringModel(ctx context.Context, modelID string) (int64, error) {
	return f.covering, nil
}
func (f *fakePreviewStore) AffectedPlansForModel(ctx context.Context, modelID string) ([]AffectedPlan, error) {
	return f.plans, nil
}
func (f *fakePreviewStore) CountActiveEntitlementsUsingPolicy(ctx context.Context, policyVersionID string) (int64, error) {
	return f.usingPolicy, nil
}
func (f *fakePreviewStore) CountInFlightRequestsForModel(ctx context.Context, modelID string) (int64, error) {
	return f.inflight, nil
}

func TestPricingPreview_PriceChangeAssembled(t *testing.T) {
	fs := &fakePreviewStore{
		model: &domain.Model{ID: "glm-4.6", DisplayName: "GLM", Lifecycle: domain.LifecycleActive,
			ContextTokens: 1000, MaxOutputTokens: 100},
		price: &PriceVersionInfo{
			ID: "pv-1", Revision: 3, InputPerMtok: 100, OutputPerMtok: 200,
			EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		},
		covering: 42,
		plans: []AffectedPlan{
			{PlanID: "cp_basic", PlanName: "Basic", Accounts: 40},
			{PlanID: "cp_pro", PlanName: "Pro", Accounts: 2},
		},
		inflight: 7,
	}
	svc := NewPricingPreviewService(fs, nil)
	out, err := svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "glm-4.6", Kind: "sale_credit", InputPerMtok: 150, OutputPerMtok: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.CurrentEffective == nil || out.CurrentEffective.Revision != 3 || out.CurrentEffective.PriceVersionID != "pv-1" {
		t.Fatalf("current = %+v", out.CurrentEffective)
	}
	// 预览不编造 revision（未落库——由发布时分配）。
	if out.Proposed.Revision != 0 {
		t.Errorf("proposed revision = %d, want 0 (预览不编造)", out.Proposed.Revision)
	}
	if out.AffectedAccounts != 42 || len(out.AffectedPlans) != 2 || out.InFlightPinnedRequests != 7 {
		t.Errorf("impact = %+v", out)
	}
	if !out.ExistingSubscriptionsKeepVersion {
		t.Error("keep-version invariant must hold (恒 true)")
	}
	if out.Proposed.InputPerMtok != "150" || out.Proposed.OutputPerMtok != "300" {
		t.Errorf("proposed rates = %+v (十进制整数字符串)", out.Proposed)
	}

	// 尚无有效版本（新定价）→ current_effective null。
	fs.price = nil
	fs.priceErr = domain.NewError(domain.CodeNotFound, "no price")
	out, err = svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "glm-4.6", Kind: "sale_credit", InputPerMtok: 100, OutputPerMtok: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.CurrentEffective != nil {
		t.Errorf("current = %+v, want null (新定价)", out.CurrentEffective)
	}
	if out.Proposed.Revision != 0 {
		t.Errorf("first preview revision = %d, want 0 (预览不编造)", out.Proposed.Revision)
	}
}

func TestPricingPreview_Validation(t *testing.T) {
	fs := &fakePreviewStore{model: &domain.Model{ID: "m", ContextTokens: 1, MaxOutputTokens: 1}, rejectModel: "ghost"}
	svc := NewPricingPreviewService(fs, nil)

	// 未知模型 → NotFound。
	if _, err := svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "ghost", Kind: "sale_credit", InputPerMtok: 1, OutputPerMtok: 1,
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("unknown model = %v, want not_found", err)
	}
	// 非法 kind。
	if _, err := svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "m", Kind: "retail", InputPerMtok: 1, OutputPerMtok: 1,
	}); err == nil {
		t.Error("bad kind accepted")
	}
	// 负价格。
	if _, err := svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "m", Kind: "sale_credit", InputPerMtok: -1, OutputPerMtok: 1,
	}); err == nil {
		t.Error("negative rate accepted")
	}
	// sale_money 缺币种。
	if _, err := svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "m", Kind: "sale_money", InputPerMtok: 1, OutputPerMtok: 1,
	}); err == nil {
		t.Error("money kind without currency accepted")
	}
	// effective_to <= from。
	from := time.Now().UTC()
	to := from.Add(-time.Minute)
	if _, err := svc.PreviewPriceChange(context.Background(), PriceChangePreview{
		ModelID: "m", Kind: "sale_credit", InputPerMtok: 1, OutputPerMtok: 1,
		EffectiveFrom: from, EffectiveTo: &to,
	}); err == nil {
		t.Error("inverted effective range accepted")
	}
}

func TestPricingPreview_PolicyChange(t *testing.T) {
	fs := &fakePreviewStore{
		model: &domain.Model{ID: "glm-4.6", ContextTokens: 1, MaxOutputTokens: 1},
		policy: &PolicyVersionInfo{
			ID: "pol-1", Name: "coding-plan", Revision: 2, ModelIDs: []string{"glm-4.6"},
		},
		usingPolicy: 15,
	}
	svc := NewPricingPreviewService(fs, nil)
	out, err := svc.PreviewPolicyChange(context.Background(), PolicyChangePreview{
		Name: "coding-plan", ModelIDs: []string{"glm-4.6", "glm-4.7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.CurrentPublished == nil || out.CurrentPublished.Revision != 2 || out.Proposed.Revision != 0 {
		t.Fatalf("policy versions = %+v/%+v (预览不编造 revision)", out.CurrentPublished, out.Proposed)
	}
	if out.AffectedAccountsKeptOnOld != 15 {
		t.Errorf("affected kept on old = %d", out.AffectedAccountsKeptOnOld)
	}
	// 模型集差异如实呈现。
	if len(out.AddedModels) != 1 || out.AddedModels[0] != "glm-4.7" || len(out.RemovedModels) != 0 {
		t.Errorf("model diff = %+v/%+v", out.AddedModels, out.RemovedModels)
	}
	// 未知策略名 = 首次定义（current_published null，非错误）。
	fs2 := &fakePreviewStore{model: &domain.Model{ID: "m", ContextTokens: 1, MaxOutputTokens: 1}}
	out2, err := NewPricingPreviewService(fs2, nil).PreviewPolicyChange(context.Background(), PolicyChangePreview{
		Name: "ghost", ModelIDs: []string{"m"},
	})
	if err != nil || out2.CurrentPublished != nil {
		t.Errorf("first-defined policy = %+v/%v, want current null", out2, err)
	}
	// 空名 → invalid。
	if _, err := svc.PreviewPolicyChange(context.Background(), PolicyChangePreview{
		ModelIDs: []string{"m"},
	}); err == nil {
		t.Error("empty policy name accepted")
	}
}
