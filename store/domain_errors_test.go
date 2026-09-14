// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"errors"
	"testing"
)

// The domain mutators report ErrNotFound for an unknown id and leave the
// stable block alone when they do nothing.
func TestDomainMutatorsUnknownID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	real, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Real"})
	before, _ := s.StableRev(ctx)

	if err := s.ArchiveDomain(ctx, db, "dZZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Errorf("archive unknown: %v, want ErrNotFound", err)
	}
	if err := s.DeleteDomain(ctx, db, "dZZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown: %v, want ErrNotFound", err)
	}
	if _, err := s.MigrateDomain(ctx, db, "dZZZZZ", real.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("migrate from unknown: %v, want ErrNotFound", err)
	}
	if _, err := s.MigrateDomain(ctx, db, real.ID, "dZZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Errorf("migrate to unknown: %v, want ErrNotFound", err)
	}
	if _, err := s.MigrateDomain(ctx, db, real.ID, real.ID); err == nil || err.Error() != "cogmem: from and to domains are the same" {
		t.Errorf("migrate onto itself: %v", err)
	}
	if err := s.UpdateDomain(ctx, db, "dZZZZZ", UpdateDomainParams{Summary: strptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v, want ErrNotFound", err)
	}
	if _, err := s.DomainByName(ctx, db, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("by name unknown: %v, want ErrNotFound", err)
	}
	if _, err := s.AddMemory(ctx, db, AddMemoryParams{DomainID: "dZZZZZ", Type: TypeFact, Text: "x", Confidence: 0.5}); !errors.Is(err, ErrNotFound) {
		t.Errorf("add memory to unknown domain: %v, want ErrNotFound", err)
	}
	if after, _ := s.StableRev(ctx); after != before {
		t.Errorf("stable_rev %d -> %d across failed mutations, want unchanged", before, after)
	}
	if d, _ := s.GetDomain(ctx, db, real.ID, false); d.Version != 1 || d.Status != StatusActive {
		t.Errorf("the real domain was touched: %+v", d)
	}
}

// Archiving is idempotent: the second call finds the domain already archived,
// changes nothing and does not bump the stable block or the version.
func TestArchiveDomainTwice(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Once"})
	if err := s.ArchiveDomain(ctx, s.DB(), d.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	rev, _ := s.StableRev(ctx)
	got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if got.Status != StatusArchived || got.ArchivedAt == nil || got.Version != 2 {
		t.Fatalf("after archive: %+v", got)
	}
	if err := s.ArchiveDomain(ctx, s.DB(), d.ID); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	again, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if again.Version != 2 || !again.ArchivedAt.Equal(*got.ArchivedAt) {
		t.Errorf("second archive changed the row: %+v", again)
	}
	if r2, _ := s.StableRev(ctx); r2 != rev {
		t.Errorf("second archive bumped stable_rev %d -> %d", rev, r2)
	}
	// An archived domain's name is free for a new active domain.
	if _, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Once"}); err != nil {
		t.Errorf("name of an archived domain still taken: %v", err)
	}
}

// Evidence.IsZero: only the all-zero range counts as absent.
func TestEvidenceIsZero(t *testing.T) {
	for _, tc := range []struct {
		e    Evidence
		want bool
	}{
		{Evidence{}, true},
		{Evidence{SeqStart: 1}, false},
		{Evidence{SeqEnd: 1}, false},
		{Evidence{SeqStart: 3, SeqEnd: 5}, false},
	} {
		if got := tc.e.IsZero(); got != tc.want {
			t.Errorf("%+v IsZero = %v, want %v", tc.e, got, tc.want)
		}
	}
}
