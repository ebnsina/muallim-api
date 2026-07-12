# muallim-api

Backend for a multi-tenant learning management system. A modular monolith in Go, backed by Postgres, publishing an OpenAPI 3.1 contract.

Its first client is [`muallim-web`](../muallim-web) (SvelteKit). A WordPress plugin, mobile apps, and an LTI tool are planned, and all of them consume this same API — which is why the spec is treated as a public interface rather than an implementation detail.

## Stack

Go 1.26 · Postgres 17 · Huma v2 (OpenAPI 3.1, RFC 9457 errors) · pgx v5 · goose · River (Postgres-backed jobs, no Redis) · argon2id + JWT.

## Quickstart

```bash
cp .env.example .env
make db-create      # role + muallim/muallim_test databases
make migrate        # apply migrations to both
make run            # serve on :8080
open http://localhost:8080/docs      # interactive API reference
```

`make seed` builds a demo workspace and prints its accounts. Full setup, development commands, and CI are in [docs/installation.md](docs/installation.md).

## Documentation

- [Installation](docs/installation.md) — requirements, setup, demo accounts, development, CI
- [Architecture](docs/architecture.md) — layout, multi-tenancy, identity and access, authoring, learning, errors
- [Performance](docs/performance.md) — the guarantees, and the tests that hold them
- [The OpenAPI contract](docs/openapi.md) — generating the spec, and why it is a public interface
- [GUIDELINES.md](GUIDELINES.md) — the engineering contract

## Contributing

Read [GUIDELINES.md](GUIDELINES.md) first. It is the engineering contract, and a change that violates it should not merge.

## License

Not yet licensed.
