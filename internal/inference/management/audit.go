// audit.go — inference 模块管理审计。设计 §9.2：凭据创建/轮换/测试/禁用、
// 模型目录写操作、权限变更均记录 actor（用户 ID）+ 服务身份 + 对象 + 原因。
// Detail 经 SanitizeDetail 脱敏后才允许落库：任何响应、日志、审计均不含
// 可用秘密。
package management

import (
	"context"
	"strings"

	"github.com/yunhou/users/internal/inference/domain"
)

// AuditEvent is one management audit fact. ActorUser is the server-verified
// user JWT subject; ActorApp is the verified X-App-ID service identity. Both
// are required for operator actions — a request can only reach the recorder
// after the authorization middleware set them.
type AuditEvent struct {
	Action     string // 动词-名词, e.g. credential.rotate / model.create / permission.grant
	ObjectType string // e.g. credential / model / permission
	ObjectID   string
	Reason     string // operator-supplied justification (required for writes)
	ActorUser  string
	ActorApp   string
	Detail     map[string]any // sanitized before Record
}

// AuditRecorder persists audit events. Implemented by the postgres store;
// tests use in-memory recorders.
type AuditRecorder interface {
	Record(ctx context.Context, ev AuditEvent) error
}

// AuditTxRecorder records the audit event INSIDE the caller's transaction —
// the audit row commits or rolls back together with the mutation it
// describes (补偿/冲正的追加审计与效果同生共死，Task 15). Implemented by
// the postgres store (RecordTx).
type AuditTxRecorder interface {
	RecordTx(ctx context.Context, w domain.UnitOfWork, ev AuditEvent) error
}

// sensitiveKeyPattern matches detail keys that must never reach the audit
// log — even "redacted" values, because a redacted marker next to a key name
// still confirms the secret's existence and invites offline brute force
// against the ciphertext.
var sensitiveKeyPattern = []string{
	"secret", "plaintext", "ciphertext", "password", "token",
	"authorization", "api_key", "apikey", "key_material", "private_key",
}

func isSensitiveKey(k string) bool {
	lower := strings.ToLower(k)
	for _, pat := range sensitiveKeyPattern {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// SanitizeDetail returns a copy of detail with sensitive keys removed
// (recursively). Safe on nil input. The service layer calls this right
// before Record as defence in depth — callers must not treat it as a
// licence to pass secrets through.
func SanitizeDetail(detail map[string]any) map[string]any {
	if detail == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(detail))
	for k, v := range detail {
		if isSensitiveKey(k) {
			continue
		}
		out[k] = sanitizeValue(v)
	}
	return out
}

func sanitizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return SanitizeDetail(t)
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, sanitizeValue(item))
		}
		return out
	default:
		return v
	}
}
