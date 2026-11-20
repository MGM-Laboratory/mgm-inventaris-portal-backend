# LabMGM inventory portal, backend

The backend of the LabMGM inventory portal, written in Go with PostgreSQL. The frontend lives in [`mgm-inventaris-portal-frontend`](https://github.com/MGM-Laboratory/mgm-inventaris-portal-frontend).

[![CI](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/ci.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/ci.yaml)
[![Security](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/security.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/security.yaml)
[![Config lint](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/config.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/config.yaml)
[![Release Please](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/release-please.yaml/badge.svg)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/actions/workflows/release-please.yaml)

[![License](https://img.shields.io/github/license/MGM-Laboratory/mgm-inventaris-portal-backend)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/blob/main/LICENSE)
[![Go](https://img.shields.io/badge/Go-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)

[![Last commit](https://img.shields.io/github/last-commit/MGM-Laboratory/mgm-inventaris-portal-backend)](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commits/main)
[![Conventional Commits](https://img.shields.io/badge/Conventional%20Commits-1.0.0-FE5196?logo=conventionalcommits&logoColor=white)](https://www.conventionalcommits.org)

## Stack

- Go 1.27, Fiber v3, pgx v5 and PostgreSQL 17, with golang-migrate for the schema.
- CI, release automation and dependency updates on GitHub Actions, Release Please and Renovate. See [`docs/ci-cd.md`](docs/ci-cd.md).

## Quick start

The service so far holds the settings, the database schema, the health routes, the route `GET /api/config` and the routes of the guest session (`POST /api/session/guest`, `GET /api/session` and `DELETE /api/session`). The other routes of the contract are not built yet.

1. Have a PostgreSQL 17 database and a role that owns it. The service applies its migrations at start.
2. Copy `.env.example` to `.env` and set `DATABASE_URL` and `PUBLIC_BASE_URL`. The service reads the process environment and not the file, so load the file with your shell or your process manager.
3. For a local run over plain http, also set `ENVIRONMENT=development` and `SECURE_COOKIE=false`. Production refuses both an http `PUBLIC_BASE_URL` and a `SECURE_COOKIE` of false.
4. Start the service with `go run ./cmd/api`.

The API listens on `HTTP_ADDR` and the health routes listen on `HEALTH_ADDR`. The health port is an internal port that the proxy does not route.

```sh
curl http://127.0.0.1:8080/api/config
curl http://127.0.0.1:8081/healthz
curl http://127.0.0.1:8081/readyz
```

## Configuration

The service reads these environment variables at start. A bad value stops the start with a message that names the setting and never prints its value. `.env.example` lists every one of them.

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | none, required | The PostgreSQL connection URL. It holds the password and is never logged |
| `PUBLIC_BASE_URL` | none, required | The public address of the portal, with the scheme and the host only. Production needs `https`. A default port (443 for https, 80 for http) is dropped, because a browser leaves it out of the `Origin` header. The QR address of a unit is this value, `/aset/` and the tag |
| `HTTP_ADDR` | `:8080` | The listen address of the API |
| `HEALTH_ADDR` | `127.0.0.1:8081` | The listen address of `/healthz` and `/readyz`. It must differ from `HTTP_ADDR` |
| `TRUSTED_PROXIES` | empty | A comma separated list of IP addresses and CIDR ranges of the reverse proxy. The service reads `X-Real-IP` only from a request whose peer is in the list. Empty trusts no header, so behind a proxy every client counts as the proxy address, and production logs a warning at start |
| `ENVIRONMENT` | `production` | `development` or `production` |
| `SECURE_COOKIE` | `true` | The `Secure` flag of the session cookie. Only `development` may set it to `false` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `DB_MAX_CONNS` | `8` | The size of the pool of one copy, from 1 to 50 |
| `MIGRATE_ON_START` | `true` | Apply the embedded migrations at start, under an advisory lock |
| `MAX_ITEMS_PER_REQUEST` | `10` | The most items in one request, from 1 to 100 |
| `MAX_DURATION_DAYS` | `30` | The longest loan in days, from 1 to 365 |
| `MAX_OPEN_TICKETS` | `3` | The most open tickets for one email or one NIM or NIP, from 1 to 50 |
| `GUEST_SIGNIN_PER_HOUR` | `60` | The most guest sign in requests that one client address may send in an hour, from 1 to 100000. Every request counts, and an IPv6 address counts as its `/64` prefix |

## Documentation

- [`docs/ci-cd.md`](docs/ci-cd.md) lists the workflows, the required checks and the release flow.
- [`openapi.yaml`](openapi.yaml) is the full contract of the API, with every route, body, answer and error code.
- [`docs/architecture.md`](docs/architecture.md), [`docs/data-model.md`](docs/data-model.md), [`docs/api.md`](docs/api.md) and [`docs/errors.md`](docs/errors.md) describe the planned design, the stock rules, the contract rules and the error codes.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`SECURITY.md`](SECURITY.md) hold the rules for contributions and for reporting a vulnerability.

## Project layout

```text
cmd/api/          The entry point of the service
internal/         The settings, the HTTP servers, the sessions, the rate limits, the janitor, the database pool, the migrations and the test helper
docs/             Design pages and CI/CD documentation
.github/          Workflows, the lint tool images, issue forms and the pull request template
```

## Contributing and security

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before you open a pull request. Report a security issue privately as [`SECURITY.md`](SECURITY.md) says, and do not open a public issue. The rules of behavior are in [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

GNU General Public License v3.0. See [`LICENSE`](LICENSE).
