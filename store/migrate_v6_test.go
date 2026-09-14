// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// legacyV5 builds a database in the shape ClawEh wrote before v6: memories
// carry source and priority columns, and some sit in the removed "review"
// status. Returns its path.
//
// This is the shape of the seven live production databases, so the migration
// has to handle exactly this and not an idealised version of it.
func legacyV5(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()

	stmts := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(5, 1)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta(key,value) VALUES('stable_rev','7')`,
		`CREATE TABLE domains (
		   id TEXT PRIMARY KEY, agent_id TEXT NOT NULL DEFAULT '', session_key TEXT NOT NULL DEFAULT '',
		   type TEXT NOT NULL DEFAULT '0', name TEXT NOT NULL, status TEXT NOT NULL,
		   version INTEGER NOT NULL DEFAULT 1, summary TEXT NOT NULL DEFAULT '',
		   state_json TEXT NOT NULL DEFAULT '{}', schema_name TEXT NOT NULL DEFAULT '',
		   schema_version INTEGER NOT NULL DEFAULT 1, last_active_at INTEGER,
		   triggers TEXT NOT NULL DEFAULT '', keyword_triggers TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, archived_at INTEGER)`,
		`INSERT INTO domains(id,name,status,created_at,updated_at,type)
		   VALUES('dGEN','General','active',1,1,'1')`,
		`CREATE TABLE memories (
		   id TEXT PRIMARY KEY, domain_id TEXT NOT NULL, type TEXT NOT NULL, text TEXT NOT NULL,
		   status TEXT NOT NULL, confidence REAL NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
		   source TEXT NOT NULL, origin TEXT NOT NULL DEFAULT 'chat', source_session TEXT,
		   source_seq_start INTEGER, source_seq_end INTEGER, supersedes_memory_id TEXT,
		   retire_reason TEXT, file_ref TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO memories(id,domain_id,type,text,status,confidence,priority,source,origin,created_at,updated_at)
		   VALUES ('hACT','dGEN','fact','an active fact','active',0.95,3,'user_explicit','consolidation',1,1),
		          ('hREV','dGEN','fact','an unconfirmed guess','review',0.90,0,'assistant_inferred','consolidation',1,1),
		          ('hRET','dGEN','fact','a retired fact','retired',0.90,0,'user_explicit','consolidation',1,1)`,
		`CREATE TABLE memory_events (
		   id TEXT PRIMARY KEY, event_type TEXT NOT NULL, domain_id TEXT, memory_id TEXT,
		   old_json TEXT, new_json TEXT, reason TEXT NOT NULL DEFAULT '',
		   evidence_json TEXT NOT NULL DEFAULT '{}', actor TEXT NOT NULL, model TEXT,
		   prompt_hash TEXT, created_at INTEGER NOT NULL)`,
		`CREATE TABLE consolidation_runs (
		   id TEXT PRIMARY KEY, trigger TEXT NOT NULL, model TEXT NOT NULL, seq_start INTEGER,
		   seq_end INTEGER, input_tokens INTEGER, output_tokens INTEGER, status TEXT NOT NULL,
		   ops_applied INTEGER NOT NULL DEFAULT 0, error TEXT, prompt_hash TEXT,
		   started_at INTEGER NOT NULL, finished_at INTEGER)`,
		`CREATE TABLE consolidation_state (
		   archive_path TEXT PRIMARY KEY, consolidated_seq INTEGER NOT NULL DEFAULT 0,
		   meaningful_count INTEGER NOT NULL DEFAULT 0, last_run_at INTEGER,
		   last_seq_seen INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE worker_leases (
		   name TEXT PRIMARY KEY, owner TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
	}
	for _, q := range stmts {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("seed legacy db (%.40s...): %v", q, err)
		}
	}
	return path
}

// The v6 migration drops the two dead columns and promotes every memory left in
// the removed review status.
//
// Review meant "written but unconfirmed" and the confirmation never came: those
// memories were excluded from the prompt, from search AND from the WebUI, so
// they were unreachable by anything. Making them active is what removing the
// gate means — leaving them behind in a status nothing understands would strand
// them permanently.
func TestMigrationV6DropsColumnsAndPromotesReview(t *testing.T) {
	path := legacyV5(t, "legacy.cogmem.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v5 database: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	have, err := s.columnSet(ctx, "memories")
	if err != nil {
		t.Fatalf("columnSet: %v", err)
	}
	for _, col := range []string{"source", "priority"} {
		if have[col] {
			t.Errorf("column %q survived the migration", col)
		}
	}

	// The unconfirmed memory is now active and reachable.
	m, err := s.GetMemory(ctx, s.DB(), "hREV")
	if err != nil {
		t.Fatalf("get promoted memory: %v", err)
	}
	if m.Status != StatusActive {
		t.Errorf("review memory has status %q, want active", m.Status)
	}

	// A retired memory is NOT swept up by the promotion: it was deliberately
	// retired, which is a different thing from never having been confirmed.
	if r, err := s.GetMemory(ctx, s.DB(), "hRET"); err != nil || r.Status != StatusRetired {
		t.Errorf("retired memory = %q (err %v), want it left alone", r.Status, err)
	}

	// Everything else survived.
	if a, err := s.GetMemory(ctx, s.DB(), "hACT"); err != nil || a.Text != "an active fact" {
		t.Errorf("active memory lost: %+v err=%v", a, err)
	}

	if v, err := s.recordedVersion(ctx); err != nil || v != schemaVersion {
		t.Errorf("recorded version = %d (err %v), want %d", v, err, schemaVersion)
	}
}

// Opening twice must not migrate twice, and must not take a second snapshot —
// the snapshot is only worth anything if it holds the state from BEFORE the
// first migration ran.
func TestMigrationIsIdempotent(t *testing.T) {
	path := legacyV5(t, "twice.cogmem.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s.Close()

	snap := path + ".pre-v5.db"
	first, err := os.Stat(snap)
	if err != nil {
		t.Fatalf("no snapshot after the first open: %v", err)
	}

	// Change something, then reopen. If the migration ran again it would
	// promote this back to active.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	ctx := context.Background()
	if err := s2.RetireMemory(ctx, s2.DB(), "hREV", "no longer true"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	_ = s2.Close()

	s3, err := Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer func() { _ = s3.Close() }()
	m, err := s3.GetMemory(ctx, s3.DB(), "hREV")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.Status != StatusRetired {
		t.Errorf("status = %q after reopen: the migration ran a second time and "+
			"undid a deliberate retire", m.Status)
	}

	second, err := os.Stat(snap)
	if err != nil {
		t.Fatalf("snapshot disappeared: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) || first.Size() != second.Size() {
		t.Error("the snapshot was rewritten on a later open, so it no longer holds " +
			"the pre-migration state")
	}
}

// The snapshot is the safety net for an upgrade nobody prepared for, so it has
// to be a real, openable database holding the pre-migration content.
func TestPreMigrationSnapshotIsUsable(t *testing.T) {
	path := legacyV5(t, "snap.cogmem.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()

	snap, err := sql.Open("sqlite", path+".pre-v5.db")
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = snap.Close() }()

	// It holds the OLD shape: the review row is still review, and the dropped
	// columns are still there. That is the point — it is what you go back to.
	var status string
	if err := snap.QueryRow(`SELECT status FROM memories WHERE id='hREV'`).Scan(&status); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if status != "review" {
		t.Errorf("snapshot has status %q, want the pre-migration 'review'", status)
	}
	var n int
	if err := snap.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('memories') WHERE name='source'`).Scan(&n); err != nil {
		t.Fatalf("inspect snapshot columns: %v", err)
	}
	if n != 1 {
		t.Error("snapshot is missing the source column, so it is not the pre-migration state")
	}
	// Row counts match the fixture exactly: one domain, three memories in the
	// three statuses the fixture wrote, and the recorded version is still 5.
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM domains`:                         1,
		`SELECT COUNT(*) FROM memories`:                        3,
		`SELECT COUNT(*) FROM memories WHERE status='active'`:  1,
		`SELECT COUNT(*) FROM memories WHERE status='review'`:  1,
		`SELECT COUNT(*) FROM memories WHERE status='retired'`: 1,
		`SELECT MAX(version) FROM schema_migrations`:           5,
	} {
		var got int
		if err := snap.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != want {
			t.Errorf("%s = %d in the snapshot, want %d", q, got, want)
		}
	}
	var integrity string
	if err := snap.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("integrity_check = %q err=%v, want ok", integrity, err)
	}
	_ = snap.Close()

	// The snapshot can be opened as a store. Doing so migrates IT (it is a v5
	// database), which is what recovery needs — the data comes through — and
	// must NOT write a nested <snap>.pre-v5.db beside it.
	rs, err := Open(path + ".pre-v5.db")
	if err != nil {
		t.Fatalf("open snapshot as a store: %v", err)
	}
	defer func() { _ = rs.Close() }()
	ctx := context.Background()
	for id, want := range map[string]Status{"hACT": StatusActive, "hREV": StatusActive, "hRET": StatusRetired} {
		m, err := rs.GetMemory(ctx, rs.DB(), id)
		if err != nil {
			t.Errorf("%s missing from the migrated snapshot: %v", id, err)
			continue
		}
		if m.Status != want {
			t.Errorf("%s status = %q after migrating the snapshot, want %q", id, m.Status, want)
		}
	}
	if doms, _ := rs.ListDomains(ctx, rs.DB()); len(doms) != 1 || doms[0].Name != "General" || !doms[0].Sticky() {
		t.Errorf("migrated snapshot domains = %+v, want the one sticky General", doms)
	}
	if _, err := os.Stat(path + ".pre-v5.db.pre-v5.db"); err == nil {
		t.Error("opening the snapshot as a store wrote a nested snapshot beside it")
	}
}

// A database being created is not an upgrade and must not leave a snapshot
// beside itself — every new session would otherwise write two files.
func TestFreshDatabaseTakesNoSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".db" && e.Name() != "fresh.cogmem.db" {
			t.Errorf("a fresh database left a snapshot behind: %s", e.Name())
		}
	}
}
