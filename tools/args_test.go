// cogmem - Cognitive Memory
// License: MIT

package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// Every handler's required-argument check produces its documented error. A
// blank string counts as missing (arguments are trimmed).
func TestMissingRequiredArgs(t *testing.T) {
	hs := newHarness(t, nil)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"domain_get", nil, "id is required"},
		{"domain_get", map[string]any{"id": "   "}, "id is required"},
		{"memory_search", nil, "query is required"},
		{"memory_search", map[string]any{"query": " "}, "query is required"},
		{"explain", nil, "id is required"},
		{"memory_create", nil, "type and text are required"},
		{"memory_create", map[string]any{"text": "t"}, "type and text are required"},
		{"memory_create", map[string]any{"type": "fact"}, "type and text are required"},
		{"memory_create", map[string]any{"type": "fact", "text": "  "}, "type and text are required"},
		{"domain_update", nil, "id is required"},
		{"memory_attach", nil, "id is required"},
		{"memory_retire", nil, "id and reason are required"},
		{"memory_retire", map[string]any{"id": "hAAAAA"}, "id and reason are required"},
		{"memory_retire", map[string]any{"reason": "r"}, "id and reason are required"},
		{"domain_create", nil, "name is required"},
		{"domain_create", map[string]any{"name": ""}, "name is required"},
		{"domain_archive", nil, "id is required"},
		{"domain_migrate", nil, "from and to domain ids are required"},
		{"domain_migrate", map[string]any{"from": "dAAAAA"}, "from and to domain ids are required"},
		{"domain_migrate", map[string]any{"to": "dAAAAA"}, "from and to domain ids are required"},
		{"memory_forget", nil, "query is required"},
	} {
		hs.fail(t, tc.tool, tc.args, tc.want)
	}
	// The required-arg check comes first: a non-string id is "missing", not
	// "not found".
	hs.fail(t, "domain_get", map[string]any{"id": 42}, "id is required")
}

// A host with no workspace has nowhere to write the export.
func TestExportWithoutWorkspace(t *testing.T) {
	hs := newHarness(t, func(h *Host) { h.Workspace = "" })
	hs.fail(t, "export", nil, "export is unavailable: the host configured no workspace to write into")
}

// Unknown ids are reported as "<id> not found" by every handler that takes
// one, and nothing is changed.
func TestUnknownIDs(t *testing.T) {
	hs := newHarness(t, nil)
	real := hs.createDomain(t, map[string]any{"name": "Real"})
	hs.fail(t, "domain_get", map[string]any{"id": "dZZZZZ"}, "dZZZZZ not found")
	hs.fail(t, "domain_archive", map[string]any{"id": "dZZZZZ"}, "dZZZZZ not found")
	hs.fail(t, "domain_update", map[string]any{"id": "dZZZZZ", "set_summary": "x"}, "dZZZZZ not found")
	// The id is checked before the nothing-to-update rule.
	hs.fail(t, "domain_update", map[string]any{"id": "dZZZZZ"}, "dZZZZZ not found")
	hs.fail(t, "memory_retire", map[string]any{"id": "hZZZZZ", "reason": "r"}, "hZZZZZ not found")
	hs.fail(t, "memory_attach", map[string]any{"id": "hZZZZZ"}, "hZZZZZ not found")
	hs.fail(t, "memory_create", map[string]any{"domain_id": "dZZZZZ", "type": "fact", "text": "x"}, "dZZZZZ not found")
	hs.fail(t, "domain_migrate", map[string]any{"from": "dZZZZZ", "to": real}, "dZZZZZ not found")
	// An unknown destination is named as such, not blamed on the source.
	hs.fail(t, "domain_migrate", map[string]any{"from": real, "to": "dYYYYY"}, "dYYYYY not found")
	hs.fail(t, "domain_migrate", map[string]any{"from": real, "to": real}, "cogmem: from and to domains are the same")
	hs.fail(t, "memory_forget", map[string]any{"query": "x", "domain_id": "dZZZZZ"}, "dZZZZZ not found")

	s := hs.store(t)
	if d, err := s.GetDomain(ctx, s.DB(), real, false); err != nil || d.Status != store.StatusActive || d.Version != 1 {
		t.Errorf("the real domain was touched by failed calls: %+v err=%v", d, err)
	}
	// Archiving twice is not an error: the second call is a no-op.
	hs.ok(t, "domain_archive", map[string]any{"id": real})
	hs.ok(t, "domain_archive", map[string]any{"id": real})
	if d, _ := s.GetDomain(ctx, s.DB(), real, false); d.Status != store.StatusArchived || d.Version != 2 {
		t.Errorf("after two archives: %+v, want archived at version 2", d)
	}
}

// memory_forget honours the domain filter, reports the retired ids sorted,
// reaches events, records the query as the retire reason, and says so when
// nothing matched.
func TestForgetWithDomainFilterAndNothingRetired(t *testing.T) {
	hs := newHarness(t, nil)
	d1 := hs.createDomain(t, map[string]any{"name": "One"})
	d2 := hs.createDomain(t, map[string]any{"name": "Two"})
	one := hs.createMemory(t, map[string]any{"domain_id": d1, "type": "fact", "text": "shared phrase one"})
	ev := hs.createMemory(t, map[string]any{"domain_id": d1, "type": "event", "text": "shared phrase event"})
	two := hs.createMemory(t, map[string]any{"domain_id": d2, "type": "fact", "text": "shared phrase two"})
	s := hs.store(t)

	if got := hs.ok(t, "memory_forget", map[string]any{"query": "shared phrase", "domain_id": d2}); got != fmt.Sprintf("Retired 1 memories: %s.", two) {
		t.Errorf("filtered forget = %q", got)
	}
	if m, _ := s.GetMemory(ctx, s.DB(), two); m.Status != store.StatusRetired || m.RetireReason == nil || *m.RetireReason != "forget: shared phrase" {
		t.Errorf("forgotten memory = %+v", m)
	}
	for _, id := range []string{one, ev} {
		if m, _ := s.GetMemory(ctx, s.DB(), id); m.Status != store.StatusActive {
			t.Errorf("%s in the unfiltered domain was retired", id)
		}
	}

	if got := hs.ok(t, "memory_forget", map[string]any{"query": "nothing here"}); got != `No active memories matched "nothing here"; nothing retired.` {
		t.Errorf("no match = %q", got)
	}
	// An unknown domain filter is an error, not a silent "nothing matched".
	hs.fail(t, "memory_forget", map[string]any{"query": "shared phrase", "domain_id": "dZZZZZ"}, "dZZZZZ not found")

	// Unfiltered: the remaining two go, the event included, ids sorted.
	ids := []string{one, ev}
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	if got := hs.ok(t, "memory_forget", map[string]any{"query": "SHARED phrase"}); got != fmt.Sprintf("Retired 2 memories: %s, %s.", ids[0], ids[1]) {
		t.Errorf("unfiltered forget = %q", got)
	}
	if got := hs.ok(t, "memory_search", map[string]any{"query": "shared phrase", "include_events": true}); got != `No active memories match "shared phrase".` {
		t.Errorf("search after forget = %q", got)
	}
}

// memory_forget retires every match, not a first page of them: a query
// matching more memories than any search limit retires them all and reports
// the true count.
func TestForgetRetiresEveryMatch(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{"name": "Bulk"})
	s := hs.store(t)
	const n = 105
	for i := 0; i < n; i++ {
		if _, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
			DomainID: domainID, Type: store.TypeFact, Text: fmt.Sprintf("bulk item %03d", i),
			Status: store.StatusActive, Confidence: 0.9,
		}); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	got := hs.ok(t, "memory_forget", map[string]any{"query": "bulk item"})
	if !strings.HasPrefix(got, fmt.Sprintf("Retired %d memories: ", n)) {
		t.Errorf("forget = %.60q..., want all %d retired", got, n)
	}
	if left, _ := s.ListMemories(ctx, s.DB(), domainID, store.StatusActive); len(left) != 0 {
		t.Errorf("%d memories still active after forget", len(left))
	}
	if retired, _ := s.ListMemories(ctx, s.DB(), domainID, store.StatusRetired); len(retired) != n {
		t.Errorf("%d memories retired, want %d", len(retired), n)
	}
}

// limit and confidence reach the store: a limit of 1 caps the results at the
// highest-confidence match, and the confidence given is what is stored.
// Both the float64 (JSON) and int forms are accepted.
func TestSearchLimitAndConfidenceArePassed(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{"name": "Caps"})
	low := hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "cap one", "confidence": 0.3})
	mid := hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "cap two", "confidence": 0.6})
	top := hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "cap three", "confidence": 1})
	s := hs.store(t)
	for id, want := range map[string]float64{low: 0.3, mid: 0.6, top: 1} {
		if m, _ := s.GetMemory(ctx, s.DB(), id); m.Confidence != want {
			t.Errorf("%s confidence = %v, want %v", id, m.Confidence, want)
		}
	}

	got := hs.ok(t, "memory_search", map[string]any{"query": "cap", "limit": float64(1)})
	if want := fmt.Sprintf("1 active memories matching \"cap\":\n  %s [fact] (domain=%s, conf=1.00) cap three\n", top, domainID); got != want {
		t.Errorf("limit 1 =\n%s\nwant\n%s", got, want)
	}
	got = hs.ok(t, "memory_search", map[string]any{"query": "cap", "limit": 2})
	if !strings.HasPrefix(got, "2 active memories matching \"cap\":\n") || strings.Contains(got, low) {
		t.Errorf("limit 2 =\n%s", got)
	}
	got = hs.ok(t, "memory_search", map[string]any{"query": "cap"})
	if !strings.HasPrefix(got, "3 active memories matching \"cap\":\n") {
		t.Errorf("no limit =\n%s", got)
	}
	// A non-positive limit falls back to the store default rather than
	// returning nothing.
	if got := hs.ok(t, "memory_search", map[string]any{"query": "cap", "limit": 0}); !strings.HasPrefix(got, "3 active memories") {
		t.Errorf("limit 0 =\n%s", got)
	}
	// int64 is accepted for both numeric arguments as well.
	if got := hs.ok(t, "memory_search", map[string]any{"query": "cap", "limit": int64(1)}); !strings.HasPrefix(got, "1 active memories") {
		t.Errorf("limit int64 =\n%s", got)
	}
	i64 := hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "cap int64", "confidence": int64(1)})
	if m, _ := s.GetMemory(ctx, s.DB(), i64); m.Confidence != 1 {
		t.Errorf("int64 confidence stored %v, want 1", m.Confidence)
	}
	if got := hs.ok(t, "explain", map[string]any{"id": low}); !strings.Contains(got, "confidence=0.30") {
		t.Errorf("explain confidence:\n%s", got)
	}
}

// A wrongly-typed argument is an error naming the argument and the type it
// wants, never a silent default: a model that sends include_events as "true"
// or set_sticky as "true" would otherwise get the opposite of what it asked
// for. Nothing is changed by a rejected call; an absent argument still takes
// its default.
func TestWrongTypedArgsAreRejected(t *testing.T) {
	hs := newHarness(t, nil)
	domainID := hs.createDomain(t, map[string]any{"name": "Typed", "keyword_triggers": []any{"keep me"}})
	hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "widget standing"})
	hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "event", "text": "widget shipped"})
	s := hs.store(t)

	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"memory_search", map[string]any{"query": "widget", "include_events": "true"}, "include_events must be a boolean"},
		{"memory_search", map[string]any{"query": "widget", "include_events": 1}, "include_events must be a boolean"},
		{"memory_search", map[string]any{"query": "widget", "limit": "5"}, "limit must be an integer"},
		{"memory_search", map[string]any{"query": "widget", "limit": true}, "limit must be an integer"},
		{"memory_search", map[string]any{"query": "widget", "limit": 5.5}, "limit must be an integer"},
		{"memory_create", map[string]any{"domain_id": domainID, "type": "fact", "text": "conf as string", "confidence": "0.3"}, "confidence must be a number"},
		{"domain_update", map[string]any{"id": domainID, "set_sticky": "true"}, "set_sticky must be a boolean"},
		{"domain_update", map[string]any{"id": domainID, "set_blockers": "x"}, "set_blockers must be a list of strings"},
		{"domain_update", map[string]any{"id": domainID, "set_next_actions": []any{"ok", 2}}, "set_next_actions must be a list of strings"},
		{"domain_update", map[string]any{"id": domainID, "set_constraints": "x"}, "set_constraints must be a list of strings"},
		{"domain_update", map[string]any{"id": domainID, "set_keyword_triggers": "x"}, "set_keyword_triggers must be a list of strings"},
		{"domain_create", map[string]any{"name": "StringSticky", "sticky": "true"}, "sticky must be a boolean"},
		{"domain_create", map[string]any{"name": "StringKeywords", "keyword_triggers": "x"}, "keyword_triggers must be a list of strings"},
	} {
		hs.fail(t, tc.tool, tc.args, tc.want)
	}

	// The rejected calls changed nothing: the domain is as created, no memory
	// was stored and no domain was created.
	d, _ := s.GetDomain(ctx, s.DB(), domainID, false)
	if d.Version != 1 || d.Sticky() || d.KeywordTriggers != "keep me" || len(d.State.Blockers) != 0 {
		t.Errorf("a rejected update changed the domain: %+v", d)
	}
	if hits, _ := s.SearchMemories(ctx, s.DB(), "conf as string", 10, true); len(hits) != 0 {
		t.Errorf("a rejected create stored the memory: %+v", hits)
	}
	if doms, _ := s.ListDomains(ctx, s.DB()); len(doms) != 2 {
		t.Errorf("domains = %d, want 2 (General + Typed): a rejected create made one", len(doms))
	}

	// Absent arguments still take their defaults: events excluded, limit 20,
	// confidence 0.9, sticky off.
	got := hs.ok(t, "memory_search", map[string]any{"query": "widget"})
	if !strings.HasPrefix(got, "1 active memories") || strings.Contains(got, "widget shipped") {
		t.Errorf("search with no include_events:\n%s", got)
	}
	id := hs.createMemory(t, map[string]any{"domain_id": domainID, "type": "fact", "text": "default confidence"})
	if m, _ := s.GetMemory(ctx, s.DB(), id); m.Confidence != 0.9 {
		t.Errorf("default confidence = %v, want 0.9", m.Confidence)
	}
}

// domain_create refuses a name in use (case-insensitively, trimmed) with a
// message that names it, and creates nothing.
func TestDomainCreateDuplicateName(t *testing.T) {
	hs := newHarness(t, nil)
	hs.createDomain(t, map[string]any{"name": "BioTech"})
	hs.fail(t, "domain_create", map[string]any{"name": "  biotech "},
		"a domain named \"biotech\" already exists — use it, rename it, or pick a unique name")
	hs.fail(t, "domain_create", map[string]any{"name": "general"},
		"a domain named \"general\" already exists — use it, rename it, or pick a unique name")
	s := hs.store(t)
	if doms, _ := s.ListDomains(ctx, s.DB()); len(doms) != 2 {
		t.Errorf("domains = %d, want 2 (General + BioTech)", len(doms))
	}
}

// memory_create with no domain re-creates General if the user deleted it, so
// there is always a default home, and it is sticky again.
func TestCreateRecreatesDeletedGeneral(t *testing.T) {
	hs := newHarness(t, nil)
	s := hs.store(t)
	gen, err := s.GeneralDomain(ctx, s.DB())
	if err != nil {
		t.Fatalf("seeded General: %v", err)
	}
	if err := s.DeleteDomain(ctx, s.DB(), gen.ID); err != nil {
		t.Fatalf("delete General: %v", err)
	}
	got := hs.ok(t, "memory_create", map[string]any{"type": "fact", "text": "needs a home"})
	newID := extractID(t, got, "d")
	if newID == gen.ID {
		t.Fatalf("memory landed in the deleted General %s", gen.ID)
	}
	re, err := s.GeneralDomain(ctx, s.DB())
	if err != nil || re.ID != newID || !re.Sticky() || re.Summary != "Global rules, preferences, and standing facts." {
		t.Errorf("re-created General = %+v err=%v", re, err)
	}
	if mems, _ := s.ListMemories(ctx, s.DB(), newID); len(mems) != 1 || mems[0].Text != "needs a home" {
		t.Errorf("memories in the new General = %+v", mems)
	}
}

// memory_search reports an empty result in words, and trims the query.
func TestSearchNoMatch(t *testing.T) {
	hs := newHarness(t, nil)
	if got := hs.ok(t, "memory_search", map[string]any{"query": "  nothing  "}); got != `No active memories match "nothing".` {
		t.Errorf("no match = %q", got)
	}
}

// memory_retire records the reason and retires exactly that memory.
func TestRetireRecordsReason(t *testing.T) {
	hs := newHarness(t, nil)
	keep := hs.createMemory(t, map[string]any{"type": "fact", "text": "keep"})
	gone := hs.createMemory(t, map[string]any{"type": "fact", "text": "gone"})
	if got := hs.ok(t, "memory_retire", map[string]any{"id": gone, "reason": "no longer true"}); got != fmt.Sprintf("Retired memory %s.", gone) {
		t.Errorf("retire = %q", got)
	}
	s := hs.store(t)
	if m, _ := s.GetMemory(ctx, s.DB(), gone); m.Status != store.StatusRetired || m.RetireReason == nil || *m.RetireReason != "no longer true" {
		t.Errorf("retired = %+v", m)
	}
	if m, _ := s.GetMemory(ctx, s.DB(), keep); m.Status != store.StatusActive {
		t.Errorf("the other memory was retired: %+v", m)
	}
	// Retiring again is accepted and keeps the newer reason.
	hs.ok(t, "memory_retire", map[string]any{"id": gone, "reason": "still gone"})
	if m, _ := s.GetMemory(ctx, s.DB(), gone); *m.RetireReason != "still gone" {
		t.Errorf("second retire reason = %q", *m.RetireReason)
	}
}

// domain_migrate reports the count moved and the deletion.
func TestDomainMigrateReportsCount(t *testing.T) {
	hs := newHarness(t, nil)
	from := hs.createDomain(t, map[string]any{"name": "From"})
	to := hs.createDomain(t, map[string]any{"name": "To"})
	a := hs.createMemory(t, map[string]any{"domain_id": from, "type": "fact", "text": "a"})
	b := hs.createMemory(t, map[string]any{"domain_id": from, "type": "event", "text": "b"})
	if got := hs.ok(t, "domain_migrate", map[string]any{"from": from, "to": to}); got != fmt.Sprintf("Migrated 2 mem(s) from domain %s into %s; %s deleted.", from, to, from) {
		t.Errorf("migrate = %q", got)
	}
	s := hs.store(t)
	for _, id := range []string{a, b} {
		if m, _ := s.GetMemory(ctx, s.DB(), id); m.DomainID != to {
			t.Errorf("%s is in %s, want %s", id, m.DomainID, to)
		}
	}
	hs.fail(t, "domain_get", map[string]any{"id": from}, from+" not found")
}
