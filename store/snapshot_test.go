package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.cogmem.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		Name: "Proj", Summary: "x", Sticky: true, Triggers: "github", KeywordTriggers: "the project",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := add(t, s, d.ID, TypeRule, "keep the build green")
	if err := s.AppendInbox(ctx, s.DB(), 9, "user", "pending"); err != nil {
		t.Fatalf("inbox: %v", err)
	}
	rev, _ := s.StableRev(ctx)
	_ = s.Close()

	dst := filepath.Join(dir, "snap.cogmem.db")
	if err := Snapshot(ctx, src, dst); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The snapshot opens and carries the domain, its memory, the inbox and the
	// bookkeeping — everything, with the same ids.
	s2, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer s2.Close()
	doms, err := s2.ListDomains(ctx, s2.DB(), StatusActive)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(doms) != 2 {
		t.Fatalf("snapshot has %d active domains, want 2 (General + Proj)", len(doms))
	}
	got, err := s2.GetDomain(ctx, s2.DB(), d.ID, true)
	if err != nil {
		t.Fatalf("snapshot missing the domain: %v", err)
	}
	if got.Name != "Proj" || got.Summary != "x" || !got.Sticky() || got.Triggers != "github" || got.KeywordTriggers != "the project" {
		t.Errorf("snapshot domain = %+v", got)
	}
	if len(got.Memories) != 1 || got.Memories[0].ID != m.ID || got.Memories[0].Text != "keep the build green" {
		t.Errorf("snapshot memories = %+v, want the one rule with its id", got.Memories)
	}
	if rows, _ := s2.InboxRange(ctx, s2.DB(), 9, 9); len(rows) != 1 || rows[0].Text != "pending" {
		t.Errorf("snapshot inbox = %+v", rows)
	}
	if r2, _ := s2.StableRev(ctx); r2 != rev {
		t.Errorf("snapshot stable_rev = %d, want %d", r2, rev)
	}
	if s2.Path() != dst {
		t.Errorf("snapshot store path = %q, want %q", s2.Path(), dst)
	}
}
