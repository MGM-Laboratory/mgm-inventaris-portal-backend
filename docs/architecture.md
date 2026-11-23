# Architecture

Status of this page. It describes the design of the service. The code so far holds the settings, the database pool with the migrations, the error body, the request id, the guest session (cookie, Origin and CSRF checks, role guards, rate limit and janitor), the admin accounts (Argon2id passwords, TOTP, recovery codes and the command that creates an admin), the routes `GET /api/config`, `POST /api/session/guest`, `GET /api/session`, `DELETE /api/session`, `POST /api/admin/session`, `GET /api/admin/me`, `POST /api/admin/me/password`, `POST /api/admin/me/totp` and `POST /api/admin/me/recovery-codes`, the catalog and asset routes (`GET /api/categories`, `GET /api/catalog`, `GET /api/assets/{tag}` and the admin routes for categories, models, units, lifecycle steps and unit events), the routes of a borrower for the tickets (`POST /api/tickets`, `GET /api/tickets`, `POST /api/tickets/lookup`, `GET /api/tickets/{number}` and `POST /api/tickets/{number}/cancel`), the routes of an admin for the tickets and the dashboard (`GET /api/admin/tickets`, `GET /api/admin/tickets/{number}`, the steps `decision`, `ready`, `handover`, `return` and `cancel`, `GET /api/admin/ticket-events` and `GET /api/admin/dashboard`), and the health routes. Every other statement here is a plan until the code that does it is merged.

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

The folders below exist today.

```
cmd/api                       the entry point and the commands create-admin, reset-totp and healthcheck
internal/config               settings from the environment
internal/admin                admin accounts, sign in checks, TOTP enrollment, recovery codes
internal/admin/admintest      an in-memory admin repository for tests
internal/catalog              categories, models, units, lifecycle steps, unit events and availability
internal/ticket               tickets, numbers, the table of statuses, creation, reads, the cancel of a guest and the steps of an admin
internal/dashboard            the counts of the dashboard, which only reads
internal/idempotency          Idempotency-Key claims and stored answers
internal/pagination           the signed cursor and the limit of a list
internal/password             Argon2id hashing with a cap on parallel hashes, and the rules for a new password
internal/totp                 TOTP codes of RFC 6238 and the otpauth address
internal/secretbox            AES-256-GCM sealing of the TOTP secret and derived secrets
internal/errchain             describes an error for a log line without request values
internal/server               API app, health app, middleware, role guards, session routes, error mapping
internal/session              sessions table, tokens, cookie
internal/ratelimit            fixed window counters in the database
internal/janitor              scheduled sweep of expired sessions, old counters and expired idempotency keys
internal/database             pool and migrations
internal/database/migrations  the embedded SQL files of golang-migrate
internal/database/dbtest      a database for each test, skipped without DATABASE_URL
```

One part is planned and has no code yet. The package `internal/mail` will hold a sending provider.

The ticket package defines the interface `Notifier`, which hears about an approval, a rejection, a ready notice and an admin cancel after the change is committed. `cmd/api` wires an implementation that only writes the kind of notice and the ticket number without its last four characters to the log. It never writes the address, the name or a note. A mail that fails never undoes a step, and a late notice needs a scheduled job that does not exist yet.

A package owns its tables and exposes plain functions. A handler never builds SQL. Other packages call the owner of a table and never write its tables. Four reads cross that line on purpose. The catalog reads the tickets to compute the held quantity of a model. The ticket package joins the model name to the items of a ticket and the tag of a unit to the units of a ticket. The dashboard package reads the tickets, the units and the categories to count them. A handover and a return write the events and the lifecycle of the units through functions of the catalog package, in the transaction of the step, so the ticket package never writes the tables of the assets.

## Security model

- A session is a random 256 bit value in an `HttpOnly`, `Secure`, `SameSite=Lax` cookie. The database stores only its SHA-256 hash.
- The `Secure` flag of the session cookie comes from the setting `SECURE_COOKIE`, which is true by default, and production refuses `false` at start. A development setup over plain http may turn it off. The flag never comes from the request scheme or from `X-Forwarded-Proto`, so a client cannot weaken it. The cookie name has the prefix `__Host-` only while the flag is on, because a browser rejects that prefix without `Secure`.
- A session has a kind, either guest or admin. A guest session holds the name, the NIM or NIP and the email that the guest typed. It is not an account.
- An admin signs in with a local account, a password and a TOTP code or a recovery code. Passwords are hashed with Argon2id, and the memory cost, the cap on parallel hashes and the sign in limits keep two copies of the service within the memory of the host. A request that cannot get a hash slot within 3 seconds answers `503` and counts as no failed sign in.
- The sign in answers `invalid_credentials` for every wrong value. An email that is not an account is checked against a dummy hash, so the time does not show which emails are accounts. The counters of the sign in limits hold failures only. A request hits its counters first and gives the hits back when it does not fail, so parallel guesses cannot all pass before the first failure is counted.
- The TOTP secret is sealed with AES-256-GCM under the key of the setting `TOTP_ENCRYPTION_KEY`, and the admin id is part of what the seal covers. A sealed value starts with the format prefix `v1.`, so a later format can live next to it. The enrollment stores nothing until its second call, because the secret of the first call is derived from the key, the admin, the session and the time of the last enrollment. A recovery code is stored as a SHA-256 hash and works once.
- Every rule of access is checked in the service. The frontend only hides screens.
- A serial number, a place and a condition of a unit are shown only to admins.
- A secret never goes in the repository, in a log or in an error answer.

The planned limits of the lab are in [`api.md`](api.md). They are settings, and the lab decides the final values.

## Concurrency

Stock rules need locks, because two admins can work on the same model at the same time. A transaction that reads availability and then writes takes a row lock on every model it touches, in the order of the model id. A step of an admin that touches stock reads the head of the ticket first and refuses a wrong status or version without a lock. It then locks the models, reads the head again, checks the stock, and makes the guarded update of the status. A handover locks the named units after the models. The details are in [`data-model.md`](data-model.md).

## Deployment

The service runs as two copies behind the reverse proxy on one host, with the PostgreSQL database in a separate container. The health routes listen on an internal port that the proxy does not route. The image has no shell, so its health check is the command `api healthcheck`, which asks `GET /healthz` on that port from inside the container. A rolling update replaces one copy at a time and waits for its health check. See [`deployment.md`](deployment.md).
