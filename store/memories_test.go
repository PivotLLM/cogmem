// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"fmt"
	"testing"
)

// MatchActiveMemories returns every active match with no cap, events
// included, case-insensitively, optionally within one domain, and never a
// retired one.
func TestMatchActiveMemories(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	d1, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "One"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d2, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Two"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// More matches than SearchMemories' default page in d1, an event and a
	// retired match too, plus one match in d2 and one non-match.
	const n = 150
	for i := 0; i < n; i++ {
		if _, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{
			DomainID: d1.ID, Type: TypeFact, Text: fmt.Sprintf("Needle %d", i), Confidence: 0.9,
		}); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: d1.ID, Type: TypeEvent, Text: "needle event", Confidence: 0.9}); err != nil {
		t.Fatalf("add event: %v", err)
	}
	gone, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: d1.ID, Type: TypeFact, Text: "needle retired", Confidence: 0.9})
	if err != nil {
		t.Fatalf("add retired: %v", err)
	}
	if err := s.RetireMemory(ctx, s.DB(), gone.ID, "gone"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: d2.ID, Type: TypeFact, Text: "NEEDLE elsewhere", Confidence: 0.9}); err != nil {
		t.Fatalf("add d2: %v", err)
	}
	if _, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: d2.ID, Type: TypeFact, Text: "hay", Confidence: 0.9}); err != nil {
		t.Fatalf("add hay: %v", err)
	}

	all, err := s.MatchActiveMemories(ctx, s.DB(), "needle", "")
	if err != nil {
		t.Fatalf("match all: %v", err)
	}
	if len(all) != n+2 {
		t.Errorf("unfiltered matches = %d, want %d (facts + event + the one in the other domain)", len(all), n+2)
	}
	for _, m := range all {
		if m.Status != StatusActive {
			t.Errorf("retired memory %s returned", m.ID)
		}
	}
	if got, err := s.SearchMemories(ctx, s.DB(), "needle", 0, true); err != nil || len(got) >= len(all) {
		t.Errorf("SearchMemories with the default limit returned %d (err=%v); this test needs more matches than that", len(got), err)
	}

	only2, err := s.MatchActiveMemories(ctx, s.DB(), "needle", d2.ID)
	if err != nil {
		t.Fatalf("match d2: %v", err)
	}
	if len(only2) != 1 || only2[0].Text != "NEEDLE elsewhere" {
		t.Errorf("filtered matches = %+v, want the one in %s", only2, d2.ID)
	}
	if none, err := s.MatchActiveMemories(ctx, s.DB(), "needle", "dZZZZZ"); err != nil || len(none) != 0 {
		t.Errorf("unknown domain: %d matches err=%v, want none", len(none), err)
	}
}
