package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/quota"
)

type walletFallbackFailureStore struct {
	*postgres.Store
	phase  string
	err    error
	begins int
}

func (s *walletFallbackFailureStore) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	s.begins++
	if s.phase == "begin" && s.begins == 2 {
		return nil, s.err
	}
	return s.Store.Begin(ctx)
}

func (s *walletFallbackFailureStore) LatestPriceVersion(ctx context.Context, model, kind string, at time.Time) (*postgres.PriceVersion, error) {
	if s.phase == "price" && kind == string(accounting.PriceSaleMoney) {
		return nil, s.err
	}
	return s.Store.LatestPriceVersion(ctx, model, kind, at)
}

func (s *walletFallbackFailureStore) GetWalletByAccount(ctx context.Context, account, currency string) (*postgres.Wallet, error) {
	if s.phase == "wallet" {
		return nil, s.err
	}
	return s.Store.GetWalletByAccount(ctx, account, currency)
}

func (s *walletFallbackFailureStore) ReserveRateTx(ctx context.Context, w domain.UnitOfWork, cmd domain.RateReserveCommand) error {
	if s.phase == "rate" {
		return s.err
	}
	return s.Store.ReserveRateTx(ctx, w, cmd)
}

func TestWalletFallback_PreservesServiceErrors(t *testing.T) {
	for _, phase := range []string{"begin", "rate", "price", "wallet"} {
		t.Run(phase, func(t *testing.T) {
			up := newUpstream(t, nonStreamUsage(10, 5))
			f := newFixture(t, up)
			seedWalletPrice(t, f)
			fundAndEnable(t, f, 100_000_000, 10_000_000)
			shrinkPlanToExhausted(t, f, "allow_overage")
			cause := domain.NewError(domain.CodeServiceUnavailable, "wallet storage unavailable")
			store := &walletFallbackFailureStore{Store: f.store, phase: phase, err: cause}
			f.gateway.store = store
			f.gateway.quotaSvc = quota.NewService(store, nil)
			before := f.walletBalance(t)
			p, key := f.principal()
			_, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if domain.CodeOf(err) != domain.CodeServiceUnavailable || !errors.Is(err, cause) {
				t.Fatalf("fallback error = %v, want the actual service failure", err)
			}
			var requests, rates, freezes int
			for _, q := range []struct {
				query string
				dest  *int
			}{
				{`SELECT COUNT(*) FROM inference_requests`, &requests},
				{`SELECT COUNT(*) FROM inference_rate_reservations`, &rates},
				{`SELECT COUNT(*) FROM inference_wallet_holds`, &freezes},
			} {
				if err := f.db.Get(q.dest, q.query); err != nil {
					t.Fatal(err)
				}
			}
			if requests != 0 || rates != 0 || freezes != 0 || up.calls.Load() != 0 || f.walletBalance(t) != before {
				t.Fatalf("failed fallback leaked admission: requests=%d rates=%d freezes=%d upstream=%d",
					requests, rates, freezes, up.calls.Load())
			}
		})
	}
}
