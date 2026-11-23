# Data model

Status of this page. It describes the planned schema. The first migration is written, and the files in `internal/database/migrations` are the source of truth. This page is updated in the same pull request as any migration.

All tables live in one PostgreSQL database. Ids are UUIDs made by the service. Timestamps are `timestamptz` and are stored in UTC. A pickup date is a plain `date` and is read as a date in the time zone Asia/Jakarta.

## Assets

| Table | Purpose | Main columns |
| --- | --- | --- |
| `categories` | Groups of models | `id`, `name` (unique), `tone`, `created_at` |
| `models` | A kind of item, such as a headset | `id`, `name` (unique), `category_id`, `description`, `prefix` (unique), `next_unit_number`, `retired_at`, `created_at` |
| `units` | One physical item with a QR label | `id`, `tag` (unique), `model_id`, `serial` (unique, may be null), `condition`, `place`, `lifecycle`, `lifecycle_note`, `created_at` |
| `unit_events` | History of a unit | `id`, `unit_id`, `at`, `action`, `admin_id`, `ticket_id` (null unless a handover or a return), `note` |

- `tag` has the form `MGM-<prefix>-<nnn>`. The QR label holds the tag, so a tag never changes and is never reused.
- `next_unit_number` is increased inside the same transaction that inserts the units. The model row is locked first, so two admins never get the same number.
- `lifecycle` is one of `active`, `maintenance`, `lost` and `retired`. Only `active` units can be loaned.
- A model or a unit is retired and never deleted. A permanent delete is allowed only for a model or a unit that no ticket has ever used.
- A model can be retired only when no ticket in the status `diajukan`, `disetujui`, `siap_diambil` or `dipinjam` asks for it. Otherwise the service answers `409`.
- The names of categories and models and the serial numbers are unique ignoring letter case, through unique indexes on the lower case value. A prefix is upper case by its pattern and unique.
- A unit changes its lifecycle only from `active` to `maintenance`, `lost` or `retired`, from `maintenance` to `active`, `lost` or `retired`, from `lost` to `active` or `retired`, and from `retired` to `active`. Every step to `active` is refused while its model is retired. Another step answers `409` with the code `status_conflict`.
- Models and units have no version. When two admins change the same one, the last write wins.
- A model can be created together with a new category in one transaction. A category name that exists reuses the existing category and ignores the submitted tone.
- A serial number has 3 to 100 characters after the service trims the spaces. An empty serial number is stored as null, so units without a serial never clash.
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
- The counter row is locked from the moment the number is made until the transaction ends, so two creations never get the same number. A creation that rolls back gives its number back, so the numbers of a month have no gaps. A new month starts at 1, and the month is the month in Asia/Jakarta. The number `9999` is the last of a month, because the shape of a number holds four digits, and the next creation fails as `internal` and is logged.
- The status is one of `diajukan`, `disetujui`, `siap_diambil`, `dipinjam`, `dikembalikan`, `dibatalkan` and `ditolak`. The status `terlambat` is not stored. It is derived when a ticket is read, from the status `dipinjam` and a due date before today in Asia/Jakarta, and the API shows it as `effectiveStatus`. The due date is the pickup date plus the approved days, or the requested days when none were approved.
- `version` starts at 1 and grows with each change. An admin request carries the version it read, and a stale version is refused.
- `decision_note` holds the latest note of an approval, a reject or an admin cancel, and the same note is in the history row. The notes of a return are only in the history and in the notes of the ticket units.
- A partial unique index on `ticket_units (unit_id) where returned_at is null` makes it impossible that two open tickets hold the same unit.
- A ticket asks for models. The units are chosen at handover, when the admin scans the QR label of each unit.
- The service stores the email of a ticket in lower case and the NIM or NIP as the guest typed it. The limit of open tickets compares the email in lower case and the NIM or NIP ignoring letter case, and a ticket counts when either one matches.
- The items of a ticket are listed by the name of the model and then by its id, because the request order is not stored.
- A pickup date has no upper bound. Only the date of today in Asia/Jakarta is the lower bound.
- An open ticket, for the limit of open tickets, is a ticket in the status `diajukan`, `disetujui`, `siap_diambil` or `dipinjam`.
- Indexes cover the lookups that the routes make, and the migration lists every one of them. The unique constraint on `tickets (number)` and the start of the primary key of `ticket_units` already serve those two lookups.

## Stock rules

- The stock of a model is the count of its units with the lifecycle `active`.
- A model is held by every ticket in the status `disetujui`, `siap_diambil` or `dipinjam`. A ticket in the status `diajukan` holds nothing. A late ticket is a ticket in the status `dipinjam`, so it still holds.
- Held quantity is the sum of `approved_qty` of those tickets for the model. Available quantity is the stock minus the held quantity, and the API shows a negative number as 0.
- A request that creates a ticket is refused when a quantity is above the available quantity at that moment. The check reads under the share locks of the creation and holds nothing, because the hold starts at an approval.
- An approval is refused when `approved_qty` is more than the available quantity at that moment. An approval also needs at least one `approved_qty` above 0, because a hold of nothing could only be undone by a cancel.
- A handover needs exactly `approved_qty` units for each model. Each unit must be active, of the right model and not in an open row of `ticket_units`.
- A unit cannot leave the status `active` while it is in an open row of `ticket_units`, and the service answers `409` with the code `unit_held`. It also cannot leave the status `active` by an admin change when the stock would fall below the held quantity of its model, and the service answers `409` with the code `stock_changed`.
- A return is all or nothing and runs in one transaction. It closes the ticket, then closes the rows of `ticket_units`, then applies the lifecycle of each unit. A unit that comes back as `lost` or `maintenance` lowers the stock after the ticket no longer holds, so the stock never drops below a hold that the same ticket made.
- A lost unit can leave another approved ticket short. That ticket keeps its status, and its handover answers `409` with the code `stock_changed` until the admin fixes the stock or cancels it.
- An admin may cancel a ticket in the status `disetujui` or `siap_diambil` with a note, which releases its hold. A ticket that nobody collects is closed this way.

## Steps of an admin

- Every step of an admin reads the ticket by its number, and a number that has the wrong shape or that no ticket has answers `404` with the code `not_found`. The step checks the status before the version. A ticket that is not in a status that allows the step answers `409` with the code `status_conflict`, also when the version of the request is stale. A stale version on a ticket in the right status answers `version_conflict`. So the retry of a request whose answer was lost finds a status conflict that says what happened.
- An approval names the approved quantity of every model of the ticket, once for each model. A model that the ticket does not ask for answers `422` with the detail code `unknown_reference`. A quantity above the requested one answers `out_of_range`. A model that is left out answers `required` on `items`. The approved days default to the requested days, and the ticket always stores them at an approval, so the due date counts the approved days.
- A rejection stores only the note. The approved quantities and the approved days stay empty, and the ticket holds nothing.
- An approval locks the models of the ticket in the order of the model id, reads the head of the ticket again, and then reads the stock. The ticket is in the status `diajukan`, so it holds nothing and its own quantities are not in the held quantity. A model with less available stock than its approved quantity answers `409` with the code `stock_changed` and one detail `items[i].approvedQty` with the code `too_many` for each short model. Two tickets that ask for the last unit both reach `diajukan`, and only the first approval wins.
- A handover reads the stock under the same locks. It answers `409` with `stock_changed` when the stock of a model of the ticket is below the quantity that the tickets hold, for example when units left the stock of the model after the approval. It then locks the named units in the order of the id. A tag that no unit has answers `422` with `unknown_reference` on `tags[i]`. A unit that is not active, is of a model that the ticket holds no quantity of, or sits in an open row answers `409` with `unit_unavailable`. A model with fewer or more units than its approved quantity answers `422` on `tags` with the detail code `required` or `too_many`. The partial unique index on the open rows is the last guard, and its violation answers `unit_unavailable` too.
- A return names every unit that the open rows of the ticket hold, once for each unit. A tag that the ticket does not hold answers `422` with `unknown_reference` on `units[i].tag`, and a held unit that no line names answers `required` on `units`. A lost unit needs a note, and any other line may carry one. The ticket moves to `dikembalikan` first, the open rows close with the outcome and the note of each line, and then the outcomes apply. A unit that comes back in maintenance or lost changes its lifecycle and keeps the note as `lifecycle_note`. The unit gets the event `returned` and, for a changed lifecycle, a second event of that lifecycle one microsecond later, so its history reads in one fixed order. Both events name the ticket and the admin.
- The notes of a return are in the history entry and in the units of the ticket, and never in `decision_note`. The history note holds one line for each note, written as the tag, the outcome in brackets, a hyphen and the note, in the order of the request. A return with no note has no history note.
- The table has no column for a late return. A loan that is late can be returned, and its lateness at that moment can be read from the time of the entry `dikembalikan` and the due date.
- A second cancel of a ticket that is already `dibatalkan` answers `200` with the ticket and changes nothing, whoever cancelled it first. An admin cancel of a ticket in the status `diajukan` answers `status_conflict`, because an admin rejects such a ticket.
- Each change of status writes the admin that made it in `ticket_history.admin_id`. The name of the admin is shown only in the answers to an admin, as `actorName` of a history entry and of an event of the feed. The answers to a borrower have no such field.
- The feed of events is the table `ticket_history` joined to the ticket. It sorts by time and then by id, newest first, and the cursor holds the time with its full precision, so rows with the same time keep one fixed order. The actor name of an admin event is the name of the admin. The actor name of a guest event is the name of the requester, and it is null after the retention job anonymized the ticket.
- The queue sorts by the creation time and then by id, newest first. The filter by status uses the effective status, so `dipinjam` lists the loans that are not late and `terlambat` lists the late ones. The search text matches the number, the name and the NIM or NIP with a position test and not with `LIKE`, so a percent sign or an underscore is plain text. A status that appears twice in the filter counts once.
- The dashboard reads three queries. The first counts all tickets by effective status in one pass over the table, the second counts the units by the status that a unit shows, and the third counts the units of each category. The open tickets of the dashboard are the five open statuses of the first query. None of them needs a new index at the size of the lab, and the queue sorts on the creation time with no index of its own.
- The notices of a decision, a ready step and an admin cancel go to an interface, and the service wires an implementation that only logs. A late notice needs a job that does not exist yet.

## Concurrency

- Every transaction that changes stock locks the model rows with `select ... for update`, in the order of the model id, before it reads. That covers an approval, a handover, a return, a lifecycle change, adding units and retiring a model. A handover and a lifecycle change on the same unit therefore never run together.
- A request that creates a ticket takes a transaction advisory lock for the normalized email and one for the normalized NIM or NIP, in a fixed order, before it counts the open tickets. Two parallel requests cannot pass the limit together. The email is compared in lower case.
- A change of status follows the table of steps in `internal/ticket/status.go`. It lists each step with the kind of actor that may make it, and the guarded update takes its expected old statuses from that table. A step raises the version and writes a history entry in the same transaction. A guest cancel has no version to check, writes an entry with the actor `guest` and no note, and leaves `decision_note` as it is. When a decision commits first, the cancel changes no row and answers `409` with the code `status_conflict`.
- A request that fills the hidden field `website` runs the whole creation, the refusals included, inside a transaction that is rolled back, so the answer has the shape of a real ticket and nothing is stored. It claims no `Idempotency-Key`.
- A scheduled job runs under an advisory lock, so only one of the two copies runs it.
- Creating a ticket takes the advisory locks first and then the requested model rows with `select ... for share`, in the order of the model id. It refuses a retired model with `validation_failed`. A permanent delete of a model or a unit locks it first and answers `409` with the code `status_conflict` when a ticket used it, and a database error never reaches the client as `500`.
- The `model_id` of a unit never changes, because a change would break the lock order.
- A status change of a ticket is a guarded update. It names the expected old status and the expected version, and it changes no row when either differs. The service then answers `409`.
- A step of an admin takes the locks in one order. It locks the models in the order of the model id and then the named units in the order of the unit id. It takes the row of the ticket last, in the guarded update. A step that only changes the status, which is the ready notice and the admin cancel, takes no lock on the models, because it releases or keeps a hold and never adds one. A transaction that waits for a model never holds the row of a ticket that another transaction needs, so the order has no cycle.
- A request that creates a ticket, a model, a category or units may carry an `Idempotency-Key`. The service stores the key for the session with the answer for 24 hours and returns the same answer for a repeat, whatever the new body says. The row has a state, `running` or `done`, and a repeat that arrives while the first request runs answers `409` with the code `request_in_progress`.
- A claim that stays in the state `running` for more than 2 minutes belongs to a copy that died. A new request with the same key takes it over, and the old request can no longer store its answer, so its transaction does not commit.
- A model that is retired takes no new units, and the service answers `409` with the code `status_conflict`.
- The cursor of a list is signed with a key derived from `TOTP_ENCRYPTION_KEY`, so a client cannot build one. A cursor that does not verify answers `422` with the detail field `cursor`.
- Database specs in CI prove these rules on a real PostgreSQL, because a mock cannot prove locks.

## People and sessions

| Table | Purpose | Main columns |
| --- | --- | --- |
| `admins` | Local admin accounts | `id`, `email` (unique), `name`, `password_hash`, `password_expires_at`, `totp_secret`, `totp_enabled_at`, `totp_last_step`, `created_at` |
| `admin_recovery_codes` | One use codes for TOTP | `admin_id`, `code_hash`, `used_at` |
| `sessions` | Guest and admin sessions, `kind` is `guest`, `admin` or `admin_setup` | `token_hash` (primary key), `kind`, `admin_id`, `guest_name`, `guest_nim_nip`, `guest_email`, `csrf_token`, `viewable_tickets`, `created_at`, `last_seen_at`, `expires_at` |
| `rate_counters` | Fixed window counters | `key`, `window_start`, `count` |
| `idempotency_keys` | Safe repeat of a request that creates something | `session_hash`, `operation`, `key` (together the primary key), `state`, `status_code`, `response`, `created_at` |
| `audit_log` | Admin actions | `id`, `at`, `admin_id`, `action`, `target`, `request_id` |

- The TOTP secret is encrypted with a key from the settings before it is stored. `totp_last_step` holds the last accepted time step, so a code cannot be used twice.
- An admin is created by a command of the service with a one use initial password that expires after 24 hours (`password_expires_at`). An admin without `totp_enabled_at`, or who still has the initial password (`password_expires_at` is set, and a new password clears it), gets only an `admin_setup` session. It lasts 15 minutes and can call only `POST /api/admin/me/password`, `POST /api/admin/me/totp`, `GET /api/session` and `DELETE /api/session`. The password route sets a new password. The TOTP route enrolls TOTP and returns the first recovery codes. Both need the current password.
- Changing the password, enrolling TOTP again and making new recovery codes need the current password and a valid TOTP code. The use of a recovery code ends the other sessions of that admin.
- Passwords are hashed with Argon2id, with 19 MiB of memory, 2 passes and 1 lane as the default. A setting caps the number of hashes that run at once, and the default is 2.
- A sign in makes a new session id and deletes the session that came with the request. A password change deletes the other sessions of that admin.
- A session has an idle limit and an absolute limit, and the service deletes expired rows. The service writes `last_seen_at` at most once a minute.
- Failed password or code checks on the account routes of an admin are counted in `rate_counters` for the session, so a stolen session cannot guess the password or a code without a limit.
- Failed sign ins are counted in `rate_counters` for each submitted email, in lower case, and address together. An email that is not an account counts the same way and costs the same hash work, so the answer never shows which emails are accounts. A stranger cannot lock the account of an admin, because the counter holds the address. A high daily count for one account raises an alert and blocks nothing.
- `viewable_tickets` lists the tickets that this session may read because it created them or looked them up with the number and the email. It holds at most 20 ids. A new id goes to the end of the list, a ticket that is already in the list moves to the end, and the oldest ids beyond 20 drop out, so a ticket that dropped out can be found again with a lookup. A ticket that the session may not read answers `404` with the code `not_found`, whether it exists or not.
- A guest session may have no name, no NIM or NIP and no email. A successful ticket lookup without a session makes such a session. A lookup with a guest session adds the ticket to that session and changes nothing else. A lookup with an admin session changes no session. A failed lookup makes no session. A session without an identity can read and cancel the tickets it found and cannot create a ticket until the guest form is sent. The lookup lets a number in lower case through and reads the email in lower case, and a number or an email that has the wrong shape answers `422` before any counter moves.

## Retention

- Ticket data is kept for 2 years after the last activity of a closed ticket. A ticket is closed when its status is `dikembalikan`, `dibatalkan` or `ditolak`.
- After that, a scheduled job sets the name, the NIM or NIP and the email to null, clears `purpose`, `decision_note`, the notes of the history and the notes of the returned units, sets `anonymized_at`, and keeps the items and the dates.
- The job runs daily and writes one audit row with the count of tickets that it changed. The job is safe to run again, so an operator runs it by hand after a restore of the database.
- Sessions, counters and idempotency keys are deleted when they expire.
