// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The store in a directory is migrated in one call at load, so a schema
// change is a single observable event at startup rather than something that
// happens whenever the store is next opened. Lazy migration left a store
// belonging to an agent nobody talked to that day on the old schema
// indefinitely, and gave an operator no moment they could call it done.
func TestMigrateUpgradesTheStore(t *testing.T) {
	dir := t.TempDir()
	src := legacyV5(t, "seed.db")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if err := os.WriteFile(DBPath(dir), data, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	r := Migrate(dir)
	if r.Err != nil {
		t.Fatalf("%s: %v", r.Path, r.Err)
	}
	if !r.Migrated() || r.From != 5 || r.To != SchemaVersion() {
		t.Fatalf("result = %+v, want migrated from 5 to %d", r, SchemaVersion())
	}
	if _, err := os.Stat(DBPath(dir) + ".pre-v5.db"); err != nil {
		t.Fatalf("pre-migration snapshot missing: %v", err)
	}
	// A second call finds nothing to do.
	if r2 := Migrate(dir); r2.Migrated() || r2.Err != nil {
		t.Fatalf("second migrate = %+v, want no-op", r2)
	}
}

// A directory with no store yet is not an error.
func TestMigrateMissingStoreIsNoOp(t *testing.T) {
	r := Migrate(filepath.Join(t.TempDir(), "absent"))
	if r.Err != nil || r.From != 0 || r.To != 0 {
		t.Fatalf("result = %+v", r)
	}
}
