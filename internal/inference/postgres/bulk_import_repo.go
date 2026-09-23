// bulk_import_repo.go — 批量导入的事务写入与幂等任务注册（migration 034，
// Task 15）。语义见 management/bulk_import.go 头部注释：commit 与目录写入
// 同一事务（全部有效才落库，绝不半发布）；task_id 唯一键兜底重复提交。

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// bulkSummaryDoc is the persisted shape of inference_bulk_imports.summary.
type bulkSummaryDoc struct {
	SchemaVersion int                          `json:"schema_version"`
	Items         []management.BulkItemResult  `json:"items"`
	Inserted      int                          `json:"inserted"`
	Skipped       int                          `json:"skipped"`
}

// GetBulkImportByTaskID re-reads a committed import task (幂等重放源).
func (s *Store) GetBulkImportByTaskID(ctx context.Context, taskID string) (*management.BulkImportResult, error) {
	var row struct {
		Summary json.RawMessage `db:"summary"`
	}
	if err := s.db.GetContext(ctx, &row,
		`SELECT summary FROM inference_bulk_imports WHERE task_id = $1`, taskID); err != nil {
		return nil, mapError("bulk import: get task", err)
	}
	var doc bulkSummaryDoc
	if err := json.Unmarshal(row.Summary, &doc); err != nil || doc.SchemaVersion != 1 {
		return nil, domain.NewError(domain.CodeInternal, "bulk import: unreadable task summary")
	}
	return &management.BulkImportResult{
		TaskID: taskID, Committed: true,
		Items: doc.Items, Inserted: doc.Inserted, Skipped: doc.Skipped,
	}, nil
}

// ApplyBulkImportTx applies one fully-validated import document inside the
// caller's transaction. The task row is inserted FIRST (ON CONFLICT →
// CodeConflict: a prior/concurrent commit owns this task_id — the caller
// replays its recorded result); catalog rows follow with natural-key
// ON CONFLICT absorption (racing imports degrade to skipped, never error);
// the summary is written back to the task row before commit.
func (s *Store) ApplyBulkImportTx(ctx context.Context, w domain.UnitOfWork, taskID, actor string, doc *management.BulkCatalog) (*management.BulkImportResult, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO inference_bulk_imports (task_id, actor, summary, item_count)
		 VALUES ($1, $2, '{"schema_version":1,"items":[],"inserted":0,"skipped":0}', 0)
		 ON CONFLICT (task_id) DO NOTHING`, taskID, actor)
	if err != nil {
		return nil, mapError("bulk import: register task", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, domain.NewError(domain.CodeConflict, "bulk import: task_id already committed")
	}

	out := &management.BulkImportResult{TaskID: taskID, Items: []management.BulkItemResult{}}
	providerIDs := map[string]string{}

	for _, p := range doc.Providers {
		status := p.Status
		if status == "" {
			status = "active"
		}
		item := management.BulkItemResult{Kind: management.BulkKindProvider, NaturalKey: p.Code}
		var id string
		ins := tx.QueryRowxContext(ctx,
			`INSERT INTO inference_providers (code, display_name, access_type, status)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (code) DO NOTHING RETURNING id::text`,
			p.Code, p.DisplayName, p.AccessType, status)
		if scanErr := ins.Scan(&id); scanErr == nil {
			item.Status, item.ID = management.BulkItemInserted, id
			out.Inserted++
		} else {
			if scanErr != sql.ErrNoRows {
				// ON CONFLICT DO NOTHING 的空结果才是"已存在"；其余扫描错误
				// （如 CHECK 违规）必须原样上报，不得误当跳过。
				return nil, mapError("bulk import: provider", scanErr)
			}
			// 自然键已存在 → 重读 ID，标 skipped（不覆盖运营编辑）。
			if qerr := tx.QueryRowxContext(ctx,
				`SELECT id::text FROM inference_providers WHERE code = $1`, p.Code).Scan(&id); qerr != nil {
				return nil, mapError("bulk import: re-read provider", qerr)
			}
			item.Status, item.ID = management.BulkItemSkipped, id
			out.Skipped++
		}
		providerIDs[p.Code] = item.ID
		out.Items = append(out.Items, item)
	}

	for _, m := range doc.Models {
		mItem := management.BulkItemResult{Kind: management.BulkKindModel, NaturalKey: m.ID}
		protocols := m.Protocols
		if protocols == nil {
			protocols = []string{}
		}
		input, output := m.InputModalities, m.OutputModalities
		if len(input) == 0 {
			input = []string{"text"}
		}
		if len(output) == 0 {
			output = []string{"text"}
		}
		aliases := m.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		insM, err := tx.ExecContext(ctx,
			`INSERT INTO inference_models
			 (id, display_name, lifecycle, model_version, aliases, input_modalities,
			  output_modalities, context_tokens, max_output_tokens, protocols,
			  supports_tools, supports_reasoning)
			 VALUES ($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 ON CONFLICT (id) DO NOTHING`,
			m.ID, m.DisplayName, m.ModelVersion, pq.Array(aliases), pq.Array(input),
			pq.Array(output), m.ContextTokens, m.MaxOutputTokens, pq.Array(protocols),
			m.SupportsTools, m.SupportsReasoning)
		if err != nil {
			return nil, mapError("bulk import: model", err)
		}
		if n, _ := insM.RowsAffected(); n > 0 {
			mItem.Status, mItem.ID = management.BulkItemInserted, m.ID
			out.Inserted++
		} else {
			mItem.Status, mItem.ID = management.BulkItemSkipped, m.ID
			out.Skipped++
		}
		out.Items = append(out.Items, mItem)

		for _, d := range m.Deployments {
			key := d.ProviderCode + "|" + d.UpstreamModel + "|" + d.BaseURL
			item := management.BulkItemResult{Kind: management.BulkKindDeployment, NaturalKey: key}
			pid := providerIDs[d.ProviderCode]
			connect, request := d.ConnectTimeoutMs, d.RequestTimeoutMs
			if connect == 0 {
				connect = 5000
			}
			if request == 0 {
				request = 600000
			}
			var depID string
			insD := tx.QueryRowxContext(ctx,
				`INSERT INTO inference_deployments
				 (provider_id, upstream_model, base_url, protocol, region,
				  connect_timeout_ms, request_timeout_ms, status)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,'draft')
				 ON CONFLICT (provider_id, upstream_model, base_url) DO NOTHING
				 RETURNING id::text`, pid, d.UpstreamModel, d.BaseURL, d.Protocol,
				d.Region, connect, request)
			if scanErr := insD.Scan(&depID); scanErr == nil {
				item.Status, item.ID = management.BulkItemInserted, depID
				out.Inserted++
			} else {
				if scanErr != sql.ErrNoRows {
					return nil, mapError("bulk import: deployment", scanErr)
				}
				if qerr := tx.QueryRowxContext(ctx,
					`SELECT id::text FROM inference_deployments
					  WHERE provider_id = $1 AND upstream_model = $2 AND base_url = $3`,
					pid, d.UpstreamModel, d.BaseURL).Scan(&depID); qerr != nil {
					return nil, mapError("bulk import: re-read deployment", qerr)
				}
				item.Status, item.ID = management.BulkItemSkipped, depID
				out.Skipped++
			}
			out.Items = append(out.Items, item)

			// 派生路由（与 env 兼容导入同一形状：priority 0 / weight 1 /
			// round_robin / enabled）。
			rItem := management.BulkItemResult{Kind: management.BulkKindRoute, NaturalKey: m.ID + "|" + key}
			insR, err := tx.ExecContext(ctx,
				`INSERT INTO inference_model_routes (model_id, deployment_id, priority, weight, account_pool_strategy, enabled)
				 VALUES ($1,$2,0,1,'round_robin',true)
				 ON CONFLICT (model_id, deployment_id) DO NOTHING`, m.ID, depID)
			if err != nil {
				return nil, mapError("bulk import: route", err)
			}
			if n, _ := insR.RowsAffected(); n > 0 {
				rItem.Status = management.BulkItemInserted
				out.Inserted++
			} else {
				rItem.Status = management.BulkItemSkipped
				out.Skipped++
			}
			out.Items = append(out.Items, rItem)
		}
	}

	summary, err := json.Marshal(bulkSummaryDoc{
		SchemaVersion: 1, Items: out.Items, Inserted: out.Inserted, Skipped: out.Skipped,
	})
	if err != nil {
		return nil, mapError("bulk import: marshal summary", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_bulk_imports SET summary = $1, item_count = $2 WHERE task_id = $3`,
		summary, len(out.Items), taskID); err != nil {
		return nil, mapError("bulk import: record summary", err)
	}
	return out, nil
}
