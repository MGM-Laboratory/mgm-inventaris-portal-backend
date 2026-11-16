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

## Tickets

| Table | Purpose | Main columns |
| --- | --- | --- |
| `tickets` | One loan request | `id`, `number` (unique), `status`, `requester_kind`, `requester_name`, `requester_nim_nip`, `requester_email`, `purpose`, `pickup_date`, `duration_days`, `approved_duration_days`, `decision_note`, `version`, `created_at`, `anonymized_at` |
| `ticket_items` | The models that a ticket asks for | `ticket_id`, `model_id`, `qty`, `approved_qty` |
| `ticket_units` | The units that a ticket really holds | `ticket_id`, `unit_id`, `handed_over_at`, `returned_at`, `return_outcome`, `return_note` |
| `ticket_history` | One row for each status change | `id`, `ticket_id`, `at`, `status`, `actor_kind`, `admin_id`, `note` |
| `ticket_counters` | The running number of each month | `period` (for example `2611`), `last` |

- The number has the form `MGM-yymm-nnnn-XXXX`. The part `yymm` is the month in Asia/Jakarta. The part `nnnn` comes from `ticket_counters` in the transaction that inserts the ticket. The suffix `XXXX` has four characters from an alphabet of 32 characters without `I`, `O`, `0` and `1`, drawn from `crypto/rand`.
- The status is one of `menunggu_verifikasi`, `diajukan`, `disetujui`, `siap_diambil`, `dipinjam`, `dikembalikan`, `dibatalkan` and `ditolak`. The status `terlambat` is not stored. It is derived when a ticket is read, from the status `dipinjam` and a due date in the past. The due date is the pickup date plus the approved days, or the requested days when none were approved.
- The status `menunggu_verifikasi` exists only while email verification is switched on.
- `version` starts at 1 and grows with each change. An admin request carries the version it read, and a stale version is refused.
- A partial unique index on `ticket_units (unit_id) where returned_at is null` makes it impossible that two open tickets hold the same unit.
- A ticket asks for models. The units are chosen at handover, when the admin scans the QR label of each unit.

## Stock rules

- The stock of a model is the count of its units with the lifecycle `active`.
- A model is held by every ticket in the status `disetujui`, `siap_diambil`, `dipinjam` or `terlambat`. A ticket in the status `diajukan` holds nothing.
- Held quantity is the sum of `approved_qty` of those tickets for the model. Available quantity is the stock minus the held quantity.
- An approval is refused when `approved_qty` is more than the available quantity at that moment.
- A handover needs exactly `approved_qty` units for each model. Each unit must be active, of the right model and not in an open row of `ticket_units`.
- A unit cannot leave the status `active` while it is in an open row of `ticket_units`. It also cannot leave the status `active` when the stock would fall below the held quantity of its model.

## Concurrency

- A transaction that reads availability and then writes locks the model rows with `select ... for update`, in the order of the model id, before it reads.
- A status change of a ticket is a guarded update. It names the expected old status and the expected version, and it changes no row when either differs. The service then answers `409`.
- A request that creates a ticket may carry an `Idempotency-Key`. The service stores the key with the answer for 24 hours and returns the same answer for a repeat.
- Database specs in CI prove these rules on a real PostgreSQL, because a mock cannot prove locks.

## People and sessions

| Table | Purpose | Main columns |
| --- | --- | --- |
| `admins` | Local admin accounts | `id`, `email` (unique), `name`, `password_hash`, `totp_secret`, `totp_enabled_at`, `failed_attempts`, `locked_until` |
| `admin_recovery_codes` | One use codes for TOTP | `admin_id`, `code_hash`, `used_at` |
| `sessions` | Guest and admin sessions | `token_hash` (primary key), `kind`, `admin_id`, `guest_name`, `guest_nim_nip`, `guest_email`, `csrf_token`, `viewable_tickets`, `created_at`, `last_seen_at`, `expires_at` |
| `rate_counters` | Fixed window counters | `key`, `window_start`, `count` |
| `idempotency_keys` | Safe repeat of a ticket request | `key`, `session_hash`, `status_code`, `response`, `created_at` |
| `audit_log` | Admin actions | `id`, `at`, `admin_id`, `action`, `target`, `request_id` |

- The TOTP secret is encrypted with a key from the settings before it is stored.
- A session has an idle limit and an absolute limit, and the service deletes expired rows.
- `viewable_tickets` lists the tickets that this session may read because it created them or looked them up with the number and the email. It holds at most 20 ids.

## Retention

- Ticket data is kept for 2 years after the last activity of the ticket. After that, a scheduled job clears the name and the NIM or NIP, replaces the email with a hash, sets `anonymized_at`, and keeps the items and the dates.
- The job runs daily and writes one audit row with the count of tickets that it changed.
- Sessions, counters and idempotency keys are deleted when they expire.
