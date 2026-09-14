// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// createDomain seeds an active domain with the given name.
func createDomain(t *testing.T, s *store.Store, name string) store.Domain {
	t.Helper()
	d, err := s.CreateDomain(context.Background(), s.DB(), store.CreateDomainParams{
		Name: name, Status: store.StatusActive,
	})
	if err != nil {
		t.Fatalf("create domain %q: %v", name, err)
	}
	return d
}

// domainByName finds an active domain by name, failing the test if absent.
func domainByName(t *testing.T, s *store.Store, name string) store.Domain {
	t.Helper()
	d, err := s.DomainByName(context.Background(), s.DB(), name)
	if err != nil {
		t.Fatalf("domain %q: %v", name, err)
	}
	return d
}

// eventsOfType filters the audit ledger by event type.
func eventsOfType(events []store.Event, typ string) []store.Event {
	var out []store.Event
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestApplySupersedeEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	// Seed: a project domain with a rule hook.
	d := createDomain(t, s, "Layout")
	h := addMemory(t, s, d.ID, store.TypeRule, "Never use the color blue.")

	out := Output{
		MemoryOps: []MemoryOp{{
			Op: "supersede", OldID: h.ID, Domain: d.ID, Type: "rule",
			Text: "Use blue for the layout.", Confidence: 0.95, Evidence: store.Evidence{SeqStart: 512, SeqEnd: 512},
		}},
		ConflictLedger: []LedgerEntry{{Resolved: "swapped blue rule", Reason: "user said so", Evidence: store.Evidence{SeqStart: 512, SeqEnd: 512}}},
	}

	n, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle", Model: "test-model"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	old, err := s.GetMemory(ctx, s.DB(), h.ID)
	if err != nil {
		t.Fatalf("get old memory: %v", err)
	}
	if old.Status != store.StatusRetired {
		t.Fatalf("old hook status = %q, want retired", old.Status)
	}
	active, err := s.ListMemories(ctx, s.DB(), d.ID, store.StatusActive)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if len(active) != 1 || active[0].Text != "Use blue for the layout." {
		t.Fatalf("active hooks = %+v", active)
	}
	if active[0].SupersedesMemoryID == nil || *active[0].SupersedesMemoryID != h.ID {
		t.Fatalf("replacement supersedes = %v, want %s", active[0].SupersedesMemoryID, h.ID)
	}

	// Ledger: the merge and the conflict resolution, in order.
	events := listEvents(t, s)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want merge then conflict_resolved", events)
	}
	merge := events[0]
	if merge.Type != "merge" || merge.DomainID != d.ID || merge.MemoryID != active[0].ID ||
		merge.Actor != "sleep_cycle" || merge.Model != "test-model" ||
		merge.Evidence != `{"seq_start":512,"seq_end":512}` {
		t.Fatalf("merge event = %+v", merge)
	}
	conflict := events[1]
	if conflict.Type != "conflict_resolved" || conflict.Reason != "swapped blue rule — user said so" ||
		conflict.Evidence != `{"seq_start":512,"seq_end":512}` ||
		conflict.Actor != "sleep_cycle" || conflict.Model != "test-model" ||
		conflict.DomainID != "" || conflict.MemoryID != "" {
		t.Fatalf("conflict event = %+v", conflict)
	}
}

func TestApplyCreateWithTmpID(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	out := Output{
		DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "New Project", Summary: "x", Reason: "new topic", Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}}},
		MemoryOps: []MemoryOp{{Op: "add", Domain: "t1", Type: "fact", Text: "a durable fact", Confidence: 0.9, Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}}},
	}
	n, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 2 {
		t.Fatalf("applied = %d, want 2", n)
	}
	proj := domainByName(t, s, "New Project")
	if proj.Sticky() || proj.Summary != "x" {
		t.Fatalf("created domain = %+v, want non-sticky with summary x", proj)
	}
	hooks, err := s.ListMemories(ctx, s.DB(), proj.ID, store.StatusActive)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Text != "a durable fact" || hooks[0].Origin != store.OriginConsolidation {
		t.Fatalf("hooks = %+v, want one consolidation-origin fact", hooks)
	}
	if hooks[0].SourceSeqStart == nil || *hooks[0].SourceSeqStart != 1 || hooks[0].SourceSeqEnd == nil || *hooks[0].SourceSeqEnd != 1 {
		t.Fatalf("hook source range = %v..%v, want 1..1", hooks[0].SourceSeqStart, hooks[0].SourceSeqEnd)
	}

	// Both creates are logged against the assigned id, not the tmp_id.
	events := listEvents(t, s)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2", events)
	}
	if events[0].Type != "create" || events[0].DomainID != proj.ID || events[0].MemoryID != "" || events[0].Reason != "new topic" {
		t.Fatalf("domain create event = %+v", events[0])
	}
	if events[1].Type != "create" || events[1].DomainID != proj.ID || events[1].MemoryID != hooks[0].ID {
		t.Fatalf("memory create event = %+v", events[1])
	}
}

func TestApplySetsTriggers(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	out := Output{
		DomainOps: []DomainOp{{
			Op: "create", TmpID: "t1", Name: "Email", Summary: "mail",
			Triggers: "google_gmail, microsoft365_mail",
			Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1},
		}},
	}
	if _, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	email := domainByName(t, s, "Email")
	if email.Triggers != "google_gmail,microsoft365_mail" {
		t.Fatalf("triggers = %q, want normalized list", email.Triggers)
	}
	if _, ok := email.MatchTrigger("mcp__fusion__google_gmail_messages_list"); !ok {
		t.Fatalf("worker-set trigger should match gmail tool")
	}
}

func TestApplySetsKeywordTriggers(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	out := Output{
		DomainOps: []DomainOp{{
			Op: "create", TmpID: "t1", Name: "Daily Ops", Summary: "ops",
			KeywordTriggers: "Morning Routine, weekly report",
			Evidence:        store.Evidence{SeqStart: 1, SeqEnd: 1},
		}},
	}
	if _, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	wf := domainByName(t, s, "Daily Ops")
	if wf.KeywordTriggers != "morning routine,weekly report" {
		t.Fatalf("keyword_triggers = %q, want normalized list", wf.KeywordTriggers)
	}
	if _, ok := wf.MatchKeyword("time for your morning routine"); !ok {
		t.Fatalf("worker-set keyword trigger should match the phrase")
	}
}

// TestApplyStickyCreateAndUpdate confirms the worker can create a sticky domain
// and later toggle stickiness via an update patch.
func TestApplyStickyCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	yes := true
	createOut := Output{
		DomainOps: []DomainOp{{
			Op: "create", TmpID: "t1", Name: "House Rules", Summary: "global",
			Sticky: &yes, Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1},
		}},
	}
	if _, err := Apply(ctx, s, createOut, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatalf("apply create: %v", err)
	}
	d := domainByName(t, s, "House Rules")
	if !d.Sticky() {
		t.Fatalf("created domain sticky=%v, want sticky", d.Sticky())
	}

	no := false
	updateOut := Output{
		DomainOps: []DomainOp{{
			Op: "update", ID: d.ID, Sticky: &no,
			Evidence: store.Evidence{SeqStart: 2, SeqEnd: 2},
		}},
	}
	if _, err := Apply(ctx, s, updateOut, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatalf("apply update: %v", err)
	}
	d2, err := s.GetDomain(ctx, s.DB(), d.ID, false)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if d2.Sticky() {
		t.Fatalf("domain still sticky after update releasing it")
	}
}

// Every field an update op can carry lands on the domain.
func TestApplyUpdateDomainAllFields(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	d := createDomain(t, s, "Roadmap")

	yes := true
	state := store.DomainState{
		Blockers:    []string{"waiting on legal"},
		NextActions: []string{"ship v2"},
		Constraints: []string{"no new deps"},
		Fields:      map[string]any{"owner": "eric"},
	}
	out := Output{DomainOps: []DomainOp{{
		Op: "update", ID: d.ID,
		Summary:         "Product roadmap for 2026",
		State:           &state,
		Status:          "archived",
		Triggers:        "Trello, google_calendar",
		KeywordTriggers: "Roadmap Review, quarterly plan",
		Sticky:          &yes,
		Reason:          "consolidated status",
		Evidence:        store.Evidence{SeqStart: 7, SeqEnd: 9},
	}}}
	n, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}

	got, err := s.GetDomain(ctx, s.DB(), d.ID, false)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got.Summary != "Product roadmap for 2026" {
		t.Errorf("summary = %q", got.Summary)
	}
	if !reflect.DeepEqual(got.State, state) {
		t.Errorf("state = %+v, want %+v", got.State, state)
	}
	if got.Status != store.StatusArchived {
		t.Errorf("status = %q, want archived", got.Status)
	}
	if got.Triggers != "trello,google_calendar" {
		t.Errorf("triggers = %q, want normalized list", got.Triggers)
	}
	if got.KeywordTriggers != "roadmap review,quarterly plan" {
		t.Errorf("keyword_triggers = %q, want normalized list", got.KeywordTriggers)
	}
	if !got.Sticky() {
		t.Error("sticky = false, want true")
	}
	if got.Version != d.Version+1 {
		t.Errorf("version = %d, want %d", got.Version, d.Version+1)
	}
	if got.Name != "Roadmap" {
		t.Errorf("name = %q, an update must not rename", got.Name)
	}

	events := eventsOfType(listEvents(t, s), "update")
	if len(events) != 1 || events[0].DomainID != d.ID || events[0].Reason != "consolidated status" ||
		events[0].Evidence != `{"seq_start":7,"seq_end":9}` {
		t.Fatalf("update events = %+v", events)
	}
}

func TestApplyArchiveDomain(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	d := createDomain(t, s, "Old Project")

	out := Output{DomainOps: []DomainOp{{
		Op: "archive", ID: d.ID, Reason: "project shipped and closed",
		Evidence: store.Evidence{SeqStart: 3, SeqEnd: 4},
	}}}
	n, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	got, err := s.GetDomain(ctx, s.DB(), d.ID, false)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got.Status != store.StatusArchived || got.ArchivedAt == nil {
		t.Fatalf("domain = status %q archived_at %v, want archived with a time", got.Status, got.ArchivedAt)
	}
	// apply.go logs an archive with event type "update", so the assertion is
	// on the domain id and reason rather than the type.
	events := listEvents(t, s)
	if len(events) != 1 || events[0].DomainID != d.ID || events[0].Reason != "project shipped and closed" ||
		events[0].Evidence != `{"seq_start":3,"seq_end":4}` || events[0].Actor != "sleep_cycle" {
		t.Fatalf("events = %+v, want one for %s with the op's reason", events, d.ID)
	}
}

func TestApplyRetireMemory(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	d := createDomain(t, s, "Prefs")
	h := addMemory(t, s, d.ID, store.TypePreference, "Likes dark mode.")

	out := Output{MemoryOps: []MemoryOp{{
		Op: "retire", ID: h.ID, Reason: "contradicted by a newer preference",
	}}}
	n, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	got, err := s.GetMemory(ctx, s.DB(), h.ID)
	if err != nil {
		t.Fatalf("get memory: %v", err)
	}
	if got.Status != store.StatusRetired || got.RetireReason == nil || *got.RetireReason != "contradicted by a newer preference" {
		t.Fatalf("memory = %+v, want retired with the op's reason", got)
	}
	events := listEvents(t, s)
	if len(events) != 1 || events[0].Type != "retire" || events[0].MemoryID != h.ID ||
		events[0].Reason != "contradicted by a newer preference" || events[0].Evidence != `{"seq_start":0,"seq_end":0}` {
		t.Fatalf("events = %+v, want one retire of %s", events, h.ID)
	}
}

// Apply is one transaction: when a later op fails, nothing from the earlier
// ones remains — no rows, no audit events.
func TestApplyRollsBackOnFailure(t *testing.T) {
	cases := []struct {
		name    string
		out     Output
		wantErr error
	}{
		{
			name: "duplicate domain name",
			out: Output{DomainOps: []DomainOp{
				{Op: "create", TmpID: "t1", Name: "Alpha", Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}},
				{Op: "create", TmpID: "t2", Name: "Alpha", Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}},
			}},
			wantErr: store.ErrDuplicateName,
		},
		{
			name: "retire of an unknown memory",
			out: Output{
				DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "Alpha", Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}}},
				MemoryOps: []MemoryOp{
					{Op: "add", Domain: "t1", Type: "fact", Text: "kept?", Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}},
					{Op: "retire", ID: "m-does-not-exist", Reason: "x"},
				},
			},
			wantErr: store.ErrNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			before, err := s.ListDomains(ctx, s.DB())
			if err != nil {
				t.Fatalf("list domains: %v", err)
			}

			// Called directly, without Validate, so the bad op reaches the store.
			_, err = Apply(ctx, s, tc.out, ApplyContext{Actor: "sleep_cycle"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("apply err = %v, want %v", err, tc.wantErr)
			}
			after, err := s.ListDomains(ctx, s.DB())
			if err != nil {
				t.Fatalf("list domains: %v", err)
			}
			if len(after) != len(before) {
				t.Fatalf("domains after rollback = %+v, want unchanged %+v", after, before)
			}
			if _, err := s.DomainByName(ctx, s.DB(), "Alpha"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("Alpha after rollback: err=%v, want not found", err)
			}
			var memories int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
				t.Fatalf("count memories: %v", err)
			}
			if memories != 0 {
				t.Fatalf("memories after rollback = %d, want 0", memories)
			}
			if events := listEvents(t, s); len(events) != 0 {
				t.Fatalf("events after rollback = %+v, want none", events)
			}
		})
	}
}
