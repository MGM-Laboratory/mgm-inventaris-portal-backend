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
- A container image for linux/amd64 and linux/arm64 on a distroless base, published to GitHub Container Registry as `ghcr.io/mgm-laboratory/mgm-inventaris-portal-backend`. See [`docs/deployment.md`](docs/deployment.md).
- CI, release automation and dependency updates on GitHub Actions, Release Please and Renovate. See [`docs/ci-cd.md`](docs/ci-cd.md).

## Quick start

The service so far holds the settings, the database schema, the health routes, the route `GET /api/config`, the routes of the guest session (`POST /api/session/guest`, `GET /api/session` and `DELETE /api/session`), the routes of the admin account (`POST /api/admin/session`, `GET /api/admin/me`, `POST /api/admin/me/password`, `POST /api/admin/me/totp` and `POST /api/admin/me/recovery-codes`) and the routes of the catalog and the assets (`GET /api/categories`, `GET /api/catalog`, `GET /api/assets/{tag}`, and for an admin the categories, models, units, lifecycle steps and unit events under `/api/admin`). The routes of the tickets and the dashboard are not built yet.

1. Have a PostgreSQL 17 database and a role that owns it. The service applies its migrations at start.
2. Copy `.env.example` to `.env` and set `DATABASE_URL`, `PUBLIC_BASE_URL` and `TOTP_ENCRYPTION_KEY`. Make the key with `openssl rand -base64 32`. The service reads the process environment and not the file, so load the file with your shell or your process manager.
3. For a local run over plain http, also set `ENVIRONMENT=development` and `SECURE_COOKIE=false`. Production refuses both an http `PUBLIC_BASE_URL` and a `SECURE_COOKIE` of false.
4. Start the service with `go run ./cmd/api`.

The API listens on `HTTP_ADDR` and the health routes listen on `HEALTH_ADDR`. The health port is an internal port that the proxy does not route.

```sh
curl http://127.0.0.1:8080/api/config
curl http://127.0.0.1:8081/healthz
curl http://127.0.0.1:8081/readyz
```

### Create the first admin

There is no sign up, so the first admin comes from a command with the settings of the service in the environment. The initial password comes from the variable `ADMIN_INITIAL_PASSWORD` or from one line of the standard input, and never from a flag, because a flag shows in the process list. The command prints the email and the id and refuses an email that exists. Until the admin sets an own password, every sign in gets only a setup session of 15 minutes, which can only set that password and enroll TOTP.

```sh
read -rs ADMIN_INITIAL_PASSWORD && export ADMIN_INITIAL_PASSWORD
go run ./cmd/api create-admin --email admin@example.org --name "Example Admin"
```

### Reset the second factor of an admin

A lost phone or a changed `TOTP_ENCRYPTION_KEY` leaves an admin without a working second factor. The command `api reset-totp --email <email>` removes the TOTP secret, the recovery codes and every session of that admin. The password stays. The next sign in gets a setup session that enrolls a new factor. The command reads the settings like the other commands, and it does not use the TOTP key, so any valid value of the setting is enough after a lost key. A sign in or an enrollment that is in flight at the very moment of the reset can still finish, so run the command when nobody is signing in.

```sh
go run ./cmd/api reset-totp --email admin@example.org
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
| `ADMIN_SIGNIN_PAIR_FAILURES` | `5` | The most failed admin sign ins in 15 minutes for one email from one client address, from 1 to 1000. The next request of that pair answers `429` until the window ends. A success does not reset the count, and a failure never locks the account |
| `ADMIN_SIGNIN_ADDRESS_FAILURES` | `20` | The most failed admin sign ins in an hour for one client address, whatever the email, from 1 to 1000 |
| `ADMIN_ACCOUNT_FAILURES` | `5` | The most failed password or code checks in 15 minutes on the account routes of one admin session, from 1 to 1000 |
| `PASSWORD_HASH_CONCURRENCY` | `2` | The most Argon2id hashes that run at once in one copy, from 1 to 16. One hash needs about 19 MiB of memory, and a request that waits more than 3 seconds for a slot answers `503` |
| `TOTP_ENCRYPTION_KEY` | none, required | The key that seals the TOTP secret of every admin, 32 random bytes in Base64. It is never logged. A lost key makes every stored TOTP secret unreadable, so keep a copy apart from the database. The all zero placeholder of `.env.example` is refused |
| `GUEST_SIGNIN_PER_HOUR` | `60` | The most guest sign in requests that one client address may send in an hour, from 1 to 100000. Every request counts, and an IPv6 address counts as its `/64` prefix |

## Documentation

- [`docs/ci-cd.md`](docs/ci-cd.md) lists the workflows, the required checks and the release flow.
- [`docs/deployment.md`](docs/deployment.md) describes the images, the files of `deploy/`, the settings of a first start and the rolling update of the copies.
- [`openapi.yaml`](openapi.yaml) is the full contract of the API, with every route, body, answer and error code.
- [`docs/architecture.md`](docs/architecture.md), [`docs/data-model.md`](docs/data-model.md), [`docs/api.md`](docs/api.md) and [`docs/errors.md`](docs/errors.md) describe the planned design, the stock rules, the contract rules and the error codes.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`SECURITY.md`](SECURITY.md) hold the rules for contributions and for reporting a vulnerability.

## Project layout

```text
cmd/api/          The entry point of the service, with the commands create-admin, reset-totp and healthcheck
internal/         The settings, the HTTP servers, the sessions, the admin accounts, the catalog of assets, the pagination cursor, the idempotency keys, the password, TOTP and secret encryption code, the rate limits, the janitor, the database pool, the migrations and the test helpers
docs/             Design pages, CI/CD documentation and the deployment guide
deploy/           The production example of the compose file, its settings template and the rolling update script
Dockerfile        The image of the service
.github/          Workflows, the lint tool images, issue forms and the pull request template
```

## Contributing and security

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before you open a pull request. Report a security issue privately as [`SECURITY.md`](SECURITY.md) says, and do not open a public issue. The rules of behavior are in [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

GNU General Public License v3.0. See [`LICENSE`](LICENSE).
