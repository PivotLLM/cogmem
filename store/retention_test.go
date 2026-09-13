// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"testing"
)

// age backdates a memory's created_at and updated_at by the given days, so a
// test can exercise a retention window without waiting one.
func age(t *testing.T, s *Store, id string, days int) {
	t.Helper()
	ts := now() - int64(days)*86400
	if _, err := s.DB().Exec(
		`UPDATE memories SET created_at=?, updated_at=? WHERE id=?`, ts, ts, id); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
}

func add(t *testing.T, s *Store, domainID string, typ MemoryType, text string) Memory {
	t.Helper()
	m, err := s.AddMemory(context.Background(), s.DB(), AddMemoryParams{
		DomainID: domainID, Type: typ, Text: text,
		Status: StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add %s: %v", text, err)
	}
	return m
}

// Old events go; everything else stays.
//
// This is the whole safety property of retention: the model's choice of type is
// what decides whether a memory is temporary, so a policy that deleted by age
// alone could silently drop a standing instruction. Only TypeEvent is ever
// removed.
func TestPurgeExpiredEventsOnlyTouchesOldEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Ops"})

	oldEvent := add(t, s, d.ID, TypeEvent, "oversight run, nothing changed")
	newEvent := add(t, s, d.ID, TypeEvent, "oversight run this morning")
	oldFact := add(t, s, d.ID, TypeFact, "home is Ottawa")
	oldRule := add(t, s, d.ID, TypeRule, "never use the word thuddy")
	oldPref := add(t, s, d.ID, TypePreference, "short replies")
	oldOper := add(t, s, d.ID, TypeOperational, "craft rules live at files/craft.md")

	for _, id := range []string{oldEvent.ID, oldFact.ID, oldRule.ID, oldPref.ID, oldOper.ID} {
		age(t, s, id, 45)
	}
	age(t, s, newEvent.ID, 2)

	n, err := s.PurgeExpiredEvents(ctx, s.DB(), 30)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d, want just the one old event", n)
	}
	if _, err := s.GetMemory(ctx, s.DB(), oldEvent.ID); err == nil {
		t.Error("the 45-day-old event survived a 30-day window")
	}
	for _, m := range []Memory{newEvent, oldFact, oldRule, oldPref, oldOper} {
		if _, err := s.GetMemory(ctx, s.DB(), m.ID); err != nil {
			t.Errorf("%s (%s) was deleted and should not have been", m.ID, m.Type)
		}
	}
}

// A domain reports how many events it holds, and that line is part of the
// cached stable block — so deleting events changes what the assistant sees even
// though no event was ever in the prompt itself.
func TestPurgeExpiredEventsInvalidatesTheStableBlock(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Ops", Sticky: true})
	m := add(t, s, d.ID, TypeEvent, "an old run")
	age(t, s, m.ID, 60)

	before, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	if _, err := s.PurgeExpiredEvents(ctx, s.DB(), 30); err != nil {
		t.Fatalf("purge: %v", err)
	}
	after, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	if after == before {
		t.Errorf("stable_rev unchanged (%d) after deleting events; the domain's "+
			"event count is part of the cached block", before)
	}
}

// Retiring leaves the row behind, so a store that retires steadily grows
// forever while showing nothing for it. Age is measured from when the memory
// was RETIRED, not when it was written: one created a year ago and retired
// yesterday has only just stopped being used.
func TestPurgeRetiredMemoriesMeasuresFromRetirement(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Ops"})

	longGone := add(t, s, d.ID, TypeFact, "retired long ago")
	justRetired := add(t, s, d.ID, TypeFact, "written long ago, retired yesterday")
	stillActive := add(t, s, d.ID, TypeFact, "still in use")

	for _, m := range []Memory{longGone, justRetired} {
		if err := s.RetireMemory(ctx, s.DB(), m.ID, "superseded"); err != nil {
			t.Fatalf("retire: %v", err)
		}
	}
	age(t, s, longGone.ID, 120)
	// Created a year ago, retired yesterday: updated_at is what counts.
	if _, err := s.DB().Exec(
		`UPDATE memories SET created_at=?, updated_at=? WHERE id=?`,
		now()-365*86400, now()-86400, justRetired.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	age(t, s, stillActive.ID, 400)

	n, err := s.PurgeRetiredMemories(ctx, s.DB(), 90)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d, want just the long-retired one", n)
	}
	if _, err := s.GetMemory(ctx, s.DB(), longGone.ID); err == nil {
		t.Error("the memory retired 120 days ago survived a 90-day window")
	}
	if _, err := s.GetMemory(ctx, s.DB(), justRetired.ID); err != nil {
		t.Error("a memory retired yesterday was deleted because it was written long ago")
	}
	if _, err := s.GetMemory(ctx, s.DB(), stillActive.ID); err != nil {
		t.Error("an ACTIVE memory was deleted by the retired-memory sweep")
	}
}

// Zero or negative days is how "keep forever" is expressed, and must not be
// read as "everything is older than zero days".
func TestPurgeKeepsEverythingWhenDisabled(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Ops"})
	ev := add(t, s, d.ID, TypeEvent, "ancient run")
	ret := add(t, s, d.ID, TypeFact, "ancient fact")
	_ = s.RetireMemory(ctx, s.DB(), ret.ID, "old")
	age(t, s, ev.ID, 3650)
	age(t, s, ret.ID, 3650)

	for _, days := range []int{0, -1} {
		if n, err := s.PurgeExpiredEvents(ctx, s.DB(), days); err != nil || n != 0 {
			t.Errorf("events days=%d deleted %d (err %v), want 0", days, n, err)
		}
		if n, err := s.PurgeRetiredMemories(ctx, s.DB(), days); err != nil || n != 0 {
			t.Errorf("retired days=%d deleted %d (err %v), want 0", days, n, err)
		}
	}
	if _, err := s.GetMemory(ctx, s.DB(), ev.ID); err != nil {
		t.Error("a 10-year-old event was deleted with retention disabled")
	}
}

// Prompt memories come back oldest first.
//
// Ids are random, so ordering by id was arbitrary and not even stable between
// stores. Chronological order makes the rendered block stable and puts the
// newest statement on a topic last, which is where a reader looks for the
// current one — and it backs up the age_days signal the consolidation prompt
// now relies on to resolve contradictions.
func TestPromptMemoriesAreOldestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Ops"})

	oldest := add(t, s, d.ID, TypeRule, "the original instruction")
	middle := add(t, s, d.ID, TypeRule, "a revision")
	newest := add(t, s, d.ID, TypeRule, "the current instruction")
	age(t, s, oldest.ID, 100)
	age(t, s, middle.ID, 50)
	age(t, s, newest.ID, 1)

	got, err := s.ListPromptMemories(ctx, s.DB(), d.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{
		"the original instruction",
		"a revision",
		"the current instruction",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d memories, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Errorf("position %d = %q, want %q (oldest first)", i, got[i].Text, want[i])
		}
	}
}
