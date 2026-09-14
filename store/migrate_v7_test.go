// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyV6WithArchiveState builds a v6-shaped database whose consolidation
// state is keyed by archive path, as every store written before v7 was.
func legacyV6WithArchiveState(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent_alice_main.cogmem.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(6, 1)`,
		`CREATE TABLE consolidation_state (
		   archive_path TEXT PRIMARY KEY, consolidated_seq INTEGER NOT NULL DEFAULT 0,
		   last_seen_seq INTEGER NOT NULL DEFAULT 0, meaningful_count INTEGER NOT NULL DEFAULT 0,
		   last_run_at INTEGER, updated_at INTEGER NOT NULL)`,
		`INSERT INTO consolidation_state VALUES
		   ('/old/sessions/a.archive.db', 40, 41, 3, NULL, 1),
		   ('/new/sessions/a.archive.db', 120, 125, 7, 5, 2)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return path
}

// TestMigrationV7CollapsesWatermark verifies the archive-path rows fold into
// the single inbox row, keeping the highest watermark: that is how far memory
// already reflects the conversation, and where the host's backfill resumes.
func TestMigrationV7CollapsesWatermark(t *testing.T) {
	s, err := Open(legacyV6WithArchiveState(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	st, err := s.GetState(ctx, s.DB(), InboxStateKey)
	if err != nil {
		t.Fatal(err)
	}
	if st.ConsolidatedSeq != 120 || st.LastSeenSeq != 125 {
		t.Fatalf("inbox state = %+v, want consolidated 120 / last seen 125", st)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM consolidation_state`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("consolidation_state rows = %d, want the single inbox row", rows)
	}
	// The inbox exists and is empty until the host backfills or observes.
	if n, err := s.InboxCount(ctx, s.DB()); err != nil || n != 0 {
		t.Fatalf("inbox count = %d err=%v, want 0", n, err)
	}
	if done, _ := s.InboxBackfilled(ctx); done {
		t.Fatal("fresh migration must leave the backfill flag unset")
	}
}

// TestFreshStoreHasNoWatermark: a store that never consolidated must not gain
// a phantom watermark from the migration.
func TestFreshStoreHasNoWatermark(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.cogmem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st, err := s.GetState(context.Background(), s.DB(), InboxStateKey)
	if err != nil {
		t.Fatal(err)
	}
	if st.ConsolidatedSeq != 0 {
		t.Fatalf("fresh store watermark = %d, want 0", st.ConsolidatedSeq)
	}
}

func TestInbox_RoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "inbox.cogmem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	for seq, text := range map[int64]string{3: "three", 1: "one", 2: "two"} {
		if err := s.AppendInbox(ctx, s.DB(), seq, "user", text); err != nil {
			t.Fatal(err)
		}
	}
	lo, hi, err := s.InboxBounds(ctx, s.DB())
	if err != nil || lo != 1 || hi != 3 {
		t.Fatalf("bounds = %d..%d err=%v", lo, hi, err)
	}
	rows, err := s.InboxRange(ctx, s.DB(), 2, 3)
	if err != nil || len(rows) != 2 || rows[0].Seq != 2 || rows[1].Text != "three" {
		t.Fatalf("range = %+v err=%v", rows, err)
	}
	if n, err := s.DeleteInboxThrough(ctx, s.DB(), 2); err != nil || n != 2 {
		t.Fatalf("delete through 2 = %d err=%v", n, err)
	}
	if n, _ := s.InboxCount(ctx, s.DB()); n != 1 {
		t.Fatalf("count after delete = %d, want 1", n)
	}
	if err := s.SetInboxBackfilled(ctx); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.InboxBackfilled(ctx); !done {
		t.Fatal("backfill flag did not stick")
	}
}

// TestMigrationV8DropsDomainOwnerColumns: a store from before v8 loses the
// agent_id and session_key columns on open, and keeps its domains.
func TestMigrationV8DropsDomainOwnerColumns(t *testing.T) {
	s, err := Open(legacyV5(t, "v5.cogmem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cols, err := s.columnSet(context.Background(), "domains")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"agent_id", "session_key"} {
		if cols[c] {
			t.Fatalf("domains.%s still present after migration", c)
		}
	}
	ctx := context.Background()
	g, err := s.DomainByName(ctx, s.DB(), "General")
	if err != nil {
		t.Fatalf("General domain lost in migration: %v", err)
	}
	if g.ID != "dGEN" || !g.Sticky() || g.Status != StatusActive {
		t.Fatalf("General = %+v, want the seeded sticky active dGEN", g)
	}
	// Everything else in the fixture came through too: all three memories,
	// the review one promoted, the recorded version current, stable_rev kept.
	if doms, _ := s.ListDomains(ctx, s.DB()); len(doms) != 1 {
		t.Errorf("domains = %d, want 1", len(doms))
	}
	all, err := s.ListMemories(ctx, s.DB(), "dGEN")
	if err != nil || len(all) != 3 {
		t.Fatalf("memories = %d err=%v, want 3", len(all), err)
	}
	for id, want := range map[string]Status{"hACT": StatusActive, "hREV": StatusActive, "hRET": StatusRetired} {
		m, err := s.GetMemory(ctx, s.DB(), id)
		if err != nil || m.Status != want {
			t.Errorf("%s = %q err=%v, want %q", id, m.Status, err, want)
		}
	}
	if v, _ := s.recordedVersion(ctx); v != schemaVersion {
		t.Errorf("recorded version = %d, want %d", v, schemaVersion)
	}
	if rev, _ := s.StableRev(ctx); rev != 7 {
		t.Errorf("stable_rev = %d, want the fixture's 7", rev)
	}
}
