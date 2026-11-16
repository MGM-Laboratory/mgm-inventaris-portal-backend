# API

Status of this page. It describes the planned contract. The hand written `openapi.yaml` in the root holds the full contract and is kept in step with the routes. This page explains the rules that every route shares.

## Basics

- The API lives under `/api` on the same hostname as the frontend. There is no CORS, and no other origin may call it with a session.
- Bodies and answers are JSON in UTF-8. A time is RFC 3339 in UTC. A pickup date is `YYYY-MM-DD` in Asia/Jakarta.
- Names in JSON are camel case. Ids of tickets are the ticket number. Ids of units are the tag. Other ids are UUIDs.
- Every answer carries `Cache-Control: no-store` and an `X-Request-Id` header. A client may send its own `X-Request-Id`.
- A list answers `{ "items": [...], "nextCursor": "..." }`. The query takes `limit` (default 50, most 100) and `cursor`. The field `nextCursor` is absent on the last page.

## Sessions and CSRF

| Item | Value |
| --- | --- |
| Session cookie | `__Host-mgm_session`, `HttpOnly`, `Secure`, `SameSite=Lax`, path `/` |
| Guest session | 12 hours idle, 7 days at most |
| Admin session | 30 minutes idle, 8 hours at most |
| CSRF header | `X-CSRF-Token` on every `POST`, `PATCH` and `DELETE` |

- `POST /api/session/guest`, `POST /api/admin/session` and `POST /api/tickets/lookup` answer with a `csrfToken` in the body. `GET /api/session` returns it again.
- These three routes can be called without a session, so they carry no CSRF token. They rely on the `Origin` check below and on their rate limits.
- A write whose `Origin` header names another host answers `403` with the code `origin_not_allowed`. A write with no `Origin` header is accepted only when `Sec-Fetch-Site` is `same-origin`.
- Any other write without the right CSRF header answers `403` with the code `csrf_failed`.
- A sign in makes a new session and ends the one that came with the request.
- An expired or missing session answers `401` with the code `session_expired` or `unauthenticated`. The frontend then shows the sign in page.
- A route that needs a role the session does not have answers `403` with the code `forbidden`.

## Client address

The proxy overwrites the header `X-Real-IP` with the address that it saw. The service reads that header only when the request comes from the proxy address in the settings. Otherwise it uses the address of the peer. An IPv6 address counts as its `/64` prefix. The health routes are served on an internal port and are not routed by the proxy.

## Errors

Every error has the same body.

```json
{
  "error": {
    "code": "stock_changed",
    "message": "The stock of one item changed. Reload and try again.",
    "details": [{ "field": "items[0].qty", "code": "too_many", "message": "At most 2 are available." }],
    "requestId": "c0a8..."
  }
}
```

| Status | When | Codes |
| --- | --- | --- |
| 400 | The body cannot be read | `bad_request` |
| 401 | No session or an expired one | `unauthenticated`, `session_expired`, `invalid_credentials` |
| 403 | The role, the identity or the CSRF check fails | `forbidden`, `identity_required`, `csrf_failed`, `origin_not_allowed` |
| 404 | The thing does not exist or may not be seen | `not_found`, `ticket_not_found` |
| 409 | The state changed under the request | `stock_changed`, `version_conflict`, `status_conflict`, `unit_held`, `unit_unavailable`, `duplicate`, `request_in_progress` |
| 422 | A field fails validation | `validation_failed` with `details` |
| 429 | A rate limit is over, also after too many failed admin sign ins | `rate_limited`, with the header `Retry-After` in seconds |
| 500 | A bug or an outage | `internal` |
| 503 | The database is not reachable | `unavailable` |

`docs/errors.md` lists each code with its cause and what a client does.

## Limits of the lab

These are settings. The lab decides the final values, and the values here are the planned defaults.

| Limit | Default |
| --- | --- |
| Items in one request (all quantities added) | 10 |
| Days of a loan | 1 to 30 |
| Open tickets for one email or one NIM or NIP | 3 |
| Guest sign in for one IP address | 60 each hour |
| New tickets for one IP address | 20 each hour |
| Failed ticket lookups for one IP address, and for one ticket number without its last part together with one email | 20 and 10 each hour |
| Failed admin sign ins for one email from one IP address | 5 in 15 minutes, then 15 minutes of `429` for that pair |
| Failed admin sign ins for one IP address | 20 each hour |
| Failed password or code checks on the account routes of one admin session | 5 in 15 minutes, then 15 minutes of `429` for that session |

The guest sign in form and the ticket form have a hidden field named `website`. A client leaves it empty. A request that fills it gets a normal looking answer and creates nothing.

The counters cover only the sign in routes, the ticket creation, the ticket lookup and the failed password or code checks on the account routes of an admin. A campus network can share one address, so the admin limit for an address counts only failures. The key of a lookup limit is the part `yymm-nnnn` of the number together with a hash of the submitted email, because the last part of the number is the secret and a stranger must not be able to block the lookup of a real borrower.

## Roles

| Role | How | Can |
| --- | --- | --- |
| Anyone | No session | Read the public settings, the public page of an asset, and look up a ticket with its number and email. A lookup starts a guest session without an identity |
| Guest | A guest session | Read the catalog, create tickets (needs the identity), read and cancel the tickets of the session |
| Admin | An admin session | The routes that read, and all admin routes. An admin session cannot create or cancel a ticket as a guest |

An identified guest session needs the name, the NIM or NIP and an email whose domain is `ub.ac.id` or ends in `.ub.ac.id`, such as `student.ub.ac.id`. The service compares the email in lower case.

## Main flows

1. **Request.** A guest signs in, reads the catalog, and posts a ticket. The service checks the limits, creates the number and the rows, and answers `201` with the ticket. The ticket starts as `diajukan`.
2. **Sign in of an admin.** The body holds the email, the password and either a TOTP code or a recovery code. The service checks all of them and answers `invalid_credentials` for any wrong value, so the answer does not say which one is wrong. An admin who has no TOTP yet, or whose initial password has expired, gets a setup session of 15 minutes. It can call only the setup route and the session routes.
3. **Decision.** An admin posts a decision with the version it read. An approval carries the approved quantity of each model and may change the days. At least one approved quantity must be above 0, and the service answers `422` otherwise, because a request that should hold nothing is a reject. The service locks the models, checks the available quantity, and answers `409` with `stock_changed` when it is too low.
4. **Handover.** The admin posts the tags of the units that it took from the shelf, one for each approved item. The ticket becomes `dipinjam`.
5. **Return.** The admin posts the tag of every unit of the ticket with an outcome, which is `ok`, `maintenance` or `lost`. The return is all or nothing, and an outcome of `lost` needs a note. The ticket becomes `dikembalikan`, and a unit with another outcome changes its lifecycle.
6. **Cancel.** A guest may cancel a ticket in the status `diajukan`. An admin may reject a ticket in the status `diajukan` with a note, and may cancel a ticket in the status `disetujui` or `siap_diambil` with a note.

## Routes

The table is a map. The exact bodies are in `openapi.yaml`.

| Route | Role | Purpose |
| --- | --- | --- |
| `GET /api/config` | Anyone | Public settings, the limits and the public address that the QR code uses |
| `POST /api/session/guest` | Anyone | Start a guest session |
| `GET /api/session` | Any session | Read the session and the CSRF token |
| `DELETE /api/session` | Any session | End the session |
| `POST /api/admin/session` | Anyone | Admin sign in with a password and a TOTP code |
| `GET /api/categories` | Guest | List the categories |
| `GET /api/catalog` | Guest | List the models with total, held and available quantity |
| `GET /api/assets/{tag}` | Anyone | Public page of one unit, with details only for an admin |
| `POST /api/tickets` | Guest | Create a ticket |
| `GET /api/tickets` | Guest | List the tickets of the session |
| `POST /api/tickets/lookup` | Anyone | Find a ticket by its number and email |
| `GET /api/tickets/{number}` | Guest | Read one ticket of the session |
| `POST /api/tickets/{number}/cancel` | Guest | Cancel a ticket |
| `GET /api/admin/tickets` | Admin | The queue, filtered by status |
| `GET /api/admin/tickets/{number}` | Admin | Read any ticket |
| `POST /api/admin/tickets/{number}/decision` | Admin | Approve or reject |
| `POST /api/admin/tickets/{number}/ready` | Admin | Mark ready for pickup |
| `POST /api/admin/tickets/{number}/handover` | Admin | Hand over the units |
| `POST /api/admin/tickets/{number}/return` | Admin | Take the units back |
| `POST /api/admin/tickets/{number}/cancel` | Admin | Cancel an approved ticket with a note |
| `GET /api/admin/dashboard` | Admin | Counts for the dashboard |
| `GET/POST /api/admin/categories` | Admin | List and add categories |
| `GET/POST /api/admin/models`, `PATCH/DELETE /api/admin/models/{id}` | Admin | Manage models |
| `POST /api/admin/models/{id}/units` | Admin | Add one to twenty units |
| `GET /api/admin/units`, `GET/PATCH/DELETE /api/admin/units/{tag}` | Admin | Manage units |
| `POST /api/admin/units/{tag}/lifecycle` | Admin | Change the lifecycle with a note |
| `GET /api/admin/units/{tag}/events` | Admin | History of a unit |
| `GET /api/admin/unit-events` | Admin | History of all units, newest first |
| `GET /api/admin/me`, `POST /api/admin/me/password`, `POST /api/admin/me/totp`, `POST /api/admin/me/recovery-codes` | Admin | Own account, setup of the password and TOTP, and new recovery codes |
| `GET /healthz`, `GET /readyz` | The proxy | Health checks on the internal port, not under `/api` |

## Status of a unit

A unit shows one status. The status is `retired`, `lost` or `maintenance` for the lifecycle of the same name. An active unit is `borrowed` while it is in an open row of a ticket, and `available` otherwise. A unit has no reserved status, because units are chosen at handover and not when a ticket is approved.
