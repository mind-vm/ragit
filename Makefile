.PHONY: test test-fast test-pg lint fmt sqlc migrate-up migrate-down migrate-status

test:
	go test ./...

test-fast:
	go test -short ./...

# Requires Docker (testcontainers boots a pgvector-enabled Postgres).
test-pg:
	go test -count=1 -run 'TestIngest|TestProcessDocument|TestVectorSearch|TestFullTextSearch|TestSearch_|TestRLS|TestMoveDocumentScope' -v ./...

lint:
	golangci-lint run ./...

fmt:
	go fmt ./...

sqlc:
	sqlc generate

# Applications should call ragit.Migrate() rather than shelling out to goose:
# the migrations are embedded in the binary and tracked in ragit's own
# ragit_migrations table, so a host app's migration sequence never collides
# with this one. These targets exist for local development against a scratch
# database, and must pass -table to look at the right version table.
migrate-up:
	goose -dir migrations -table ragit_migrations postgres "$(DATABASE_URL)" up

migrate-down:
	goose -dir migrations -table ragit_migrations postgres "$(DATABASE_URL)" down

migrate-status:
	goose -dir migrations -table ragit_migrations postgres "$(DATABASE_URL)" status
