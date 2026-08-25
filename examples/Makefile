.PHONY: up down logs verify build fmt clean

# Bring the shared infrastructure up and wait for it to be healthy. xberg
# downloads models on first boot, so the first `up` is slow.
up:
	docker compose up -d --wait

down:
	docker compose down

# Also drops the volumes. Needed after changing the embedding dimension: the
# vector column's width is fixed at CREATE TABLE time, so a different dimension
# needs a fresh database, not another migration.
clean:
	docker compose down -v

logs:
	docker compose logs -f

# Checks the environment is what the examples assume — RLS live, pgvector codec
# registered, both migration lines applied. Run this before anything else.
verify:
	go run ./verify

build:
	go build ./...

fmt:
	go fmt ./...
