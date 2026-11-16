# Architecture

Status of this page. It describes the planned design of the service. The service code is not written yet, so every statement here is a plan until the code that does it is merged.

## What the service does

The backend of the LabMGM inventory portal keeps the loan records of the lab. A borrower asks for items, an admin approves the request, hands the items over and takes them back. The service stores the assets (categories, models and units), the loan tickets, the sessions and the admin accounts in one PostgreSQL database.

## Components

| Part | Choice | Reason |
| --- | --- | --- |
| Language | Go | One static binary and a small image |
| HTTP | Fiber v3 | A Go web framework with a small middleware model |
| Database | PostgreSQL through pgx v5 | Transactions, row locks and partial unique indexes carry the stock rules |
| Migrations | golang-migrate with SQL files embedded in the binary | The binary applies them at start under an advisory lock |
| Settings | Environment variables read at start | See the Configuration section of the README |
| Logs and metrics | JSON logs with a request id, Prometheus metrics, error reports | One request id ties a log line, a metric and an error report together |

## Request flow

1. The reverse proxy sends `/api` to the service and every other path to the frontend. The browser sees one hostname.
2. A middleware assigns a request id, reads the session cookie and loads the session.
3. A write request must also carry the CSRF header, except the three routes that start a session or look a ticket up, and its `Origin` header must match the public hostname.
4. The sign in routes, the ticket creation, the ticket lookup and the account routes of an admin count in the database and answer `429` when a limit is over. Ticket creation and the guest sign in count each request, and the admin sign in, the lookup and the account routes count failures. Other routes are not counted.
5. The handler validates the body, runs one database transaction and answers with JSON.

## Package layout

```
cmd/api            the entry point, also the admin commands (create an admin)
internal/config    settings from the environment
internal/server    router, middleware, error mapping
internal/session   sessions, cookies, CSRF
internal/auth      admin accounts, Argon2id, TOTP, recovery codes
internal/catalog   categories, models, units, availability
internal/ticket    tickets, numbers, status changes, handover, return
internal/ratelimit counters in the database
internal/janitor   scheduled jobs (expired sessions, counters, anonymization)
internal/database  pool, migrations, test helpers
internal/mail      sending interface, no provider yet
```

A package owns its tables and exposes plain functions. A handler never builds SQL. Other packages call the owner of a table and never read its tables.

## Security model

- A session is a random 256 bit value in an `HttpOnly`, `Secure`, `SameSite=Lax` cookie. The database stores only its SHA-256 hash.
- A session has a kind, either guest or admin. A guest session holds the name, the NIM or NIP and the email that the guest typed. It is not an account.
- An admin signs in with a local account, a password and a TOTP code or a recovery code. Passwords are hashed with Argon2id, and the memory cost and the sign in limits keep two copies of the service within the memory of the host.
- Every rule of access is checked in the service. The frontend only hides screens.
- A serial number, a place and a condition of a unit are shown only to admins.
- A secret never goes in the repository, in a log or in an error answer.

The planned limits of the lab are in [`api.md`](api.md). They are settings, and the lab decides the final values.

## Concurrency

Stock rules need locks, because two admins can work on the same model at the same time. A transaction that reads availability and then writes takes a row lock on every model it touches, in the order of the model id. The details are in [`data-model.md`](data-model.md).

## Deployment

The service runs as two copies behind the reverse proxy on one host, with the PostgreSQL database in a separate container. The health routes listen on an internal port that the proxy does not route. A rolling update replaces one copy at a time and waits for its health check.
