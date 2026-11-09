package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These cases catch per-process counters or locks, and partial commits on
// rejection. Different Keys must still share the account-level bottlenecks.
func TestTwoProcessContendRateAndConcurrency(t *testing.T) {
	for _, mode := range []string{
		"account_rpm", "policy_rpm", "key_rpm", "account_tpm",
		"account_concurrency", "key_concurrency",
	} {
		t.Run(mode, func(t *testing.T) {
			db, store := testDB(t)
			f := seedFixture(t, store, true)
			priceID := seedPrice(t, store, f.modelID)
			keys := []string{f.keyID, f.keyID}
			if strings.HasPrefix(mode, "key_") {
				column := "rpm_limit"
				if mode == "key_concurrency" {
					column = "concurrency_limit"
				}
				// column is an internal test constant, never user input.
				if _, err := db.Exec("UPDATE inference_api_keys SET "+column+"=3 WHERE id=$1", f.keyID); err != nil {
					t.Fatal(err)
				}
			} else {
				keys[1] = addSiblingKey(t, store, f.accountID)
			}
			const demandPerProcess = 8
			results := runRateWorkerPair(t, f, priceID, keys, mode, demandPerProcess)
			var admitted, rejected, other int
			for _, r := range results {
				admitted += r.Admitted
				if strings.HasSuffix(mode, "concurrency") {
					rejected += r.InsufficientCapacity
					other += r.Other + r.QuotaExceeded + r.RateLimited
				} else {
					rejected += r.RateLimited
					other += r.Other + r.QuotaExceeded + r.InsufficientCapacity
				}
			}
			if results[0].PID <= 0 || results[1].PID <= 0 || results[0].PID == results[1].PID {
				t.Fatalf("expected two real worker processes: %+v", results)
			}
			if admitted != 3 || rejected != 13 || other != 0 {
				t.Errorf("admitted=%d rejected=%d other=%d, want 3/13/0: %+v", admitted, rejected, other, results)
			}
			if n := countRequests(t, db, f.entID, ""); n != 3 {
				t.Errorf("requests=%d, want 3 (rejections must roll back)", n)
			}
			if used, reserved := windowSums(t, db, f.entID); used != 0 || reserved != 90 {
				t.Errorf("used=%d reserved=%d, want 0/90 (3 windows x 3 requests x 10)", used, reserved)
			}
			wantLeases := 0
			if strings.HasSuffix(mode, "concurrency") {
				wantLeases = 3
			}
			for _, check := range []struct {
				query string
				want  int
			}{
				{`SELECT COUNT(*) FROM inference_rate_reservations`, 6},
				{`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='account' AND state='reserved' AND reserved_tokens=11`, 3},
				{`SELECT COUNT(*) FROM inference_reservations WHERE state='held'`, 12},
				{`SELECT COUNT(*) FROM inference_reservations WHERE target_kind='key_budget' AND state='held' AND amount_micros=10`, 3},
				{`SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`, wantLeases},
				{`SELECT COUNT(*) FROM inference_ledger_entries`, 0},
			} {
				var got int
				if err := db.Get(&got, check.query); err != nil {
					t.Fatal(err)
				}
				if got != check.want {
					t.Errorf("%s = %d, want %d", check.query, got, check.want)
				}
			}
		})
	}
}

func runRateWorkerPair(t *testing.T, f fixture, priceID string, keys []string, mode string, demand int) []workerResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	dir := t.TempDir()
	startPath := filepath.Join(dir, "start")
	type result struct {
		worker workerResult
		err    error
	}
	done := make(chan result, 2)
	readyPaths := make([]string, 2)
	for i := range readyPaths {
		readyPaths[i] = filepath.Join(dir, "ready-"+strconv.Itoa(i))
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestQuotaWorkerProcess$", "-test.count=1", "-test.timeout=75s")
		cmd.Env = append(os.Environ(),
			"KAYA_QUOTA_WORKER=1", "DATABASE_URL="+testDSN,
			"KAYA_WORKER_DEMAND="+strconv.Itoa(demand),
			"KAYA_WORKER_HOLD=10", "KAYA_WORKER_LIMIT=1099511627776",
			"KAYA_WORKER_ANCHOR=2026-09-08T04:00:00Z",
			"KAYA_WORKER_ACCOUNT="+f.accountID, "KAYA_WORKER_KEY="+keys[i],
			"KAYA_WORKER_ENT="+f.entID, "KAYA_WORKER_POLICY="+f.policyID,
			"KAYA_WORKER_MODEL="+f.modelID, "KAYA_WORKER_PRICE="+priceID,
			"KAYA_WORKER_MODE="+mode, "KAYA_WORKER_READY="+readyPaths[i],
			"KAYA_WORKER_START="+startPath,
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := cmd.CombinedOutput()
			var r workerResult
			found := false
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "RESULT ") {
					found = true
					if decodeErr := json.Unmarshal([]byte(strings.TrimPrefix(line, "RESULT ")), &r); decodeErr != nil {
						err = decodeErr
					}
				}
			}
			if !found && err == nil {
				err = fmt.Errorf("missing worker result")
			}
			if err != nil {
				err = fmt.Errorf("%w\n%s", err, out)
			}
			done <- result{r, err}
		}()
	}
	// Neither worker admits until both independent pools are ready.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready := 0
		for _, path := range readyPaths {
			if _, err := os.Stat(path); err == nil {
				ready++
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		if ready == 2 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("worker exited before admission gate: %+v", r)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if err := os.WriteFile(startPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var results []workerResult
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			results = append(results, r.worker)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	t.Logf("two-process %s: %+v", mode, results)
	return results
}
