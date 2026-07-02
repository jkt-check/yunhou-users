.PHONY: build run test e2e migrate lint deps generate-keys

build:
	go build -o bin/server ./cmd/server

run:
	go run ./cmd/server

test:
	go test -race -cover ./internal/...

e2e:
	go test -race -count=1 -v ./tests/e2e/

migrate:
	@echo "Run migrations manually (in order):"
	@echo "  psql -d yunhou_users -f migrations/001_init.sql"
	@echo "  psql -d yunhou_users -f migrations/002_simplify_plans.sql"
	@echo "  psql -d yunhou_users -f migrations/003_payments.sql"
	@echo "  psql -d yunhou_users -f migrations/004_ls_channel.sql"

lint:
	go vet ./...

deps:
	go mod tidy

generate-keys:
	@mkdir -p keys
	openssl genpkey -algorithm RSA -out keys/private.pem -pkeyopt rsa_keygen_bits:2048
	openssl rsa -pubout -in keys/private.pem -out keys/public.pem
