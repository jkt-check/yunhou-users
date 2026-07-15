.PHONY: build run test e2e migrate migrate-status lint deps generate-keys

build:
	go build -o bin/server ./cmd/server
	go build -o bin/migrate ./cmd/migrate

run:
	go run ./cmd/server

test:
	go test -race -cover ./internal/...

# ci-test mirrors the GitHub Actions workflow so local runs catch the
# same regressions CI would.
ci-test:
	go test -race -coverprofile=coverage.out ./internal/... ./cmd/...

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