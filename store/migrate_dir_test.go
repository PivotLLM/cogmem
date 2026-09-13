// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Every store in the directory is migrated in one pass, so a schema change is a
// single observable event at startup rather than something that happens to each
// database whenever its session is next opened. Lazy migration left a store
// belonging to an agent nobody talked to that day on the old schema
// indefinitely, and gave an operator no moment they could call it done.
func TestMigrateDirUpgradesEveryStore(t *testing.T) {
	dir := t.TempDir()
	// Two legacy databases, plus a file that is not one.
	for _, name := range []string{"a.cogmem.db", "b.cogmem.db"} {
		src := legacyV5(t, name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read seed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write seed: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}

	results := MigrateDir(dir)
	if len(results) != 2 {
		t.Fatalf("migrated %d stores, want 2 (the .txt must be ignored)", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Path, r.Err)
			continue
		}
		if r.From != 5 {
			t.Errorf("%s: From = %d, want the pre-migration 5", r.Path, r.From)
		}
		if r.To != SchemaVersion() {
			t.Errorf("%s: To = %d, want %d", r.Path, r.To, SchemaVersion())
		}
		if !r.Migrated() {
			t.Errorf("%s: Migrated() false although %d -> %d", r.Path, r.From, r.To)
		}
		// The snapshot the operator would restore from is named after the
		// version the store came FROM, which differs per store in practice.
		snap := r.Path + ".pre-v5.db"
		if _, err := os.Stat(snap); err != nil {
			t.Errorf("%s: no snapshot at %s", r.Path, snap)
		}
	}
}

// Running it again must report no change: the migration has already happened,
// and a second snapshot taken afterwards would hold nothing worth restoring.
func TestMigrateDirIsQuietOnceCurrent(t *testing.T) {
	dir := t.TempDir()
	src := legacyV5(t, "a.cogmem.db")
	data, _ := os.ReadFile(src)
	path := filepath.Join(dir, "a.cogmem.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if first := MigrateDir(dir); len(first) != 1 || !first[0].Migrated() {
		t.Fatalf("first pass did not migrate: %+v", first)
	}
	second := MigrateDir(dir)
	if len(second) != 1 {
		t.Fatalf("second pass returned %d results, want 1", len(second))
	}
	if second[0].Migrated() {
		t.Errorf("second pass reports a migration: %d -> %d", second[0].From, second[0].To)
	}
	if second[0].From != SchemaVersion() || second[0].To != SchemaVersion() {
		t.Errorf("second pass: From=%d To=%d, want both %d",
			second[0].From, second[0].To, SchemaVersion())
	}
}

// A directory with no stores, or one that does not exist, is not an error: most
// agents are created before they have ever held a session.
func TestMigrateDirHandlesNothingToDo(t *testing.T) {
	if r := MigrateDir(t.TempDir()); len(r) != 0 {
		t.Errorf("empty dir returned %d results", len(r))
	}
	if r := MigrateDir(filepath.Join(t.TempDir(), "nope")); len(r) != 0 {
		t.Errorf("missing dir returned %d results", len(r))
	}
}

// A file that is not a usable database is reported rather than skipped
// silently, and does not stop the stores beside it from migrating — the point
// of doing this at startup is to learn about all of them at once.
func TestMigrateDirReportsABadStoreAndContinues(t *testing.T) {
	dir := t.TempDir()
	src := legacyV5(t, "good.cogmem.db")
	data, _ := os.ReadFile(src)
	if err := os.WriteFile(filepath.Join(dir, "good.cogmem.db"), data, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.cogmem.db"),
		[]byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	results := MigrateDir(dir)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	var good, bad *MigrationResult
	for i := range results {
		if filepath.Base(results[i].Path) == "good.cogmem.db" {
			good = &results[i]
		} else {
			bad = &results[i]
		}
	}
	if good == nil || !good.Migrated() {
		t.Errorf("the good store did not migrate alongside the bad one: %+v", good)
	}
	if bad == nil || bad.Err == nil {
		t.Errorf("the unusable store was not reported: %+v", bad)
	}
}
