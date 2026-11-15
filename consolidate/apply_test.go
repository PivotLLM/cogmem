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

	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle", Model: "test-model"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("applied = %d, want 1", res.Applied)
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
	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 2 {
		t.Fatalf("applied = %d, want 2", res.Applied)
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
	if wf.AutoKeywords != "morning routine,weekly report" || wf.KeywordTriggers != "" {
		t.Fatalf("auto=%q agent=%q, want the normalized list in consolidation's own", wf.AutoKeywords, wf.KeywordTriggers)
	}
	if kw := wf.MatchKeywords("time for your morning routine"); len(kw) != 1 {
		t.Fatalf("worker-set keyword trigger should match the phrase")
	}
}

// A domain op's keyword_triggers sets consolidation's own list only. It can
// never add a phrase the agent blocked, duplicate one of the agent's, or store
// a generic one; it never alters the agent's or the blocked list; and dropping
// phrases does not fail the apply.
func TestApplyKeywordOwnership(t *testing.T) {
	const ev = 1
	for _, tc := range []struct {
		name                 string
		agent, auto, blocked string // existing domain
		others               int    // other domains already using "release train"
		proposed             string
		wantAuto             string
		wantDropped          []DroppedKeyword // DomainID filled in below
	}{
		{name: "replaces the auto list", auto: "old phrase", proposed: "Kestrel, falcon nest", wantAuto: "kestrel,falcon nest"},
		{name: "skips agent phrases", agent: "invoice run", proposed: "invoice run,vendor bills", wantAuto: "vendor bills"},
		{name: "skips blocked phrases", blocked: "vendor bills", proposed: "vendor bills,ledger close", wantAuto: "ledger close"},
		{
			name: "drops generic phrases", auto: "kestrel", proposed: "today,good morning,kestrel", wantAuto: "kestrel",
			wantDropped: []DroppedKeyword{{Phrase: "today", Reason: "too common"}, {Phrase: "good morning", Reason: "too common"}},
		},
		{
			name: "drops widely shared phrases", others: 3, proposed: "release train,kestrel", wantAuto: "kestrel",
			wantDropped: []DroppedKeyword{{Phrase: "release train", Reason: "already used by 3 domains"}},
		},
		{name: "two other users is fine", others: 2, proposed: "release train", wantAuto: "release train"},
		{
			name: "own phrase kept although now widely shared", auto: "release train", others: 3,
			proposed: "release train,kestrel", wantAuto: "release train,kestrel",
		},
		{
			name: "nothing surviving leaves the list unchanged", auto: "kestrel", agent: "invoice run", blocked: "ledger close",
			proposed: "invoice run,today,ledger close", wantAuto: "kestrel",
			wantDropped: []DroppedKeyword{{Phrase: "today", Reason: "too common"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			for i := range tc.others {
				if _, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
					Name: "Other " + string(rune('A'+i)), AutoKeywords: "release train",
				}); err != nil {
					t.Fatal(err)
				}
			}
			d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
				Name: "Target", KeywordTriggers: tc.agent, AutoKeywords: tc.auto, BlockedKeywords: tc.blocked,
			})
			if err != nil {
				t.Fatal(err)
			}
			out := Output{DomainOps: []DomainOp{{
				Op: "update", ID: d.ID, KeywordTriggers: tc.proposed, Evidence: store.Evidence{SeqStart: ev, SeqEnd: ev},
			}}}
			res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
			if err != nil || res.Applied != 1 {
				t.Fatalf("apply: applied=%d err=%v, want the op applied", res.Applied, err)
			}
			got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
			if got.AutoKeywords != tc.wantAuto {
				t.Errorf("auto = %q, want %q", got.AutoKeywords, tc.wantAuto)
			}
			if got.KeywordTriggers != d.KeywordTriggers || got.BlockedKeywords != d.BlockedKeywords {
				t.Errorf("agent=%q blocked=%q changed from %q %q", got.KeywordTriggers, got.BlockedKeywords, d.KeywordTriggers, d.BlockedKeywords)
			}
			var want []DroppedKeyword
			for _, w := range tc.wantDropped {
				w.DomainID = d.ID
				want = append(want, w)
			}
			if !reflect.DeepEqual(res.DroppedKeywords, want) {
				t.Errorf("dropped = %+v, want %+v", res.DroppedKeywords, want)
			}
		})
	}
}

// Apply reads the agent's and blocked lists inside its own transaction, not
// from the view the model was shown: a phrase the agent blocked after the view
// was built is still not added.
func TestApplyReadsBlockedListAtApplyTime(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Billing", AutoKeywords: "vendor bills"})
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(s, nil)
	view := w.currentState(ctx)
	if dv := domainView(t, Input{CurrentState: view}, d.ID); dv.KeywordTriggers != "vendor bills" || dv.BlockedKeywords != "" {
		t.Fatalf("view = %+v", dv)
	}
	// The agent removes the phrase (it becomes blocked) while the model works.
	blocked, empty := "vendor bills", ""
	if err = s.UpdateDomain(ctx, s.DB(), d.ID, store.UpdateDomainParams{AutoKeywords: &empty, BlockedKeywords: &blocked}); err != nil {
		t.Fatal(err)
	}
	out := Output{DomainOps: []DomainOp{{Op: "update", ID: d.ID, KeywordTriggers: "vendor bills,accounts payable"}}}
	if err = out.Validate(Input{CurrentState: view}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err = Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if got.AutoKeywords != "accounts payable" || got.BlockedKeywords != "vendor bills" {
		t.Fatalf("auto=%q blocked=%q, want the blocked phrase kept out", got.AutoKeywords, got.BlockedKeywords)
	}
}

// A keyword-only update may carry no evidence; it applies, and its audit
// event records the empty range rather than failing.
func TestApplyKeywordOnlyUpdateWithoutEvidence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Birds"})
	if err != nil {
		t.Fatal(err)
	}
	in := Input{
		CurrentState: CurrentState{Domains: []DomainView{{ID: d.ID, Name: "Birds"}}},
		NewMessages:  []Message{{Seq: 40, Role: "user", Text: "unrelated"}},
	}
	out := Output{DomainOps: []DomainOp{{Op: "update", ID: d.ID, KeywordTriggers: "kestrel,falcon nest"}}}
	if err = out.Validate(in); err != nil {
		t.Fatalf("keyword-only update without evidence rejected: %v", err)
	}
	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil || res.Applied != 1 {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if got, _ := s.GetDomain(ctx, s.DB(), d.ID, false); got.AutoKeywords != "kestrel,falcon nest" {
		t.Fatalf("auto = %q", got.AutoKeywords)
	}
	events := eventsOfType(listEvents(t, s), "update")
	if len(events) != 1 || events[0].DomainID != d.ID || events[0].Evidence != `{"seq_start":0,"seq_end":0}` {
		t.Fatalf("update events = %+v", events)
	}
}

// On create, generic phrases are dropped and reported against the new id.
func TestApplyCreateDropsGenericKeywords(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	out := Output{DomainOps: []DomainOp{{
		Op: "create", TmpID: "t1", Name: "Birds", KeywordTriggers: "thanks,kestrel",
		Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1},
	}}}
	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	d := domainByName(t, s, "Birds")
	if d.AutoKeywords != "kestrel" || d.KeywordTriggers != "" || d.BlockedKeywords != "" {
		t.Fatalf("domain = %+v", d)
	}
	want := []DroppedKeyword{{DomainID: d.ID, Phrase: "thanks", Reason: "too common"}}
	if !reflect.DeepEqual(res.DroppedKeywords, want) {
		t.Fatalf("dropped = %+v, want %+v", res.DroppedKeywords, want)
	}
}

// An update op without keyword_triggers leaves consolidation's list alone.
func TestApplyUpdateWithoutKeywordsKeepsThem(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Birds", AutoKeywords: "kestrel"})
	if err != nil {
		t.Fatal(err)
	}
	out := Output{DomainOps: []DomainOp{{Op: "update", ID: d.ID, Summary: "raptors", Evidence: store.Evidence{SeqStart: 1, SeqEnd: 1}}}}
	if _, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetDomain(ctx, s.DB(), d.ID, false); got.AutoKeywords != "kestrel" {
		t.Fatalf("auto = %q, want unchanged", got.AutoKeywords)
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
	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("applied = %d, want 1", res.Applied)
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
	if got.AutoKeywords != "roadmap review,quarterly plan" || got.KeywordTriggers != "" {
		t.Errorf("auto=%q agent=%q, want the normalized list in consolidation's own", got.AutoKeywords, got.KeywordTriggers)
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
	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("applied = %d, want 1", res.Applied)
	}
	got, err := s.GetDomain(ctx, s.DB(), d.ID, false)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got.Status != store.StatusArchived || got.ArchivedAt == nil {
		t.Fatalf("domain = status %q archived_at %v, want archived with a time", got.Status, got.ArchivedAt)
	}
	events := listEvents(t, s)
	if len(events) != 1 || events[0].Type != "archive" || events[0].DomainID != d.ID ||
		events[0].Reason != "project shipped and closed" ||
		events[0].Evidence != `{"seq_start":3,"seq_end":4}` || events[0].Actor != "sleep_cycle" {
		t.Fatalf("events = %+v, want one archive event for %s with the op's reason", events, d.ID)
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
	res, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("applied = %d, want 1", res.Applied)
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
			res, err := Apply(ctx, s, tc.out, ApplyContext{Actor: "sleep_cycle"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("apply err = %v, want %v", err, tc.wantErr)
			}
			if res.Applied != 0 {
				t.Fatalf("applied = %d on a rolled-back apply, want 0", res.Applied)
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
