// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// InboxStateKey is the consolidation_state row that tracks the inbox
// watermark. The table was once keyed by archive path, when consolidation read
// the session archive; the inbox is the store's own copy of the conversation,
// so there is exactly one row now.
const InboxStateKey = "inbox"

// InboxMessage is one conversation message waiting to be consolidated. Seq is
// the host's transcript sequence number, so evidence cited by a consolidation
// run refers to the same numbers the rest of the system uses.
type InboxMessage struct {
	Seq       int64
	Role      string
	Text      string
	CreatedAt time.Time
}

// AppendInbox records a message for the next consolidation run. Idempotent on
// seq: a replayed message (a retried turn) leaves the first copy in place.
func (s *Store) AppendInbox(ctx context.Context, q DBTX, seq int64, role, text string) error {
	_, err := q.ExecContext(ctx,
		`INSERT OR IGNORE INTO inbox(seq, role, text, created_at) VALUES(?,?,?,?)`,
		seq, role, text, now())
	return err
}

// InboxBounds returns the lowest and highest seq held, or (0, 0) when empty.
func (s *Store) InboxBounds(ctx context.Context, q DBTX) (minSeq, maxSeq int64, err error) {
	var lo, hi sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MIN(seq), MAX(seq) FROM inbox`).Scan(&lo, &hi); err != nil {
		return 0, 0, err
	}
	return lo.Int64, hi.Int64, nil
}

// InboxRange returns messages with seq in [minSeq, maxSeq], ascending.
func (s *Store) InboxRange(ctx context.Context, q DBTX, minSeq, maxSeq int64) ([]InboxMessage, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT seq, role, text, created_at FROM inbox WHERE seq >= ? AND seq <= ? ORDER BY seq`,
		minSeq, maxSeq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []InboxMessage
	for rows.Next() {
		var m InboxMessage
		var created int64
		if err := rows.Scan(&m.Seq, &m.Role, &m.Text, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = timeUnix(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// InboxCount returns how many messages are waiting.
func (s *Store) InboxCount(ctx context.Context, q DBTX) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox`).Scan(&n)
	return n, err
}

// DeleteInboxThrough removes every message with seq <= uptoSeq. Called after a
// run has advanced the watermark past them; the inbox holds at most one
// consolidation window.
func (s *Store) DeleteInboxThrough(ctx context.Context, q DBTX, uptoSeq int64) (int, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM inbox WHERE seq <= ?`, uptoSeq)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// InboxBackfilled reports whether the host has already copied the messages
// that predate the inbox (archived before this store learned to keep its own
// copy) into it. The host checks it once per store and sets it when done.
func (s *Store) InboxBackfilled(ctx context.Context) (bool, error) {
	v, err := getMetaInt(ctx, s.db, "inbox_backfilled")
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // never recorded: the backfill has not run
	}
	if err != nil {
		return false, err
	}
	return v > 0, nil
}

// SetInboxBackfilled records that the one-time backfill has run.
func (s *Store) SetInboxBackfilled(ctx context.Context) error {
	return setMetaInt(ctx, s.db, "inbox_backfilled", 1)
}
