// cogmem - Cognitive Memory
// License: MIT

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/portable"
	"github.com/PivotLLM/cogmem/store"
)

// explain renders a domain's header, dates and summary; a memory's origin,
// evidence range, attached file, supersedes link and retire reason; and an
// unknown id of either kind is "<id> not found".
func TestExplainDomainAndMemory(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{"name": "BioTech", "summary": "the report", "sticky": true})
	s := hs.store(t)
	d, err := s.GetDomain(ctx, s.DB(), domainID, false)
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	day := func(tm time.Time) string { return tm.Format("2006-01-02") }
	lastUsed, _ := d.LastActive()
	want := fmt.Sprintf("Domain %s \"BioTech\"\n  sticky=true status=active version=1\n  created=%s updated=%s\n  last used=%s\n  summary: the report\n",
		domainID, day(d.CreatedAt), day(d.UpdatedAt), day(lastUsed))
	if got := hs.ok(t, "explain", map[string]any{"id": domainID}); got != want {
		t.Errorf("explain domain =\n%s\nwant\n%s", got, want)
	}

	start, end := int64(40), int64(44)
	m, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: domainID, Type: store.TypeFact, Text: "the report targets Q3",
		Status: store.StatusActive, Confidence: 0.8, Origin: store.OriginConsolidation,
		SourceSeqStart: &start, SourceSeqEnd: &end, FileRef: "files/report.md",
	})
	if err != nil {
		t.Fatalf("add memory: %v", err)
	}
	want = fmt.Sprintf("Memory %s (domain %s)\n  type=fact status=active confidence=0.80 origin=consolidation\n  text: the report targets Q3\n  attached file: files/report.md (full contents load into context with this memory)\n  evidence: seq 40..44\n",
		m.ID, domainID)
	if got := hs.ok(t, "explain", map[string]any{"id": m.ID}); got != want {
		t.Errorf("explain memory =\n%s\nwant\n%s", got, want)
	}

	m2, err := s.SupersedeMemory(ctx, s.DB(), m.ID, store.AddMemoryParams{
		DomainID: domainID, Type: store.TypeFact, Text: "the report targets Q4",
		Status: store.StatusActive, Confidence: 0.9, Origin: store.OriginChat,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	// The replacement inherited the file and links back; the original shows
	// its retirement.
	want = fmt.Sprintf("Memory %s (domain %s)\n  type=fact status=active confidence=0.90 origin=chat\n  text: the report targets Q4\n  attached file: files/report.md (full contents load into context with this memory)\n  supersedes: %s\n",
		m2.ID, domainID, m.ID)
	if got := hs.ok(t, "explain", map[string]any{"id": m2.ID}); got != want {
		t.Errorf("explain replacement =\n%s\nwant\n%s", got, want)
	}
	got := hs.ok(t, "explain", map[string]any{"id": m.ID})
	if !strings.Contains(got, "status=retired") || !strings.HasSuffix(got, "  retire reason: superseded\n") {
		t.Errorf("explain retired memory =\n%s", got)
	}

	hs.fail(t, "explain", map[string]any{"id": "hZZZZZ"}, "hZZZZZ not found")
	hs.fail(t, "explain", map[string]any{"id": "dZZZZZ"}, "dZZZZZ not found")
}

// memory_attach with a checker: a permitted file is attached and its size
// reported; a denied one is refused and the memory left as it was; omitting
// the file detaches without consulting the checker.
func TestMemoryAttach(t *testing.T) {
	var checked []string
	hs := newHarness(t, func(h *Host) {
		h.CheckAttachment = func(ref string) (int64, error) {
			checked = append(checked, ref)
			if ref == "files/voice.md" {
				return 1234, nil
			}
			return 0, errors.New("outside the agent's readable files")
		}
	})
	memoryID := hs.createMemory(t, map[string]any{"type": "rule", "text": "Write in my voice."})
	s := hs.store(t)

	got := hs.ok(t, "memory_attach", map[string]any{"id": memoryID, "file": " files/voice.md "})
	want := fmt.Sprintf("Attached files/voice.md (1234 bytes) to memory %s; its full contents load into context whenever this memory does.", memoryID)
	if got != want {
		t.Errorf("attach = %q, want %q", got, want)
	}
	if m, _ := s.GetMemory(ctx, s.DB(), memoryID); m.FileRef != "files/voice.md" {
		t.Errorf("stored file ref = %q, want files/voice.md", m.FileRef)
	}

	hs.fail(t, "memory_attach", map[string]any{"id": memoryID, "file": "../secret.md"},
		"attachment rejected: outside the agent's readable files")
	if m, _ := s.GetMemory(ctx, s.DB(), memoryID); m.FileRef != "files/voice.md" {
		t.Errorf("a refused attach changed the file ref to %q", m.FileRef)
	}

	got = hs.ok(t, "memory_attach", map[string]any{"id": memoryID})
	want = fmt.Sprintf("Detached the file from memory %s; it no longer loads a document into context.", memoryID)
	if got != want {
		t.Errorf("detach = %q, want %q", got, want)
	}
	if m, _ := s.GetMemory(ctx, s.DB(), memoryID); m.FileRef != "" {
		t.Errorf("file ref after detach = %q, want empty", m.FileRef)
	}
	if !reflect.DeepEqual(checked, []string{"files/voice.md", "../secret.md"}) {
		t.Errorf("checker saw %v, want the two attach attempts only (a detach is not checked)", checked)
	}

	// The checker runs before the memory is looked up, so an unknown id with
	// a permitted file still consults it and then fails on the id.
	hs.fail(t, "memory_attach", map[string]any{"id": "hZZZZZ", "file": "files/voice.md"}, "hZZZZZ not found")
	if len(checked) != 3 {
		t.Errorf("checker calls = %d, want 3", len(checked))
	}
	hs.fail(t, "memory_attach", map[string]any{"file": "files/voice.md"}, "id is required")
}

// memory_create with a permitted file stores the reference and reports the
// size; a denied one stores nothing at all.
func TestCreateWithAttachment(t *testing.T) {
	hs := newHarness(t, func(h *Host) {
		h.CheckAttachment = func(ref string) (int64, error) {
			if ref == "files/voice.md" {
				return 77, nil
			}
			return 0, errors.New("no such file")
		}
	})
	got := hs.ok(t, "memory_create", map[string]any{"type": "rule", "text": "Use my voice.", "file": "files/voice.md"})
	memoryID := extractID(t, got, "h")
	s := hs.store(t)
	gen, _ := s.GeneralDomain(ctx, s.DB())
	want := fmt.Sprintf("Stored memory %s in domain %s (type=rule). Attached files/voice.md (77 bytes); its full contents load into context whenever this memory does.", memoryID, gen.ID)
	if got != want {
		t.Errorf("create = %q\nwant     %q", got, want)
	}
	if m, _ := s.GetMemory(ctx, s.DB(), memoryID); m.FileRef != "files/voice.md" {
		t.Errorf("stored file ref = %q", m.FileRef)
	}

	hs.fail(t, "memory_create", map[string]any{"type": "rule", "text": "Nothing stored.", "file": "files/missing.md"},
		"attachment rejected: no such file")
	if hits, _ := s.SearchMemories(ctx, s.DB(), "Nothing stored", 10, true); len(hits) != 0 {
		t.Errorf("a refused create stored the memory anyway: %+v", hits)
	}
}

// domain_update applies every field it accepts, in one call, and the store
// shows all of them; empty values clear the lists and triggers; the errors
// for nothing-to-update, a rename collision and an unknown id are exact.
func TestDomainUpdateEveryField(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{"name": "Proj", "summary": "orig"})
	s := hs.store(t)

	got := hs.ok(t, "domain_update", map[string]any{
		"id": domainID, "set_sticky": true, "set_summary": "new summary",
		"set_triggers": "Gmail, *GitHub*", "set_keyword_triggers": []any{"Morning Routine", "weekly report"},
		"set_name": "Project X", "set_next_actions": []any{"a1", "a2"},
		"set_constraints": []any{"c1"}, "set_blockers": []any{"b1"},
	})
	if want := fmt.Sprintf("Updated domain %s.", domainID); got != want {
		t.Errorf("update = %q, want %q", got, want)
	}
	d, err := s.GetDomain(ctx, s.DB(), domainID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if d.Name != "Project X" || !d.Sticky() || d.Summary != "new summary" || d.Version != 2 ||
		d.Triggers != "gmail,github" || d.KeywordTriggers != "morning routine,weekly report" {
		t.Errorf("after update: name=%q sticky=%v summary=%q version=%d triggers=%q keywords=%q",
			d.Name, d.Sticky(), d.Summary, d.Version, d.Triggers, d.KeywordTriggers)
	}
	wantState := store.DomainState{Blockers: []string{"b1"}, NextActions: []string{"a1", "a2"}, Constraints: []string{"c1"}}
	if !reflect.DeepEqual(d.State, wantState) {
		t.Errorf("state = %+v, want %+v", d.State, wantState)
	}

	// Empty string / empty list clear triggers; the other fields are untouched.
	hs.ok(t, "domain_update", map[string]any{"id": domainID, "set_triggers": "", "set_keyword_triggers": []any{}})
	d, _ = s.GetDomain(ctx, s.DB(), domainID, false)
	if d.Triggers != "" || d.KeywordTriggers != "" {
		t.Errorf("triggers not cleared: %q / %q", d.Triggers, d.KeywordTriggers)
	}
	if d.Name != "Project X" || !d.Sticky() || d.Summary != "new summary" || !reflect.DeepEqual(d.State, wantState) || d.Version != 3 {
		t.Errorf("clearing triggers disturbed other fields: %+v", d)
	}

	// One list at a time: an empty list clears only that list.
	hs.ok(t, "domain_update", map[string]any{"id": domainID, "set_blockers": []any{}})
	d, _ = s.GetDomain(ctx, s.DB(), domainID, false)
	if len(d.State.Blockers) != 0 || !reflect.DeepEqual(d.State.NextActions, []string{"a1", "a2"}) || !reflect.DeepEqual(d.State.Constraints, []string{"c1"}) {
		t.Errorf("clearing blockers disturbed the other lists: %+v", d.State)
	}

	hs.ok(t, "domain_update", map[string]any{"id": domainID, "set_sticky": false})
	if d, _ = s.GetDomain(ctx, s.DB(), domainID, false); d.Sticky() {
		t.Error("set_sticky=false left the domain sticky")
	}

	// An empty set_summary clears the summary, like the other set_* fields;
	// an empty set_name is refused, since a domain cannot be nameless.
	hs.ok(t, "domain_update", map[string]any{"id": domainID, "set_summary": ""})
	if d, _ = s.GetDomain(ctx, s.DB(), domainID, false); d.Summary != "" {
		t.Errorf("set_summary \"\" left the summary %q", d.Summary)
	}
	hs.fail(t, "domain_update", map[string]any{"id": domainID, "set_name": ""}, "set_name must not be empty")
	hs.fail(t, "domain_update", map[string]any{"id": domainID, "set_name": "  "}, "set_name must not be empty")
	if d, _ = s.GetDomain(ctx, s.DB(), domainID, false); d.Name != "Project X" {
		t.Errorf("a rejected empty rename changed the name to %q", d.Name)
	}

	hs.fail(t, "domain_update", map[string]any{"id": domainID},
		"nothing to update (set_name, set_summary, set_sticky, set_triggers, set_keyword_triggers, or a list field)")
	hs.createDomain(t, map[string]any{"name": "Other"})
	hs.fail(t, "domain_update", map[string]any{"id": domainID, "set_name": " other "},
		"a domain named that already exists — pick a unique name")
	if d, _ = s.GetDomain(ctx, s.DB(), domainID, false); d.Name != "Project X" {
		t.Errorf("a rejected rename changed the name to %q", d.Name)
	}
	hs.fail(t, "domain_update", map[string]any{"id": "dZZZZZ", "set_summary": "x"}, "dZZZZZ not found")
}

// memory_create with a domain_hint lands in the named domain, creating it
// once and reusing it (case- and whitespace-insensitively) after that; a
// domain_id wins over a hint; no domain at all means General.
func TestCreateWithDomainHintLandsInHintedDomain(t *testing.T) {
	hs := newHarness(t, nil)
	got := hs.ok(t, "memory_create", map[string]any{"domain_hint": "New Project", "type": "preference", "text": "use tabs"})
	memoryID, domainID := extractID(t, got, "h"), extractID(t, got, "d")
	if want := fmt.Sprintf("Stored memory %s in domain %s (type=preference).", memoryID, domainID); got != want {
		t.Errorf("create = %q, want %q", got, want)
	}
	s := hs.store(t)
	d, err := s.DomainByName(ctx, s.DB(), "New Project")
	if err != nil {
		t.Fatalf("hinted domain was not created: %v", err)
	}
	if d.ID != domainID || d.Sticky() || d.Status != store.StatusActive || d.Summary != "" {
		t.Errorf("hinted domain = %+v, want a plain non-sticky active domain with id %s", d, domainID)
	}
	m, _ := s.GetMemory(ctx, s.DB(), memoryID)
	if m.DomainID != d.ID || m.Type != store.TypePreference || m.Origin != store.OriginChat || m.Confidence != 0.9 || m.Status != store.StatusActive {
		t.Errorf("memory = %+v", m)
	}

	got = hs.ok(t, "memory_create", map[string]any{"domain_hint": "  new project ", "type": "fact", "text": "second"})
	if extractID(t, got, "d") != domainID {
		t.Errorf("second hint did not reuse the domain: %s", got)
	}
	doms, _ := s.ListDomains(ctx, s.DB())
	named := 0
	for _, dom := range doms {
		if strings.EqualFold(strings.TrimSpace(dom.Name), "new project") {
			named++
		}
	}
	if named != 1 || len(doms) != 2 {
		t.Errorf("domains named 'new project' = %d of %d total, want exactly 1 of 2", named, len(doms))
	}
	if mems, _ := s.ListMemories(ctx, s.DB(), domainID); len(mems) != 2 {
		t.Errorf("hinted domain holds %d memories, want 2", len(mems))
	}

	hs.fail(t, "memory_create", map[string]any{"domain_id": "dZZZZZ", "type": "fact", "text": "x"}, "dZZZZZ not found")

	got = hs.ok(t, "memory_create", map[string]any{"domain_id": domainID, "domain_hint": "Ignored", "type": "fact", "text": "y"})
	if extractID(t, got, "d") != domainID {
		t.Errorf("domain_id did not win over domain_hint: %s", got)
	}
	if _, err := s.DomainByName(ctx, s.DB(), "Ignored"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a hint beside a domain_id created a domain (err=%v)", err)
	}

	gen, _ := s.GeneralDomain(ctx, s.DB())
	got = hs.ok(t, "memory_create", map[string]any{"type": "fact", "text": "z"})
	if extractID(t, got, "d") != gen.ID {
		t.Errorf("no domain given did not land in General: %s", got)
	}
}

// domain_create with sticky=true makes a sticky domain, and domain_list marks
// it; the list honours the status filter, and an unknown status value is
// refused with the allowed values named.
func TestDomainCreateStickyAndList(t *testing.T) {
	hs := newHarness(t, nil)
	got := hs.ok(t, "domain_create", map[string]any{"name": "Always", "sticky": true, "summary": "always on"})
	alwaysID := extractID(t, got, "d")
	if want := fmt.Sprintf("Created domain %s (name=\"Always\", sticky=true).", alwaysID); got != want {
		t.Errorf("create = %q, want %q", got, want)
	}
	s := hs.store(t)
	if d, _ := s.GetDomain(ctx, s.DB(), alwaysID, false); !d.Sticky() {
		t.Error("sticky=true did not make the domain sticky")
	}
	got = hs.ok(t, "domain_create", map[string]any{"name": "Topic"})
	topicID := extractID(t, got, "d")
	if !strings.HasSuffix(got, "(name=\"Topic\", sticky=false).") {
		t.Errorf("default create = %q, want sticky=false", got)
	}
	doneID := hs.createDomain(t, map[string]any{"name": "Done"})
	if got := hs.ok(t, "domain_archive", map[string]any{"id": doneID}); got != fmt.Sprintf("Archived domain %s.", doneID) {
		t.Errorf("archive = %q", got)
	}
	gen, _ := s.GeneralDomain(ctx, s.DB())

	// No filter: everything, sticky ones marked, missing summaries named.
	got = hs.ok(t, "domain_list", nil)
	if !strings.HasPrefix(got, "4 domain(s):\n") {
		t.Errorf("unfiltered list header: %q", got)
	}
	for _, line := range []string{
		fmt.Sprintf("  %s · General [sticky] · Global rules, preferences, and standing facts. · active\n", gen.ID),
		fmt.Sprintf("  %s · Always [sticky] · always on · active\n", alwaysID),
		fmt.Sprintf("  %s · Topic · (no summary) · active\n", topicID),
		fmt.Sprintf("  %s · Done · (no summary) · archived\n", doneID),
	} {
		if !strings.Contains(got, line) {
			t.Errorf("unfiltered list lacks %q:\n%s", line, got)
		}
	}
	if strings.Count(got, "\n") != 5 {
		t.Errorf("unfiltered list has %d lines, want header + 4:\n%s", strings.Count(got, "\n"), got)
	}

	got = hs.ok(t, "domain_list", map[string]any{"status": "archived"})
	if want := fmt.Sprintf("1 domain(s):\n  %s · Done · (no summary) · archived\n", doneID); got != want {
		t.Errorf("archived list = %q, want %q", got, want)
	}
	got = hs.ok(t, "domain_list", map[string]any{"status": "active"})
	if !strings.HasPrefix(got, "3 domain(s):\n") || strings.Contains(got, doneID) {
		t.Errorf("active list = %q", got)
	}

	// A status outside the enum is an error naming the allowed values, not an
	// empty listing that looks like "nothing archived".
	hs.fail(t, "domain_list", map[string]any{"status": "bogus"}, "status must be one of: active, archived")
	hs.fail(t, "domain_list", map[string]any{"status": "retired"}, "status must be one of: active, archived")
}

// domain_get renders the header with sticky/status/version, the summary,
// both trigger lines, the three state lines and the memory list exactly.
func TestDomainGetRendersEverything(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{
		"name": "Ops", "sticky": true, "summary": "ops summary",
		"triggers": "mail, github", "keyword_triggers": []any{"morning routine"},
	})
	hs.ok(t, "domain_update", map[string]any{
		"id": domainID, "set_blockers": []any{"b1", "b2"}, "set_next_actions": []any{"n1"}, "set_constraints": []any{"c1"},
	})
	want := fmt.Sprintf("Domain %s \"Ops\" (sticky=true, status=active, version=2)\n"+
		"Summary: ops summary\n"+
		"Tool triggers (auto-load on tool use): mail,github\n"+
		"Keyword triggers (auto-load when mentioned): morning routine\n"+
		"Blockers: b1; b2\n"+
		"Next actions: n1\n"+
		"Constraints: c1\n"+
		"Memories: (none active)\n", domainID)
	if got := hs.ok(t, "domain_get", map[string]any{"id": domainID}); got != want {
		t.Errorf("domain_get =\n%s\nwant\n%s", got, want)
	}

	s := hs.store(t)
	m, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: domainID, Type: store.TypeRule, Text: "keep the pager quiet",
		Status: store.StatusActive, Confidence: 0.75, FileRef: "files/oncall.md",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	ev := extractID(t, hs.ok(t, "memory_create", map[string]any{"domain_id": domainID, "type": "event", "text": "paged at 03:00"}), "h")
	got := hs.ok(t, "domain_get", map[string]any{"id": domainID})
	// Both active memories are listed — the event too, since this is the
	// operator's view of the domain, not the prompt.
	if !strings.Contains(got, "Memories (2 active):\n") {
		t.Errorf("memory header missing:\n%s", got)
	}
	if line := fmt.Sprintf("  %s [rule] (conf=0.75) keep the pager quiet [file: files/oncall.md]\n", m.ID); !strings.Contains(got, line) {
		t.Errorf("memory line %q missing:\n%s", line, got)
	}
	if line := fmt.Sprintf("  %s [event] (conf=0.90) paged at 03:00\n", ev); !strings.Contains(got, line) {
		t.Errorf("event line %q missing:\n%s", line, got)
	}

	// A bare domain renders only the header and the empty memory line.
	plain := hs.createDomain(t, map[string]any{"name": "Plain"})
	if got := hs.ok(t, "domain_get", map[string]any{"id": plain}); got != fmt.Sprintf("Domain %s \"Plain\" (sticky=false, status=active, version=1)\nMemories: (none active)\n", plain) {
		t.Errorf("plain domain_get = %q", got)
	}
	hs.fail(t, "domain_get", map[string]any{"id": "dZZZZZ"}, "dZZZZZ not found")
}

// status renders the counts of what is held, by status, and the last
// consolidation run once one is recorded.
func TestStatusRendersLastRun(t *testing.T) {
	hs := newHarness(t, nil)
	s := hs.store(t)
	started := time.Unix(1_700_000_000, 0)
	if err := s.RecordRun(ctx, s.DB(), store.Run{
		ID: "run-1", Trigger: "manual", Model: "m", Status: "ok", OpsApplied: 3, StartedAt: started,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}
	// General plus one created domain, one archived; one active memory, one
	// retired.
	hs.createDomain(t, map[string]any{"name": "Counted"})
	archived := hs.createDomain(t, map[string]any{"name": "Archived"})
	hs.ok(t, "domain_archive", map[string]any{"id": archived})
	hs.createMemory(t, map[string]any{"type": "fact", "text": "counted too"})
	retired := hs.createMemory(t, map[string]any{"type": "fact", "text": "retired"})
	hs.ok(t, "memory_retire", map[string]any{"id": retired, "reason": "counted as retired"})

	want := fmt.Sprintf("Cognitive memory database: %s (healthy)\n"+
		"Domains: 2 active (1 archived); memories: 1 active (1 retired)\n"+
		"Last consolidation run: run-1 (trigger=manual, status=ok, ops=3) at %s\n"+
		"Consolidation worker: not running\n",
		store.DBPath(hs.dir), started.Format("2006-01-02 15:04:05"))
	got := hs.ok(t, "status", nil)
	if got != want {
		t.Errorf("status =\n%s\nwant\n%s", got, want)
	}

	// A later run replaces the line: the newest by start time is reported.
	later := started.Add(time.Hour)
	if err := s.RecordRun(ctx, s.DB(), store.Run{
		ID: "run-2", Trigger: "idle", Model: "m", Status: "error", OpsApplied: 0, Error: "boom", StartedAt: later,
	}); err != nil {
		t.Fatalf("record run 2: %v", err)
	}
	got = hs.ok(t, "status", nil)
	if !strings.Contains(got, fmt.Sprintf("Last consolidation run: run-2 (trigger=idle, status=error, ops=0) at %s\n", later.Format("2006-01-02 15:04:05"))) {
		t.Errorf("status after a second run:\n%s", got)
	}
}

// The export tool's document imports into a fresh store with the domain's
// stickiness, triggers and keyword triggers intact, and reports its counts.
func TestExportRoundTripsThroughPortableImport(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{
		"name": "BioTech", "sticky": true, "summary": "the report",
		"triggers": "google_gmail, github", "keyword_triggers": []any{"biotech report", "q3"},
	})
	hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "the report targets Q3", "confidence": 0.7})
	hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "event", "text": "results published Sep 4"})

	got := hs.ok(t, "export", nil)
	if got != "Exported 2 domain(s) and 2 memory(ies) to files/MEMORY_EXPORT.yaml." {
		t.Errorf("export = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(hs.ws, "files", exportFilename))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	doc, err := portable.Unmarshal(data)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}

	dst, err := store.Open(filepath.Join(t.TempDir(), "dst.cogmem.db"))
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer func() { _ = dst.Close() }()
	res, err := portable.Import(ctx, dst, doc, portable.ImportReplace)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.DomainsCreated != 2 || res.MemoriesCreated != 2 {
		t.Errorf("import result = %+v, want 2 domains and 2 memories created", res)
	}
	d, err := dst.DomainByName(ctx, dst.DB(), "BioTech")
	if err != nil {
		t.Fatalf("imported domain: %v", err)
	}
	if !d.Sticky() || d.Summary != "the report" || d.Triggers != "google_gmail,github" || d.KeywordTriggers != "biotech report,q3" {
		t.Errorf("imported domain = sticky %v summary %q triggers %q keywords %q",
			d.Sticky(), d.Summary, d.Triggers, d.KeywordTriggers)
	}
	mems, _ := dst.ListMemories(ctx, dst.DB(), d.ID)
	if len(mems) != 2 {
		t.Fatalf("imported memories = %d, want 2", len(mems))
	}
	for _, m := range mems {
		switch m.Text {
		case "the report targets Q3":
			if m.Type != store.TypeFact || m.Confidence != 0.7 || m.Origin != store.OriginChat {
				t.Errorf("fact = %+v", m)
			}
		case "results published Sep 4":
			if m.Type != store.TypeEvent || m.Confidence != 0.9 {
				t.Errorf("event = %+v", m)
			}
		default:
			t.Errorf("unexpected memory %q", m.Text)
		}
	}
	// A second export overwrites the file in place.
	hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "third"})
	if got := hs.ok(t, "export", nil); got != "Exported 2 domain(s) and 3 memory(ies) to files/MEMORY_EXPORT.yaml." {
		t.Errorf("second export = %q", got)
	}
}
