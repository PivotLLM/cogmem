// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"errors"
	"testing"
)

// DomainByNameAny finds a domain by name whatever its status, prefers the
// active one when an archived domain shares the name, matches
// case-insensitively and trimmed, and reports ErrNotFound for no match.
func TestDomainByNameAny(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	old, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Project"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.ArchiveDomain(ctx, s.DB(), old.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// DomainByName no longer sees it; DomainByNameAny does.
	if _, err := s.DomainByName(ctx, s.DB(), "project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DomainByName of an archived domain: err = %v, want ErrNotFound", err)
	}
	got, err := s.DomainByNameAny(ctx, s.DB(), "  project ")
	if err != nil || got.ID != old.ID || got.Status != StatusArchived {
		t.Fatalf("DomainByNameAny(archived) = %+v err=%v, want %s archived", got, err, old.ID)
	}

	// An active domain with the same name wins over the archived one.
	cur, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "project"})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if got, err := s.DomainByNameAny(ctx, s.DB(), "Project"); err != nil || got.ID != cur.ID {
		t.Errorf("DomainByNameAny with both = %s err=%v, want the active %s", got.ID, err, cur.ID)
	}

	if _, err := s.DomainByNameAny(ctx, s.DB(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown name: err = %v, want ErrNotFound", err)
	}
}

// Counts reports domains and memories by status; a fresh store holds only
// the seeded General domain.
func TestCounts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	got, err := s.Counts(ctx, s.DB())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if want := (Counts{ActiveDomains: 1}); got != want {
		t.Fatalf("fresh store counts = %+v, want %+v", got, want)
	}

	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Counted"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gone, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Gone"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.ArchiveDomain(ctx, s.DB(), gone.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	for _, txt := range []string{"a", "b", "c"} {
		if _, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: txt, Confidence: 0.9}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	retired, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: gone.ID, Type: TypeEvent, Text: "r", Confidence: 0.9})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.RetireMemory(ctx, s.DB(), retired.ID, "done"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	got, err = s.Counts(ctx, s.DB())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if want := (Counts{ActiveDomains: 2, ArchivedDomains: 1, ActiveMemories: 3, RetiredMemories: 1}); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// UpdateDomain keeps archived_at in step with the status, the way
// ArchiveDomain does: stamped on the transition to archived, cleared when the
// domain is active again, and untouched by an update that leaves status alone.
func TestUpdateDomainStatusStampsArchivedAt(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Lifecycle"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	get := func() Domain {
		t.Helper()
		got, err := s.GetDomain(ctx, s.DB(), d.ID, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}
	if get().ArchivedAt != nil {
		t.Fatal("a new domain must not carry archived_at")
	}

	archived := StatusArchived
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Status: &archived}); err != nil {
		t.Fatalf("archive via update: %v", err)
	}
	first := get()
	if first.Status != StatusArchived || first.ArchivedAt == nil {
		t.Fatalf("after archiving via update: status=%s archived_at=%v, want archived with a timestamp", first.Status, first.ArchivedAt)
	}

	summary := "still archived"
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Summary: &summary}); err != nil {
		t.Fatalf("update summary: %v", err)
	}
	if got := get(); got.ArchivedAt == nil || !got.ArchivedAt.Equal(*first.ArchivedAt) {
		t.Fatalf("an update without a status change moved archived_at: %v -> %v", first.ArchivedAt, got.ArchivedAt)
	}

	active := StatusActive
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Status: &active}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if got := get(); got.Status != StatusActive || got.ArchivedAt != nil {
		t.Fatalf("after reactivating: status=%s archived_at=%v, want active with no timestamp", got.Status, got.ArchivedAt)
	}
}
