package workers

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers/connector"
)

func newHealthScanEnv(t *testing.T, count int) (refreshEnv, []string) {
	t.Helper()
	env := newRefreshEnv(t)
	spec := env.registry["testvendor"]
	spec.QuotaURL = ""
	env.registry["testvendor"] = spec
	ids := make([]string, count)
	for i := range ids {
		_, ids[i] = env.seedExpiringCredential(t)
	}
	sort.Strings(ids)
	return env, ids
}

func healthScanWorker(env refreshEnv, store UpstreamHealthStore, limit int) *UpstreamHealth {
	return NewUpstreamHealth(store, env.vault,
		&connector.Client{HTTP: env.vendor.srv.Client()}, env.refresher(),
		env.registry, nil, UpstreamHealthConfig{BatchLimit: limit}, nil)
}

func setHealthScanDue(t *testing.T, env refreshEnv, id string) {
	t.Helper()
	if _, err := env.db.Exec(`UPDATE inference_upstream_accounts
		SET status = 'cooldown', updated_at = now() - interval '10 minutes'
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func requireHealthScanActive(t *testing.T, env refreshEnv, id string) {
	t.Helper()
	a, err := env.store.GetUpstreamAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AccountActive {
		t.Fatalf("later eligible account %s starved: status=%s, want active", id, a.Status)
	}
}

func TestUpstreamHealthScanRotatesUnchangedPrefix(t *testing.T) {
	for _, prefix := range []string{"healthy_without_quota", "static_skip", "connector_error"} {
		t.Run(prefix, func(t *testing.T) {
			env, ids := newHealthScanEnv(t, 3)
			for _, id := range ids[:2] {
				var err error
				switch prefix {
				case "static_skip":
					_, err = env.db.Exec(`UPDATE inference_credentials
						SET auth_type = 'api_key', connector = ''
						WHERE id = (SELECT credential_id FROM inference_upstream_accounts WHERE id = $1)`, id)
				case "connector_error":
					_, err = env.db.Exec(`UPDATE inference_credentials SET connector = 'ghost-vendor'
						WHERE id = (SELECT credential_id FROM inference_upstream_accounts WHERE id = $1)`, id)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			before := make([]domain.UpstreamAccount, 2)
			for i, id := range ids[:2] {
				a, err := env.store.GetUpstreamAccount(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				before[i] = *a
			}
			setHealthScanDue(t, env, ids[2])
			w := healthScanWorker(env, env.store, 2)
			first, err := w.RunPass(context.Background())
			if err != nil || first.Scanned != 2 || first.Recovered != 0 {
				t.Fatalf("first bounded page: %+v, err=%v", first, err)
			}
			second, err := w.RunPass(context.Background())
			if err != nil || second.Scanned != 1 || second.Recovered != 1 {
				t.Errorf("second page must reach the due account: %+v, err=%v", second, err)
			}
			requireHealthScanActive(t, env, ids[2])
			for i, id := range ids[:2] {
				a, err := env.store.GetUpstreamAccount(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if a.Status != before[i].Status || !a.UpdatedAt.Equal(before[i].UpdatedAt) {
					t.Fatalf("rotation must not rewrite unchanged prefix: before=%+v after=%+v", before[i], *a)
				}
			}
		})
	}
}

func TestUpstreamHealthScanWrapsToNewlyDueLowerID(t *testing.T) {
	for _, tail := range []string{"exact_page", "short_page", "ineligible_tail", "deleted_tail"} {
		t.Run(tail, func(t *testing.T) {
			count := 3
			if tail == "exact_page" {
				count = 4
			}
			env, ids := newHealthScanEnv(t, count)
			w := healthScanWorker(env, env.store, 2)
			if m, err := w.RunPass(context.Background()); err != nil || m.Scanned != 2 {
				t.Fatalf("first page: %+v, err=%v", m, err)
			}
			switch tail {
			case "ineligible_tail":
				if _, err := env.db.Exec(`UPDATE inference_upstream_accounts SET status = 'disabled' WHERE id = $1`, ids[2]); err != nil {
					t.Fatal(err)
				}
			case "deleted_tail":
				if _, err := env.db.Exec(`DELETE FROM inference_upstream_accounts WHERE id = $1`, ids[2]); err != nil {
					t.Fatal(err)
				}
			default:
				want := 1
				if tail == "exact_page" {
					want = 2
				}
				for _, id := range ids[2:] {
					setHealthScanDue(t, env, id)
				}
				if m, err := w.RunPass(context.Background()); err != nil || m.Scanned != want || m.Recovered != want {
					t.Fatalf("tail page: %+v, err=%v, want scanned/recovered=%d", m, err, want)
				}
			}
			setHealthScanDue(t, env, ids[0])
			m, err := w.RunPass(context.Background())
			if err != nil || m.Scanned != 2 || m.Recovered != 1 {
				t.Fatalf("wrap must recover newly due lower UUID within the batch: %+v, err=%v", m, err)
			}
			requireHealthScanActive(t, env, ids[0])
		})
	}
}

func TestUpstreamHealthScanNormalizesOversizedBatch(t *testing.T) {
	env, ids := newHealthScanEnv(t, 101)
	setHealthScanDue(t, env, ids[100])
	w := healthScanWorker(env, env.store, 501)
	if m, err := w.RunPass(context.Background()); err != nil || m.Scanned != 100 || m.Recovered != 0 {
		t.Fatalf("oversized batch must use repository default: %+v, err=%v", m, err)
	}
	if m, err := w.RunPass(context.Background()); err != nil || m.Scanned != 1 || m.Recovered != 1 {
		t.Fatalf("normalized batch must not reset the cursor early: %+v, err=%v", m, err)
	}
	requireHealthScanActive(t, env, ids[100])
}

type healthScanInterruptStore struct {
	UpstreamHealthStore
	interrupt func()
}

func (s *healthScanInterruptStore) GetCredential(ctx context.Context, id string) (*domain.Credential, error) {
	if f := s.interrupt; f != nil {
		s.interrupt = nil
		f()
	}
	return s.UpstreamHealthStore.GetCredential(ctx, id)
}

func TestUpstreamHealthScanCancellationPreservesUnvisitedRows(t *testing.T) {
	env, ids := newHealthScanEnv(t, 2)
	setHealthScanDue(t, env, ids[1])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &healthScanInterruptStore{UpstreamHealthStore: env.store, interrupt: cancel}
	w := healthScanWorker(env, store, 2)
	m, err := w.RunPass(ctx)
	if !errors.Is(err, context.Canceled) || m.Scanned != 1 {
		t.Errorf("canceled pass must stop before the unvisited row: %+v, err=%v", m, err)
	}
	m, err = w.RunPass(context.Background())
	if err != nil || m.Scanned != 1 || m.Recovered != 1 {
		t.Fatalf("next pass must resume at the unvisited row: %+v, err=%v", m, err)
	}
	requireHealthScanActive(t, env, ids[1])
}

func TestUpstreamHealthScanGuardedPanicAdvancesCursor(t *testing.T) {
	env, ids := newHealthScanEnv(t, 2)
	setHealthScanDue(t, env, ids[1])
	store := &healthScanInterruptStore{
		UpstreamHealthStore: env.store, interrupt: func() { panic("test probe panic") },
	}
	w := healthScanWorker(env, store, 1)
	passReturned := false
	if err := runGuarded("test health scan", func() error {
		_, err := w.RunPass(context.Background())
		passReturned = true
		return err
	}); err != nil {
		t.Fatalf("guarded pass: %v", err)
	}
	if passReturned {
		t.Fatal("expected probe panic to interrupt the pass")
	}
	m, err := w.RunPass(context.Background())
	if err != nil || m.Scanned != 1 || m.Recovered != 1 {
		t.Fatalf("panic must not pin the next pass to the same account: %+v, err=%v", m, err)
	}
	requireHealthScanActive(t, env, ids[1])
}

func TestUpstreamHealthScanConcurrentPassesRotate(t *testing.T) {
	env, ids := newHealthScanEnv(t, 2)
	setHealthScanDue(t, env, ids[1])
	w := healthScanWorker(env, env.store, 1)
	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		m   UpstreamHealthMetrics
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			m, err := w.RunPass(context.Background())
			results <- result{m, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	recovered := 0
	for r := range results {
		if r.err != nil || r.m.Scanned != 1 {
			t.Fatalf("concurrent bounded pass: %+v, err=%v", r.m, r.err)
		}
		recovered += r.m.Recovered
	}
	if recovered != 1 {
		t.Fatalf("serialized passes must visit both accounts, recovered=%d", recovered)
	}
	requireHealthScanActive(t, env, ids[1])
}
