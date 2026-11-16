# Errors

Status of this page. It describes the planned error codes. The code of the service is the source of truth once it exists.

Every error answer has the body described in [`api.md`](api.md). The field `code` is stable and a client branches on it. The field `message` is for people and may change.

| Code | Status | Cause | What a client does |
| --- | --- | --- | --- |
| `bad_request` | 400 | The body is not valid JSON or a size limit is over | Fix the request |
| `unauthenticated` | 401 | The route needs a session and the request has none | Show the sign in page |
| `session_expired` | 401 | The session reached its idle or absolute limit | Show the sign in page |
| `invalid_credentials` | 401 | The email, the password or the code is wrong | Show one message for all of them, so the answer does not reveal which one is wrong |
| `forbidden` | 403 | The session has not the role that the route needs | Show a message |
| `identity_required` | 403 | The route needs a guest session with a name, a NIM or NIP and an email, and the session has none | Show the guest form |
| `csrf_failed` | 403 | The CSRF header is missing or wrong | Read the token with `GET /api/session` and try again once |
| `origin_not_allowed` | 403 | The `Origin` header of a write names another host, or it is missing and the request is not same origin | Do not retry |
| `not_found` | 404 | The thing does not exist, or the session may not see it | Show not found |
| `ticket_not_found` | 404 | The number and the email do not match a ticket | Show one message, so the answer does not reveal whether the number exists |
| `stock_changed` | 409 | An approval or a handover asks for more than is available now, or a lifecycle change would leave less stock than is held | Reload the ticket and the stock, then decide again |
| `version_conflict` | 409 | The version in the request is older than the stored one | Reload and apply the change again |
| `status_conflict` | 409 | The ticket is not in a status that allows the change, or a permanent delete finds that a ticket used the thing | Reload the ticket |
| `unit_held` | 409 | A unit is in an open ticket and cannot change its lifecycle | Finish the ticket first |
| `unit_unavailable` | 409 | A unit at handover is not active, not of the right model or already held | Choose another unit |
| `duplicate` | 409 | A name, a prefix or a serial number is already used | Choose another value |
| `request_in_progress` | 409 | A repeat of a ticket request arrives while the first one still runs | Wait a moment and read the ticket list |
| `validation_failed` | 422 | A field fails a rule, with one entry in `details` for each field | Show each message at its field |
| `rate_limited` | 429 | A rate limit is over, also after too many failed admin sign ins | Wait for the number of seconds in `Retry-After` |
| `internal` | 500 | A bug or an unexpected failure | Show a general message and give the request id to a maintainer |
| `unavailable` | 503 | The database is not reachable | Try again later |

A `details` entry has `field`, `code` and `message`. The codes of a field are short words such as `required`, `too_long`, `too_many`, `invalid_format`, `not_ub_email` and `out_of_range`.
