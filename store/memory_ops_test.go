// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// SetMemoryType: an unknown type is refused; the same type is a no-op that
// does not rebuild the stable block; a real change does, even in a non-sticky
// domain, because retyping to or from event moves the memory in or out of the
// prompt.
func TestSetMemoryType(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "P"})
	m := add(t, s, d.ID, TypeFact, "a trip log filed as a fact")

	if _, err := s.SetMemoryType(ctx, db, m.ID, MemoryType("observation")); err == nil ||
		err.Error() != `cogmem: invalid memory type "observation"` {
		t.Fatalf("invalid type err = %v", err)
	}
	if _, err := s.SetMemoryType(ctx, db, "hNOPE1", TypeRule); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}

	before, _ := s.StableRev(ctx)
	got, err := s.SetMemoryType(ctx, db, m.ID, TypeFact)
	if err != nil {
		t.Fatalf("same type: %v", err)
	}
	if got.Type != TypeFact || got.ID != m.ID {
		t.Fatalf("same type returned %+v", got)
	}
	if rev, _ := s.StableRev(ctx); rev != before {
		t.Fatalf("stable_rev %d -> %d on a no-op retype, want unchanged", before, rev)
	}

	got, err = s.SetMemoryType(ctx, db, m.ID, TypeEvent)
	if err != nil {
		t.Fatalf("retype: %v", err)
	}
	if got.Type != TypeEvent {
		t.Fatalf("returned type = %q, want event", got.Type)
	}
	if rev, _ := s.StableRev(ctx); rev != before+1 {
		t.Fatalf("stable_rev %d -> %d on retype, want +1", before, rev)
	}
	if re, _ := s.GetMemory(ctx, db, m.ID); re.Type != TypeEvent {
		t.Fatalf("stored type = %q, want event", re.Type)
	}
	if prompt, _ := s.ListPromptMemories(ctx, db, d.ID); len(prompt) != 0 {
		t.Fatalf("retyped event still in the prompt path: %+v", prompt)
	}
	if n, _ := s.CountEvents(ctx, db, d.ID); n != 1 {
		t.Fatalf("event count = %d, want 1", n)
	}
}

// RestoreMemory is the inverse of RetireMemory: status back to active, reason
// cleared, stable block rebuilt when the memory is sticky content. Restoring
// an active memory is a no-op; an unknown id is ErrNotFound.
func TestRestoreMemory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	gen, _ := s.GeneralDomain(ctx, db) // sticky
	m := add(t, s, gen.ID, TypeFact, "restore me")
	if err := s.RetireMemory(ctx, db, m.ID, "wrong"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if r, _ := s.GetMemory(ctx, db, m.ID); r.Status != StatusRetired || r.RetireReason == nil || *r.RetireReason != "wrong" {
		t.Fatalf("after retire: %+v", r)
	}

	before, _ := s.StableRev(ctx)
	if err := s.RestoreMemory(ctx, db, m.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r, _ := s.GetMemory(ctx, db, m.ID)
	if r.Status != StatusActive {
		t.Errorf("status = %q, want active", r.Status)
	}
	if r.RetireReason != nil {
		t.Errorf("retire_reason = %q, want cleared", *r.RetireReason)
	}
	if rev, _ := s.StableRev(ctx); rev != before+1 {
		t.Errorf("stable_rev %d -> %d on restore into a sticky domain, want +1", before, rev)
	}

	// Already active: nothing happens.
	if err := s.RestoreMemory(ctx, db, m.ID); err != nil {
		t.Fatalf("restore active: %v", err)
	}
	if rev, _ := s.StableRev(ctx); rev != before+1 {
		t.Errorf("stable_rev bumped by a no-op restore: %d", rev)
	}
	if err := s.RestoreMemory(ctx, db, "hNOPE1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
}

// Vacuum runs outside a transaction and leaves the store usable.
func TestVacuumRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Gone"})
	add(t, s, d.ID, TypeFact, "to be vacuumed away")
	if err := s.DeleteDomain(ctx, s.DB(), d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if _, err := s.GeneralDomain(ctx, s.DB()); err != nil {
		t.Fatalf("store unusable after vacuum: %v", err)
	}
}

// Open applies its pragmas and options: WAL, foreign keys, the default busy
// timeout, an override, and the rule that a non-positive override keeps the
// default. Path reports what was opened.
func TestOpenPragmasAndOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opts.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.Path() != path {
		t.Errorf("Path() = %q, want %q", s.Path(), path)
	}
	if mode := pragmaStr(t, s, "journal_mode"); mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	if fk := pragmaInt(t, s, "foreign_keys"); fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	if ms := pragmaInt(t, s, "busy_timeout"); ms != 5000 {
		t.Errorf("default busy_timeout = %d, want 5000", ms)
	}
	_ = s.Close()

	for _, tc := range []struct {
		name string
		opts []Option
		want int64
	}{
		{"zero keeps default", []Option{WithBusyTimeout(0)}, 5000},
		{"negative keeps default", []Option{WithBusyTimeout(-time.Second)}, 5000},
		{"override applies", []Option{WithBusyTimeout(1500 * time.Millisecond)}, 1500},
		{"last positive wins", []Option{WithBusyTimeout(time.Second), WithBusyTimeout(250 * time.Millisecond)}, 250},
	} {
		s, err := Open(path, tc.opts...)
		if err != nil {
			t.Fatalf("%s: open: %v", tc.name, err)
		}
		if ms := pragmaInt(t, s, "busy_timeout"); ms != tc.want {
			t.Errorf("%s: busy_timeout = %d, want %d", tc.name, ms, tc.want)
		}
		_ = s.Close()
	}
}

func pragmaStr(t *testing.T, s *Store, name string) string {
	t.Helper()
	var v string
	if err := s.DB().QueryRow(`PRAGMA ` + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func pragmaInt(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	var v int64
	if err := s.DB().QueryRow(`PRAGMA ` + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// WithTx: an error from fn rolls back everything fn wrote, even though the
// writes were visible inside the transaction.
func TestWithTxRollsBackOnError(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	sentinel := errors.New("boom")
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.AppendInbox(ctx, tx, 1, "user", "hello"); err != nil {
			return err
		}
		if n, err := s.InboxCount(ctx, tx); err != nil || n != 1 {
			t.Errorf("inside tx: count = %d err=%v, want 1", n, err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx err = %v, want the callback's error", err)
	}
	if n, _ := s.InboxCount(ctx, s.DB()); n != 0 {
		t.Fatalf("inbox count after rollback = %d, want 0", n)
	}
}

// WithTx: a panic in fn propagates to the caller AND rolls the transaction
// back, leaving the store usable.
func TestWithTxRollsBackOnPanic(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = s.WithTx(ctx, func(tx *sql.Tx) error {
			if err := s.AppendInbox(ctx, tx, 1, "user", "hello"); err != nil {
				t.Errorf("append inside tx: %v", err)
			}
			panic("boom")
		})
	}()
	if recovered != "boom" {
		t.Fatalf("recovered %v, want the panic value to propagate", recovered)
	}
	if n, _ := s.InboxCount(ctx, s.DB()); n != 0 {
		t.Fatalf("inbox count after panic = %d, want 0 (rolled back)", n)
	}
	// No transaction left dangling: a plain write still goes through.
	if err := s.AppendInbox(ctx, s.DB(), 2, "user", "after"); err != nil {
		t.Fatalf("write after panic: %v", err)
	}
	if n, _ := s.InboxCount(ctx, s.DB()); n != 1 {
		t.Fatalf("inbox count = %d, want 1", n)
	}
}

// WithTx commits on success, so a write inside is visible afterwards.
func TestWithTxCommits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		return s.AppendInbox(ctx, tx, 7, "assistant", "committed")
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	rows, _ := s.InboxRange(ctx, s.DB(), 7, 7)
	if len(rows) != 1 || rows[0].Text != "committed" || rows[0].Role != "assistant" {
		t.Fatalf("committed row = %+v", rows)
	}
}
