# ragit examples

Two example host applications, built to answer one question: **how much of the
pipeline should xberg own, and does ragit's current API let a consumer choose?**
See [`../docs/examples-plan.md`](../docs/examples-plan.md) for the full plan and
the shape questions these are measured against.

| | extraction | chunking | embedding | storage + search |
|---|---|---|---|---|
| `extract-only/` *(not written yet)* | xberg | ragit | EdenAI | ragit |
| `xberg-owned/` *(not written yet)* | xberg | xberg | xberg | ragit |

Both run against the same infrastructure, so the only thing that differs
between them is pipeline shape.

## Running it

```bash
make up          # postgres+pgvector, xberg, minio
make verify      # assert the environment is what the examples assume
```

`make verify` is not a smoke test — it is the gate. Two of the properties these
examples depend on fail *silently*:

- **Row-level security is inert for a superuser.** An example connecting as the
  compose file's `POSTGRES_USER` would demonstrate a tenant isolation it does
  not have, and every assertion about it would still pass. So `verify` writes a
  row as one tenant and reads it back four ways with raw SQL — including once
  as the superuser, which *must* see it, because otherwise three empty results
  prove nothing but an empty table.
- **pgvector's binary codec is registered per connection.** Without it
  embeddings still move, as text, several times slower, and nothing errors.

Configuration comes from `.env` (copy `.env.example`) and then the environment,
which wins. The EdenAI key is read from the environment only and is never
written into this repo:

```bash
set -a; . ~/.config/envs/valiro-go.env; set +a
```

Ports are deliberately odd — Postgres on 5455, xberg on 8234, MinIO on 9200 —
because the obvious alternatives were already taken on the machine this was
built on. Override with `POSTGRES_PORT`, `XBERG_PORT`, `MINIO_PORT`.

## What's here

```
compose.yaml            postgres+pgvector, xberg, minio
verify/                 the environment gate described above
fixtures/               three documents: markdown, csv, pdf
internal/bootstrap/     migrations, the unprivileged role, the host app's schema
```

`internal/bootstrap` is where a host application's startup path would live. Two
things in it are worth reading before writing a real one:

- **`MigrateDemoSchema`** runs the *host application's* own schema, declared
  with sqlb, entirely separately from `ragit.Migrate`. Two migration lines in
  one database, neither knowing the other's version numbers, is the thing the
  `ragit_` table prefix exists for — and `demo_uploads` sitting next to
  `ragit_documents` is what makes these examples host applications rather than
  CLI wrappers.
- **`ensureAppRole`** creates the role the examples connect as, spelling out
  `NOSUPERUSER NOBYPASSRLS`. Every consumer of ragit has to do this or its RLS
  policies are decorative. Whether that should stay the consumer's job is one of
  the open shape questions.

## This is its own Go module

`examples/go.mod` has a `replace` back to `../`. The examples pull in things the
library does not, and folding those into ragit's own `go.mod` would misrepresent
what importing ragit costs.

```bash
cd examples && go build ./...
```
