// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"testing"
	"time"
)

// Every field of an event survives the ledger, and the two defaults LogEvent
// supplies (a generated id, an empty evidence object) are what comes back.
func TestLogEventListEventsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	full := Event{
		ID: "evt-1", Type: "create", DomainID: "dAAAAA", MemoryID: "hAAAAA",
		OldJSON: `{"text":"old"}`, NewJSON: `{"text":"new"}`, Reason: "user said so",
		Evidence: `{"seq_start":3,"seq_end":5}`, Actor: "sleep_cycle",
		Model: "test-model", PromptHash: "abc123",
	}
	if err := s.LogEvent(ctx, s.DB(), full); err != nil {
		t.Fatalf("log full: %v", err)
	}
	minimal := Event{Type: "retire", Actor: "mcp_tool"}
	if err := s.LogEvent(ctx, s.DB(), minimal); err != nil {
		t.Fatalf("log minimal: %v", err)
	}

	events, err := s.ListEvents(ctx, s.DB())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0] != full {
		t.Errorf("full event changed:\n  logged %+v\n  read   %+v", full, events[0])
	}
	got := events[1]
	// A generated id is a UUID (36 chars) and never collides with the given one.
	if len(got.ID) != 36 || got.ID == full.ID {
		t.Errorf("generated id = %q, want a fresh UUID", got.ID)
	}
	if got.Evidence != "{}" {
		t.Errorf("default evidence = %q, want {}", got.Evidence)
	}
	got.ID, got.Evidence = "", ""
	if got != minimal {
		t.Errorf("minimal event changed:\n  logged %+v\n  read   %+v", minimal, got)
	}
}

// Oldest first: by created_at, and by insertion order within one second.
func TestListEventsIsOldestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := s.LogEvent(ctx, s.DB(), Event{ID: id, Type: "create", Actor: "operator"}); err != nil {
			t.Fatalf("log %s: %v", id, err)
		}
	}
	// All three share a created_at second; e1, e2, e3 is insertion order. Now
	// backdate e3 so created_at, not rowid, decides.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE memory_events SET created_at = created_at - 100 WHERE id='e3'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	events, err := s.ListEvents(ctx, s.DB())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []string
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	want := []string{"e3", "e1", "e2"}
	if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}

// An empty ledger lists as empty, not as an error.
func TestListEventsEmpty(t *testing.T) {
	s := openTest(t)
	events, err := s.ListEvents(context.Background(), s.DB())
	if err != nil || len(events) != 0 {
		t.Fatalf("events = %v err=%v, want none", events, err)
	}
}

// A lease whose expiry has passed can be taken by another owner; a live one
// cannot; the holder may renew it; only the holder may release it.
func TestLeaseExpiryRenewalAndRelease(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const name = "consolidate"

	// worker-1 held the lease and died: the row is there with expires_at in the
	// past. Written directly so the test does not have to wait a TTL out.
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO worker_leases(name, owner, expires_at) VALUES(?,?,?)`,
		name, "worker-1", now()-60); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}
	ok, err := s.AcquireLease(ctx, s.DB(), name, "worker-2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire over an expired lease: ok=%v err=%v, want true", ok, err)
	}
	owner, exp := leaseRow(t, s, name)
	if owner != "worker-2" {
		t.Fatalf("owner = %q, want worker-2", owner)
	}
	if exp < now()+50 || exp > now()+61 {
		t.Fatalf("expires_at = %d, want about now+60", exp)
	}

	// Renewal by the holder extends the lease.
	ok, err = s.AcquireLease(ctx, s.DB(), name, "worker-2", 2*time.Minute)
	if err != nil || !ok {
		t.Fatalf("renew: ok=%v err=%v, want true", ok, err)
	}
	if owner, exp2 := leaseRow(t, s, name); owner != "worker-2" || exp2 <= exp {
		t.Fatalf("after renewal owner=%q expires_at=%d (was %d), want the same owner and a later expiry", owner, exp2, exp)
	}

	// A live lease is not taken by someone else, and stays with its holder.
	ok, err = s.AcquireLease(ctx, s.DB(), name, "worker-3", time.Minute)
	if err != nil || ok {
		t.Fatalf("acquire a live lease: ok=%v err=%v, want false", ok, err)
	}
	if owner, _ := leaseRow(t, s, name); owner != "worker-2" {
		t.Fatalf("owner after failed acquire = %q, want worker-2", owner)
	}

	// Release by a non-holder does nothing; release by the holder removes it.
	if err := s.ReleaseLease(ctx, s.DB(), name, "worker-3"); err != nil {
		t.Fatalf("release by non-holder: %v", err)
	}
	if owner, _ := leaseRow(t, s, name); owner != "worker-2" {
		t.Fatalf("a non-holder released the lease")
	}
	if err := s.ReleaseLease(ctx, s.DB(), name, "worker-2"); err != nil {
		t.Fatalf("release: %v", err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_leases WHERE name=?`, name).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lease rows after release = %d err=%v, want 0", n, err)
	}
}

func leaseRow(t *testing.T, s *Store, name string) (owner string, expiresAt int64) {
	t.Helper()
	if err := s.DB().QueryRow(`SELECT owner, expires_at FROM worker_leases WHERE name=?`, name).Scan(&owner, &expiresAt); err != nil {
		t.Fatalf("read lease %s: %v", name, err)
	}
	return owner, expiresAt
}

// RecordRun with every field set, including a finish time, reads back exactly.
func TestRecordRunRoundTripsEveryField(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	started := time.Unix(1_700_000_000, 0)
	finished := started.Add(42 * time.Second)
	want := Run{
		ID: "run-1", Trigger: "nightly", Model: "test-model", SeqStart: 10, SeqEnd: 25,
		InputTokens: 1200, OutputTokens: 340, Status: "ok", OpsApplied: 7,
		Note: "auto-repaired: x", PromptHash: "deadbeef",
		StartedAt: started, FinishedAt: &finished,
	}
	if err := s.RecordRun(ctx, s.DB(), want); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, ok, err := s.LastRun(ctx, s.DB())
	if err != nil || !ok {
		t.Fatalf("last run: ok=%v err=%v", ok, err)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, finished)
	}
	got.FinishedAt, want.FinishedAt = nil, nil
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	got.StartedAt, want.StartedAt = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("run changed:\n  wrote %+v\n  read  %+v", want, got)
	}
}
