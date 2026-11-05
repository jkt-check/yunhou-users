# Users Model Gateway — Verification Runbook (2026-11-05)

Companion runbook for the **Users model gateway completion** plan
(`.superpowers/sdd/2026-11-05-users-model-gateway-completion/`).

This file is the single place where every verification command, its exit code,
and its skip/fail inventory is recorded. Each task T1–T10 appends its own
evidence to the matching section. **A skipped database test is not a pass.**

Scope: Users model gateway only. No adjacent repositories, no product sales,
no payments/refunds, no deployment, no merge, no push.

---

## 1. Baseline environment

| Item | Value |
|---|---|
| Worktree | `/Users/jack-tian/Downloads/github/yunhou-users-model-gateway` (isolated) |
| Branch | `fix/model-gateway-completion` |
| T0 base SHA | `35cf836274e870d5e0cd9604c264a57f28ca8f8b` |
| Plan merge base | `299fc49` |
| Go | `go1.27.1 darwin/arm64` |
| PostgreSQL | 16.15 (Homebrew `/opt/homebrew/opt/postgresql@16`), disposable instance |
| OS | macOS (Apple Silicon) |

### 1.1 Module cache / network

`go mod download` fails against `proxy.golang.org` (i/o timeouts) in this
environment. The **existing module cache is sufficient**: every command below
runs with

```bash
export GOPROXY=off
export GOFLAGS=-mod=mod
```

`go build ./...` completes with exit code 0 under `GOPROXY=off`, which proves
all dependencies are already cached. `go.mod` / `go.sum` are **never** modified
(verified with `git diff --stat go.mod go.sum` → empty).

### 1.2 Disposable PostgreSQL recipe

The baseline **must never** use the default local business database. Several
`TestMain` functions fall back to `postgres://postgres@localhost/yunhou_users`
when `DATABASE_URL` is unset, and their fixtures `TRUNCATE` shared tables — so
an unset DSN silently points at (and wipes) whatever is on 5432.

Create one instance per verification session, in a fresh temporary directory,
on a port confirmed free **before** the instance exists:

```bash
set -euo pipefail
PGBIN=/opt/homebrew/opt/postgresql@16/bin

# 1. pick a port and PROVE it is free BEFORE the instance exists.
#    (the baseline run used 55432; any free high port is equivalent)
PGPORT=55432
while lsof -nP -iTCP:"$PGPORT" -sTCP:LISTEN >/dev/null 2>&1 \
   || nc -z 127.0.0.1 "$PGPORT" >/dev/null 2>&1; do
  PGPORT=$((PGPORT + 1))
done
echo "using free port $PGPORT"

# 2. fresh temp dir; initdb refuses a non-empty PGDATA, so keep logs outside it
RUN_DIR=$(mktemp -d /tmp/t0-pgrun-XXXXXXXX)
PGDATA="$RUN_DIR/pgdata"; mkdir -p "$PGDATA"; chmod 700 "$RUN_DIR" "$PGDATA"
"$PGBIN/initdb" -D "$PGDATA" -U t0 --auth=trust --encoding=UTF8 --locale=C \
  >"$RUN_DIR/initdb.log" 2>&1

# 3. start: loopback only, unix socket inside the same temp dir
SOCK_DIR=$(mktemp -d /tmp/t0-pgsock-XXXXXXXX)
"$PGBIN/pg_ctl" -D "$PGDATA" -l "$RUN_DIR/server.log" -w -t 60 \
  -o "-p $PGPORT -k $SOCK_DIR -c listen_addresses=127.0.0.1 -c unix_socket_directories=$SOCK_DIR" \
  start

# 4. two disposable databases, so the app-side and e2e-side suites cannot
#    contaminate each other
"$PGBIN/createdb" -h 127.0.0.1 -p "$PGPORT" -U t0 t0_app
"$PGBIN/createdb" -h 127.0.0.1 -p "$PGPORT" -U t0 t0_e2e

# 5. export the DSNs at runtime — the DSN string is NEVER committed
export PGHOST=127.0.0.1 PGPORT="$PGPORT" PGUSER=t0
export DATABASE_URL="postgres://${PGUSER}@${PGHOST}:${PGPORT}/t0_app?sslmode=disable"
export E2E_DATABASE_URL="postgres://${PGUSER}@${PGHOST}:${PGPORT}/t0_e2e?sslmode=disable"
```

**Stop only the instance you created** (never `pkill postgres`, never touch
another instance's data directory):

```bash
"$PGBIN/pg_ctl" -D "$PGDATA" -m fast stop
rm -rf "$RUN_DIR" "$SOCK_DIR"
```

Both DSNs are always exported **explicitly** for every test command. The
snippet above is the only DSN material in this repository: it is a runtime
template built from `PGHOST` / `PGPORT` / `PGUSER`, it points at a throwaway
instance living in a temp directory, and no DSN of any real or persistent
system is committed.

If `DATABASE_URL` is empty the `TestMain` helpers in
`internal/inference/{catalog,httpapi,postgres,gateway}` silently fall back to
`postgres://postgres@localhost/yunhou_users`; when that connect fails they set
`testDSN = ""` and every DB-backed test calls `t.Skip(...)`. That silent skip
is the main hazard this runbook exists to prevent.

---

## 2. T0 — isolated test environment and baseline

Recorded on 2026-11-06 at base SHA `35cf836`.

### 2.1 Workspace baseline

| Command | Exit | Result |
|---|---|---|
| `pwd` | 0 | `/Users/jack-tian/Downloads/github/yunhou-users-model-gateway` |
| `git branch --show-current` | 0 | `fix/model-gateway-completion` |
| `git status --short` | 0 | *(empty — clean worktree)* |
| `git rev-parse HEAD` | 0 | `35cf836274e870d5e0cd9604c264a57f28ca8f8b` |
| `go build ./...` (`GOPROXY=off`) | 0 | module cache sufficient |

### 2.2 Pure-unit baseline

```bash
go test -count=1 ./internal/inference/access ./internal/inference/accounting \
  ./internal/inference/quota ./internal/inference/management \
  ./internal/inference/domain ./internal/inference/providers \
  ./internal/inference/providers/connector
```

**Exit code: 0.** All 7 packages `ok`.

| Package | Result |
|---|---|
| `internal/inference/access` | ok |
| `internal/inference/accounting` | ok |
| `internal/inference/quota` | ok |
| `internal/inference/management` | ok |
| `internal/inference/domain` | ok |
| `internal/inference/providers` | ok |
| `internal/inference/providers/connector` | ok |

Verbose re-run for the skip inventory: **448 PASS, 0 SKIP, 0 FAIL**, exit 0.
These seven packages contain no `TestMain` and no `sqlx.Connect` /
`sql.Open` / `migrate.Apply` call — they are genuinely DB-free.

### 2.3 Migration idempotency

Run against the disposable `t0_app` database (`DATABASE_URL` explicit).
49 migration files present in `migrations/`.

| # | Command | Exit | Result |
|---|---|---|---|
| 1 | `go run ./cmd/migrate` | **0** | `[migrate] applied=49 skipped=0` |
| 2 | `go run ./cmd/migrate` | **0** | `[migrate] applied=0 skipped=49` (49 × `skip … already applied`) |
| 3 | `go run ./cmd/migrate -status` | **0** | 49 × `✅` |

The e2e-side disposable database (`t0_e2e`, via `E2E_DATABASE_URL`) was migrated
the same way: run 1 `applied=49 skipped=0` (exit 0), run 2 `applied=0 skipped=49`
(exit 0). `_migrations` ledger holds **49** rows in each database; `t0_app`
holds **56** tables in `public`.

### 2.4 Serial inference test baseline

```bash
DATABASE_URL=<disposable> go test -race -count=1 -p 1 ./internal/inference/...
```

**Exit code: 0.** All 14 packages `ok` under `-race`:

`access`, `accounting`, `catalog`, `credentials`, `domain`, `gateway`,
`httpapi`, `management`, `postgres`, `providers`, `providers/connector`,
`quota`, `routing`, `workers`.

Verbose re-run for the skip inventory: **1088 PASS, 0 FAIL, 1 SKIP**, exit 0.

### 2.5 Existing gateway integration tests

```bash
DATABASE_URL=<disposable> go test -race -count=1 -v -run 'TestProtocols_' ./tests/integration/...
```

**Exit code: 0.** 5 top-level tests / 13 subtests `PASS`, 0 skips:

| Test | Result |
|---|---|
| `TestProtocols_MultiTurnToolCallIDPreserved` | PASS (3 subtests) |
| `TestProtocols_BrokenStreamSequence` | PASS (3 subtests) |
| `TestProtocols_LedgerConsistencyCrossProtocol` | PASS |
| `TestProtocols_ExplicitRejectionMatrix` | PASS (5 subtests) |
| `TestProtocols_SessionBindingAttributionAndIsolation` | PASS |

Full package (all files in `tests/integration/...`):

```bash
DATABASE_URL=<disposable> go test -race -count=1 -v ./tests/integration/...
```

**Exit code: 0** — 52 PASS, 0 FAIL, 2 SKIP.

`tests/integration` cannot skip on a missing database: `setupDB` calls
`dbURL()` (which reads `DATABASE_URL`) and `t.Fatalf`s on connect failure —
the file header states this explicitly ("skip 不算通过").

### 2.6 Skip inventory (T0)

Every skip observed, with the reason it is **not** a lost database check:

| # | Skipped test | Package | Reason | Lost coverage? |
|---|---|---|---|---|
| 1 | `TestQuotaWorkerProcess` | `internal/inference/postgres` | Child-process entry point; skips unless re-executed with `KAYA_QUOTA_WORKER=1` (`internal/inference/postgres/quota_concurrency_test.go:773`). The parent `TestTwoProcessContendLastCredit` re-execs the binary with that flag and **PASSED** with real contention output (`proc A {Admitted:1 QuotaExceeded:7 Other:0}, proc B {Admitted:2 QuotaExceeded:6 Other:0}`), so the two-process scenario really executed. | No |
| 2 | `TestLoadDrill_Throughput` | `tests/integration` | Explicit non-default gate `KAYA_LOAD=1` (`tests/integration/kaya_load_drill_test.go:35`). Load drill, not a correctness test. | No |
| 3 | `TestLoadDrill_LongConnections` | `tests/integration` | Same `KAYA_LOAD=1` gate. | No |

**Database-test skips: 0.** No inference or integration test skipped for
"no postgres available".

### 2.7 Proof that the disposable DB was actually exercised

Post-run row counts in the disposable `t0_app` database:

```
inference_requests=2  inference_ledger_entries=1  inference_api_keys=1
inference_billing_accounts=1  users=1  plans=3
```

And the host had exactly one PostgreSQL listener — the disposable instance:

```
postgres 66667 jack-tian 5u IPv4 ... TCP 127.0.0.1:55432 (LISTEN)
```

`lsof -nP -iTCP -sTCP:LISTEN | grep -i postgres` returned that single line
only. Databases in the disposable instance: `postgres`, `t0_app`, `t0_e2e`,
`template0`, `template1` — no business database was reachable or touched.

### 2.8 Supplementary CI-equivalent sweep

`make ci-test` equivalent (no coverage profile — the coverage artefact is not
part of T0):

```bash
DATABASE_URL=<disposable> go test -race -count=1 -p 1 ./internal/... ./cmd/...
```

**Exit code: 0** — 32 packages `ok`, 0 `FAIL`
(`internal/analytics`, `internal/billing/{paddle,paypal,wechat}`,
`internal/config`, `internal/handler`, all 14 `internal/inference/*`
packages, `internal/llm`, `internal/middleware`, `internal/migrate`,
`internal/model`, `internal/relay`, `internal/repo`, `internal/router`,
`internal/service`, `internal/util`, `cmd/admin-bootstrap`,
`cmd/inference-entitlement-backfill`, `cmd/server`; `cmd/migrate` has no test
files).

This is the same package set the `go-ci` job runs, minus the coverage
profile. **The whole repository baseline is green against the disposable
PostgreSQL** — a later task that turns any of these red has introduced the
regression itself.

Runtime note: the sweep takes ~7 min 20 s, dominated by `internal/migrate`
(70 s), `internal/service` (67 s), `internal/inference/httpapi` (53 s) and
`internal/util` (44 s).

---

## 3. Standing rules for every task

1. **Always** export `DATABASE_URL` and `E2E_DATABASE_URL` explicitly, pointing
   at a disposable instance. Never rely on the default fallback DSN.
2. **Always** pass `-p 1` to `go test` when the suite touches the database:
   `internal/repo`, `internal/service`, `internal/inference/*` and
   `tests/integration` share one Postgres and `TRUNCATE` each other's tables.
3. **Always** use `-race` for the inference and integration suites.
4. A **skipped** DB test is not a pass. Record every skip and its reason.
5. `GOPROXY=off` + `GOFLAGS=-mod=mod`; never modify `go.mod` / `go.sum`.
6. Stop only the PostgreSQL instance you created (`pg_ctl -D <your PGDATA> stop`).
7. Never commit a DSN, a key, a refresh token, image base64 or a user
   conversation into the repo, the logs, or this runbook.
8. Stage only the files belonging to the current task. Never `git add .`.
   No push, no merge, no deploy.

---

## 4. Per-task evidence (T1–T10)

Each task appends: the exact command, the exit code, the skip inventory, the
migration result if it added a migration, and the files changed.

### T1 — OAuth hot-path authentication

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T2 — Window-less settlement and conflict classification

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T3 — Provider disable, model state and sticky consistency

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T4 — PostgreSQL-backed RPM/TPM

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T5 — Per-key concurrency

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T6 — Protocol error shape and Retry-After

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T7 — Configuration-behaviour convergence and connect timeouts

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T8 — Capability routing and protocol truthfulness

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T9 — Probe, capability description and operational visibility

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

### T10 — Integration acceptance and implementation report

- Status: not started
- Commands / exit codes:
- Skips:
- Migration:
- Files changed:
- Notes:

---

## 5. Open concerns

- The Go module proxy is unreachable from this environment; the cached module
  set is currently sufficient. A task that introduces a **new** external
  dependency cannot be verified here — report the blocker instead of
  weakening the verification.
- `internal/inference/httpapi` takes ~58 s under `-race` and dominates the
  inference baseline runtime; budget for it when running the full suite.
