// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"fmt"
)

// PurgeExpiredEvents deletes active event memories older than days, and returns
// how many went.
//
// Events are things that happened at a point in time, and they stop being
// useful long before they stop accumulating — an agent recording an hourly
// "nothing changed" note reached 300 of them. They are already kept out of the
// prompt, so the cost of holding them forever is not context but the database
// and the search results, and neither improves with age.
//
// Only TypeEvent is ever deleted by age. A fact, preference, rule or
// operational note is permanent, so no retention policy can silently drop a
// standing instruction — the model chooses the type, and that choice is what
// decides whether a memory is temporary. That is deliberately the only knob:
// giving the model a per-memory retention argument as well would reopen exactly
// the multi-field guesswork the type redesign closed.
//
// days <= 0 is a no-op, which is how "keep forever" is expressed.
func (s *Store) PurgeExpiredEvents(ctx context.Context, q DBTX, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := now() - int64(days)*86400
	res, err := q.ExecContext(ctx,
		`DELETE FROM memories WHERE type=? AND status=? AND created_at < ?`,
		string(TypeEvent), string(StatusActive), cutoff)
	if err != nil {
		return 0, fmt.Errorf("cogmem: purge expired events: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// A domain reports how many events it holds, and that line is part of
		// the cached stable block — so removing events changes what the
		// assistant sees even though no event was ever in the prompt itself.
		if err := bumpStableRev(ctx, q); err != nil {
			return int(n), err
		}
	}
	return int(n), nil
}

// PurgeRetiredMemories deletes memories retired more than days ago, and returns
// how many went.
//
// Retiring takes a memory out of use but leaves the row behind, so a store that
// retires steadily grows forever while showing nothing for it. Age is measured
// from updated_at — when the memory was retired — not from when it was created,
// because a memory written a year ago and retired yesterday has only just
// stopped being used.
//
// days <= 0 is a no-op.
func (s *Store) PurgeRetiredMemories(ctx context.Context, q DBTX, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := now() - int64(days)*86400
	res, err := q.ExecContext(ctx,
		`DELETE FROM memories WHERE status=? AND updated_at < ?`,
		string(StatusRetired), cutoff)
	if err != nil {
		return 0, fmt.Errorf("cogmem: purge retired memories: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
