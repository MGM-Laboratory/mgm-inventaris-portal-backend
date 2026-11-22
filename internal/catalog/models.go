package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const (
	catalogKind = "catalog"
	modelsKind  = "models"
)

// modelColumns lists a model with its stock and the quantity that the tickets hold. The order is the one that scanModel reads.
const modelColumns = `
	m.id::text as id, m.name as name, m.category_id::text as category_id, m.description as description, m.prefix as prefix,
	m.retired_at as retired_at,
	(select count(*) from units u where u.model_id = m.id and u.lifecycle = 'active')::int as total,
	coalesce((select sum(ti.approved_qty) from ticket_items ti join tickets t on t.id = ti.ticket_id
	          where ti.model_id = m.id and t.status in (` + holdingStatuses + `)), 0)::int as held`

func scanModel(row pgx.Row) (Model, error) {
	var (
		m       Model
		retired *time.Time
	)

	if err := row.Scan(&m.ID, &m.Name, &m.CategoryID, &m.Description, &m.Prefix, &retired, &m.Total, &m.Held); err != nil {
		return Model{}, err //nolint:wrapcheck // the callers wrap it with what they read
	}

	m.RetiredAt = timeTextPtr(retired)
	m.Available = max(m.Total-m.Held, 0)

	return m, nil
}

func modelByID(ctx context.Context, q Querier, id string) (Model, error) {
	m, err := scanModel(q.QueryRow(ctx, `select `+modelColumns+` from models m where m.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, ErrNotFound
	}

	if err != nil {
		return Model{}, fmt.Errorf("read the model %w", err)
	}

	return m, nil
}

// modelFilterSQL is the part of the list query that every list shares. The parameters are the search text, the category, whether a
// retired model counts, and whether the description is searched too.
const modelFilterSQL = `
	where ($1::text = '' or position(lower($1::text) in lower(m.name)) > 0 or ($4::bool and position(lower($1::text) in lower(m.description)) > 0))
	  and ($2::uuid is null or m.category_id = $2::uuid)
	  and ($3::bool or m.retired_at is null)`

func (s *Store) listModels(ctx context.Context, kind string, f ModelFilter, req PageRequest, searchDescription bool) (pagination.Page[Model], error) {
	f, err := f.check()
	if err != nil {
		return pagination.Page[Model]{}, err
	}

	keys, err := s.codec.Decode(kind, req.Cursor, 1)
	if err != nil {
		return pagination.Page[Model]{}, fmt.Errorf("read the cursor %w", err)
	}

	var (
		category any
		after    any
	)

	if f.CategoryID != "" {
		category = f.CategoryID
	}

	if keys != nil {
		after = keys[0]
	}

	core := `select ` + modelColumns + `, lower(m.name) as sort_name from models m` + modelFilterSQL

	rows, err := s.db.Query(ctx, `
		select id, name, category_id, description, prefix, retired_at, total, held, sort_name from (`+core+`) x
		where (not $5::bool or total > held) and ($6::text is null or sort_name > $6::text)
		order by sort_name limit $7`,
		f.Query, category, f.IncludeRetired, searchDescription, f.Available, after, req.Limit+1)
	if err != nil {
		return pagination.Page[Model]{}, fmt.Errorf("list the models %w", err)
	}

	defer rows.Close()

	items := []Model{}
	sortKeys := []string{}

	for rows.Next() {
		var (
			m       Model
			retired *time.Time
			sortKey string
		)

		if err := rows.Scan(&m.ID, &m.Name, &m.CategoryID, &m.Description, &m.Prefix, &retired, &m.Total, &m.Held, &sortKey); err != nil {
			return pagination.Page[Model]{}, fmt.Errorf("read a model %w", err)
		}

		m.RetiredAt = timeTextPtr(retired)
		m.Available = max(m.Total-m.Held, 0)
		items = append(items, m)
		sortKeys = append(sortKeys, sortKey)
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[Model]{}, fmt.Errorf("list the models %w", err)
	}

	var total int

	err = s.db.QueryRow(ctx, `select count(*)::int from (`+core+`) x where (not $5::bool or total > held)`,
		f.Query, category, f.IncludeRetired, searchDescription, f.Available).Scan(&total)
	if err != nil {
		return pagination.Page[Model]{}, fmt.Errorf("count the models %w", err)
	}

	items, more := pagination.Trim(items, req.Limit)
	page := pagination.Page[Model]{Items: items, Total: total}

	if more {
		page.NextCursor = s.codec.Encode(kind, sortKeys[len(items)-1])
	}

	return page, nil
}

// ListCatalog returns the page of the models that a guest reads. A retired model is not listed. The search reads the name and the
// description.
func (s *Store) ListCatalog(ctx context.Context, f ModelFilter, req PageRequest) (pagination.Page[CatalogModel], error) {
	f.IncludeRetired = false

	page, err := s.listModels(ctx, catalogKind, f, req, true)
	if err != nil {
		return pagination.Page[CatalogModel]{}, err
	}

	out := pagination.Page[CatalogModel]{Items: make([]CatalogModel, 0, len(page.Items)), Total: page.Total, NextCursor: page.NextCursor}
	for _, m := range page.Items {
		out.Items = append(out.Items, m.CatalogModel)
	}

	return out, nil
}

// ListModels returns the page of the models that an admin reads. The search reads the name only.
func (s *Store) ListModels(ctx context.Context, f ModelFilter, req PageRequest) (pagination.Page[Model], error) {
	return s.listModels(ctx, modelsKind, f, req, false)
}

// CreateModel adds a model in an existing category or in a new one. A new category whose name exists reuses the existing category.
func (s *Store) CreateModel(ctx context.Context, in NewModel, rec Recorder) (Model, error) {
	clean, err := in.check()
	if err != nil {
		return Model{}, err
	}

	description := ""
	if clean.Description != nil {
		description = *clean.Description
	}

	var out Model

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		categoryID := ""

		if clean.NewCategory != nil {
			category, err := categoryByName(ctx, tx, clean.NewCategory.Name, clean.NewCategory.Tone)
			if err != nil {
				return err
			}

			categoryID = category.ID
		} else {
			categoryID = *clean.CategoryID
		}

		err := tx.QueryRow(ctx, `
			insert into models (name, category_id, description, prefix) values ($1::text, $2::uuid, $3::text, $4::text)
			returning id::text`, clean.Name, categoryID, description, clean.Prefix).Scan(&out.ID)
		if err != nil {
			return mapModelWriteError(err)
		}

		out = Model{
			CatalogModel: CatalogModel{ID: out.ID, Name: clean.Name, CategoryID: categoryID, Description: description},
			Prefix:       clean.Prefix,
		}

		return record(ctx, tx, rec, http.StatusCreated, out)
	})

	return out, err
}

// mapModelWriteError turns the constraint errors of a model write into the errors of the contract.
func mapModelWriteError(err error) error {
	code, constraint := violation(err)

	switch {
	case code == uniqueViolation && constraint == modelNameIndex:
		return &DuplicateError{Field: "name"}
	case code == uniqueViolation && constraint == modelPrefixKey:
		return &DuplicateError{Field: "prefix"}
	case code == foreignKeyViolation && constraint == "models_category_id_fkey":
		return &ValidationError{Details: []FieldError{{Field: "categoryId", Code: DetailUnknownReference, Message: "This category does not exist."}}}
	default:
		return fmt.Errorf("write the model %w", err)
	}
}

// UpdateModel changes a model. Retiring it is refused while an open ticket asks for it.
func (s *Store) UpdateModel(ctx context.Context, id string, in ModelPatch) (Model, error) {
	if !isUUID(id) {
		return Model{}, ErrNotFound
	}

	clean, err := in.check()
	if err != nil {
		return Model{}, err
	}

	var out Model

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := LockModels(ctx, tx, id); err != nil {
			return err
		}

		current, err := modelByID(ctx, tx, id)
		if err != nil {
			return err
		}

		// 0 keeps the retirement, 1 retires and 2 makes the model active again
		retirement := 0

		if clean.Retired != nil {
			retirement = 2

			if *clean.Retired {
				retirement = 1

				if current.RetiredAt == nil {
					if err := refuseWhileAsked(ctx, tx, id); err != nil {
						return err
					}
				}
			}
		}

		_, err = tx.Exec(ctx, `
			update models set name = coalesce($2::text, name), category_id = coalesce($3::uuid, category_id),
			       description = coalesce($4::text, description),
			       retired_at = case $5::int when 1 then coalesce(retired_at, $6::timestamptz) when 2 then null else retired_at end
			where id = $1::uuid`, id, clean.Name, clean.CategoryID, clean.Description, retirement, s.moment())
		if err != nil {
			return mapModelWriteError(err)
		}

		out, err = modelByID(ctx, tx, id)

		return err
	})

	return out, err
}

// refuseWhileAsked gives a RuleError when an open ticket asks for the model.
func refuseWhileAsked(ctx context.Context, q Querier, modelID string) error {
	var asked bool

	err := q.QueryRow(ctx, `
		select exists (select 1 from ticket_items ti join tickets t on t.id = ti.ticket_id
		               where ti.model_id = $1::uuid and t.status in (`+openStatuses+`))`, modelID).Scan(&asked)
	if err != nil {
		return fmt.Errorf("read the open tickets of the model %w", err)
	}

	if asked {
		return &RuleError{Kind: RuleStatusConflict, Message: "An open ticket asks for this model, so it cannot be retired."}
	}

	return nil
}

// DeleteModel removes a model for good, with its units. It works only for a model that no ticket has ever used.
func (s *Store) DeleteModel(ctx context.Context, id string) error {
	if !isUUID(id) {
		return ErrNotFound
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockModel(ctx, tx, id); err != nil {
			return err
		}

		var used bool

		err := tx.QueryRow(ctx, `
			select exists (select 1 from ticket_items where model_id = $1::uuid)
			    or exists (select 1 from ticket_units tu join units u on u.id = tu.unit_id where u.model_id = $1::uuid)`, id).Scan(&used)
		if err != nil {
			return fmt.Errorf("read the use of the model %w", err)
		}

		if used {
			return usedByTicket("model")
		}

		if _, err := tx.Exec(ctx, `delete from models where id = $1::uuid`, id); err != nil {
			if code, _ := violation(err); code == foreignKeyViolation {
				return usedByTicket("model")
			}

			return fmt.Errorf("delete the model %w", err)
		}

		return nil
	})
}

func usedByTicket(what string) error {
	return &RuleError{Kind: RuleStatusConflict, Message: "A ticket used this " + what + ", so retire it and do not delete it."}
}
