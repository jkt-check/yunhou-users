package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/management"
)

// TestApplyBulkImportTx_UndeclaredInputModalitiesStayEmpty pins the
// default-on rule at the bulk-import write path: a model whose document
// entry does not declare input_modalities must be stored with an EMPTY
// input_modalities array (not materialized as ["text"] — that would make it
// indistinguishable from an explicitly text-only model), and read back as
// image-accepting per domain.Model.AcceptsImages.
func TestApplyBulkImportTx_UndeclaredInputModalitiesStayEmpty(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()

	doc := &management.BulkCatalog{
		Providers: []management.BulkProvider{{
			Code: "bulk-mm", DisplayName: "Bulk MM", AccessType: "official_api",
		}},
		Models: []management.BulkModel{{
			ID: "bulk-undeclared", DisplayName: "Undeclared",
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols: []string{"openai_chat"},
			// InputModalities deliberately undeclared (nil).
		}},
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := s.ApplyBulkImportTx(ctx, uow, "task-"+uuid.NewString(), "test", doc)
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ApplyBulkImportTx: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Inserted != 2 {
		t.Errorf("inserted = %d, want 2（provider + model）: %+v", res.Inserted, res.Items)
	}

	// 存储层钉：列值是空数组 '{}'，不是 '{text}'（NOT NULL 约束同时被
	// 满足——pq.Array(nil) 会写 NULL，strArr 兜住）。
	var raw []string
	if err := db.QueryRowContext(ctx,
		`SELECT input_modalities FROM inference_models WHERE id = 'bulk-undeclared'`).Scan(pq.Array(&raw)); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("stored input_modalities = %v, want empty（未声明语义落库）", raw)
	}

	m, err := s.GetModel(ctx, "bulk-undeclared")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if len(m.InputModalities) != 0 {
		t.Errorf("read-back InputModalities = %v, want empty", m.InputModalities)
	}
	if !m.AcceptsImages() {
		t.Error("undeclared model must accept images after read-back（default-on）")
	}
}

// TestApplyBulkImportTx_DerivedRouteUsesSupportedPoolStrategy pins the bulk
// import write path to the SAME pool_strategy supported set as operator CRUD:
// the derived route must carry round_robin (常规池唯一支持的策略) — 批量导入
// 不得落下一个「保存后假装生效」的值。
func TestApplyBulkImportTx_DerivedRouteUsesSupportedPoolStrategy(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()

	doc := &management.BulkCatalog{
		Providers: []management.BulkProvider{{
			Code: "bulk-pool", DisplayName: "Bulk Pool", AccessType: "official_api",
		}},
		Models: []management.BulkModel{{
			ID: "bulk-pool-model", DisplayName: "Bulk Pool",
			ContextTokens: 1000, MaxOutputTokens: 100,
			Protocols: []string{"openai_chat"},
			Deployments: []management.BulkDeployment{{
				ProviderCode: "bulk-pool", UpstreamModel: "up-pool",
				BaseURL: "https://bulk-pool.example.com", Protocol: "openai_chat",
			}},
		}},
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ApplyBulkImportTx(ctx, uow, "task-"+uuid.NewString(), "test", doc); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("ApplyBulkImportTx: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var strategy string
	if err := db.QueryRowContext(ctx,
		`SELECT account_pool_strategy FROM inference_model_routes WHERE model_id = 'bulk-pool-model'`).
		Scan(&strategy); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strategy != "round_robin" {
		t.Errorf("derived route pool strategy = %q, want round_robin（批量导入必须落在支持集内）", strategy)
	}
}
