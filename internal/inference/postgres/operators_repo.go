// operators_repo.go — 运营角色（operator_roles）与管理审计
// （inference_audit_log）的 SQL 实现，迁移 028。

package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/yunhou/users/internal/inference/management"
)

// RolesForUser returns the role names granted to a user (empty when the
// user is not an operator — fail closed upstream).
func (s *Store) RolesForUser(ctx context.Context, userID string) ([]string, error) {
	var roles []string
	err := s.db.SelectContext(ctx, &roles,
		`SELECT role FROM operator_roles WHERE user_id = $1`, userID)
	if err != nil {
		return nil, mapError("list operator roles", err)
	}
	return roles, nil
}

// GrantRole idempotently grants a role to a user. grantedBy is the granting
// operator's user ID (nil for the offline bootstrap command).
func (s *Store) GrantRole(ctx context.Context, userID, role string, grantedBy *string, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO operator_roles (user_id, role, granted_by, reason)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id, role) DO NOTHING`,
		userID, role, grantedBy, reason)
	if err != nil {
		return false, mapError("grant operator role", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeRole removes a role grant. Returns false when the grant did not
// exist (idempotent revoke).
func (s *Store) RevokeRole(ctx context.Context, userID, role string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM operator_roles WHERE user_id = $1 AND role = $2`, userID, role)
	if err != nil {
		return false, mapError("revoke operator role", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListOperators returns every grant, newest first.
func (s *Store) ListOperators(ctx context.Context) ([]management.OperatorGrant, error) {
	var out []management.OperatorGrant
	err := s.db.SelectContext(ctx, &out,
		`SELECT user_id, role, granted_by, reason, created_at
		   FROM operator_roles
		  ORDER BY created_at DESC, user_id, role`)
	if err != nil {
		return nil, mapError("list operators", err)
	}
	return out, nil
}

// FindUserIDByEmail resolves a user through social_identities.email (users
// itself carries no email column). Used by the offline bootstrap command.
func (s *Store) FindUserIDByEmail(ctx context.Context, email string) (string, error) {
	var id string
	err := s.db.GetContext(ctx, &id,
		`SELECT user_id FROM social_identities WHERE email = $1 LIMIT 1`, email)
	if err != nil {
		return "", mapError("find user by email", err)
	}
	return id, nil
}

// Record persists one management audit event (management.AuditRecorder).
// The detail is stored sanitized by the caller; this method adds a final
// belt-and-suspenders pass so no caller mistake can leak secrets.
func (s *Store) Record(ctx context.Context, ev management.AuditEvent) error {
	detail := management.SanitizeDetail(ev.Detail)
	raw, err := json.Marshal(detail)
	if err != nil {
		return mapError("marshal audit detail", err)
	}
	var actorUser interface{}
	if ev.ActorUser != "" {
		actorUser = ev.ActorUser
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO inference_audit_log
		 (actor_user_id, actor_app_id, action, object_type, object_id, reason, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		actorUser, ev.ActorApp, ev.Action, ev.ObjectType, ev.ObjectID, ev.Reason, raw)
	if err != nil {
		return mapError("insert inference audit", err)
	}
	return nil
}

// InferenceAuditEntry is one row of inference_audit_log (readback for tests
// and future admin audit queries).
type InferenceAuditEntry struct {
	ID          int64           `db:"id"`
	OccurredAt  time.Time       `db:"occurred_at"`
	ActorUserID *string         `db:"actor_user_id"`
	ActorAppID  string          `db:"actor_app_id"`
	Action      string          `db:"action"`
	ObjectType  string          `db:"object_type"`
	ObjectID    string          `db:"object_id"`
	Reason      string          `db:"reason"`
	Detail      json.RawMessage `db:"detail"`
}

// ListAudit returns audit entries filtered by action prefix / object, newest
// first. Empty filters return the most recent `limit` entries.
func (s *Store) ListAudit(ctx context.Context, actionPrefix, objectType, objectID string, limit int) ([]InferenceAuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id, occurred_at, actor_user_id, actor_app_id, action, object_type, object_id, reason, detail
	  FROM inference_audit_log WHERE TRUE`
	var args []interface{}
	if actionPrefix != "" {
		args = append(args, actionPrefix+"%")
		query += ` AND action LIKE $` + strconv.Itoa(len(args))
	}
	if objectType != "" {
		args = append(args, objectType)
		query += ` AND object_type = $` + strconv.Itoa(len(args))
	}
	if objectID != "" {
		args = append(args, objectID)
		query += ` AND object_id = $` + strconv.Itoa(len(args))
	}
	query += ` ORDER BY id DESC LIMIT ` + strconv.Itoa(limit)
	var out []InferenceAuditEntry
	if err := s.db.SelectContext(ctx, &out, query, args...); err != nil {
		return nil, mapError("list inference audit", err)
	}
	return out, nil
}
