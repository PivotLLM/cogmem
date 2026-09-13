// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestMemoryFileRefRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, err := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Writing"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	m, err := s.AddMemory(ctx, db, AddMemoryParams{
		DomainID: d.ID, Type: TypeRule, Text: "Use my voice.",
		Status: StatusActive, Confidence: 0.9,
		FileRef: "  files/voice.md  ",
	})
	if err != nil {
		t.Fatalf("add memory: %v", err)
	}
	if m.FileRef != "files/voice.md" {
		t.Fatalf("file ref not stored trimmed: %q", m.FileRef)
	}
	got, err := s.GetMemory(ctx, db, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FileRef != "files/voice.md" {
		t.Fatalf("file ref did not round-trip: %q", got.FileRef)
	}
	list, err := s.ListMemories(ctx, db, d.ID, StatusActive)
	if err != nil || len(list) != 1 || list[0].FileRef != "files/voice.md" {
		t.Fatalf("list did not carry file ref: %+v err=%v", list, err)
	}
}

func TestMemoryWithoutFileRefIsEmpty(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Plain"})
	m, err := s.AddMemory(ctx, db, AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "no doc",
		Status: StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if m.FileRef != "" {
		t.Fatalf("expected empty file ref, got %q", m.FileRef)
	}
}

// TestSupersedeCarriesFileRefForward guards the consolidation path: a reworded
// memory must keep pointing at its document unless the replacement names one.
func TestSupersedeCarriesFileRefForward(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Writing"})
	old, _ := s.AddMemory(ctx, db, AddMemoryParams{
		DomainID: d.ID, Type: TypeRule, Text: "Use my voice.",
		Status: StatusActive, Confidence: 0.9,
		FileRef: "files/voice.md",
	})

	kept, err := s.SupersedeMemory(ctx, db, old.ID, AddMemoryParams{
		DomainID: d.ID, Type: TypeRule, Text: "Write in the user's voice.",
		Status: StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if kept.FileRef != "files/voice.md" {
		t.Fatalf("attachment lost on supersede: %q", kept.FileRef)
	}

	replaced, err := s.SupersedeMemory(ctx, db, kept.ID, AddMemoryParams{
		DomainID: d.ID, Type: TypeRule, Text: "New doc.",
		Status: StatusActive, Confidence: 0.9,
		FileRef: "files/voice-v2.md",
	})
	if err != nil {
		t.Fatalf("supersede 2: %v", err)
	}
	if replaced.FileRef != "files/voice-v2.md" {
		t.Fatalf("explicit replacement ref ignored: %q", replaced.FileRef)
	}
}

// TestSetMemoryFileRefAttachesAndDetaches covers the in-place pointer edit: the
// memory keeps its id and history across an attach, a repoint, and a detach.
func TestSetMemoryFileRefAttachesAndDetaches(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Writing"})
	m, err := s.AddMemory(ctx, db, AddMemoryParams{
		DomainID: d.ID, Type: TypeRule, Text: "Use my voice.",
		Status: StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := s.SetMemoryFileRef(ctx, db, m.ID, "  files/voice.md  ")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got.ID != m.ID || got.FileRef != "files/voice.md" || got.Text != m.Text {
		t.Fatalf("attach changed more than the pointer: %+v", got)
	}

	got, err = s.SetMemoryFileRef(ctx, db, m.ID, "files/voice-v2.md")
	if err != nil {
		t.Fatalf("repoint: %v", err)
	}
	if got.FileRef != "files/voice-v2.md" {
		t.Fatalf("repoint failed: %q", got.FileRef)
	}

	got, err = s.SetMemoryFileRef(ctx, db, m.ID, "")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got.FileRef != "" {
		t.Fatalf("detach failed: %q", got.FileRef)
	}
	if got.ID != m.ID || got.Status != StatusActive {
		t.Fatalf("detach disturbed the memory: %+v", got)
	}
}

// A pointer change on a sticky (always-in-context) memory must invalidate the
// cached stable block, or the prompt would keep the previous document.
func TestSetMemoryFileRefBumpsStableRev(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	gen, err := s.GeneralDomain(ctx, db) // seeded sticky domain
	if err != nil {
		t.Fatalf("general domain: %v", err)
	}
	m, _ := s.AddMemory(ctx, db, AddMemoryParams{
		DomainID: gen.ID, Type: TypeRule, Text: "Use my voice.",
		Status: StatusActive, Confidence: 0.9,
	})

	before, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	if _, err := s.SetMemoryFileRef(ctx, db, m.ID, "files/voice.md"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	after, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	if after <= before {
		t.Fatalf("stable_rev not bumped: %d -> %d", before, after)
	}
}

func TestSetMemoryFileRefUnknownMemory(t *testing.T) {
	s := openTest(t)
	if _, err := s.SetMemoryFileRef(context.Background(), s.DB(), "hNOPE1", "files/x.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestMigrateAddsFileRefColumn opens a database whose memories table predates
// the file_ref column and verifies the additive migration backfills it as empty
// without disturbing existing rows.
func TestMigrateAddsFileRefColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.cogmem.db")
	ctx := context.Background()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// A pre-file_ref memories table (schema v3 shape).
	_, err = db.ExecContext(ctx, `
		CREATE TABLE memories (
		  id TEXT PRIMARY KEY, domain_id TEXT NOT NULL, type TEXT NOT NULL,
		  text TEXT NOT NULL, status TEXT NOT NULL, confidence REAL NOT NULL,
		  priority INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL,
		  origin TEXT NOT NULL DEFAULT 'chat', source_session TEXT,
		  source_seq_start INTEGER, source_seq_end INTEGER,
		  supersedes_memory_id TEXT, retire_reason TEXT,
		  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		INSERT INTO memories(id, domain_id, type, text, status, confidence, source, created_at, updated_at)
		VALUES('hOLD01','dOLD01','fact','legacy row','active',0.9,'tool_write',1,1);`)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer func() { _ = s.Close() }()

	m, err := s.GetMemory(ctx, s.DB(), "hOLD01")
	if err != nil {
		t.Fatalf("get legacy memory after migration: %v", err)
	}
	if m.Text != "legacy row" || m.FileRef != "" {
		t.Fatalf("legacy row not migrated cleanly: %+v", m)
	}
}
