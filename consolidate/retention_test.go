// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// backdate moves a memory's timestamps into the past so a retention window can
// be exercised without waiting one out.
func backdate(t *testing.T, s *store.Store, id string, days int) {
	t.Helper()
	if _, err := s.DB().Exec(
		`UPDATE memories SET created_at = created_at - ?, updated_at = updated_at - ? WHERE id=?`,
		int64(days)*86400, int64(days)*86400, id); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

func addMemory(t *testing.T, s *store.Store, domainID string, typ store.MemoryType, text string) store.Memory {
	t.Helper()
	m, err := s.AddMemory(context.Background(), s.DB(), store.AddMemoryParams{
		DomainID: domainID, Type: typ, Text: text,
		Status: store.StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add %q: %v", text, err)
	}
	return m
}

func countMemories(t *testing.T, s *store.Store) map[string]bool {
	t.Helper()
	rows, err := s.DB().Query(`SELECT id FROM memories`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	present := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		present[id] = true
	}
	return present
}

// Retention runs as part of every consolidation and it DELETES rows. The store
// primitives are tested on their own; what was not, until this, is that the
// worker calls them at all and with the configured windows. A regression that
// silently stops the purge grows the store forever, and one that purges with
// the wrong window destroys memories — neither announces itself.
func TestRunOnce_AppliesRetention(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	domainID, _ := seedDomain(t, s)

	oldEvent := addMemory(t, s, domainID, store.TypeEvent, "hourly check, nothing changed")
	freshEvent := addMemory(t, s, domainID, store.TypeEvent, "checked this morning")
	oldFact := addMemory(t, s, domainID, store.TypeFact, "home is Ottawa")
	oldRetired := addMemory(t, s, domainID, store.TypeRule, "a rule that was replaced")
	freshRetired := addMemory(t, s, domainID, store.TypeRule, "recently replaced")

	for _, id := range []string{oldRetired.ID, freshRetired.ID} {
		if err := s.RetireMemory(ctx, s.DB(), id, "superseded"); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, s, oldEvent.ID, 45)    // past a 30-day event window
	backdate(t, s, oldFact.ID, 45)     // same age, but a fact — must survive
	backdate(t, s, oldRetired.ID, 120) // past a 90-day retired window
	backdate(t, s, freshEvent.ID, 2)
	backdate(t, s, freshRetired.ID, 10)

	seedInbox(t, s, sampleMessages())
	w := NewWorker(s,
		&fakeModel{raw: `{"domain_ops":[],"memory_ops":[],"conflict_ledger":[]}`},
		WithRetention(30, 90))
	if _, err := w.RunOnce(ctx, params()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	present := countMemories(t, s)
	tests := []struct {
		name string
		id   string
		want bool
		why  string
	}{
		{"an event past the window", oldEvent.ID, false, "events accumulate without bound and go stale"},
		{"a recent event", freshEvent.ID, true, "inside the window"},
		{"an old fact", oldFact.ID, true, "only events are removed by age — type is what decides"},
		{"a long-retired memory", oldRetired.ID, false, "retiring leaves the row behind"},
		{"a recently retired memory", freshRetired.ID, true, "inside the window"},
	}
	for _, tc := range tests {
		if present[tc.id] != tc.want {
			t.Errorf("%s: present = %v, want %v (%s)", tc.name, present[tc.id], tc.want, tc.why)
		}
	}
}

// Zero means keep forever, and it is what an install that has never set a
// retention window gets. Purging on a zero window would delete an agent's
// history the first time it consolidated.
func TestRunOnce_RetentionDisabledKeepsEverything(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	domainID, _ := seedDomain(t, s)

	ancientEvent := addMemory(t, s, domainID, store.TypeEvent, "an event from long ago")
	ancientRetired := addMemory(t, s, domainID, store.TypeRule, "retired long ago")
	if err := s.RetireMemory(ctx, s.DB(), ancientRetired.ID, "superseded"); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, ancientEvent.ID, 3650)
	backdate(t, s, ancientRetired.ID, 3650)

	seedInbox(t, s, sampleMessages())
	w := NewWorker(s,
		&fakeModel{raw: `{"domain_ops":[],"memory_ops":[],"conflict_ledger":[]}`},
		WithRetention(0, 0))
	if _, err := w.RunOnce(ctx, params()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	present := countMemories(t, s)
	if !present[ancientEvent.ID] {
		t.Error("a ten-year-old event was deleted with retention disabled")
	}
	if !present[ancientRetired.ID] {
		t.Error("a ten-year-old retired memory was deleted with retention disabled")
	}
}
