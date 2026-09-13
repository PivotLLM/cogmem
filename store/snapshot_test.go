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
	if _, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Proj", Summary: "x"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = s.Close()

	dst := filepath.Join(dir, "snap.cogmem.db")
	if err := Snapshot(ctx, src, dst); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// The snapshot opens and carries the domain.
	s2, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer s2.Close()
	doms, err := s2.ListDomains(ctx, s2.DB(), StatusActive)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, d := range doms {
		if d.Name == "Proj" {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot missing the domain; got %d domains", len(doms))
	}
}
