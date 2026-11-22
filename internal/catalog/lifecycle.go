package catalog

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// steps lists the lifecycle steps that docs/data-model.md allows, by the state that the unit leaves.
var steps = map[string][]string{
	LifecycleActive:      {LifecycleMaintenance, LifecycleLost, LifecycleRetired},
	LifecycleMaintenance: {LifecycleActive, LifecycleLost, LifecycleRetired},
	LifecycleLost:        {LifecycleActive, LifecycleRetired},
	LifecycleRetired:     {LifecycleActive},
}

// eventAction is the action of the history for a step to a lifecycle.
var eventAction = map[string]string{
	LifecycleActive:      "reactivated",
	LifecycleMaintenance: "maintenance",
	LifecycleLost:        "lost",
	LifecycleRetired:     "retired",
}

// ChangeLifecycle moves a unit along one allowed step and writes the event. The transaction locks the model first and then the unit,
// so it never runs together with a handover or another change of the same stock.
func (s *Store) ChangeLifecycle(ctx context.Context, tag, adminID string, in LifecycleChange) (Unit, error) {
	if !ValidTag(tag) {
		return Unit{}, ErrNotFound
	}

	clean, err := in.check()
	if err != nil {
		return Unit{}, err
	}

	var out Unit

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		id, from, model, err := lockUnit(ctx, tx, tag)
		if err != nil {
			return err
		}

		if !slices.Contains(steps[from], clean.Lifecycle) {
			return &RuleError{Kind: RuleStatusConflict, Message: "The unit cannot go from " + from + " to " + clean.Lifecycle + "."}
		}

		if clean.Lifecycle == LifecycleActive && model.Retired {
			return &RuleError{Kind: RuleStatusConflict, Message: "The model is retired, so its units stay out of use."}
		}

		if from == LifecycleActive {
			if err := refuseLeavingActive(ctx, tx, id, model.ID); err != nil {
				return err
			}
		}

		note := clean.Note
		if clean.Lifecycle == LifecycleActive {
			note = nil
		}

		if _, err := tx.Exec(ctx, `update units set lifecycle = $2, lifecycle_note = $3 where id = $1::uuid`, id, clean.Lifecycle, note); err != nil {
			return fmt.Errorf("change the lifecycle %w", err)
		}

		if err := addEvent(ctx, tx, id, eventAction[clean.Lifecycle], adminID, clean.Note); err != nil {
			return err
		}

		out, err = unitByTag(ctx, tx, tag)

		return err
	})

	return out, err
}
