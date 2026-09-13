// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A run has two kinds of message and they must not share a column.
//
// Error means the run FAILED. Note means the run SUCCEEDED and something is
// worth reporting — an auto-repaired contract deviation, in practice. They were
// one field, so the consolidator wrote its "auto-repaired: …" note into Error,
// the API served it as `error`, and the memory page painted a successful run in
// red. Anyone reading the page reasonably concluded their memory was broken.
func TestRunNoteIsSeparateFromError(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	want := Run{
		Trigger:    "manual",
		Model:      "test-model",
		Status:     "ok",
		OpsApplied: 5,
		Note:       "auto-repaired: memory_ops[0]: inferred item active→review",
	}
	if err := s.RecordRun(ctx, s.DB(), want); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	got, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("LastRun: %v (ok=%v)", err, ok)
	}
	if got.Note != want.Note {
		t.Errorf("Note = %q, want %q", got.Note, want.Note)
	}
	if got.Error != "" {
		t.Errorf("Error = %q on a successful run, want empty — a note must not "+
			"land in the error field", got.Error)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
}

// A genuine failure still records its reason in Error, and carries no note.
func TestRunErrorSurvivesTheSplit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if err := s.RecordRun(ctx, s.DB(), Run{
		Trigger: "idle",
		Model:   "test-model",
		Status:  "aborted",
		Error:   `memory_ops[0]: unknown domain "dCWY8"`,
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	got, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("LastRun: %v (ok=%v)", err, ok)
	}
	if got.Error != `memory_ops[0]: unknown domain "dCWY8"` {
		t.Errorf("Error = %q, want the abort reason", got.Error)
	}
	if got.Note != "" {
		t.Errorf("Note = %q on a failed run, want empty", got.Note)
	}
}

// The seven live databases predate the note column, so Open must add it rather
// than failing to read them. Builds a consolidation_runs table WITHOUT the
// column, exactly as an older ClawEh created it, then opens it normally.
func TestOpenAddsNoteColumnToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.cogmem.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE consolidation_runs (
		  id            TEXT PRIMARY KEY,
		  trigger       TEXT NOT NULL,
		  model         TEXT NOT NULL,
		  seq_start     INTEGER,
		  seq_end       INTEGER,
		  input_tokens  INTEGER,
		  output_tokens INTEGER,
		  status        TEXT NOT NULL,
		  ops_applied   INTEGER NOT NULL DEFAULT 0,
		  error         TEXT,
		  prompt_hash   TEXT,
		  started_at    INTEGER NOT NULL,
		  finished_at   INTEGER
		)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	// A historical row, as prod has: a note stranded in the error column.
	if _, err := raw.Exec(`INSERT INTO consolidation_runs
		(id, trigger, model, status, ops_applied, error, started_at)
		VALUES ('r1','manual','old-model','ok',5,'auto-repaired: memory_ops[0]: inferred item active→review',1)`,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-note database: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	got, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("LastRun after migration: %v (ok=%v)", err, ok)
	}
	// The stranded note is moved across, so the memory page stops showing a
	// successful run in red immediately rather than after the next run.
	if got.Note == "" {
		t.Error("the stranded auto-repair note was not moved into note")
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty — the note should have moved out of it", got.Error)
	}

	// And a NEW run on the migrated database uses the new column properly.
	if err := s.RecordRun(ctx, s.DB(), Run{
		Trigger: "manual", Model: "m", Status: "ok", OpsApplied: 1,
		Note: "auto-repaired: memory_ops[0]: inferred item active→review",
	}); err != nil {
		t.Fatalf("RecordRun after migration: %v", err)
	}
	got, _, err = s.LastRun(ctx, s.DB())
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if got.Note == "" || got.Error != "" {
		t.Errorf("after migration: Note=%q Error=%q, want the note set and the error empty",
			got.Note, got.Error)
	}
}

// The backfill must move ONLY auto-repair notes on successful runs. A real
// failure reason has to stay in error, or the migration would quietly hide
// genuine problems — the opposite of the bug it fixes.
func TestMigrationLeavesRealFailuresAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy2.cogmem.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE consolidation_runs (
		  id TEXT PRIMARY KEY, trigger TEXT NOT NULL, model TEXT NOT NULL,
		  seq_start INTEGER, seq_end INTEGER, input_tokens INTEGER,
		  output_tokens INTEGER, status TEXT NOT NULL,
		  ops_applied INTEGER NOT NULL DEFAULT 0, error TEXT,
		  prompt_hash TEXT, started_at INTEGER NOT NULL, finished_at INTEGER
		)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	rows := []struct{ id, status, errText string }{
		// Genuine failures — must be untouched.
		{"r1", "aborted", `memory_ops[0]: unknown domain "dCWY8"`},
		{"r2", "invalid_json", "unexpected end of JSON input"},
		{"r3", "error", "cogmem: memory models returned no usable response"},
		// A note stranded on a successful run — must move.
		{"r4", "ok", "auto-repaired: memory_ops[0]: inferred item active→review"},
		// An 'ok' run whose message is NOT an auto-repair (the mark-consolidated
		// failure) — a real error on a successful run, so it stays put.
		{"r5", "ok", "mark consolidated: disk full"},
	}
	for i, r := range rows {
		if _, err := raw.Exec(
			`INSERT INTO consolidation_runs (id,trigger,model,status,ops_applied,error,started_at)
			 VALUES (?,'manual','m',?,0,?,?)`, r.id, r.status, r.errText, i+1); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	got := map[string][2]string{}
	rs, err := s.DB().QueryContext(context.Background(),
		`SELECT id, COALESCE(error,''), COALESCE(note,'') FROM consolidation_runs`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rs.Close() }()
	for rs.Next() {
		var id, e, n string
		if err := rs.Scan(&id, &e, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = [2]string{e, n}
	}

	for _, id := range []string{"r1", "r2", "r3", "r5"} {
		if got[id][0] == "" {
			t.Errorf("%s: error was cleared, but it is a genuine failure reason", id)
		}
		if got[id][1] != "" {
			t.Errorf("%s: note = %q, want empty", id, got[id][1])
		}
	}
	if got["r4"][1] == "" || got["r4"][0] != "" {
		t.Errorf("r4: error=%q note=%q, want the note moved across",
			got["r4"][0], got["r4"][1])
	}
}
