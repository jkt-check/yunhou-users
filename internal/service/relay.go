package service

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/yunhou/users/internal/repo"
)

// relayAccessTimeout 与 chat 的 access 读库超时同口径(chat.go 的
// chatAccessTimeout = 10s)。
const relayAccessTimeout = 10 * time.Second

// RelayService 编排 relay 的 entitlement 判定与 ticket 签发。
// entitlement 语义与 /chat 完全一致(见已决事项 1)。
type RelayService struct {
	subRepo  repo.SubscriptionRepo
	planRepo repo.PlanRepo
	tickets  *RelayTicketService
}

func NewRelayService(subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo, tickets *RelayTicketService) *RelayService {
	return &RelayService{subRepo: subRepo, planRepo: planRepo, tickets: tickets}
}

// CheckAccess 校验用户是否有远程功能权限;无权限返回 ErrRelayNoAccess。
func (s *RelayService) CheckAccess(ctx context.Context, userID, appID string) error {
	ctx, cancel := context.WithTimeout(ctx, relayAccessTimeout)
	defer cancel()

	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelayNoAccess
		}
		return err
	}
	if sub == nil || (sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now())) {
		return ErrRelayNoAccess
	}
	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelayNoAccess
		}
		return err
	}
	if plan == nil || !plan.IsActive || !slices.Contains(plan.Apps, appID) {
		return ErrRelayNoAccess
	}
	return nil
}

func (s *RelayService) IssueTicket(userID string) (string, int, error) {
	return s.tickets.Issue(userID)
}
