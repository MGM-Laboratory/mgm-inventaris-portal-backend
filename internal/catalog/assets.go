package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetPublicAsset returns the page of a unit for anyone. It holds only the tag, the status and the model.
func (s *Store) GetPublicAsset(ctx context.Context, tag string) (PublicAsset, error) {
	asset, err := s.readAsset(ctx, tag)
	if err != nil {
		return PublicAsset{}, err
	}

	return PublicAsset{Tag: asset.Tag, Status: asset.Status, Model: asset.Model}, nil
}

// GetAdminAsset returns the page of a unit for an admin, with the serial number, the condition, the place and the open ticket.
func (s *Store) GetAdminAsset(ctx context.Context, tag string) (AdminAsset, error) {
	return s.readAsset(ctx, tag)
}

// readAsset reads the unit with its model, its category and its stock. The late flag of the open ticket is computed in SQL from the
// due date, in the time zone Asia/Jakarta, as docs/data-model.md says.
func (s *Store) readAsset(ctx context.Context, tag string) (AdminAsset, error) {
	if !ValidTag(tag) {
		return AdminAsset{}, ErrNotFound
	}

	var (
		page         AdminAsset
		total, held  int
		number       *string
		ticketStatus *string
		late         *bool
		requester    *string
	)

	err := s.db.QueryRow(ctx, `
		select u.tag,
		       case when u.lifecycle <> 'active' then u.lifecycle when ot.number is not null then 'borrowed' else 'available' end,
		       m.id::text, m.name, m.description, c.id::text, c.name, c.tone,
		       (select count(*) from units a where a.model_id = m.id and a.lifecycle = 'active')::int,
		       coalesce((select sum(ti.approved_qty) from ticket_items ti join tickets h on h.id = ti.ticket_id
		                 where ti.model_id = m.id and h.status in (`+holdingStatuses+`)), 0)::int,
		       u.serial, u.condition, u.place, ot.number, ot.status, ot.late, ot.requester_name
		from units u
		join models m on m.id = u.model_id
		join categories c on c.id = m.category_id
		left join lateral (
			select t.number, t.status, t.requester_name,
			       (t.status = 'dipinjam'
			        and (timezone('Asia/Jakarta', $2::timestamptz))::date > t.pickup_date + coalesce(t.approved_duration_days, t.duration_days)) as late
			from ticket_units tu join tickets t on t.id = tu.ticket_id
			where tu.unit_id = u.id and tu.returned_at is null limit 1
		) ot on true
		where u.tag = $1`, tag, s.moment()).Scan(&page.Tag, &page.Status, &page.Model.ID, &page.Model.Name, &page.Model.Description,
		&page.Model.Category.ID, &page.Model.Category.Name, &page.Model.Category.Tone, &total, &held, &page.Serial, &page.Condition,
		&page.Place, &number, &ticketStatus, &late, &requester)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminAsset{}, ErrNotFound
	}

	if err != nil {
		return AdminAsset{}, fmt.Errorf("read the asset %w", err)
	}

	page.Model.CategoryID = page.Model.Category.ID
	page.Model.Available = max(total-held, 0)

	if number != nil && ticketStatus != nil {
		effective := *ticketStatus
		if late != nil && *late {
			effective = "terlambat"
		}

		page.RelatedTicket = &RelatedTicket{Number: *number, EffectiveStatus: effective, RequesterName: requester}
	}

	return page, nil
}
