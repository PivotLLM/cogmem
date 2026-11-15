// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// A transaction that reads and then writes must not fail because another
// connection committed in between. With a deferred BEGIN the read takes a
// snapshot, the other commit makes it stale, and the upgrade to a write lock
// fails at once with SQLITE_BUSY — busy_timeout cannot help a stale snapshot.
// WithTx therefore takes the write lock up front, so the other writer waits.
func TestWithTxReadThenWriteSurvivesConcurrentCommit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Billing"})
	if err != nil {
		t.Fatal(err)
	}

	otherDone := make(chan error, 1)
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, gerr := s.GetDomain(ctx, tx, d.ID, false); gerr != nil {
			return gerr
		}
		// Another connection writes while this transaction is open.
		go func() {
			_, xerr := s.DB().ExecContext(ctx, `UPDATE domains SET summary='other' WHERE id=?`, d.ID)
			otherDone <- xerr
		}()
		// Let it commit if it can; with the write lock held it waits instead.
		select {
		case xerr := <-otherDone:
			otherDone <- xerr
		case <-time.After(300 * time.Millisecond):
		}
		summary := "mine"
		return s.UpdateDomain(ctx, tx, d.ID, UpdateDomainParams{Summary: &summary})
	})
	if err != nil {
		t.Fatalf("read-then-write transaction failed: %v", err)
	}
	if xerr := <-otherDone; xerr != nil {
		t.Fatalf("concurrent writer failed: %v", xerr)
	}
	got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if got.Summary != "other" {
		t.Fatalf("summary = %q, want the later writer's", got.Summary)
	}
}
