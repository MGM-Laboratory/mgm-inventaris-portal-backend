package ticket

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

// CancelByGuest cancels a ticket in the status diajukan for a session that may read it. A ticket that is already cancelled is returned
// as it is and changes nothing, so a caller that lost the answer can send the request again. Any other status gives a status conflict.
// A ticket in the status diajukan holds no stock, so the cancel takes no lock on the models. The update names the expected status, so
// a decision that commits first leaves no row for it to change, and the read that follows names the status that won.
func (s *Store) CancelByGuest(ctx context.Context, viewable []string, number string) (Ticket, error) {
	if !ValidNumber(number) || len(viewable) == 0 {
		return Ticket{}, ErrNotFound
	}

	var out Ticket

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		current, err := s.readOne(ctx, tx, `t.number = $1 and t.id = any($2::text[]::uuid[])`, number, viewable)
		if err != nil {
			return err
		}

		if current.Status == StatusCancelled {
			out = current

			return nil
		}

		changed, err := s.moveStatus(ctx, tx, move{
			ID: current.ID, From: Sources(StatusCancelled, ActorGuest), To: StatusCancelled, Actor: ActorGuest,
		})
		if err != nil {
			return err
		}

		if !changed {
			// the status changed between the read and the update, so the new status says what the answer is
			latest, err := s.readOne(ctx, tx, `t.id = $1::uuid`, current.ID)
			if err != nil {
				return err
			}

			if latest.Status != StatusCancelled {
				return &catalog.RuleError{Kind: catalog.RuleStatusConflict, Message: "Only a ticket in the status " + StatusSubmitted + " can be cancelled."}
			}

			out = latest

			return nil
		}

		out, err = s.readOne(ctx, tx, `t.id = $1::uuid`, current.ID)

		return err
	})

	return out, err
}
