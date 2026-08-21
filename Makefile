.PHONY: test test-fast test-pg lint fmt sqlc migrate-up migrate-down migrate-status

test:
	go test ./...

test-fast:
	go test -short ./...

# Requires Docker (testcontainers boots a pgvector-enabled Postgres).
test-pg:
	go test -run TestIngest -v ./...

lint:
	golangci-lint run ./...

fmt:
	go fmt ./...

sqlc:
	sqlc generate

migrate-up:
	goose -dir migrations postgres "$(DATABASE_URL)" up

migrate-down:
	goose -dir migrations postgres "$(DATABASE_URL)" down

migrate-status:
	goose -dir migrations postgres "$(DATABASE_URL)" status
