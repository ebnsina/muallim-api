# Installation

## Requirements

Go 1.26 and Postgres 17. Docker optional (`make db-up`) if you would rather not run Postgres locally.

The database role must **not** be a superuser: superusers bypass row-level security, which would silently disable tenant isolation. Both paths below create it `NOSUPERUSER NOBYPASSRLS`, and CI asserts as much before any test runs.

## Getting started

```bash
cp .env.example .env
make db-create      # role + muallim/muallim_test databases
make migrate        # apply migrations to both
make run            # serve on :8080
```

Or in Docker, where `scripts/postgres-init.sql` creates the same role and databases on first boot, so `make db-create` is unnecessary:

```bash
cp .env.example .env
make db-up          # Postgres 17 on :5432
make migrate
make run
```

```bash
curl -s localhost:8080/v1/healthz | jq
curl -s localhost:8080/v1/readyz  | jq          # also checks Postgres
curl -s -H 'Host: acme.muallim.test' localhost:8080/v1/courses | jq
open http://localhost:8080/docs                 # interactive API reference
```

Every setting is an environment variable prefixed `MUALLIM_`; `.env.example` lists them all with their defaults.

## Object storage

Assignment uploads go to an S3-compatible store. `make storage-up` starts a local MinIO on `:9002`, with its console on `:9003`. Without it, `MUALLIM_S3_ENDPOINT` is unset and the API refuses every upload with a 503.

## Development

```bash
make check          # vet, format check, and race-enabled tests — what CI runs
make test           # database tests skip without MUALLIM_TEST_DATABASE_URL
make test-db        # every test, including the ones that need Postgres
make lint
make fmt
make seed           # a demo workspace with a demo account and enough data to click around
make seed-huge      # the same at ~1.1M rows, to judge a page at the size it will be
make seed-test      # only the bare workspace the end-to-end tests need
make worker         # background jobs
make build          # binaries into bin/
```

## Demo accounts

`make seed` builds the `localhost` workspace and prints this table. Every account
shares one password.

| Email | Password | Role |
|---|---|---|
| `demo@muallim.test` | `demo-password-please-change` | owner |
| `instructor@muallim.test` | `demo-password-please-change` | instructor |
| `marker@muallim.test` | `demo-password-please-change` | instructor, with essays waiting |
| `student@muallim.test` | `demo-password-please-change` | student |

These are fixtures, not secrets: they exist only in a database `make seed` will
happily delete and rebuild, on a reserved `.test` domain that resolves nowhere.
`make seed -reset` drops the workspace and every account in it, which is why the
accounts you had before it ran are gone.

The seeder writes assignments but no submissions: it holds a database connection and cannot reach the object store, and a row pointing at a key with no object behind it is a download that 404s. Upload a real file as `student@` instead.

## CI

`.github/workflows/ci.yml` runs `make check`, `staticcheck`, `make spec`, and a build, against a real Postgres 17.

The `muallim` role it creates is `NOSUPERUSER NOBYPASSRLS`, and a step asserts as much before any test runs. A superuser bypasses row-level security, so every tenant-isolation test would pass against a database enforcing nothing — the most expensive kind of green build.
