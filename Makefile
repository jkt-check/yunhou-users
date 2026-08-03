.PHONY: build run test e2e migrate migrate-status lint deps generate-keys regen-test-keys

build:
	go build -o bin/server ./cmd/server
	go build -o bin/migrate ./cmd/migrate

run:
	go run ./cmd/server

# -p 1: package test binaries run sequentially. internal/repo and
# internal/service DB-backed tests share ONE Postgres (both default
# dbURL() to postgres://postgres@localhost/yunhou_users) and each wipes
# tables in setup — run in parallel they TRUNCATE each other's data
# (deadlocks, FK violations, duplicate keys). Serializing packages is the
# cheap fix; per-package test DBs would force every dev to migrate two.
test:
	go test -race -cover -p 1 ./internal/...

# ci-test mirrors the GitHub Actions workflow so local runs catch the
# same regressions CI would.
ci-test:
	go test -race -coverprofile=coverage.out -p 1 ./internal/... ./cmd/...

e2e:
	go test -race -count=1 -v ./tests/e2e/

# Apply all pending migrations from the directory named by MIGRATIONS_DIR
# (default /migrations; falls back to ./migrations in dev). The binary
# records each applied file in the _migrations ledger so re-running is a
# no-op. See internal/migrate and migrations/README.md for the contract.
migrate:
	go run ./cmd/migrate

# Print the ledger status without applying anything. Useful before a
# deploy to confirm what's pending.
migrate-status:
	go run ./cmd/migrate -status

lint:
	go vet ./internal/... ./cmd/...

deps:
	go mod tidy

generate-keys:
	@mkdir -p keys
	openssl genpkey -algorithm RSA -out keys/private.pem -pkeyopt rsa_keygen_bits:2048
	openssl rsa -pubout -in keys/private.pem -out keys/public.pem

# Regenerate the WeChat sign-test fixtures (testdata/sign_test_key.pem +
# sign_test_cert.pem + sign_test_vector.json). Use only when the
# sign-string format changes (e.g. WeChat docs revision) — never run
# this in CI. After regenerating, manually update the embedded
# Authorization header in sign_test_vector.json to match.
regen-test-keys:
	@mkdir -p internal/billing/wechat/testdata
	@openssl genrsa -out internal/billing/wechat/testdata/sign_test_key.pem 2048
	@openssl req -new -x509 -key internal/billing/wechat/testdata/sign_test_key.pem \
	    -out internal/billing/wechat/testdata/sign_test_cert.pem -days 36500 \
	    -subj "/CN=yunhou-users-test"
	@echo "Generated new test keys. Re-capture sign_test_vector.json via the"
	@echo "capture script in the plan, then run: go test ./internal/billing/wechat/..."