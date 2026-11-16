# Data model

Status of this page. It describes the planned schema. The migrations are not written yet, so the code is the source of truth once it exists, and this page is updated in the same pull request as any migration.

All tables live in one PostgreSQL database. Ids are UUIDs made by the service. Timestamps are `timestamptz` and are stored in UTC. A pickup date is a plain `date` and is read as a date in the time zone Asia/Jakarta.

## Assets

| Table | Purpose | Main columns |
| --- | --- | --- |
| `categories` | Groups of models | `id`, `name` (unique), `tone` |
| `models` | A kind of item, such as a headset | `id`, `name` (unique), `category_id`, `description`, `prefix` (unique), `next_unit_number`, `retired_at` |
| `units` | One physical item with a QR label | `id`, `tag` (unique), `model_id`, `serial` (unique, may be null), `condition`, `place`, `lifecycle`, `lifecycle_note` |
| `unit_events` | History of a unit | `id`, `unit_id`, `at`, `action`, `admin_id`, `note` |

- `tag` has the form `MGM-<prefix>-<nnn>`. The QR label holds the tag, so a tag never changes and is never reused.
- `next_unit_number` is increased inside the same transaction that inserts the units. The model row is locked first, so two admins never get the same number.
- `lifecycle` is one of `active`, `maintenance`, `lost` and `retired`. Only `active` units can be loaned.
- A model or a unit is retired and never deleted. A permanent delete is allowed only for a model or a unit that no ticket has ever used.
- A model can be retired only when no ticket in the status `diajukan`, `disetujui`, `siap_diambil` or `dipinjam` asks for it. Otherwise the service answers `409`.
- An empty serial number is stored as null, so units without a serial never clash.
- A lifecycle change to `lost` or `retired` needs a note.

## Tickets

| Table | Purpose | Main columns |
| --- | --- | --- |
| `tickets` | One loan request | `id`, `number` (unique), `status`, `requester_kind`, `requester_name`, `requester_nim_nip`, `requester_email`, `purpose`, `pickup_date`, `duration_days`, `approved_duration_days`, `decision_note`, `version`, `created_at`, `anonymized_at` |
| `ticket_items` | The models that a ticket asks for | `ticket_id`, `model_id`, `qty`, `approved_qty` (at most `qty`), unique on `ticket_id` and `model_id` |
| `ticket_units` | The units that a ticket really holds | `ticket_id`, `unit_id`, `handed_over_at`, `returned_at`, `return_outcome`, `return_note` |
| `ticket_history` | One row for each status change | `id`, `ticket_id`, `at`, `status`, `actor_kind`, `admin_id`, `note` |
| `ticket_counters` | The running number of each month | `period` (for example `2611`), `last` |

- The number has the form `MGM-yymm-nnnn-XXXX`. The part `yymm` is the month in Asia/Jakarta. The part `nnnn` comes from `ticket_counters` in the transaction that inserts the ticket. The suffix `XXXX` has four characters from an alphabet of 32 characters without `I`, `O`, `0` and `1`, drawn from `crypto/rand`.
- The status is one of `diajukan`, `disetujui`, `siap_diambil`, `dipinjam`, `dikembalikan`, `dibatalkan` and `ditolak`. The status `terlambat` is not stored. It is derived when a ticket is read, from the status `dipinjam` and a due date in the past, and the API shows it as `effectiveStatus`. The due date is the pickup date plus the approved days, or the requested days when none were approved.
- `version` starts at 1 and grows with each change. An admin request carries the version it read, and a stale version is refused.
- A partial unique index on `ticket_units (unit_id) where returned_at is null` makes it impossible that two open tickets hold the same unit.
- A ticket asks for models. The units are chosen at handover, when the admin scans the QR label of each unit.
- An open ticket, for the limit of open tickets, is a ticket in the status `diajukan`, `disetujui`, `siap_diambil` or `dipinjam`.
- Indexes cover the lookups that the routes make. They are `tickets (number)`, `tickets (status, created_at)`, `tickets (lower(requester_email))`, `tickets (requester_nim_nip)`, `ticket_units (ticket_id)` and `units (model_id)`.

## Stock rules

- The stock of a model is the count of its units with the lifecycle `active`.
- A model is held by every ticket in the status `disetujui`, `siap_diambil` or `dipinjam`. A ticket in the status `diajukan` holds nothing. A late ticket is a ticket in the status `dipinjam`, so it still holds.
- Held quantity is the sum of `approved_qty` of those tickets for the model. Available quantity is the stock minus the held quantity, and the API shows a negative number as 0.
- An approval is refused when `approved_qty` is more than the available quantity at that moment. An approval also needs at least one `approved_qty` above 0, because a hold of nothing could only be undone by a cancel.
- A handover needs exactly `approved_qty` units for each model. Each unit must be active, of the right model and not in an open row of `ticket_units`.
- A unit cannot leave the status `active` while it is in an open row of `ticket_units`, and the service answers `409` with the code `unit_held`. It also cannot leave the status `active` by an admin change when the stock would fall below the held quantity of its model, and the service answers `409` with the code `stock_changed`.
- A return is all or nothing and runs in one transaction. It closes the ticket, then closes the rows of `ticket_units`, then applies the lifecycle of each unit. A unit that comes back as `lost` or `maintenance` lowers the stock after the ticket no longer holds, so the stock never drops below a hold that the same ticket made.
- A lost unit can leave another approved ticket short. That ticket keeps its status, and its handover answers `409` with the code `stock_changed` until the admin fixes the stock or cancels it.
- An admin may cancel a ticket in the status `disetujui` or `siap_diambil` with a note, which releases its hold. A ticket that nobody collects is closed this way.

## Concurrency

- Every transaction that changes stock locks the model rows with `select ... for update`, in the order of the model id, before it reads. That covers an approval, a handover, a return, a lifecycle change, adding units and retiring a model. A handover and a lifecycle change on the same unit therefore never run together.
- A request that creates a ticket takes a transaction advisory lock for the normalized email and one for the normalized NIM or NIP, in a fixed order, before it counts the open tickets. Two parallel requests cannot pass the limit together. The email is compared in lower case.
- A scheduled job runs under an advisory lock, so only one of the two copies runs it.
- Creating a ticket takes the advisory locks first and then the requested model rows with `select ... for share`, in the order of the model id. It refuses a retired model with `validation_failed`. A permanent delete of a model or a unit locks it first and answers `409` with the code `status_conflict` when a ticket used it, and a database error never reaches the client as `500`.
- The `model_id` of a unit never changes, because a change would break the lock order.
- A status change of a ticket is a guarded update. It names the expected old status and the expected version, and it changes no row when either differs. The service then answers `409`.
- A request that creates a ticket may carry an `Idempotency-Key`. The service stores the key for the session with the answer for 24 hours and returns the same answer for a repeat, whatever the new body says. The row has a state, `running` or `done`, and a repeat that arrives while the first request runs answers `409` with the code `request_in_progress`.
- Database specs in CI prove these rules on a real PostgreSQL, because a mock cannot prove locks.

## People and sessions

| Table | Purpose | Main columns |
| --- | --- | --- |
| `admins` | Local admin accounts | `id`, `email` (unique), `name`, `password_hash`, `password_expires_at`, `totp_secret`, `totp_enabled_at`, `totp_last_step` |
| `admin_recovery_codes` | One use codes for TOTP | `admin_id`, `code_hash`, `used_at` |
| `sessions` | Guest and admin sessions, `kind` is `guest`, `admin` or `admin_setup` | `token_hash` (primary key), `kind`, `admin_id`, `guest_name`, `guest_nim_nip`, `guest_email`, `csrf_token`, `viewable_tickets`, `created_at`, `last_seen_at`, `expires_at` |
| `rate_counters` | Fixed window counters | `key`, `window_start`, `count` |
| `idempotency_keys` | Safe repeat of a ticket request | `session_hash`, `key` (together the primary key), `state`, `status_code`, `response`, `created_at` |
| `audit_log` | Admin actions | `id`, `at`, `admin_id`, `action`, `target`, `request_id` |

- The TOTP secret is encrypted with a key from the settings before it is stored. `totp_last_step` holds the last accepted time step, so a code cannot be used twice.
- An admin is created by a command of the service with a one use initial password that expires after 24 hours (`password_expires_at`). An admin without `totp_enabled_at`, or with an expired initial password, gets only an `admin_setup` session. It lasts 15 minutes and can call only the setup route, `GET /api/session` and `DELETE /api/session`. The setup route sets a new password, enrolls TOTP and returns the first recovery codes, and it needs the current password.
- Changing the password, enrolling TOTP again and making new recovery codes need the current password and a valid TOTP code. The use of a recovery code ends the other sessions of that admin.
- Passwords are hashed with Argon2id, with 19 MiB of memory, 2 passes and 1 lane as the default. A setting caps the number of hashes that run at once, and the default is 2.
- A sign in makes a new session id and deletes the session that came with the request. A password change deletes the other sessions of that admin.
- A session has an idle limit and an absolute limit, and the service deletes expired rows. The service writes `last_seen_at` at most once a minute.
- Failed password or code checks on the account routes of an admin are counted in `rate_counters` for the session, so a stolen session cannot guess the password or a code without a limit.
- Failed sign ins are counted in `rate_counters` for each submitted email, in lower case, and address together. An email that is not an account counts the same way and costs the same hash work, so the answer never shows which emails are accounts. A stranger cannot lock the account of an admin, because the counter holds the address. A high daily count for one account raises an alert and blocks nothing.
- `viewable_tickets` lists the tickets that this session may read because it created them or looked them up with the number and the email. It holds at most 20 ids.
- A guest session may have no name, no NIM or NIP and no email. A successful ticket lookup without a session makes such a session. A lookup with a guest session adds the ticket to that session and changes nothing else. A lookup with an admin session changes no session. A failed lookup makes no session. A session without an identity can read the tickets it found and cannot create a ticket until the guest form is sent.

## Retention

- Ticket data is kept for 2 years after the last activity of a closed ticket. A ticket is closed when its status is `dikembalikan`, `dibatalkan` or `ditolak`.
- After that, a scheduled job sets the name, the NIM or NIP and the email to null, clears `purpose`, `decision_note`, the notes of the history and the notes of the returned units, sets `anonymized_at`, and keeps the items and the dates.
- The job runs daily and writes one audit row with the count of tickets that it changed. The job is safe to run again, so an operator runs it by hand after a restore of the database.
- Sessions, counters and idempotency keys are deleted when they expire.
