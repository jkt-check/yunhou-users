package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yunhou/users/internal/repo"
)

// AdminUsersService backs the dashboard user-management admin endpoints
// (dashboard-admin-api spec §3–§5): GET /admin/users/search,
// GET /admin/users/:id and POST /admin/users/:id/vip. All VIP business
// rules (spec §5.1) live here — the dashboard renders, it does not judge.
type AdminUsersService struct {
	repo repo.AdminUsersRepo
}

func NewAdminUsersService(r repo.AdminUsersRepo) *AdminUsersService {
	return &AdminUsersService{repo: r}
}

const (
	// adminSearchMaxQueryLen mirrors dashboard MAX_QUERY_LEN: the query is
	// truncated server-side to 64 characters (runes) before matching.
	adminSearchMaxQueryLen = 64
	// adminSearchUserLimit caps the aggregated result at 20 users
	// (dashboard SEARCH_USER_LIMIT; the repo's row-level LIMIT is 60
	// because one user can match via multiple identities).
	adminSearchUserLimit = 20
	// adminHistoryLimit mirrors dashboard HISTORY_LIMIT (5).
	adminHistoryLimit = 5
	// AdminVipMinDays/AdminVipMaxDays bound POST /admin/users/:id/vip days
	// (spec §5). Retired plans ('free'/'quarterly', dashboard RETIRED_PLANS)
	// are rejected in AddVipDays regardless of plans.is_active.
	AdminVipMinDays      = 1
	AdminVipMaxDays      = 3650
	adminVipActionGrant  = "vip.grant"
	adminVipActionExtend = "vip.extend"
	adminVipActionReject = "vip.reject"
)

// adminTime formats a timestamp per spec §1.3: ISO 8601 UTC, second
// precision ("2006-09-23T08:00:00Z").
func adminTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func adminTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := adminTime(*t)
	return &s
}

// AdminIdentity is one social_identities entry in API shape.
type AdminIdentity struct {
	Provider    string  `json:"provider"`
	ProviderUID string  `json:"providerUid"`
	Email       *string `json:"email"`
}

// AdminUserSummary is one user in the search result.
type AdminUserSummary struct {
	ID         string          `json:"id"`
	Nickname   *string         `json:"nickname"`
	Status     string          `json:"status"`
	CreatedAt  string          `json:"createdAt"`
	Identities []AdminIdentity `json:"identities"`
}

// AdminUserSearchResult is the response data of GET /admin/users/search.
type AdminUserSearchResult struct {
	Users []AdminUserSummary `json:"users"`
}

// AdminUserInfo is the user block of the detail response.
type AdminUserInfo struct {
	ID        string  `json:"id"`
	Nickname  *string `json:"nickname"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"createdAt"`
}

// AdminActiveSubscription is the detail response's activeSubscription
// (null when the user has no active kaya-membership row). ExpiresAt null
// means lifetime VIP. PlanIsActive/Price come from a LEFT JOIN and are
// null when the plan row is missing.
type AdminActiveSubscription struct {
	PlanID       string   `json:"planId"`
	StartedAt    string   `json:"startedAt"`
	ExpiresAt    *string  `json:"expiresAt"`
	PlanIsActive *bool    `json:"planIsActive"`
	Price        *float64 `json:"price"`
}

// AdminSubscriptionHistoryItem is one row of the detail response's
// history (kaya-membership only, created_at DESC, max 5).
type AdminSubscriptionHistoryItem struct {
	PlanID    string  `json:"planId"`
	Status    string  `json:"status"`
	StartedAt string  `json:"startedAt"`
	ExpiresAt *string `json:"expiresAt"`
	CreatedAt string  `json:"createdAt"`
}

// AdminUserDetail is the response data of GET /admin/users/:id.
type AdminUserDetail struct {
	User               AdminUserInfo                  `json:"user"`
	Identities         []AdminIdentity                `json:"identities"`
	ActiveSubscription *AdminActiveSubscription       `json:"activeSubscription"`
	History            []AdminSubscriptionHistoryItem `json:"history"`
}

// AdminVipSub is the before/after shape of the VIP write response.
type AdminVipSub struct {
	PlanID    string  `json:"planId"`
	ExpiresAt *string `json:"expiresAt"`
}

// AdminVipResult is the response data of POST /admin/users/:id/vip.
// Action is "granted" (new subscription, Before null) or "extended".
type AdminVipResult struct {
	Action string       `json:"action"`
	PlanID string       `json:"planId"`
	Before *AdminVipSub `json:"before"`
	After  *AdminVipSub `json:"after"`
}

// escapeAdminLike escapes the ILIKE special characters for a pattern used
// with ESCAPE '\': backslash first, then % and _ (dashboard likeEscape).
var adminLikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// truncateRunes caps s at n characters (runes), matching the dashboard's
// slice(0, 64) truncation semantics at character granularity.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func adminIdentityFromRow(row repo.AdminUserSearchRow) (AdminIdentity, bool) {
	if row.Provider == nil {
		return AdminIdentity{}, false
	}
	uid := ""
	if row.ProviderUID != nil {
		uid = *row.ProviderUID
	}
	return AdminIdentity{Provider: *row.Provider, ProviderUID: uid, Email: row.Email}, true
}

// SearchUsers implements spec §3: trim, empty→400, truncate to 64 chars,
// then aggregate the row-level result by user (order preserved =
// created_at DESC) capped at 20 users.
func (s *AdminUsersService) SearchUsers(ctx context.Context, q string) (*AdminUserSearchResult, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, &AdminParamError{Reason: "搜索关键词不能为空"}
	}
	q = truncateRunes(q, adminSearchMaxQueryLen)

	rows, err := s.repo.SearchUsers(ctx, q, adminLikeEscaper.Replace(q))
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}

	out := &AdminUserSearchResult{Users: []AdminUserSummary{}}
	index := make(map[string]int, len(rows))
	for _, row := range rows {
		i, ok := index[row.ID]
		if !ok {
			if len(out.Users) >= adminSearchUserLimit {
				// Rows are created_at DESC; once the cap is hit, later rows
				// can only belong to already-seen users or users beyond the
				// cap — skip creating new entries.
				continue
			}
			out.Users = append(out.Users, AdminUserSummary{
				ID:         row.ID,
				Nickname:   row.Nickname,
				Status:     row.Status,
				CreatedAt:  adminTime(row.CreatedAt),
				Identities: []AdminIdentity{},
			})
			i = len(out.Users) - 1
			index[row.ID] = i
		}
		if ident, ok := adminIdentityFromRow(row); ok {
			out.Users[i].Identities = append(out.Users[i].Identities, ident)
		}
	}
	return out, nil
}

// GetUserDetail implements spec §4: user + identities + the active
// kaya-membership subscription + the 5 most recent kaya-membership
// history rows. Unknown user → ErrUserNotFound (handler maps to 404).
func (s *AdminUsersService) GetUserDetail(ctx context.Context, userID string) (*AdminUserDetail, error) {
	rows, err := s.repo.FindUserWithIdentities(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("user detail: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrUserNotFound
	}

	first := rows[0]
	detail := &AdminUserDetail{
		User: AdminUserInfo{
			ID:        first.ID,
			Nickname:  first.Nickname,
			Status:    first.Status,
			CreatedAt: adminTime(first.CreatedAt),
		},
		Identities: []AdminIdentity{},
		History:    []AdminSubscriptionHistoryItem{},
	}
	for _, row := range rows {
		if ident, ok := adminIdentityFromRow(row); ok {
			detail.Identities = append(detail.Identities, ident)
		}
	}

	active, err := s.repo.FindActiveMembership(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("user detail active subscription: %w", err)
	}
	if active != nil {
		detail.ActiveSubscription = &AdminActiveSubscription{
			PlanID:       active.PlanID,
			StartedAt:    adminTime(active.StartedAt),
			ExpiresAt:    adminTimePtr(active.ExpiresAt),
			PlanIsActive: active.PlanIsActive,
			Price:        active.Price,
		}
	}

	history, err := s.repo.ListMembershipHistory(ctx, userID, adminHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("user detail history: %w", err)
	}
	for _, h := range history {
		detail.History = append(detail.History, AdminSubscriptionHistoryItem{
			PlanID:    h.PlanID,
			Status:    h.Status,
			StartedAt: adminTime(h.StartedAt),
			ExpiresAt: adminTimePtr(h.ExpiresAt),
			CreatedAt: adminTime(h.CreatedAt),
		})
	}
	return detail, nil
}

// errAdminIdemRace forces a rollback when a concurrent same-key request
// committed its idempotency row between our replay check and our insert;
// AddVipDays then re-reads and replays the winner's response.
var errAdminIdemRace = errors.New("admin idempotency key committed concurrently")

// AddVipDays implements spec §5: the eight VIP rules, the audit rows
// (success AND rejection, committed in the same tx) and idempotent replay
// scoped to (appID, idempotencyKey). actor is adminActorID(c)
// ("admin:<appID>"); appID scopes the idempotency key.
//
// Rejection is signalled by committing the audit row and returning the
// AdminVipRejection AFTER WithTx commits — returning the rejection from
// inside fn would roll back the audit row.
func (s *AdminUsersService) AddVipDays(ctx context.Context, appID, actor, userID string, days int, idemKey string) (*AdminVipResult, error) {
	target := "user:" + userID

	var (
		result    *AdminVipResult
		rejection *AdminVipRejection
	)

	err := s.repo.WithTx(ctx, func(tx repo.AdminUsersTx) error {
		// Idempotent replay: a stored response means the first attempt
		// committed; return it verbatim without touching subscriptions or
		// writing a second audit row (spec §5.3).
		if idemKey != "" {
			stored, err := tx.GetIdempotencyResponse(ctx, appID, idemKey)
			if err != nil {
				return fmt.Errorf("read idempotency key: %w", err)
			}
			if stored != nil {
				var r AdminVipResult
				if err := json.Unmarshal(stored, &r); err != nil {
					return fmt.Errorf("decode idempotent response: %w", err)
				}
				result = &r
				return nil
			}
		}

		exists, err := tx.UserExists(ctx, userID)
		if err != nil {
			return fmt.Errorf("check user: %w", err)
		}
		if !exists {
			return ErrUserNotFound
		}

		cur, err := tx.FindActiveMembershipForUpdate(ctx, userID)
		if err != nil {
			return fmt.Errorf("lock active subscription: %w", err)
		}

		// reject writes the vip.reject audit row and captures the rejection;
		// fn returns nil so the audit commits. Only reached for 409s.
		reject := func(reason string) error {
			auditCtx := map[string]any{"days": days, "reject_reason": reason}
			if cur != nil {
				auditCtx["before_expires_at"] = adminTimePtr(cur.ExpiresAt)
			}
			if idemKey != "" {
				auditCtx["idempotency_key"] = idemKey
			}
			if err := tx.InsertAudit(ctx, actor, adminVipActionReject, target, auditCtx); err != nil {
				return fmt.Errorf("write reject audit: %w", err)
			}
			rejection = &AdminVipRejection{Reason: reason}
			return nil
		}

		if cur == nil {
			// Rule 5: no active membership row → INSERT 'monthly'. A
			// missing/mismatched monthly plan row raises in the
			// trg_subscriptions_plan_product trigger → generic 500.
			planID, expiresAt, err := tx.InsertMembershipSub(ctx, userID, days)
			if err != nil {
				return fmt.Errorf("insert membership subscription: %w", err)
			}
			result = &AdminVipResult{
				Action: "granted",
				PlanID: planID,
				After:  &AdminVipSub{PlanID: planID, ExpiresAt: adminTimePtr(&expiresAt)},
			}
			if err := s.writeVipAudit(ctx, tx, actor, adminVipActionGrant, target, days, nil, result.After.ExpiresAt, idemKey); err != nil {
				return err
			}
		} else {
			// Rule 2: lifetime VIP (expires_at NULL) → 409, never shorten.
			if cur.ExpiresAt == nil {
				return reject("该用户是终身 VIP（expires_at 为空），不支持加时长")
			}
			// Rule 3: retired plan → 409. Plan row missing (LEFT JOIN null)
			// counts as retired, same as the dashboard.
			if cur.PlanIsActive == nil || !*cur.PlanIsActive || cur.PlanID == "free" || cur.PlanID == "quarterly" {
				return reject(fmt.Sprintf("该订阅在已退役套餐（%s）上，需人工处理", cur.PlanID))
			}
			// Rules 4/6: extend (trial rows allowed — extends the trial).
			updated, planID, expiresAt, err := tx.ExtendMembershipSub(ctx, userID, days)
			if err != nil {
				return fmt.Errorf("extend membership subscription: %w", err)
			}
			if !updated {
				// Rule 7: pre-read row was concurrently cancelled.
				return reject("订阅状态已变化（可能刚被取消），请刷新后重试")
			}
			result = &AdminVipResult{
				Action: "extended",
				PlanID: planID,
				Before: &AdminVipSub{PlanID: cur.PlanID, ExpiresAt: adminTimePtr(cur.ExpiresAt)},
				After:  &AdminVipSub{PlanID: planID, ExpiresAt: adminTimePtr(&expiresAt)},
			}
			if err := s.writeVipAudit(ctx, tx, actor, adminVipActionExtend, target, days, result.Before.ExpiresAt, result.After.ExpiresAt, idemKey); err != nil {
				return err
			}
		}

		// Record the idempotency key with the first-success response, in
		// the same transaction as the subscription write + audit row.
		if idemKey != "" {
			payload, err := json.Marshal(result)
			if err != nil {
				return fmt.Errorf("marshal idempotent response: %w", err)
			}
			inserted, err := tx.InsertIdempotencyKey(ctx, appID, idemKey, "vip."+result.Action, target, payload)
			if err != nil {
				return fmt.Errorf("insert idempotency key: %w", err)
			}
			if !inserted {
				// A concurrent same-key request committed first: roll our
				// mutation + audit back and replay the winner's response.
				result = nil
				return errAdminIdemRace
			}
		}
		return nil
	})

	if errors.Is(err, errAdminIdemRace) {
		return s.replayIdempotentResponse(ctx, appID, idemKey)
	}
	if err != nil {
		return nil, err
	}
	if rejection != nil {
		return nil, rejection
	}
	return result, nil
}

// writeVipAudit appends the success audit row inside the tx.
func (s *AdminUsersService) writeVipAudit(ctx context.Context, tx repo.AdminUsersTx, actor, action, target string, days int, beforeExpiresAt, afterExpiresAt *string, idemKey string) error {
	auditCtx := map[string]any{
		"days":              days,
		"before_expires_at": beforeExpiresAt,
		"after_expires_at":  afterExpiresAt,
	}
	if idemKey != "" {
		auditCtx["idempotency_key"] = idemKey
	}
	if err := tx.InsertAudit(ctx, actor, action, target, auditCtx); err != nil {
		return fmt.Errorf("write %s audit: %w", action, err)
	}
	return nil
}

// replayIdempotentResponse re-reads the committed winner row after a
// same-key race rollback.
func (s *AdminUsersService) replayIdempotentResponse(ctx context.Context, appID, idemKey string) (*AdminVipResult, error) {
	stored, err := s.repo.GetIdempotencyResponse(ctx, appID, idemKey)
	if err != nil {
		return nil, fmt.Errorf("re-read idempotency key: %w", err)
	}
	if stored == nil {
		return nil, fmt.Errorf("idempotency key %q raced but no committed response found", idemKey)
	}
	var r AdminVipResult
	if err := json.Unmarshal(stored, &r); err != nil {
		return nil, fmt.Errorf("decode idempotent response: %w", err)
	}
	return &r, nil
}
