package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const unitsKind = "units"

// unitCore lists a unit with its model, its category and the open ticket that holds it. The status is the lifecycle, except that an
// active unit is borrowed while it sits in an open row of ticket_units. The order of the columns is the one that scanUnit reads.
const unitCore = `
	select u.tag as tag, u.model_id::text as model_id, m.name as model_name, m.category_id::text as category_id,
	       c.name as category_name, u.serial as serial, u.condition as condition, u.place as place, u.lifecycle as lifecycle,
	       u.lifecycle_note as lifecycle_note,
	       case when u.lifecycle <> 'active' then u.lifecycle when ot.number is not null then 'borrowed' else 'available' end as status,
	       ot.number as open_ticket, u.created_at as created_at
	from units u
	join models m on m.id = u.model_id
	join categories c on c.id = m.category_id
	left join lateral (
		select t.number from ticket_units tu join tickets t on t.id = tu.ticket_id
		where tu.unit_id = u.id and tu.returned_at is null limit 1
	) ot on true`

func scanUnit(row pgx.Row) (Unit, error) {
	var (
		u       Unit
		ticket  *string
		created time.Time
	)

	err := row.Scan(&u.Tag, &u.ModelID, &u.ModelName, &u.CategoryID, &u.CategoryName, &u.Serial, &u.Condition, &u.Place, &u.Lifecycle,
		&u.LifecycleNote, &u.Status, &ticket, &created)
	if err != nil {
		return Unit{}, err //nolint:wrapcheck // the callers wrap it with what they read
	}

	if ticket != nil {
		u.OpenTicketNumber = *ticket
	}

	u.CreatedAt = timeText(created)

	return u, nil
}

func unitByTag(ctx context.Context, q Querier, tag string) (Unit, error) {
	if !ValidTag(tag) {
		return Unit{}, ErrNotFound
	}

	u, err := scanUnit(q.QueryRow(ctx, `select * from (`+unitCore+`) x where tag = $1`, tag))
	if errors.Is(err, pgx.ErrNoRows) {
		return Unit{}, ErrNotFound
	}

	if err != nil {
		return Unit{}, fmt.Errorf("read the unit %w", err)
	}

	return u, nil
}

// GetUnit returns one unit as an admin sees it.
func (s *Store) GetUnit(ctx context.Context, tag string) (Unit, error) {
	return unitByTag(ctx, s.db, tag)
}

// unitFilterSQL is the part of the list query that every list of units shares. The parameters are the search text, the model, the
// category and the status. The core query gives the model and category ids as text, so the uuid parameters are compared as text.
const unitFilterSQL = `
	where ($1::text = '' or position(lower($1::text) in lower(tag)) > 0 or position(lower($1::text) in lower(model_name)) > 0
	       or position(lower($1::text) in lower(coalesce(serial, ''))) > 0)
	  and ($2::uuid is null or model_id = ($2::uuid)::text)
	  and ($3::uuid is null or category_id = ($3::uuid)::text)
	  and ($4::text = '' or status = $4::text)`

// ListUnits returns a page of units ordered by tag.
func (s *Store) ListUnits(ctx context.Context, f UnitFilter, req PageRequest) (pagination.Page[Unit], error) {
	f, err := f.check()
	if err != nil {
		return pagination.Page[Unit]{}, err
	}

	keys, err := s.codec.Decode(unitsKind, req.Cursor, 1)
	if err != nil {
		return pagination.Page[Unit]{}, fmt.Errorf("read the cursor %w", err)
	}

	var model, category, after any

	if f.ModelID != "" {
		model = f.ModelID
	}

	if f.CategoryID != "" {
		category = f.CategoryID
	}

	if keys != nil {
		after = keys[0]
	}

	rows, err := s.db.Query(ctx, `
		select tag, model_id, model_name, category_id, category_name, serial, condition, place, lifecycle, lifecycle_note, status,
		       open_ticket, created_at
		from (`+unitCore+`) x`+unitFilterSQL+` and ($5::text is null or tag > $5::text)
		order by tag limit $6`, f.Query, model, category, f.Status, after, req.Limit+1)
	if err != nil {
		return pagination.Page[Unit]{}, fmt.Errorf("list the units %w", err)
	}

	defer rows.Close()

	items := []Unit{}

	for rows.Next() {
		u, err := scanUnit(rows)
		if err != nil {
			return pagination.Page[Unit]{}, fmt.Errorf("read a unit %w", err)
		}

		items = append(items, u)
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[Unit]{}, fmt.Errorf("list the units %w", err)
	}

	var total int

	err = s.db.QueryRow(ctx, `select count(*)::int from (`+unitCore+`) x`+unitFilterSQL, f.Query, model, category, f.Status).Scan(&total)
	if err != nil {
		return pagination.Page[Unit]{}, fmt.Errorf("count the units %w", err)
	}

	items, more := pagination.Trim(items, req.Limit)
	page := pagination.Page[Unit]{Items: items, Total: total}

	if more {
		page.NextCursor = s.codec.Encode(unitsKind, items[len(items)-1].Tag)
	}

	return page, nil
}

// CreateUnits adds one to twenty units to a model. The tags follow the counter of the model, and the transaction holds the lock of
// the model row, so two requests never get the same number. All units are added or none.
func (s *Store) CreateUnits(ctx context.Context, modelID, adminID string, in UnitInput, rec Recorder) (UnitList, error) {
	if !isUUID(modelID) {
		return UnitList{}, ErrNotFound
	}

	clean, err := in.check()
	if err != nil {
		return UnitList{}, err
	}

	var out UnitList

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		model, err := lockModel(ctx, tx, modelID)
		if err != nil {
			return err
		}

		if model.Retired {
			return &RuleError{Kind: RuleStatusConflict, Message: "The model is retired, so it takes no new units."}
		}

		count := *clean.Count
		tags := make([]string, count)

		for i := range tags {
			tags[i] = fmt.Sprintf("MGM-%s-%03d", model.Prefix, model.Next+i)
		}

		if _, err := tx.Exec(ctx, `update models set next_unit_number = $2 where id = $1::uuid`, modelID, model.Next+count); err != nil {
			return fmt.Errorf("advance the counter of the model %w", err)
		}

		if _, err := tx.Exec(ctx, `
			insert into units (tag, model_id, serial, condition, place)
			select t, $2::uuid, $3::text, $4::text, $5::text from unnest($1::text[]) as t`, tags, modelID, clean.Serial, clean.Condition, clean.Place); err != nil {
			return fmt.Errorf("insert the units %w", err)
		}

		if _, err := tx.Exec(ctx, `
			insert into unit_events (unit_id, action, admin_id)
			select id, 'added', $2::uuid from units where tag = any($1::text[])`, tags, adminID); err != nil {
			return fmt.Errorf("write the events of the units %w", err)
		}

		out, err = unitsByTags(ctx, tx, tags)
		if err != nil {
			return err
		}

		return record(ctx, tx, rec, http.StatusCreated, out)
	})
	if err != nil {
		return UnitList{}, s.mapSerialError(ctx, err, clean.Serial)
	}

	return out, nil
}

// unitsByTags reads the units of the tags in the order of the tags.
func unitsByTags(ctx context.Context, q Querier, tags []string) (UnitList, error) {
	rows, err := q.Query(ctx, `
		select tag, model_id, model_name, category_id, category_name, serial, condition, place, lifecycle, lifecycle_note, status,
		       open_ticket, created_at
		from (`+unitCore+`) x where tag = any($1::text[])`, tags)
	if err != nil {
		return UnitList{}, fmt.Errorf("read the new units %w", err)
	}

	defer rows.Close()

	items := make([]Unit, 0, len(tags))

	for rows.Next() {
		u, err := scanUnit(rows)
		if err != nil {
			return UnitList{}, fmt.Errorf("read a new unit %w", err)
		}

		items = append(items, u)
	}

	if err := rows.Err(); err != nil {
		return UnitList{}, fmt.Errorf("read the new units %w", err)
	}

	slices.SortFunc(items, func(a, b Unit) int { return slices.Index(tags, a.Tag) - slices.Index(tags, b.Tag) })

	return UnitList{Items: items}, nil
}

// mapSerialError turns the break of the unique index on the serial number into a DuplicateError that names the other unit. The
// lookup runs after the transaction ended, because a failed transaction cannot read.
func (s *Store) mapSerialError(ctx context.Context, err error, serial *string) error {
	code, constraint := violation(err)
	if code != uniqueViolation || constraint != unitSerialIndex {
		return err
	}

	dup := &DuplicateError{Field: "serial"}

	if serial != nil {
		var tag string
		if s.db.QueryRow(ctx, `select tag from units where lower(serial) = lower($1)`, *serial).Scan(&tag) == nil {
			dup.ExistingTag = tag
		}
	}

	return dup
}

// UpdateUnit changes the details of a unit. The lifecycle has its own function.
func (s *Store) UpdateUnit(ctx context.Context, tag, adminID string, in UnitPatch) (Unit, error) {
	if !ValidTag(tag) {
		return Unit{}, ErrNotFound
	}

	clean, err := in.check()
	if err != nil {
		return Unit{}, err
	}

	var out Unit

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var id string

		err := tx.QueryRow(ctx, `
			update units set serial = case when $2::bool then $3::text else serial end, condition = coalesce($4::text, condition),
			       place = coalesce($5::text, place)
			where tag = $1 returning id::text`, tag, clean.SerialSet, clean.Serial, clean.Condition, clean.Place).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return fmt.Errorf("update the unit %w", err)
		}

		if err := addEvent(ctx, tx, id, "updated", adminID, nil); err != nil {
			return err
		}

		out, err = unitByTag(ctx, tx, tag)

		return err
	})
	if err != nil {
		return Unit{}, s.mapSerialError(ctx, err, clean.Serial)
	}

	return out, nil
}

// addEvent writes one row of the history of a unit.
func addEvent(ctx context.Context, q Querier, unitID, action, adminID string, note *string) error {
	_, err := q.Exec(ctx, `insert into unit_events (unit_id, action, admin_id, note) values ($1::uuid, $2, $3::uuid, $4)`,
		unitID, action, adminID, note)
	if err != nil {
		return fmt.Errorf("write the event of the unit %w", err)
	}

	return nil
}

// lockUnit locks the model of a unit and then the unit, in the order that docs/data-model.md names. It returns the id of the unit,
// its lifecycle and its model. A tag that names no unit gives ErrNotFound.
func lockUnit(ctx context.Context, q Querier, tag string) (id, lifecycle string, model lockedModel, err error) {
	var modelID string

	err = q.QueryRow(ctx, `select model_id::text from units where tag = $1`, tag).Scan(&modelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", lockedModel{}, ErrNotFound
	}

	if err != nil {
		return "", "", lockedModel{}, fmt.Errorf("find the model of the unit %w", err)
	}

	model, err = lockModel(ctx, q, modelID)
	if err != nil {
		return "", "", lockedModel{}, err
	}

	// the unit may have been deleted while the transaction waited for the model, and then there is nothing to change
	err = q.QueryRow(ctx, `select id::text, lifecycle from units where tag = $1 for update`, tag).Scan(&id, &lifecycle)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", lockedModel{}, ErrNotFound
	}

	if err != nil {
		return "", "", lockedModel{}, fmt.Errorf("lock the unit %w", err)
	}

	return id, lifecycle, model, nil
}

// refuseLeavingActive gives the error for a unit that is about to stop counting as stock. A unit in an open ticket row is held. A
// change that leaves less stock than the tickets hold changes the stock under them.
func refuseLeavingActive(ctx context.Context, q Querier, unitID, modelID string) error {
	var held bool

	err := q.QueryRow(ctx, `select exists (select 1 from ticket_units where unit_id = $1::uuid and returned_at is null)`, unitID).Scan(&held)
	if err != nil {
		return fmt.Errorf("read the open rows of the unit %w", err)
	}

	if held {
		return &RuleError{Kind: RuleUnitHeld, Message: "The unit is in an open ticket. Finish the ticket first."}
	}

	total, heldQty, err := stockOf(ctx, q, modelID)
	if err != nil {
		return err
	}

	if total-1 < heldQty {
		return &RuleError{Kind: RuleStockChanged, Message: "The tickets hold the rest of the stock of this model."}
	}

	return nil
}

// DeleteUnit removes a unit for good. It works only for a unit that no ticket has ever used.
func (s *Store) DeleteUnit(ctx context.Context, tag string) error {
	if !ValidTag(tag) {
		return ErrNotFound
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		id, lifecycle, model, err := lockUnit(ctx, tx, tag)
		if err != nil {
			return err
		}

		var used bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from ticket_units where unit_id = $1::uuid)`, id).Scan(&used); err != nil {
			return fmt.Errorf("read the use of the unit %w", err)
		}

		if used {
			return usedByTicket("unit")
		}

		if lifecycle == LifecycleActive {
			if err := refuseLeavingActive(ctx, tx, id, model.ID); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `delete from units where id = $1::uuid`, id); err != nil {
			if code, _ := violation(err); code == foreignKeyViolation {
				return usedByTicket("unit")
			}

			return fmt.Errorf("delete the unit %w", err)
		}

		return nil
	})
}
