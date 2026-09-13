// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validID checks the 6-char id format: 1-char prefix + idRandomLen Crockford chars.
func validID(id, prefix string) bool {
	if len(id) != 1+idRandomLen || id[:1] != prefix {
		return false
	}
	for _, c := range id[1:] {
		if !strings.ContainsRune(crockfordAlphabet, c) {
			return false
		}
	}
	return true
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.cogmem.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestOpenMigrateSeeds verifies a brand-new DB seeds a single sticky "General"
// domain (which bumps stable_rev to 1).
func TestOpenMigrateSeeds(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	rev, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	if rev != 1 {
		t.Fatalf("initial stable_rev = %d, want 1 (seeded General)", rev)
	}
	g, err := s.GeneralDomain(ctx, s.DB())
	if err != nil {
		t.Fatalf("general domain: %v", err)
	}
	if !g.Sticky() {
		t.Fatalf("seeded General is not sticky")
	}
}

func TestDomainCreateAndIDs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d1, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		AgentID: "alice", SessionKey: "agent:alice:main",
		Name: "Website Redesign", Status: StatusActive,
		Summary: "CSS grid migration",
	})
	if err != nil {
		t.Fatalf("create d1: %v", err)
	}
	if !validID(d1.ID, "d") {
		t.Fatalf("first domain id = %q, want d+5 chars", d1.ID)
	}
	d2, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		AgentID: "alice", SessionKey: "agent:alice:main",
		Name: "BioTech",
	})
	if err != nil {
		t.Fatalf("create d2: %v", err)
	}
	if !validID(d2.ID, "d") || d2.ID == d1.ID {
		t.Fatalf("second domain id = %q (d1=%q): want distinct d+5 chars", d2.ID, d1.ID)
	}
	// stable_rev: 1 (seeded General) + 2 (one bump per create) = 3.
	if rev, _ := s.StableRev(ctx); rev != 3 {
		t.Fatalf("stable_rev = %d, want 3", rev)
	}
	got, err := s.GetDomain(ctx, s.DB(), d1.ID, false)
	if err != nil || got.Name != "Website Redesign" {
		t.Fatalf("get d1: %v name=%q", err, got.Name)
	}
}

// TestCreateDuplicateNameRejected confirms domain names are unique
// (case-insensitive, trimmed): a second create with the same name is rejected.
func TestCreateDuplicateNameRejected(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Dup"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "  dup  "}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("duplicate create err = %v, want ErrDuplicateName", err)
	}
	// The seeded "General" name is also taken.
	if _, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "general"}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("duplicate General err = %v, want ErrDuplicateName", err)
	}
}

func TestDomainTriggersRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		AgentID: "a", Name: "Email",
		Triggers: "  Google_Gmail, microsoft365_mail ,, system  ",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Stored normalized: trimmed, lowercased, empties dropped.
	if d.Triggers != "google_gmail,microsoft365_mail,system" {
		t.Fatalf("triggers = %q, want normalized", d.Triggers)
	}
	// Case-insensitive substring match against a full tool name.
	if tok, ok := d.MatchTrigger("mcp__fusion__google_gmail_messages_list"); !ok || tok != "google_gmail" {
		t.Fatalf("MatchTrigger gmail = %q,%v", tok, ok)
	}
	if tok, ok := d.MatchTrigger("mcp__fusion__system__get"); !ok || tok != "system" {
		t.Fatalf("MatchTrigger system = %q,%v", tok, ok)
	}
	if _, ok := d.MatchTrigger("web_fetch"); ok {
		t.Fatalf("web_fetch should not match")
	}

	// Update replaces triggers; empty clears them.
	empty := ""
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Triggers: &empty}); err != nil {
		t.Fatalf("update clear: %v", err)
	}
	got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if got.Triggers != "" || len(got.TriggerTokens()) != 0 {
		t.Fatalf("triggers not cleared: %q", got.Triggers)
	}
}

// TestDomainTriggerWildcardsAreDecorative confirms '*' wrappers are stripped and
// behave identically to the bare substring (matching is always "contains").
func TestDomainTriggerWildcardsAreDecorative(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		AgentID: "a", Name: "Dev",
		Triggers: "*github*, *mail*",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Stored form has the asterisks stripped — same as the bare words.
	if d.Triggers != "github,mail" {
		t.Fatalf("triggers = %q, want %q", d.Triggers, "github,mail")
	}
	// "*github*" matches an MCP tool from the github server.
	if tok, ok := d.MatchTrigger("mcp_github_search"); !ok || tok != "github" {
		t.Fatalf("MatchTrigger github = %q,%v", tok, ok)
	}
	// "*mail*" matches anywhere in the name.
	if _, ok := d.MatchTrigger("google_gmail_send"); !ok {
		t.Fatalf("*mail* should match google_gmail_send")
	}
	if _, ok := d.MatchTrigger("web_fetch"); ok {
		t.Fatalf("web_fetch should not match")
	}
}

func TestTriggerUnderscoreInsensitive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// Token written with double underscores is collapsed on store.
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		AgentID: "a", Name: "M", Triggers: "fusion__google",
	})
	if d.Triggers != "fusion_google" {
		t.Fatalf("triggers = %q, want collapsed fusion_google", d.Triggers)
	}
	// Matches whether the tool name uses single or double underscores.
	for _, name := range []string{
		"mcp__fusion__google_gmail_messages_list", // double-underscore MCP separators
		"mcp_fusion_google_x",                     // single underscore
	} {
		if tok, ok := d.MatchTrigger(name); !ok || tok != "fusion_google" {
			t.Fatalf("MatchTrigger(%q) = %q,%v", name, tok, ok)
		}
	}
}

// TestNormalizeLegacyTypes confirms migrate() folds legacy values: retired memory
// types collapse to "fact", the legacy domain "type" string becomes a sticky-
// priority int (old "general" → sticky, any other string → not sticky).
func TestNormalizeLegacyTypes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Two domains + a memory, then force legacy string values directly.
	topic, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "P"})
	legacyGen, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "LegacyGlobal"})
	m, _ := s.AddMemory(ctx, s.DB(), AddMemoryParams{DomainID: topic.ID, Type: TypeFact, Text: "x", Status: StatusActive, Confidence: 0.9})
	if _, err := s.DB().ExecContext(ctx, `UPDATE memories SET type='lesson' WHERE id=?`, m.ID); err != nil {
		t.Fatalf("force legacy memory type: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE domains SET type='repo' WHERE id=?`, topic.ID); err != nil {
		t.Fatalf("force legacy domain type: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE domains SET type='general' WHERE id=?`, legacyGen.ID); err != nil {
		t.Fatalf("force legacy general type: %v", err)
	}
	_ = s.Close()

	// Reopen → migrate() normalizes the legacy values.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	gm, _ := s2.GetMemory(ctx, s2.DB(), m.ID)
	if gm.Type != TypeFact {
		t.Fatalf("memory type = %q, want fact", gm.Type)
	}
	gd, _ := s2.GetDomain(ctx, s2.DB(), topic.ID, false)
	if gd.Sticky() {
		t.Fatalf("legacy 'repo' domain became sticky, want not sticky")
	}
	gg, _ := s2.GetDomain(ctx, s2.DB(), legacyGen.ID, false)
	if !gg.Sticky() {
		t.Fatalf("legacy 'general' domain not sticky after migrate")
	}
}

func TestDedupeActiveMemories(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{AgentID: "a", Name: "P"})
	other, _ := s.CreateDomain(ctx, db, CreateDomainParams{AgentID: "a", Name: "Q"})

	add := func(domainID, text string) {
		_, _ = s.AddMemory(ctx, db, AddMemoryParams{DomainID: domainID, Type: TypeFact, Text: text, Status: StatusActive, Confidence: 0.9})
	}
	// Three identical "The Frame" in domain d (the runaway-loop case) + one unique.
	add(d.ID, "The Frame")
	add(d.ID, "The Frame")
	add(d.ID, "The Frame")
	add(d.ID, "unique fact")
	// Same text in a DIFFERENT domain must NOT be treated as a duplicate.
	add(other.ID, "The Frame")

	n, err := s.DedupeActiveMemories(ctx)
	if err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if n != 2 {
		t.Fatalf("retired = %d, want 2 (two of the three dups)", n)
	}
	// d keeps exactly one "The Frame" + the unique fact.
	act, _ := s.ListMemories(ctx, db, d.ID, StatusActive)
	frames := 0
	for _, m := range act {
		if m.Text == "The Frame" {
			frames++
		}
	}
	if frames != 1 || len(act) != 2 {
		t.Fatalf("domain d active = %d (frames=%d), want 2 (1 frame + 1 unique)", len(act), frames)
	}
	// Other domain's "The Frame" is untouched.
	oa, _ := s.ListMemories(ctx, db, other.ID, StatusActive)
	if len(oa) != 1 {
		t.Fatalf("other domain active = %d, want 1", len(oa))
	}
	// Idempotent.
	if n2, _ := s.DedupeActiveMemories(ctx); n2 != 0 {
		t.Fatalf("second dedupe retired %d, want 0", n2)
	}
}

// TestSeedGeneralOnce confirms the sticky "General" domain is seeded only on a
// brand-new DB: once the user deletes it, reopening does NOT bring it back.
func TestSeedGeneralOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seed.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	g, err := s.GeneralDomain(ctx, s.DB())
	if err != nil {
		t.Fatalf("seeded general missing: %v", err)
	}
	if err := s.DeleteDomain(ctx, s.DB(), g.ID); err != nil {
		t.Fatalf("delete general: %v", err)
	}
	_ = s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GeneralDomain(ctx, s2.DB()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("General was re-seeded after deletion (err=%v); seed must run only once", err)
	}
}

// TestMigrateDomain confirms MigrateDomain moves all memories into the target and
// deletes the source domain.
func TestMigrateDomain(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	from, _ := s.CreateDomain(ctx, db, CreateDomainParams{AgentID: "a", Name: "From"})
	to, _ := s.CreateDomain(ctx, db, CreateDomainParams{AgentID: "a", Name: "To"})
	add := func(domainID, text string) {
		_, _ = s.AddMemory(ctx, db, AddMemoryParams{DomainID: domainID, Type: TypeFact, Text: text, Status: StatusActive, Confidence: 0.9})
	}
	add(from.ID, "a")
	add(from.ID, "b")
	add(to.ID, "c")

	moved, err := s.MigrateDomain(ctx, db, from.ID, to.ID)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	if _, err := s.GetDomain(ctx, db, from.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source domain survived migrate (err=%v)", err)
	}
	mems, _ := s.ListMemories(ctx, db, to.ID, StatusActive)
	if len(mems) != 3 {
		t.Fatalf("target active memories = %d, want 3", len(mems))
	}
}

func TestPurgeNonActive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()

	// Active topic domain with an active + a retired memory.
	keep, _ := s.CreateDomain(ctx, db, CreateDomainParams{AgentID: "a", Name: "Keep", Status: StatusActive})
	_, _ = s.AddMemory(ctx, db, AddMemoryParams{DomainID: keep.ID, Type: TypeFact, Text: "active fact", Status: StatusActive, Confidence: 0.9})
	_, _ = s.AddMemory(ctx, db, AddMemoryParams{DomainID: keep.ID, Type: TypeFact, Text: "old fact", Status: StatusRetired, Confidence: 0.9})

	// Archived domain with a (still-active) memory — both should go.
	arch, _ := s.CreateDomain(ctx, db, CreateDomainParams{AgentID: "a", Name: "Arch", Status: StatusActive})
	_, _ = s.AddMemory(ctx, db, AddMemoryParams{DomainID: arch.ID, Type: TypeFact, Text: "archived domain fact", Status: StatusActive, Confidence: 0.9})
	if err := s.ArchiveDomain(ctx, db, arch.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Dry run reports counts without deleting: 1 retired memory + 1 archived
	// domain's memory = 2 memories, 1 domain.
	st, err := s.PurgeNonActive(ctx, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if st.Memories != 2 || st.Domains != 1 {
		t.Fatalf("dry run counts = %+v, want {2,1}", st)
	}
	// Nothing deleted yet.
	if doms, _ := s.ListDomains(ctx, db, StatusActive); len(doms) != 2 { // Keep + General
		t.Fatalf("dry run deleted domains: %d active remain", len(doms))
	}

	// Apply.
	st, err = s.PurgeNonActive(ctx, true)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if st.Memories != 2 || st.Domains != 1 {
		t.Fatalf("purge counts = %+v, want {2,1}", st)
	}

	// Only the active "Keep" memory + the seeded general remain; archived domain gone.
	all, _ := s.ListDomains(ctx, db) // all statuses
	for _, d := range all {
		if d.ID == arch.ID {
			t.Fatalf("archived domain survived purge")
		}
	}
	mems, _ := s.ListMemories(ctx, db, keep.ID) // all statuses
	if len(mems) != 1 || mems[0].Text != "active fact" {
		t.Fatalf("kept memories = %+v, want only 'active fact'", mems)
	}
}

// TestDomainUpdatePatch confirms UpdateDomain is a true patch: only provided
// fields change, the version bumps, and no expected-version is required.
func TestDomainUpdatePatch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "P", Summary: "orig"})
	sum := "updated summary"
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Summary: &sum}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if got.Version != 2 || got.Summary != sum {
		t.Fatalf("after update: version=%d summary=%q", got.Version, got.Summary)
	}
	if got.Name != "P" {
		t.Fatalf("name changed by summary-only patch: %q", got.Name)
	}
	// Set sticky on; toggling it bumps the version again.
	on := true
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Sticky: &on}); err != nil {
		t.Fatalf("set sticky: %v", err)
	}
	got, _ = s.GetDomain(ctx, s.DB(), d.ID, false)
	if !got.Sticky() || got.Version != 3 {
		t.Fatalf("after sticky patch: sticky=%v version=%d", got.Sticky(), got.Version)
	}
	// Rename onto an existing name is rejected.
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Name: strptr("General")}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("rename collision err = %v, want ErrDuplicateName", err)
	}
}

func strptr(s string) *string { return &s }

func TestHookLifecycleAndStableRev(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// Sticky domain so active hooks affect stable_rev.
	base, _ := s.GeneralDomain(ctx, s.DB()) // the seeded sticky general domain
	revAfterDomain, _ := s.StableRev(ctx)

	h, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: base.ID, Type: TypeRule, Text: "Never use blue.",
		Status: StatusActive, Confidence: 0.95,
	})
	if err != nil {
		t.Fatalf("add hook: %v", err)
	}
	if !validID(h.ID, "h") {
		t.Fatalf("hook id = %q, want h+5 chars", h.ID)
	}
	if rev, _ := s.StableRev(ctx); rev <= revAfterDomain {
		t.Fatalf("stable_rev did not bump on sticky active hook add")
	}

	// Supersede.
	h2, err := s.SupersedeMemory(ctx, s.DB(), h.ID, AddMemoryParams{
		DomainID: base.ID, Type: TypeRule, Text: "Use blue for the layout.",
		Status: StatusActive, Confidence: 0.95,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	old, _ := s.GetMemory(ctx, s.DB(), h.ID)
	if old.Status != StatusRetired {
		t.Fatalf("old hook status = %q, want retired", old.Status)
	}
	if h2.SupersedesMemoryID == nil || *h2.SupersedesMemoryID != h.ID {
		t.Fatalf("supersede link not set on %s", h2.ID)
	}
	active, _ := s.ListMemories(ctx, s.DB(), base.ID, StatusActive)
	if len(active) != 1 || active[0].Text != "Use blue for the layout." {
		t.Fatalf("active hooks = %+v", active)
	}
}

// TestDeleteMemory confirms a hard memory delete removes the row and is
// idempotent-safe (ErrNotFound on a second delete).
func TestDeleteMemory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "P"})
	m, _ := s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "delete me", Status: StatusActive,
		Confidence: 0.9,
	})
	if err := s.DeleteMemory(ctx, s.DB(), m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMemory(ctx, s.DB(), m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("memory survived delete (err=%v)", err)
	}
	if err := s.DeleteMemory(ctx, s.DB(), m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

// TestMemoryOriginRoundTrip confirms origin is persisted, defaults to chat when
// unset, and round-trips through GetMemory.
func TestMemoryOriginRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "P"})

	withOrigin, _ := s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "from a human", Status: StatusActive,
		Confidence: 0.9, Origin: OriginUser,
	})
	if withOrigin.Origin != OriginUser {
		t.Fatalf("origin = %q, want user", withOrigin.Origin)
	}

	// Origin unset → defaults to chat.
	noOrigin, _ := s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "agent note", Status: StatusActive,
		Confidence: 0.9,
	})
	if noOrigin.Origin != OriginChat {
		t.Fatalf("default origin = %q, want chat", noOrigin.Origin)
	}

	got, _ := s.GetMemory(ctx, s.DB(), withOrigin.ID)
	if got.Origin != OriginUser {
		t.Fatalf("reloaded origin = %q, want user", got.Origin)
	}
}

// Search reaches active memories, and reaches events only when asked.
//
// Events are the one type kept out of the prompt, so search is the ONLY way to
// retrieve one. Excluding them by default keeps an ordinary lookup from being
// buried under recurring notes — one production agent holds 279 of them in a
// single domain — and the flag is the whole retrieval path when the question is
// actually about the past.
func TestSearchExcludesEventsUnlessAsked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "P"})
	_, _ = s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "The BioTech report targets Q3.",
		Status: StatusActive, Confidence: 0.9,
	})
	_, _ = s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeEvent, Text: "BioTech status as of Sep 4: shipped.",
		Status: StatusActive, Confidence: 0.9,
	})

	hits, err := s.SearchMemories(ctx, s.DB(), "biotech", 10, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Type != TypeFact {
		t.Fatalf("default search returned %d hits (%+v), want just the fact", len(hits), hits)
	}

	hits, err = s.SearchMemories(ctx, s.DB(), "biotech", 10, true)
	if err != nil {
		t.Fatalf("search with events: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("include_events returned %d hits, want both", len(hits))
	}

	// A retired memory stays out of both.
	m, _ := s.AddMemory(ctx, s.DB(), AddMemoryParams{
		DomainID: d.ID, Type: TypeFact, Text: "BioTech was cancelled.",
		Status: StatusActive, Confidence: 0.9,
	})
	if err := s.RetireMemory(ctx, s.DB(), m.ID, "superseded"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if h, _ := s.SearchMemories(ctx, s.DB(), "cancelled", 10, true); len(h) != 0 {
		t.Fatalf("retired memory leaked into search: %+v", h)
	}
}

// The composer's read path excludes events; the domain reports their count
// instead, so they stay out of the prompt without disappearing.
func TestPromptMemoriesExcludeEventsAndCountThem(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{AgentID: "a", Name: "Trips"})
	for _, tc := range []struct {
		typ  MemoryType
		text string
	}{
		{TypeFact, "Home is Ottawa."},
		{TypeRule, "Do not use the word thuddy."},
		{TypeOperational, "Craft rules live at files/craft.md."},
		{TypeEvent, "Drove to the KOA on Sep 4."},
		{TypeEvent, "Drove home on Sep 7."},
	} {
		if _, err := s.AddMemory(ctx, s.DB(), AddMemoryParams{
			DomainID: d.ID, Type: tc.typ, Text: tc.text,
			Status: StatusActive, Confidence: 0.9,
		}); err != nil {
			t.Fatalf("add %s: %v", tc.typ, err)
		}
	}

	prompt, err := s.ListPromptMemories(ctx, s.DB(), d.ID)
	if err != nil {
		t.Fatalf("ListPromptMemories: %v", err)
	}
	if len(prompt) != 3 {
		t.Fatalf("prompt memories = %d, want 3 (events excluded): %+v", len(prompt), prompt)
	}
	for _, m := range prompt {
		if m.Type == TypeEvent {
			t.Errorf("event %s reached the prompt path", m.ID)
		}
	}

	n, err := s.CountEvents(ctx, s.DB(), d.ID)
	if err != nil || n != 2 {
		t.Fatalf("CountEvents = %d (err %v), want 2", n, err)
	}

	// ListMemories is the operator's view and shows everything.
	all, err := s.ListMemories(ctx, s.DB(), d.ID, StatusActive)
	if err != nil || len(all) != 5 {
		t.Fatalf("ListMemories = %d (err %v), want all 5", len(all), err)
	}
}

func TestWatermarkAndLease(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ap := "/sessions/agent_alice_main.archive.db"
	if err := s.IncMeaningful(ctx, s.DB(), ap, 3); err != nil {
		t.Fatalf("inc: %v", err)
	}
	st, _ := s.GetState(ctx, s.DB(), ap)
	if st.MeaningfulCount != 3 {
		t.Fatalf("meaningful = %d, want 3", st.MeaningfulCount)
	}
	if err := s.SetWatermark(ctx, s.DB(), ap, 100, 100); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	st, _ = s.GetState(ctx, s.DB(), ap)
	if st.ConsolidatedSeq != 100 || st.MeaningfulCount != 0 {
		t.Fatalf("after watermark: seq=%d meaningful=%d", st.ConsolidatedSeq, st.MeaningfulCount)
	}

	ok, err := s.AcquireLease(ctx, s.DB(), "consolidate:"+ap, "worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: %v ok=%v", err, ok)
	}
	ok2, _ := s.AcquireLease(ctx, s.DB(), "consolidate:"+ap, "worker-2", time.Minute)
	if ok2 {
		t.Fatalf("second worker acquired a held lease")
	}
	if err := s.ReleaseLease(ctx, s.DB(), "consolidate:"+ap, "worker-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok3, _ := s.AcquireLease(ctx, s.DB(), "consolidate:"+ap, "worker-2", time.Minute)
	if !ok3 {
		t.Fatalf("worker-2 could not acquire after release")
	}
}

func TestDomainKeywordTriggers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), CreateDomainParams{
		AgentID: "a", Name: "Briefing",
		KeywordTriggers: "  Morning Routine , weekly report ,, ",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Normalized: trimmed, lowercased, empties dropped.
	if d.KeywordTriggers != "morning routine,weekly report" {
		t.Fatalf("keyword_triggers = %q, want normalized", d.KeywordTriggers)
	}

	// Whole-phrase, word-boundary, case-insensitive matches.
	if kw, ok := d.MatchKeyword("Time for the MORNING ROUTINE, everyone"); !ok || kw != "morning routine" {
		t.Fatalf("MatchKeyword phrase = %q,%v", kw, ok)
	}
	if _, ok := d.MatchKeyword("please file the weekly report by noon"); !ok {
		t.Fatalf("expected 'weekly report' to match")
	}
	// The individual words must NOT match on their own (phrase only).
	if _, ok := d.MatchKeyword("good morning"); ok {
		t.Fatalf("bare 'morning' should not match the phrase 'morning routine'")
	}
	// No substring-in-word false positives.
	if _, ok := d.MatchKeyword("this is legitimate work"); ok {
		t.Fatalf("should not match inside another word")
	}

	// Update replaces; empty clears.
	empty := ""
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{KeywordTriggers: &empty}); err != nil {
		t.Fatalf("update clear: %v", err)
	}
	got, _ := s.GetDomain(ctx, s.DB(), d.ID, false)
	if got.KeywordTriggers != "" || len(got.KeywordPhrases()) != 0 {
		t.Fatalf("keyword triggers not cleared: %q", got.KeywordTriggers)
	}
}
