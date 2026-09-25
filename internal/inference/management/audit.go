// audit.go — inference 模块管理审计。设计 §9.2：凭据创建/轮换/测试/禁用、
// 模型目录写操作、权限变更均记录 actor（用户 ID）+ 服务身份 + 对象 + 原因。
// Detail 经 SanitizeDetail 脱敏后才允许落库：任何响应、日志、审计均不含
// 可用秘密。
package management

import (
	"context"
	"log"
	"strings"

	"github.com/yunhou/users/internal/inference/domain"
)

// AuditAlertHook 是审计域的告警通道（安全审查 I-7/I-6）。两类调用方共享:
// catalog 写面的审计在业务变更提交后补写,失败无法回滚已生效的变更;
// 网关生命周期门(I-6)命中非 active 模型时也经此上报(默认实现输出
// LIFECYCLE_GATE_HIT 结构化日志之外的第二通道)。默认实现输出带
// AUDIT_ALERT 标记的结构化 ERROR 日志(含动作/对象/双腿归因,便于巡检
// 规则命中);「审计缺失需人工补录」这类具体语义由调用方封装进 cause,
// 保证日志陈述始终属实。生产应在启动时把它接到 on-call 通道
// (pager/webhook),与 SnapshotCache.OnRefreshError 同级的「响亮失败」
// 约定。不得静默丢弃。调用方保证 hook 非 nil 且不 panic。
var AuditAlertHook = func(ctx context.Context, ev AuditEvent, cause error) {
	log.Printf("AUDIT_ALERT action=%s object=%s/%s actor=%s/%s reason=%q err=%v",
		ev.Action, ev.ObjectType, ev.ObjectID, ev.ActorUser, ev.ActorApp, ev.Reason, cause)
}

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
