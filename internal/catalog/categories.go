package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

const categoriesKind = "categories"

// ListCategories returns a page of categories ordered by name.
func (s *Store) ListCategories(ctx context.Context, req PageRequest) (pagination.Page[Category], error) {
	keys, err := s.codec.Decode(categoriesKind, req.Cursor, 1)
	if err != nil {
		return pagination.Page[Category]{}, fmt.Errorf("read the cursor %w", err)
	}

	var after any
	if keys != nil {
		after = keys[0]
	}

	rows, err := s.db.Query(ctx, `
		select id::text, name, tone, lower(name) from categories
		where ($1::text is null or lower(name) > $1::text)
		order by lower(name) limit $2`, after, req.Limit+1)
	if err != nil {
		return pagination.Page[Category]{}, fmt.Errorf("list the categories %w", err)
	}

	defer rows.Close()

	items := []Category{}
	sortKeys := []string{}

	for rows.Next() {
		var (
			c       Category
			sortKey string
		)

		if err := rows.Scan(&c.ID, &c.Name, &c.Tone, &sortKey); err != nil {
			return pagination.Page[Category]{}, fmt.Errorf("read a category %w", err)
		}

		items = append(items, c)
		sortKeys = append(sortKeys, sortKey)
	}

	if err := rows.Err(); err != nil {
		return pagination.Page[Category]{}, fmt.Errorf("list the categories %w", err)
	}

	var total int
	if err := s.db.QueryRow(ctx, `select count(*)::int from categories`).Scan(&total); err != nil {
		return pagination.Page[Category]{}, fmt.Errorf("count the categories %w", err)
	}

	items, more := pagination.Trim(items, req.Limit)
	page := pagination.Page[Category]{Items: items, Total: total}

	if more {
		page.NextCursor = s.codec.Encode(categoriesKind, sortKeys[len(items)-1])
	}

	return page, nil
}

// CreateCategory adds a category. A name that another category has, ignoring letter case, gives a DuplicateError.
func (s *Store) CreateCategory(ctx context.Context, in CategoryInput, rec Recorder) (Category, error) {
	var c checker

	clean := in.check("", &c)
	if err := c.err(); err != nil {
		return Category{}, err
	}

	var out Category

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `insert into categories (name, tone) values ($1::text, $2::text) returning id::text, name, tone`, clean.Name, clean.Tone).
			Scan(&out.ID, &out.Name, &out.Tone)
		if code, constraint := violation(err); code == uniqueViolation && constraint == categoryNameIndex {
			return &DuplicateError{Field: "name"}
		}

		if err != nil {
			return fmt.Errorf("insert the category %w", err)
		}

		return record(ctx, tx, rec, http.StatusCreated, out)
	})

	return out, err
}

// categoryByName finds the category of a name ignoring letter case, or inserts it with the tone. It returns the existing category and
// ignores the tone when the name exists.
func categoryByName(ctx context.Context, q Querier, name, tone string) (Category, error) {
	var c Category

	err := q.QueryRow(ctx, `
		insert into categories (name, tone) values ($1::text, $2::text)
		on conflict (lower(name)) do nothing
		returning id::text, name, tone`, name, tone).Scan(&c.ID, &c.Name, &c.Tone)
	if err == nil {
		return c, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return Category{}, fmt.Errorf("insert the category %w", err)
	}

	err = q.QueryRow(ctx, `select id::text, name, tone from categories where lower(name) = lower($1)`, name).Scan(&c.ID, &c.Name, &c.Tone)
	if err != nil {
		return Category{}, fmt.Errorf("read the category %w", err)
	}

	return c, nil
}
