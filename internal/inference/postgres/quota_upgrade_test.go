package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/quota"
)

func quotaUpgradeCommand(f fixture, priceID string, hold int64) quota.AdmitCommand {
	cmd := admitCmd(f, priceID, hold, 100, 100, 100, nil)
	cmd.At = time.Date(2026, 9, 8, 4, 1, 0, 0, time.UTC)
	return cmd
}

func quotaUpgradePolicy(t *testing.T, s *Store, modelID string, five, weekly, monthly *domain.Microcredit) *PolicyVersion {
	t.Helper()
	p := &PolicyVersion{
		Name: "upgrade-" + uuid.NewString(), Revision: 1, ModelIDs: []string{modelID},
		FiveHourLimit: five, WeeklyLimit: weekly, MonthlyLimit: monthly,
	}
	if err := s.InsertPolicyVersion(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func reviseQuotaPolicy(t *testing.T, s *Store, cmd quota.AdmitCommand, p *PolicyVersion) quota.AdmitCommand {
	t.Helper()
	ent, err := s.GetEntitlement(context.Background(), cmd.Entitlement.ID)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := s.ReviseEntitlement(context.Background(), ent.ID, domain.EntitlementPatch{
		ExpectedRevision: ent.Revision, PolicyVersionID: &p.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Request.ID = uuid.NewString()
	cmd.Entitlement = *revised
	cmd.Policy = p.Pure()
	return cmd
}

func quotaUpgradeWindows(t *testing.T, s *Store, entID string) []domain.QuotaWindow {
	t.Helper()
	var rows []windowRow
	if err := s.db.Select(&rows,
		`SELECT * FROM inference_quota_windows WHERE entitlement_id=$1 ORDER BY id`, entID); err != nil {
		t.Fatal(err)
	}
	out := make([]domain.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.toDomain())
	}
	return out
}

type quotaUpgradeEffects struct {
	Requests, Holds, Rates, Leases, KeyUsed int64
	Windows                                 []domain.QuotaWindow
}

func quotaUpgradeSnapshot(t *testing.T, s *Store, f fixture) quotaUpgradeEffects {
	t.Helper()
	var out quotaUpgradeEffects
	if err := s.db.QueryRow(
		`SELECT
		   (SELECT COUNT(*) FROM inference_requests),
		   (SELECT COUNT(*) FROM inference_reservations),
		   (SELECT COUNT(*) FROM inference_rate_reservations),
		   (SELECT COUNT(*) FROM inference_concurrency_leases),
		   (SELECT budget_used_micros FROM inference_api_keys WHERE id=$1)`, f.keyID).
		Scan(&out.Requests, &out.Holds, &out.Rates, &out.Leases, &out.KeyUsed); err != nil {
		t.Fatal(err)
	}
	out.Windows = quotaUpgradeWindows(t, s, f.entID)
	return out
}

func TestQuotaPolicyRevision_UpgradeRefreshesLiveLimits(t *testing.T) {
	for _, bottleneck := range domain.WindowOrder {
		t.Run(string(bottleneck), func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			cmd := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
			first, err := newQuotaService(s).Admit(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(
				`UPDATE inference_quota_windows SET used_micros=90 WHERE id=$1`,
				first.WindowIDs[bottleneck]); err != nil {
				t.Fatal(err)
			}
			before := quotaUpgradeWindows(t, s, f.entID)
			if len(before) != 3 {
				t.Fatalf("fixture windows=%d, want 3", len(before))
			}
			next := reviseQuotaPolicy(t, s, cmd,
				quotaUpgradePolicy(t, s, f.modelID, micro(200), micro(200), micro(200)))
			next.EstimatedInputTokens = 20
			admitted, err := newQuotaService(s).Admit(context.Background(), next)
			if err != nil {
				t.Fatalf("upgraded limit 200, used 90, reserved 5 must admit hold 20: %v", err)
			}
			if !reflect.DeepEqual(admitted.WindowIDs, first.WindowIDs) {
				t.Errorf("upgrade replaced bound windows: %v -> %v", first.WindowIDs, admitted.WindowIDs)
			}
			for i := range before {
				before[i].Limit = 200
				before[i].Reserved = 25
			}
			if after := quotaUpgradeWindows(t, s, f.entID); !reflect.DeepEqual(after, before) {
				t.Errorf("upgrade must preserve window identity, boundaries and consumption:\n got %+v\nwant %+v", after, before)
			}
		})
	}
}

func TestQuotaPolicyRevision_EffectiveDecreaseAndRollback(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	cmd := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
	cmd.Policy.FiveHourLimit, cmd.Policy.WeeklyLimit, cmd.Policy.MonthlyLimit = micro(200), micro(200), micro(200)
	if _, err := newQuotaService(s).Admit(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE inference_quota_windows SET used_micros=90 WHERE entitlement_id=$1`, f.entID); err != nil {
		t.Fatal(err)
	}
	next := reviseQuotaPolicy(t, s, cmd,
		quotaUpgradePolicy(t, s, f.modelID, micro(100), micro(100), micro(100)))
	next.EstimatedInputTokens = 20
	before := quotaUpgradeSnapshot(t, s, f)
	_, err := newQuotaService(s).Admit(context.Background(), next)
	var exceeded *domain.QuotaExceededError
	if !errors.As(err, &exceeded) {
		t.Errorf("effective limit 100 must reject 90+5+20: %v", err)
	} else if len(exceeded.BlockedBy) != 3 {
		t.Errorf("blocked windows=%+v, want all three", exceeded.BlockedBy)
	}
	if after := quotaUpgradeSnapshot(t, s, f); !reflect.DeepEqual(after, before) {
		t.Errorf("rejected decrease changed admission state:\n got %+v\nwant %+v", after, before)
	}
	next.Request.ID = uuid.NewString()
	next.EstimatedInputTokens = 5
	if _, err := newQuotaService(s).Admit(context.Background(), next); err != nil {
		t.Fatalf("remaining 5 must admit: %v", err)
	}
	for _, w := range quotaUpgradeWindows(t, s, f.entID) {
		if w.Limit != 100 || w.Used != 90 || w.Reserved != 10 {
			t.Errorf("decreased window=%+v, want limit/used/reserved=100/90/10", w)
		}
	}
	next.Request.ID = uuid.NewString()
	next.EstimatedInputTokens = 1
	if _, err := newQuotaService(s).Admit(context.Background(), next); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Errorf("exhausted decreased window admitted more consumption: %v", err)
	}
}

func TestQuotaPolicyRevision_PreservesHistoryAndDisabledKinds(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	cmd := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
	cmd.Policy.MonthlyLimit = nil
	if _, err := newQuotaService(s).Admit(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	expired := &domain.QuotaWindow{
		EntitlementID: f.entID, Kind: domain.WindowFiveHour,
		Start: cmd.At.Add(-6 * time.Hour), End: cmd.At.Add(-time.Hour), Limit: 70, Used: 30, Reserved: 2,
	}
	voided := &domain.QuotaWindow{
		EntitlementID: f.entID, Kind: domain.WindowFiveHour,
		Start: cmd.At.Add(-time.Minute), End: cmd.At.Add(5 * time.Hour), Limit: 80, Voided: true,
	}
	for _, w := range []*domain.QuotaWindow{expired, voided} {
		if err := s.InsertQuotaWindow(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	before := quotaUpgradeWindows(t, s, f.entID)
	next := reviseQuotaPolicy(t, s, cmd,
		quotaUpgradePolicy(t, s, f.modelID, micro(200), nil, micro(300)))
	next.EstimatedInputTokens = 20
	result, err := newQuotaService(s).Admit(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.WindowIDs) != 2 || result.WindowIDs[domain.WindowWeekly] != "" {
		t.Errorf("bound windows=%v, want five_hour and monthly only", result.WindowIDs)
	}
	after := quotaUpgradeWindows(t, s, f.entID)
	if len(after) != len(before)+1 {
		t.Fatalf("windows=%d, want %d (newly enabled monthly only)", len(after), len(before)+1)
	}
	for _, original := range before {
		want := original
		if original.Kind == domain.WindowFiveHour && original.ID != expired.ID && !original.Voided {
			want.Limit, want.Reserved = 200, 25
		}
		found := false
		for _, got := range after {
			if got.ID == original.ID {
				found = true
				if !reflect.DeepEqual(got, want) {
					t.Errorf("existing window changed incorrectly:\n got %+v\nwant %+v", got, want)
				}
			}
		}
		if !found {
			t.Errorf("window %s disappeared", original.ID)
		}
	}
	for _, w := range after {
		if w.Kind == domain.WindowMonthly &&
			(w.Limit != 300 || w.Used != 0 || w.Reserved != 20 ||
				!w.Start.Equal(cmd.Entitlement.AnchorAt) ||
				!w.End.Equal(time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC))) {
			t.Errorf("newly enabled monthly window=%+v", w)
		}
	}
}

func TestQuotaPolicyRevision_StaleSnapshotHasNoAdmissionEffects(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "before_first_window"
		if existing {
			name = "after_limit_refresh"
		}
		t.Run(name, func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			old := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
			if existing {
				if _, err := newQuotaService(s).Admit(context.Background(), old); err != nil {
					t.Fatal(err)
				}
			}
			next := reviseQuotaPolicy(t, s, old,
				quotaUpgradePolicy(t, s, f.modelID, micro(200), micro(200), micro(200)))
			if existing {
				if _, err := newQuotaService(s).Admit(context.Background(), next); err != nil {
					t.Fatal(err)
				}
			}
			old.Request.ID = uuid.NewString()
			two := 2
			old.Policy.ConcurrencyLimit = &two
			before := quotaUpgradeSnapshot(t, s, f)
			_, err := newQuotaService(s).Admit(context.Background(), old)
			if domain.CodeOf(err) != domain.CodeConflict || errors.Is(err, quota.ErrWindowActivationConflict) {
				t.Errorf("stale policy admission=%v, want non-retriable conflict", err)
			}
			if after := quotaUpgradeSnapshot(t, s, f); !reflect.DeepEqual(after, before) {
				t.Errorf("stale policy changed admission state:\n got %+v\nwant %+v", after, before)
			}
		})
	}
}

func TestQuotaPolicyRevision_RejectsWrongAccount(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	other := seedFixtureModel(t, s, true, "other-model")
	cmd := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
	cmd.Request.BillingAccountID = other.accountID
	cmd.Request.APIKeyID = &other.keyID
	before := quotaUpgradeSnapshot(t, s, f)
	if _, err := newQuotaService(s).Admit(context.Background(), cmd); domain.CodeOf(err) != domain.CodeConflict {
		t.Errorf("cross-account entitlement admission=%v, want conflict", err)
	}
	if after := quotaUpgradeSnapshot(t, s, f); !reflect.DeepEqual(after, before) {
		t.Errorf("cross-account rejection changed admission state:\n got %+v\nwant %+v", after, before)
	}
}

type quotaRevisionBarrier struct {
	*Store
	started   chan int
	activated chan struct{}
	resume    <-chan struct{}
}

func (s *quotaRevisionBarrier) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	uow, err := s.Store.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if s.started != nil {
		tx, err := sqlTx(uow)
		if err == nil {
			var pid int
			err = tx.GetContext(ctx, &pid, `SELECT pg_backend_pid()`)
			if err == nil {
				s.started <- pid
			}
		}
		if err != nil {
			_ = uow.Rollback(context.Background())
			return nil, err
		}
	}
	return uow, nil
}

func (s *quotaRevisionBarrier) ActivateWindowsTx(ctx context.Context, uow domain.UnitOfWork, cmd quota.ActivateWindowsCommand) ([]domain.QuotaWindow, error) {
	windows, err := s.Store.ActivateWindowsTx(ctx, uow, cmd)
	if err != nil || s.activated == nil {
		return windows, err
	}
	close(s.activated)
	select {
	case <-s.resume:
		return windows, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitQuotaRevisionLock(ctx context.Context, s *Store, pid int, finished <-chan struct{}) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := s.db.GetContext(ctx, &waiting,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			 WHERE pid=$1 AND wait_event_type='Lock')`, pid); err != nil {
			return err
		}
		if waiting {
			return nil
		}
		select {
		case <-finished:
			return fmt.Errorf("operation finished without waiting on entitlement lock")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestQuotaPolicyRevision_AdmissionLockPrecedesRevision(t *testing.T) {
	for _, policyOnly := range []bool{false, true} {
		name := "optimistic_revision"
		if policyOnly {
			name = "policy_only_update"
		}
		t.Run(name, func(t *testing.T) { quotaAdmissionBeforeRevision(t, policyOnly) })
	}
}

func applyQuotaRevision(ctx context.Context, s *Store, uow domain.UnitOfWork, entID, policyID string, policyOnly bool) error {
	if !policyOnly {
		_, err := s.ReviseEntitlementTx(ctx, uow, entID, domain.EntitlementPatch{
			ExpectedRevision: 1, PolicyVersionID: &policyID,
		})
		return err
	}
	// Policy-only updates take NO KEY UPDATE, unlike revision increments
	// covered by the entitlement's unique key. FK KEY SHARE is not enough.
	tx, err := sqlTx(uow)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE inference_entitlements SET policy_version_id=$2 WHERE id=$1`, entID, policyID)
	return err
}

func quotaAdmissionBeforeRevision(t *testing.T, policyOnly bool) {
	t.Helper()
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	old := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
	p := quotaUpgradePolicy(t, s, f.modelID, micro(200), micro(200), micro(200))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	barrier := &quotaRevisionBarrier{Store: s, activated: make(chan struct{}), resume: resume}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		release()
		wg.Wait()
	})
	admissionResult := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := quota.NewService(barrier, nil).Admit(ctx, old)
		admissionResult <- err
	}()
	select {
	case <-barrier.activated:
	case <-ctx.Done():
		t.Fatal("admission never activated windows")
	}
	revision, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := mustTx(t, revision).GetContext(ctx, &pid, `SELECT pg_backend_pid()`); err != nil {
		_ = revision.Rollback(context.Background())
		t.Fatal(err)
	}
	revisionResult, finished := make(chan error, 1), make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(finished)
		defer revision.Rollback(context.Background())
		err := applyQuotaRevision(ctx, s, revision, f.entID, p.ID, policyOnly)
		if err == nil {
			err = revision.Commit(ctx)
		}
		revisionResult <- err
	}()
	if err := waitQuotaRevisionLock(ctx, s, pid, finished); err != nil {
		t.Errorf("policy revision must wait until old-policy admission commits: %v", err)
	}
	release()
	if err := <-admissionResult; err != nil {
		t.Errorf("admission holding original policy lock: %v", err)
	}
	if err := <-revisionResult; err != nil {
		t.Fatalf("revision after admission: %v", err)
	}
	ent, err := s.GetEntitlement(ctx, f.entID)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.Request.ID, next.Entitlement, next.Policy = uuid.NewString(), *ent, p.Pure()
	if _, err := newQuotaService(s).Admit(ctx, next); err != nil {
		t.Fatal(err)
	}
	for _, w := range quotaUpgradeWindows(t, s, f.entID) {
		if w.Limit != 200 || w.Reserved != 10 {
			t.Errorf("post-revision window limit/reserved=%d/%d, want 200/10", w.Limit, w.Reserved)
		}
	}
}

func TestQuotaPolicyRevision_RevisionLockPrecedesAdmission(t *testing.T) {
	for _, policyOnly := range []bool{false, true} {
		name := "optimistic_revision"
		if policyOnly {
			name = "policy_only_update"
		}
		t.Run(name, func(t *testing.T) { quotaRevisionBeforeAdmission(t, policyOnly) })
	}
}

func quotaRevisionBeforeAdmission(t *testing.T, policyOnly bool) {
	t.Helper()
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	old := quotaUpgradeCommand(f, seedPrice(t, s, f.modelID), 5)
	p := quotaUpgradePolicy(t, s, f.modelID, micro(200), micro(200), micro(200))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	revision, err := s.Begin(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		_ = revision.Rollback(context.Background())
		wg.Wait()
	})
	if err := applyQuotaRevision(ctx, s, revision, f.entID, p.ID, policyOnly); err != nil {
		t.Fatal(err)
	}
	before := quotaUpgradeSnapshot(t, s, f)
	barrier := &quotaRevisionBarrier{Store: s, started: make(chan int, 1)}
	admissionResult, finished := make(chan error, 1), make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(finished)
		_, err := quota.NewService(barrier, nil).Admit(ctx, old)
		admissionResult <- err
	}()
	var pid int
	select {
	case pid = <-barrier.started:
	case <-ctx.Done():
		t.Fatal("admission never began")
	}
	if err := waitQuotaRevisionLock(ctx, s, pid, finished); err != nil {
		t.Errorf("admission must wait for pending policy revision: %v", err)
	}
	if err := revision.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-admissionResult; domain.CodeOf(err) != domain.CodeConflict || errors.Is(err, quota.ErrWindowActivationConflict) {
		t.Errorf("waiting old-policy admission=%v, want non-retriable conflict", err)
	}
	if after := quotaUpgradeSnapshot(t, s, f); !reflect.DeepEqual(after, before) {
		t.Errorf("waiting stale admission changed state:\n got %+v\nwant %+v", after, before)
	}
}
