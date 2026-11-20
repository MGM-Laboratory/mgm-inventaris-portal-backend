-- The whole schema of docs/data-model.md.
-- Enum values are text columns with check constraints, so a new value is a small migration and not an ALTER TYPE.

create table admins (
    id uuid primary key default gen_random_uuid(),
    email text not null,
    name text not null,
    password_hash text not null,
    password_expires_at timestamptz,
    totp_secret text,
    totp_enabled_at timestamptz,
    totp_last_step bigint,
    created_at timestamptz not null default now(),
    constraint admins_email_key unique (email),
    constraint admins_email_lower check (email = lower(email)),
    constraint admins_totp_enabled_has_secret check (totp_enabled_at is null or totp_secret is not null)
);

create table admin_recovery_codes (
    admin_id uuid not null references admins (id) on delete cascade,
    code_hash text not null,
    used_at timestamptz,
    primary key (admin_id, code_hash)
);

create table categories (
    id uuid primary key default gen_random_uuid(),
    name text not null,
    tone text not null,
    created_at timestamptz not null default now(),
    constraint categories_name_length check (char_length(name) between 2 and 60),
    constraint categories_tone_values check (tone in ('blue', 'yellow', 'red', 'green'))
);
create unique index categories_name_lower_key on categories (lower(name));

create table models (
    id uuid primary key default gen_random_uuid(),
    name text not null,
    category_id uuid not null references categories (id),
    description text not null default '',
    prefix text not null,
    next_unit_number integer not null default 1,
    retired_at timestamptz,
    created_at timestamptz not null default now(),
    constraint models_name_length check (char_length(name) between 2 and 100),
    constraint models_description_length check (char_length(description) <= 500),
    constraint models_prefix_shape check (prefix ~ '^[A-Z0-9]{2,6}$'),
    constraint models_prefix_key unique (prefix),
    constraint models_next_unit_number_positive check (next_unit_number >= 1)
);
create unique index models_name_lower_key on models (lower(name));
create index models_category_id_idx on models (category_id);

-- A unit follows its model, so deleting a model that no ticket used also deletes its units.
-- A ticket row blocks that, because ticket_units and ticket_items reference units and models with no cascade.
create table units (
    id uuid primary key default gen_random_uuid(),
    tag text not null,
    model_id uuid not null references models (id) on delete cascade,
    serial text,
    condition text not null,
    place text not null,
    lifecycle text not null default 'active',
    lifecycle_note text,
    created_at timestamptz not null default now(),
    constraint units_tag_key unique (tag),
    constraint units_tag_shape check (tag ~ '^MGM-[A-Z0-9]{2,6}-[0-9]{3,}$'),
    constraint units_serial_length check (serial is null or (serial = btrim(serial) and char_length(serial) between 3 and 100)),
    constraint units_condition_length check (char_length(condition) between 2 and 200),
    constraint units_place_length check (char_length(place) between 2 and 200),
    constraint units_lifecycle_values check (lifecycle in ('active', 'maintenance', 'lost', 'retired')),
    constraint units_lifecycle_note_length check (lifecycle_note is null or char_length(lifecycle_note) between 1 and 500),
    constraint units_lost_retired_has_note check (lifecycle not in ('lost', 'retired') or lifecycle_note is not null)
);
create unique index units_serial_lower_key on units (lower(serial));
create index units_model_id_idx on units (model_id);

create table unit_events (
    id uuid primary key default gen_random_uuid(),
    unit_id uuid not null references units (id) on delete cascade,
    at timestamptz not null default now(),
    action text not null,
    admin_id uuid references admins (id),
    note text,
    constraint unit_events_action_values check (
        action in ('added', 'updated', 'maintenance', 'lost', 'retired', 'reactivated', 'handed_over', 'returned')
    )
);
create index unit_events_unit_id_at_idx on unit_events (unit_id, at);
create index unit_events_at_idx on unit_events (at desc);

-- The requester columns are null only after the retention job anonymized the ticket.
-- The index on number is the unique constraint, and the index on ticket_units (ticket_id) is the start of its primary key.
create table tickets (
    id uuid primary key default gen_random_uuid(),
    number text not null,
    status text not null default 'diajukan',
    requester_kind text not null default 'guest',
    requester_name text,
    requester_nim_nip text,
    requester_email text,
    purpose text,
    pickup_date date not null,
    duration_days integer not null,
    approved_duration_days integer,
    decision_note text,
    version integer not null default 1,
    created_at timestamptz not null default now(),
    anonymized_at timestamptz,
    constraint tickets_number_key unique (number),
    constraint tickets_number_shape check (number ~ '^MGM-[0-9]{4}-[0-9]{4}-[A-HJ-NP-Z2-9]{4}$'),
    constraint tickets_status_values check (
        status in ('diajukan', 'disetujui', 'siap_diambil', 'dipinjam', 'dikembalikan', 'dibatalkan', 'ditolak')
    ),
    constraint tickets_requester_kind_values check (requester_kind in ('guest')),
    constraint tickets_duration_positive check (duration_days >= 1),
    constraint tickets_approved_duration_positive check (approved_duration_days is null or approved_duration_days >= 1),
    constraint tickets_version_positive check (version >= 1),
    constraint tickets_requester_kept_until_anonymized check (
        anonymized_at is not null
        or (requester_name is not null and requester_nim_nip is not null and requester_email is not null and purpose is not null)
    ),
    constraint tickets_anonymized_clears_requester check (
        anonymized_at is null
        or (requester_name is null and requester_nim_nip is null and requester_email is null and purpose is null)
    )
);
create index tickets_status_created_at_idx on tickets (status, created_at);
create index tickets_requester_email_lower_idx on tickets (lower(requester_email));
create index tickets_requester_nim_nip_idx on tickets (requester_nim_nip);

-- A handover or a return of a unit points to its ticket, so the unit history shows the ticket number.
alter table unit_events add column ticket_id uuid references tickets (id);
create index unit_events_ticket_id_idx on unit_events (ticket_id) where ticket_id is not null;

create table ticket_items (
    ticket_id uuid not null references tickets (id),
    model_id uuid not null references models (id),
    qty integer not null,
    approved_qty integer,
    primary key (ticket_id, model_id),
    constraint ticket_items_qty_positive check (qty >= 1),
    constraint ticket_items_approved_qty_range check (approved_qty >= 0 and approved_qty <= qty)
);
create index ticket_items_model_id_idx on ticket_items (model_id);

create table ticket_units (
    ticket_id uuid not null references tickets (id),
    unit_id uuid not null references units (id),
    handed_over_at timestamptz not null default now(),
    returned_at timestamptz,
    return_outcome text,
    return_note text,
    primary key (ticket_id, unit_id),
    constraint ticket_units_outcome_values check (return_outcome in ('ok', 'maintenance', 'lost')),
    constraint ticket_units_return_is_complete check ((returned_at is null) = (return_outcome is null))
);
-- a unit sits in at most one open row, which is what stops two tickets from holding it together
create unique index ticket_units_open_unit_key on ticket_units (unit_id) where returned_at is null;

create table ticket_history (
    id uuid primary key default gen_random_uuid(),
    ticket_id uuid not null references tickets (id),
    at timestamptz not null default now(),
    status text not null,
    actor_kind text not null,
    admin_id uuid references admins (id),
    note text,
    constraint ticket_history_status_values check (
        status in ('diajukan', 'disetujui', 'siap_diambil', 'dipinjam', 'dikembalikan', 'dibatalkan', 'ditolak')
    ),
    constraint ticket_history_actor_kind_values check (actor_kind in ('guest', 'admin'))
);
create index ticket_history_ticket_id_at_idx on ticket_history (ticket_id, at);
create index ticket_history_at_idx on ticket_history (at desc);

create table ticket_counters (
    period text primary key,
    last integer not null default 0,
    constraint ticket_counters_period_shape check (period ~ '^[0-9]{4}$'),
    constraint ticket_counters_last_not_negative check (last >= 0)
);

create table sessions (
    token_hash bytea primary key,
    kind text not null,
    admin_id uuid references admins (id) on delete cascade,
    guest_name text,
    guest_nim_nip text,
    guest_email text,
    csrf_token text not null,
    viewable_tickets uuid[] not null default '{}',
    created_at timestamptz not null default now(),
    last_seen_at timestamptz not null default now(),
    expires_at timestamptz not null,
    constraint sessions_token_hash_length check (octet_length(token_hash) = 32),
    constraint sessions_kind_values check (kind in ('guest', 'admin', 'admin_setup')),
    constraint sessions_admin_kinds_have_admin check ((kind in ('admin', 'admin_setup')) = (admin_id is not null)),
    constraint sessions_identity_is_complete check (
        (guest_name is null) = (guest_nim_nip is null) and (guest_name is null) = (guest_email is null)
    ),
    constraint sessions_admin_has_no_identity check (kind = 'guest' or guest_name is null),
    constraint sessions_viewable_tickets_cap check (cardinality(viewable_tickets) <= 20)
);
create index sessions_expires_at_idx on sessions (expires_at);
create index sessions_admin_id_idx on sessions (admin_id);

create table rate_counters (
    key text not null,
    window_start timestamptz not null,
    count integer not null default 0,
    primary key (key, window_start),
    constraint rate_counters_count_not_negative check (count >= 0)
);
create index rate_counters_window_start_idx on rate_counters (window_start);

create table idempotency_keys (
    session_hash bytea not null,
    operation text not null,
    key text not null,
    state text not null,
    status_code integer,
    response jsonb,
    created_at timestamptz not null default now(),
    primary key (session_hash, operation, key),
    constraint idempotency_keys_key_shape check (key ~ '^[A-Za-z0-9_-]{8,64}$'),
    constraint idempotency_keys_state_values check (state in ('running', 'done')),
    constraint idempotency_keys_done_has_answer check (state = 'running' or (status_code is not null and response is not null))
);
create index idempotency_keys_created_at_idx on idempotency_keys (created_at);

create table audit_log (
    id uuid primary key default gen_random_uuid(),
    at timestamptz not null default now(),
    admin_id uuid references admins (id),
    action text not null,
    target text,
    request_id text
);
create index audit_log_at_idx on audit_log (at desc);
