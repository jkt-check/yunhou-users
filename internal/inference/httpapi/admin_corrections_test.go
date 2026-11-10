package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
)

type correctionHTTPFixture struct {
	env                           *task4Env
	store                         *postgres.Store
	accountID, requestID, attempt string
	price                         *postgres.PriceVersion
	windowID                      string
}

func newCorrectionHTTPFixture(t *testing.T, wallet, extras bool) *correctionHTTPFixture {
	t.Helper()
	env := setupTask4(t)
	s := postgres.NewStore(env.db)
	ctx := context.Background()
	acct, err := s.EnsureBillingAccount(ctx, task4PlainUser)
	if err != nil {
		t.Fatal(err)
	}
	modelID := "correction-" + uuid.NewString()
	if err := s.InsertModel(ctx, &domain.Model{
		ID: modelID, DisplayName: "Correction", ContextTokens: 1000, MaxOutputTokens: 200,
	}); err != nil {
		t.Fatal(err)
	}
	p := &postgres.PriceVersion{
		ModelID: modelID, Kind: postgres.PriceSaleCredit, Unit: "microcredit",
		InputPerMtok: 1_000_000, CacheReadPerMtok: 1_000_000,
		CacheWritePerMtok: 1_000_000, OutputPerMtok: 2_000_000,
		Revision: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}
	if wallet {
		p.Kind, p.Unit, p.Currency = postgres.PriceSaleMoney, "micromoney", "CNY"
	}
	if extras {
		p.ExtraRates = domain.ExtensionConfig{SchemaVersion: 1,
			Raw: json.RawMessage(`{"schema_version":1,"rates":{"web_search":3}}`)}
	}
	if err := s.InsertPriceVersion(ctx, p); err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: modelID, Revision: 1, ModelIDs: []string{modelID},
		FiveHourLimit: mustMicro(1000), Status: "published",
	}
	if err := s.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: uuid.NewString(), ModelIDs: []string{modelID},
		PolicyVersionID: pol.ID, AnchorAt: now.Add(-time.Hour), EffectiveFrom: now.Add(-time.Hour),
	}
	if err := s.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	req := domain.Request{
		ID: uuid.NewString(), BillingAccountID: acct.ID, EntitlementID: ent.ID,
		ModelID: modelID, Protocol: domain.ProtocolOpenAIChat,
		PriceVersionID: &p.ID, PolicyVersionID: pol.ID,
	}
	if extras {
		req.ExtraBounds = map[string]int64{"web_search": 2}
	}
	f := &correctionHTTPFixture{env: env, store: s, accountID: acct.ID,
		requestID: req.ID, attempt: uuid.NewString(), price: p}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(ctx)
	if wallet {
		for _, source := range []struct {
			name   accounting.WalletSource
			amount int64
		}{{accounting.WalletCash, 1000}, {accounting.WalletBonus, 40}} {
			if _, err := s.ApplyWalletAdjustmentTx(ctx, uow, postgres.WalletAdjustmentCommand{
				AccountID: acct.ID, Currency: "CNY", Source: source.name, Direction: accounting.DirCredit,
				AmountMicros: source.amount, Reason: "fixture", OperatorSubject: "fixture",
				IdempotencyKey: uuid.NewString(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		limit := int64(2000)
		w, err := s.SetOverageTx(ctx, uow, acct.ID, "CNY", true, &limit, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		start, end := accounting.MonthBoundsUTC(now)
		if _, err := s.ReserveWallet(ctx, uow, domain.ReserveWalletCommand{
			Request: req, WalletID: w.ID, HoldMicros: 100, PriceVersionID: p.ID,
			AdmittedAt: now, MonthStart: start, MonthEnd: end,
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		win := &domain.QuotaWindow{
			EntitlementID: ent.ID, Kind: domain.WindowFiveHour,
			Start: now.Add(-time.Hour), End: now.Add(4 * time.Hour), Limit: 1000,
		}
		if err := s.InsertQuotaWindow(ctx, win); err != nil {
			t.Fatal(err)
		}
		f.windowID = win.ID
		if _, err := s.Reserve(ctx, uow, domain.ReserveCommand{
			Request: req, AdmittedAt: now,
			Holds: []domain.HoldSpec{{TargetKind: domain.TargetWindowFiveHour, WindowID: &win.ID, Amount: 100}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertAttemptTx(ctx, uow, &domain.Attempt{
		ID: f.attempt, RequestID: req.ID, AttemptNo: 1, Status: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID: req.ID, BillingAccountID: acct.ID, PolicyVersionID: pol.ID,
		Limits: domain.RateLimits{PolicyTPM: 1000}, ReservedTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(40), int64(20)
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: req.ID, ChargeMicros: 80, SettledAt: now,
		Usage: domain.UsageRecord{
			RequestID: req.ID, AttemptID: f.attempt, Source: domain.UsageEstimated,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *correctionHTTPFixture) body() string {
	return fmt.Sprintf(`{"attempt_id":%q,"buckets":{"input_tokens":70,"cache_read_tokens":0,
	    "cache_write_tokens":0,"output_tokens":20,"reasoning_tokens":5},
	    "extras":{},"evidence_ref":"provider-report:123","reason":"verified report",
	    "idempotency_key":"http-correction"}`, f.attempt)
}

func (f *correctionHTTPFixture) snapshot(t *testing.T) string {
	t.Helper()
	var snapshot string
	if err := f.env.db.Get(&snapshot, `SELECT jsonb_build_object(
	    'request',(SELECT to_jsonb(r) FROM inference_requests r WHERE id=$1),
	    'usage',(SELECT jsonb_agg(to_jsonb(u) ORDER BY revision) FROM inference_usage_records u WHERE request_id=$1),
	    'rate',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM inference_rate_reservations r WHERE request_id=$1),
	    'receipts',(SELECT jsonb_agg(to_jsonb(r)) FROM inference_usage_corrections r WHERE request_id=$1),
	    'adjustments',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM inference_adjustments a),
	    'ledger',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM inference_ledger_entries l),
	    'wallet',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM inference_wallet_entries w),
	    'windows',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM inference_quota_windows w),
	    'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM inference_reconciliation_jobs j),
	    'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM inference_audit_log a))::text`, f.requestID); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestAdminCorrections_PinnedPriceAndReplay(t *testing.T) {
	for _, wallet := range []bool{false, true} {
		t.Run(fmt.Sprintf("wallet_%t", wallet), func(t *testing.T) {
			f := newCorrectionHTTPFixture(t, wallet, false)
			// A newer catalog price must not reprice historical consumption.
			next := *f.price
			next.ID, next.Revision, next.InputPerMtok = "", 2, 9_000_000
			next.EffectiveFrom = time.Now().UTC().Add(-time.Minute)
			if err := f.store.InsertPriceVersion(context.Background(), &next); err != nil {
				t.Fatal(err)
			}
			path := "/admin/model-requests/" + f.requestID + "/corrections"
			var originalAt time.Time
			if err := f.env.db.Get(&originalAt, `SELECT settled_at FROM inference_rate_reservations
			    WHERE request_id=$1 AND scope='account'`, f.requestID); err != nil {
				t.Fatal(err)
			}
			w := postJSON(t, f.env.engine, path, f.env.headers(t, task4Admin), f.body())
			if w.Code != http.StatusCreated {
				t.Fatalf("correction: %d %s", w.Code, w.Body.String())
			}
			req, err := f.store.GetRequest(context.Background(), f.requestID)
			if err != nil || req.SettledMicros == nil || *req.SettledMicros != 110 ||
				req.UsageStatus != domain.UsageReported {
				t.Fatalf("pinned quote = %+v, %v; want 110 reported", req, err)
			}
			usage, err := f.store.LoadUsageRecords(context.Background(), f.requestID)
			if err != nil || len(usage) != 2 || usage[0].Source != domain.UsageEstimated ||
				usage[1].Source != domain.UsageReported || usage[1].Buckets.InputTokens == nil ||
				*usage[1].Buckets.InputTokens != 70 || usage[1].Buckets.CacheReadTokens == nil {
				t.Fatalf("usage revisions = %+v, %v", usage, err)
			}
			var evidence struct {
				SchemaVersion int              `json:"schema_version"`
				EvidenceRef   string           `json:"evidence_ref"`
				Extras        map[string]int64 `json:"extras"`
			}
			if err := json.Unmarshal(usage[1].RawUsage.Raw, &evidence); err != nil ||
				evidence.SchemaVersion != 1 || evidence.EvidenceRef != "provider-report:123" {
				t.Fatalf("evidence = %+v, %v", evidence, err)
			}
			var tokens int64
			var settledAt time.Time
			if err := f.env.db.QueryRow(`SELECT settled_tokens,settled_at FROM inference_rate_reservations
			    WHERE request_id=$1 AND scope='account'`, f.requestID).Scan(&tokens, &settledAt); err != nil {
				t.Fatal(err)
			}
			if tokens != 90 || !settledAt.Equal(originalAt) {
				t.Errorf("TPM = %d/%v, want 90/%v", tokens, settledAt, originalAt)
			}
			var actorUser, actorApp, reason string
			if err := f.env.db.QueryRow(`SELECT actor_user_id,actor_app_id,reason FROM inference_audit_log
			    WHERE object_id=$1 AND action='settlement.correct'`, f.requestID).
				Scan(&actorUser, &actorApp, &reason); err != nil {
				t.Fatal(err)
			}
			if actorUser != task4Admin || actorApp != task4AppID || reason != "verified report" {
				t.Errorf("audit attribution = %s/%s/%s", actorUser, actorApp, reason)
			}
			if wallet {
				bal, err := f.store.WalletBalance(context.Background(), f.accountID, "CNY", time.Now())
				if err != nil || bal.Balance.CashAvailable != 930 || bal.Balance.BonusAvailable != 0 || bal.MonthSpentMicros != 110 {
					t.Fatalf("wallet = %+v, %v", bal, err)
				}
			} else {
				var used int64
				if err := f.env.db.Get(&used, `SELECT used_micros FROM inference_quota_windows WHERE id=$1`, f.windowID); err != nil || used != 110 {
					t.Fatalf("window = %d, %v", used, err)
				}
			}
			before := f.snapshot(t)
			w = postJSON(t, f.env.engine, path, f.env.headers(t, task4Admin), f.body())
			if w.Code != http.StatusConflict || f.snapshot(t) != before {
				t.Fatalf("replay: %d %s; changed=%t", w.Code, w.Body.String(), f.snapshot(t) != before)
			}
		})
	}
}

func TestAdminCorrections_RejectIncompleteEvidence(t *testing.T) {
	f := newCorrectionHTTPFixture(t, false, true)
	base := strings.Replace(f.body(), `"extras":{}`, `"extras":{"web_search":2}`, 1)
	cases := []struct{ name, body string }{
		{"missing_bucket", strings.Replace(base, `"cache_read_tokens":0,`, "", 1)},
		{"null_bucket", strings.Replace(base, `"cache_read_tokens":0`, `"cache_read_tokens":null`, 1)},
		{"negative", strings.Replace(base, `"input_tokens":70`, `"input_tokens":-1`, 1)},
		{"fractional", strings.Replace(base, `"input_tokens":70`, `"input_tokens":0.5`, 1)},
		{"overflow", strings.Replace(base, `"input_tokens":70`, `"input_tokens":9223372036854775807`, 1)},
		{"reasoning_overlap", strings.Replace(base, `"reasoning_tokens":5`, `"reasoning_tokens":21`, 1)},
		{"null_reasoning", strings.Replace(base, `"reasoning_tokens":5`, `"reasoning_tokens":null`, 1)},
		{"negative_reasoning", strings.Replace(base, `"reasoning_tokens":5`, `"reasoning_tokens":-1`, 1)},
		{"unknown_bucket", strings.Replace(base, `"input_tokens":70`, `"unknown_tokens":1,"input_tokens":70`, 1)},
		{"caller_amount", strings.Replace(base, `"reason":`, `"amount_micros":"1","reason":`, 1)},
		{"caller_actor", strings.Replace(base, `"reason":`, `"actor_user_id":"forged","reason":`, 1)},
		{"unknown_extra_zero", strings.Replace(base, `"web_search":2`, `"web_search":2,"mystery":0`, 1)},
		{"negative_extra", strings.Replace(base, `"web_search":2`, `"web_search":-1`, 1)},
		{"null_extra", strings.Replace(base, `"web_search":2`, `"web_search":null`, 1)},
		{"missing_extra", strings.Replace(base, `"web_search":2`, "", 1)},
		{"empty_reason", strings.Replace(base, `"verified report"`, `" "`, 1)},
		{"empty_reference", strings.Replace(base, `"provider-report:123"`, `""`, 1)},
		{"empty_key", strings.Replace(base, `"http-correction"`, `" "`, 1)},
		{"bad_attempt", strings.Replace(base, f.attempt, "not-a-uuid", 1)},
		{"foreign_attempt", strings.Replace(base, f.attempt, uuid.NewString(), 1)},
		{"trailing", base + `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := f.snapshot(t)
			w := postJSON(t, f.env.engine, "/admin/model-requests/"+f.requestID+"/corrections",
				f.env.headers(t, task4Admin), tc.body)
			if w.Code != http.StatusBadRequest || f.snapshot(t) != before {
				t.Fatalf("invalid evidence: %d %s; changed=%t", w.Code, w.Body.String(), f.snapshot(t) != before)
			}
		})
	}
	for _, tc := range []struct {
		id     string
		status int
	}{{"not-a-uuid", http.StatusBadRequest}, {uuid.NewString(), http.StatusNotFound}} {
		before := f.snapshot(t)
		w := postJSON(t, f.env.engine, "/admin/model-requests/"+tc.id+"/corrections",
			f.env.headers(t, task4Admin), base)
		if w.Code != tc.status || f.snapshot(t) != before {
			t.Fatalf("request ID %s: %d %s", tc.id, w.Code, w.Body.String())
		}
	}
	// Explicit zero is evidence, not omission, and must still be accepted.
	base = strings.Replace(base, `"web_search":2`, `"web_search":0`, 1)
	w := postJSON(t, f.env.engine, "/admin/model-requests/"+f.requestID+"/corrections",
		f.env.headers(t, task4Admin), base)
	if w.Code != http.StatusCreated {
		t.Fatalf("explicit zero extra: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminCorrections_AuthAndAtomicAudit(t *testing.T) {
	for _, wallet := range []bool{false, true} {
		t.Run(fmt.Sprintf("wallet_%t", wallet), func(t *testing.T) {
			f := newCorrectionHTTPFixture(t, wallet, false)
			path := "/admin/model-requests/" + f.requestID + "/corrections"
			for _, missing := range []string{"Authorization", "X-App-Secret"} {
				headers := f.env.headers(t, task4Admin)
				delete(headers, missing)
				before := f.snapshot(t)
				w := postJSON(t, f.env.engine, path, headers, f.body())
				if w.Code != http.StatusUnauthorized || f.snapshot(t) != before {
					t.Fatalf("missing %s: %d %s", missing, w.Code, w.Body.String())
				}
			}
			for _, user := range []string{task4User, task4Auditor, task4PlainUser} {
				before := f.snapshot(t)
				w := postJSON(t, f.env.engine, path, f.env.headers(t, user), f.body())
				if w.Code != http.StatusForbidden || f.snapshot(t) != before {
					t.Fatalf("unauthorized: %d %s", w.Code, w.Body.String())
				}
			}
			headers := f.env.headers(t, task4Admin)
			token, err := f.env.tok.SignAccessToken(task4Admin, "another-app", nil)
			if err != nil {
				t.Fatal(err)
			}
			headers["Authorization"] = "Bearer " + token
			before := f.snapshot(t)
			w := postJSON(t, f.env.engine, path, headers, f.body())
			if w.Code != http.StatusForbidden || f.snapshot(t) != before {
				t.Fatalf("app mismatch: %d %s", w.Code, w.Body.String())
			}
			if _, err := f.env.db.Exec(`CREATE FUNCTION test_correction_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$
			    BEGIN RAISE EXCEPTION 'injected correction audit failure'; END $$;
			    CREATE TRIGGER test_correction_audit_failure BEFORE INSERT ON inference_audit_log
			    FOR EACH ROW WHEN (NEW.action='settlement.correct') EXECUTE FUNCTION test_correction_audit_failure()`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = f.env.db.Exec(`DROP TRIGGER IF EXISTS test_correction_audit_failure ON inference_audit_log`)
				_, _ = f.env.db.Exec(`DROP FUNCTION IF EXISTS test_correction_audit_failure()`)
			})
			before = f.snapshot(t)
			w = postJSON(t, f.env.engine, path, f.env.headers(t, task4Admin), f.body())
			if w.Code != http.StatusInternalServerError || f.snapshot(t) != before {
				t.Fatalf("audit failure: %d %s; changed=%t", w.Code, w.Body.String(), f.snapshot(t) != before)
			}
			if _, err := f.env.db.Exec(`DROP TRIGGER test_correction_audit_failure ON inference_audit_log`); err != nil {
				t.Fatal(err)
			}
			w = postJSON(t, f.env.engine, path, f.env.headers(t, task4Admin), f.body())
			if w.Code != http.StatusCreated {
				t.Fatalf("same key after repair: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAdminCorrections_RequiresBillingGateAndTransactionalAudit(t *testing.T) {
	f := newCorrectionHTTPFixture(t, false, false)
	for _, withGate := range []bool{false, true} {
		engine := gin.New()
		g := engine.Group("/admin", middleware.InternalAppAuth(repo.NewAppRepo(f.env.db)),
			middleware.JWTAuth(f.env.tok))
		ops := &httpapi.AdminOps{Corrections: httpapi.NewAdminCorrectionsHandler(f.store, nil, nil)}
		want := http.StatusNotFound
		if withGate {
			ops.RequireBilling = httpapi.OperatorAuthz(f.store, management.PermBillingAdjust)
			want = http.StatusInternalServerError
		}
		ops.Mount(g)
		before := f.snapshot(t)
		w := postJSON(t, engine, "/admin/model-requests/"+f.requestID+"/corrections",
			f.env.headers(t, task4Admin), f.body())
		if w.Code != want || f.snapshot(t) != before {
			t.Fatalf("gate=%t without transactional audit: %d %s", withGate, w.Code, w.Body.String())
		}
	}
}

func TestAdminCorrections_EqualMoneyReportsChangedTokens(t *testing.T) {
	for _, wallet := range []bool{false, true} {
		t.Run(fmt.Sprintf("wallet_%t", wallet), func(t *testing.T) {
			f := newCorrectionHTTPFixture(t, wallet, false)
			// 60 input + 10 output * 2 = 80; the original 40 + 20 * 2
			// costs the same but has only 60 rather than 70 tokens.
			body := strings.Replace(f.body(), `"input_tokens":70`, `"input_tokens":60`, 1)
			body = strings.Replace(body, `"output_tokens":20`, `"output_tokens":10`, 1)
			w := postJSON(t, f.env.engine, "/admin/model-requests/"+f.requestID+"/corrections",
				f.env.headers(t, task4Admin), body)
			if w.Code != http.StatusCreated {
				t.Fatalf("metering-only correction: %d %s", w.Code, w.Body.String())
			}
			var source string
			var tokens, adjustments, amount int64
			if err := f.env.db.QueryRow(`SELECT r.usage_status,r.settled_micros,
			    rr.settled_tokens,(SELECT count(*) FROM inference_adjustments WHERE request_id=r.id)
			    FROM inference_requests r JOIN inference_rate_reservations rr ON rr.request_id=r.id
			    WHERE r.id=$1 AND rr.scope='account'`, f.requestID).
				Scan(&source, &amount, &tokens, &adjustments); err != nil {
				t.Fatal(err)
			}
			if source != "reported" || amount != 80 || tokens != 70 || adjustments != 0 {
				t.Fatalf("correction = %s/%d/%d/%d, want reported/80/70/0", source, amount, tokens, adjustments)
			}
		})
	}
}

func TestAdminCorrections_RejectsUnsettledRetryAttempt(t *testing.T) {
	for _, tc := range []struct {
		wallet, equalMoney bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("wallet_%t/equal_money_%t", tc.wallet, tc.equalMoney), func(t *testing.T) {
			f := newCorrectionHTTPFixture(t, tc.wallet, false)
			if _, err := f.env.db.Exec(`UPDATE inference_attempts SET attempt_no=2 WHERE id=$1`, f.attempt); err != nil {
				t.Fatal(err)
			}
			unused := uuid.NewString()
			uow, err := f.store.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer uow.Rollback(context.Background())
			if err := f.store.InsertAttemptTx(context.Background(), uow, &domain.Attempt{
				ID: unused, RequestID: f.requestID, AttemptNo: 1, Status: "failed",
			}); err != nil {
				t.Fatal(err)
			}
			if err := uow.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}

			path := "/admin/model-requests/" + f.requestID + "/corrections"
			before := f.snapshot(t)
			validBody := f.body()
			wantTokens := int64(90)
			if tc.equalMoney {
				validBody = strings.Replace(validBody, `"input_tokens":70`, `"input_tokens":60`, 1)
				validBody = strings.Replace(validBody, `"output_tokens":20`, `"output_tokens":10`, 1)
				wantTokens = 70
			}
			body := strings.Replace(validBody, f.attempt, unused, 1)
			w := postJSON(t, f.env.engine, path, f.env.headers(t, task4Admin), body)
			if w.Code != http.StatusBadRequest || f.snapshot(t) != before {
				t.Fatalf("unsettled retry attempt: %d %s; changed=%t",
					w.Code, w.Body.String(), f.snapshot(t) != before)
			}
			// Rejection must not consume the key needed for the settled retry.
			w = postJSON(t, f.env.engine, path, f.env.headers(t, task4Admin), validBody)
			if w.Code != http.StatusCreated {
				t.Fatalf("settled retry with the same key: %d %s", w.Code, w.Body.String())
			}
			var latest, rate int64
			if err := f.env.db.QueryRow(`SELECT
			    (SELECT sum(COALESCE(input_tokens,0)+COALESCE(cache_read_tokens,0)
			        +COALESCE(cache_write_tokens,0)+COALESCE(output_tokens,0))
			     FROM (SELECT DISTINCT ON (attempt_id) * FROM inference_usage_records
			         WHERE request_id=$1 ORDER BY attempt_id,revision DESC) u),
			    (SELECT settled_tokens FROM inference_rate_reservations
			        WHERE request_id=$1 AND scope='account')`, f.requestID).Scan(&latest, &rate); err != nil {
				t.Fatal(err)
			}
			if latest != wantTokens || rate != wantTokens {
				t.Fatalf("latest usage/TPM = %d/%d, want %d/%d", latest, rate, wantTokens, wantTokens)
			}
		})
	}
}
