.PHONY: build run test migrate lint

build:
	go build -o bin/server ./cmd/server

run:
	go run ./cmd/server

test:
	go test -race -cover ./...

migrate:
	@echo "Run migration manually:"
	@echo "  psql -d yunhou_users -f migrations/001_init.sql"

lint:
	go vet ./...

deps:
	go mod tidy

generate-keys:
	@mkdir -p keys
	openssl genpkey -algorithm RSA -out keys/private.pem -pkeyopt rsa_keygen_bits:2048
	openssl rsa -pubout -in keys/private.pem -out keys/public.pem
