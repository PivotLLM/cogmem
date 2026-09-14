// cogmem - Cognitive Memory
// License: MIT

package portable

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/store"
)

func newStore(t *testing.T, name string) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seed builds a store with every shape a round trip could lose: a topic domain
// holding one memory of every type plus a retired one, memories of each origin,
// one with an attached file, a sticky topic domain, and an archived domain.
// Domain-level state (blockers, next actions, constraints) is set too.
//
// Named domains: "Craft" (topic, not sticky), "Standing" (sticky topic), "Old"
// (archived). The store also seeds "General" (sticky) on its own.
func seed(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
		Name: "Craft", Summary: "how to write",
		Triggers: "file_write", KeywordTriggers: "style guide",
		State: store.DomainState{
			Blockers:    []string{"waiting on the brief"},
			NextActions: []string{"draft chapter 2", "send outline"},
			Constraints: []string{"no more than 2000 words"},
		},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	for _, m := range []struct {
		typ    store.MemoryType
		text   string
		origin store.Origin
		file   string
	}{
		{store.TypeFact, "The house style is Oxford commas.", store.OriginChat, ""},
		{store.TypePreference, "Short paragraphs.", store.OriginUser, ""},
		{store.TypeRule, "Do not use the word thuddy.", store.OriginConsolidation, ""},
		{store.TypeOperational, "Craft rules live at files/craft.md.", store.OriginChat, "files/craft.md"},
		{store.TypeEvent, "Chapter 1 delivered Sep 4.", store.OriginChat, ""},
	} {
		if _, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
			DomainID: d.ID, Type: m.typ, Text: m.text,
			Status: store.StatusActive, Confidence: 0.9, Origin: m.origin, FileRef: m.file,
		}); err != nil {
			t.Fatalf("add %s: %v", m.typ, err)
		}
	}
	retired, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "The old house style.",
		Status: store.StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add retired: %v", err)
	}
	if err := s.RetireMemory(ctx, s.DB(), retired.ID, "superseded"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	sticky, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
		Name: "Standing", Sticky: true, Summary: "always in context",
		KeywordTriggers: "standing orders",
	})
	if err != nil {
		t.Fatalf("create sticky domain: %v", err)
	}
	if _, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: sticky.ID, Type: store.TypeRule, Text: "Reply in English.",
		Status: store.StatusActive, Confidence: 1, Origin: store.OriginUser,
	}); err != nil {
		t.Fatalf("add to sticky: %v", err)
	}

	old, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
		Name: "Old", Summary: "finished project",
		State: store.DomainState{Constraints: []string{"read only"}},
	})
	if err != nil {
		t.Fatalf("create archived domain: %v", err)
	}
	if _, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: old.ID, Type: store.TypeFact, Text: "Shipped in 2025.",
		Status: store.StatusActive, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("add to archived: %v", err)
	}
	if err := s.ArchiveDomain(ctx, s.DB(), old.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
}

// The whole point of the format: what comes out can go back in. The Markdown
// export it replaces could not, which is why it was only ever something to look
// at rather than a backup.
func TestRoundTripPreservesEverything(t *testing.T) {
	ctx := context.Background()
	src := newStore(t, "src.cogmem.db")
	seed(t, src)

	doc, err := Export(ctx, src)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// The seed's four domains (General is store-seeded) and eight memories.
	if len(doc.Domains) != 4 {
		t.Fatalf("exported %d domains, want 4 (General, Craft, Standing, Old)", len(doc.Domains))
	}
	if n := countMemories(doc); n != 8 {
		t.Fatalf("exported %d memories, want 8", n)
	}
	data, err := Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dst := newStore(t, "dst.cogmem.db")
	res, err := Import(ctx, dst, parsed, ImportReplace)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.DomainsCreated != 4 || res.DomainsMatched != 0 || res.MemoriesCreated != 8 || res.MemoriesSkipped != 0 {
		t.Fatalf("replace result = %+v, want 4 domains and 8 memories created", res)
	}

	got, err := Export(ctx, dst)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	compare(t, doc, got)
}

func countMemories(d Document) int {
	n := 0
	for _, dom := range d.Domains {
		n += len(dom.Memories)
	}
	return n
}

// compare checks two documents hold the same domains and memories, ignoring
// ids and timestamps — which import deliberately does not preserve. Every
// domain-level field (sticky, status, summary, triggers, keyword triggers, the
// three state lists) and every memory field must survive.
func compare(t *testing.T, want, got Document) {
	t.Helper()
	domains := func(d Document) map[string]Domain {
		out := map[string]Domain{}
		for _, dom := range d.Domains {
			dom.ID, dom.Memories = "", nil
			out[dom.Name] = dom
		}
		return out
	}
	wd, gd := domains(want), domains(got)
	if len(wd) != len(gd) {
		t.Fatalf("domain count %d -> %d after a round trip", len(wd), len(gd))
	}
	for name, w := range wd {
		g, ok := gd[name]
		if !ok {
			t.Errorf("domain lost in the round trip: %s", name)
			continue
		}
		if !reflect.DeepEqual(w, g) {
			t.Errorf("domain %s changed:\n  before %+v\n  after  %+v", name, w, g)
		}
	}

	index := func(d Document) map[string]Memory {
		out := map[string]Memory{}
		for _, dom := range d.Domains {
			for _, m := range dom.Memories {
				m.ID, m.CreatedAt = "", ""
				out[dom.Name+"|"+m.Text] = m
			}
		}
		return out
	}
	w, g := index(want), index(got)
	if len(w) != len(g) {
		t.Fatalf("memory count %d -> %d after a round trip", len(w), len(g))
	}
	for k, wm := range w {
		gm, ok := g[k]
		if !ok {
			t.Errorf("memory lost in the round trip: %s", k)
			continue
		}
		if wm != gm {
			t.Errorf("%s changed:\n  before %+v\n  after  %+v", k, wm, gm)
		}
	}
}

// The seed's domain fields reach the document exactly, so a compare that
// passes is comparing something: every field is asserted here against what
// seed wrote, not merely against itself after a trip.
func TestExportCarriesDomainFields(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "fields.cogmem.db")
	seed(t, s)
	doc, err := Export(ctx, s)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	byName := map[string]Domain{}
	for _, d := range doc.Domains {
		byName[d.Name] = d
	}
	craft := byName["Craft"]
	craft.ID, craft.Memories = "", nil
	wantCraft := Domain{
		Name: "Craft", Status: "active", Summary: "how to write",
		Triggers: "file_write", KeywordTriggers: "style guide",
		Blockers:    []string{"waiting on the brief"},
		NextActions: []string{"draft chapter 2", "send outline"},
		Constraints: []string{"no more than 2000 words"},
	}
	if !reflect.DeepEqual(craft, wantCraft) {
		t.Errorf("Craft = %+v\nwant    %+v", craft, wantCraft)
	}
	standing := byName["Standing"]
	if !standing.Sticky || standing.Status != "active" || standing.KeywordTriggers != "standing orders" || standing.Summary != "always in context" {
		t.Errorf("Standing = %+v", standing)
	}
	old := byName["Old"]
	if old.Status != "archived" || old.Summary != "finished project" || !reflect.DeepEqual(old.Constraints, []string{"read only"}) {
		t.Errorf("Old = %+v", old)
	}
	if g := byName["General"]; !g.Sticky || g.Status != "active" {
		t.Errorf("General = %+v, want the seeded sticky active domain", g)
	}

	// Memory-level fields: origin of each kind and the attached file.
	mems := map[string]Memory{}
	for _, m := range byName["Craft"].Memories {
		mems[m.Text] = m
	}
	if m := mems["Short paragraphs."]; m.Origin != "user" || m.Type != "preference" {
		t.Errorf("user-origin memory = %+v", m)
	}
	if m := mems["Do not use the word thuddy."]; m.Origin != "consolidation" || m.Type != "rule" {
		t.Errorf("consolidation-origin memory = %+v", m)
	}
	if m := mems["Craft rules live at files/craft.md."]; m.FileRef != "files/craft.md" || m.Type != "operational" {
		t.Errorf("file-ref memory = %+v", m)
	}
	if m := mems["The old house style."]; m.Status != "retired" || m.RetireReason != "superseded" {
		t.Errorf("retired memory = %+v", m)
	}
	if m := mems["Chapter 1 delivered Sep 4."]; m.Type != "event" || m.Status != "active" {
		t.Errorf("event memory = %+v", m)
	}
}

// What the export leaves out, by design.
//
// Evidence (source session and seq range), the supersedes link and updated_at
// are bookkeeping about THIS database's conversation history: the numbers mean
// nothing in another agent's store, so the document does not carry them, and a
// memory restored from a document has none. created_at IS written — for a
// human reading the file — but import mints a fresh row and does not preserve
// it: the memory is new in the store it lands in. Both are intended.
func TestExportDropsEvidenceAndImportDoesNotKeepCreatedAt(t *testing.T) {
	ctx := context.Background()
	src := newStore(t, "drop-src.cogmem.db")
	d, err := src.CreateDomain(ctx, src.DB(), store.CreateDomainParams{Name: "Evidence"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sess, start, end := "chan:123", int64(40), int64(44)
	first, err := src.AddMemory(ctx, src.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "the first statement",
		Status: store.StatusActive, Confidence: 0.9,
		SourceSession: &sess, SourceSeqStart: &start, SourceSeqEnd: &end,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	second, err := src.SupersedeMemory(ctx, src.DB(), first.ID, store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "the corrected statement",
		Status: store.StatusActive, Confidence: 0.95,
		SourceSession: &sess, SourceSeqStart: &start, SourceSeqEnd: &end,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	// Backdate created_at so it is distinguishable from "now" after import.
	old := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	if _, err := src.DB().ExecContext(ctx,
		`UPDATE memories SET created_at=?, updated_at=? WHERE id=?`,
		old.Unix(), old.Unix()+3600, second.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	doc, err := Export(ctx, src)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	data, err := Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	for _, absent := range []string{"seq_start", "seq_end", "source_session", "supersedes", "updated_at", sess} {
		if strings.Contains(out, absent) {
			t.Errorf("export carries %q, which is dropped by design:\n%s", absent, out)
		}
	}
	var exported Memory
	for _, dom := range doc.Domains {
		for _, m := range dom.Memories {
			if m.Text == "the corrected statement" {
				exported = m
			}
		}
	}
	if exported.CreatedAt != "2024-03-01T12:00:00Z" {
		t.Errorf("exported created_at = %q, want the backdated RFC3339 value", exported.CreatedAt)
	}

	dst := newStore(t, "drop-dst.cogmem.db")
	if _, err := Import(ctx, dst, doc, ImportMerge); err != nil {
		t.Fatalf("import: %v", err)
	}
	imported, err := dst.DomainByName(ctx, dst.DB(), "Evidence")
	if err != nil {
		t.Fatalf("imported domain: %v", err)
	}
	mems, err := dst.ListMemories(ctx, dst.DB(), imported.ID)
	if err != nil || len(mems) != 2 {
		t.Fatalf("imported memories = %d err=%v, want 2", len(mems), err)
	}
	for _, m := range mems {
		if m.SourceSession != nil || m.SourceSeqStart != nil || m.SourceSeqEnd != nil {
			t.Errorf("%s: evidence survived import (%v %v %v); the document does not carry it",
				m.Text, m.SourceSession, m.SourceSeqStart, m.SourceSeqEnd)
		}
		if m.SupersedesMemoryID != nil {
			t.Errorf("%s: supersedes link survived import: %s", m.Text, *m.SupersedesMemoryID)
		}
		if m.Text == "the corrected statement" && !m.CreatedAt.After(old.Add(24*time.Hour)) {
			t.Errorf("created_at %v was preserved on import; intended behaviour is a fresh timestamp", m.CreatedAt)
		}
		// The retire reason and status DO travel (they are what the memory is,
		// not where it came from).
		if m.Text == "the first statement" && (m.Status != store.StatusRetired || m.RetireReason == nil || *m.RetireReason != "superseded") {
			t.Errorf("retired memory = status %q reason %v, want retired/superseded", m.Status, m.RetireReason)
		}
	}
}

// Memory text is prose, and a literal block is the reason this format is YAML
// rather than JSON: it stays readable and editable in a text editor. A single
// escaped line would defeat the purpose.
func TestLongTextIsWrittenAsALiteralBlock(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "block.cogmem.db")
	d, _ := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Voice"})
	long := "Name specific sounds (thud, click, scrape) or omit vague auditory " +
		"descriptions entirely, because a reader cannot picture a soft sound."
	multi := "First line of guidance.\nSecond line of guidance."
	for _, txt := range []string{long, multi} {
		if _, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
			DomainID: d.ID, Type: store.TypeRule, Text: txt,
			Status: store.StatusActive, Confidence: 0.9,
		}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	doc, err := Export(ctx, s)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	data, err := Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)

	if strings.Count(out, "text: |") < 2 {
		t.Errorf("expected both long texts as literal blocks, got:\n%s", out)
	}
	if strings.Contains(out, `\n`) {
		t.Errorf("text was escaped onto one line rather than written as a block:\n%s", out)
	}

	// Still has to survive the trip.
	parsed, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var seenLong, seenMulti bool
	for _, dom := range parsed.Domains {
		for _, m := range dom.Memories {
			switch m.Text {
			case long:
				seenLong = true
			case multi:
				seenMulti = true
			}
		}
	}
	if !seenLong || !seenMulti {
		t.Errorf("literal-block text did not round trip (long=%v multi=%v)", seenLong, seenMulti)
	}
}

// Merge adds what is missing and never removes anything, and it is
// idempotent: importing the same document twice does nothing the second
// time — no domain created or updated, no memory created or retired, and the
// stable revision untouched.
func TestMergeIsAdditiveAndRepeatable(t *testing.T) {
	ctx := context.Background()
	src := newStore(t, "m-src.cogmem.db")
	seed(t, src)
	doc, _ := Export(ctx, src)

	dst := newStore(t, "m-dst.cogmem.db")
	d, _ := dst.CreateDomain(ctx, dst.DB(), store.CreateDomainParams{Name: "Craft"})
	kept, _ := dst.AddMemory(ctx, dst.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "Something only Bob knows.",
		Status: store.StatusActive, Confidence: 0.9,
	})

	first, err := Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	// Craft and the auto-seeded General already exist and are matched by name;
	// Standing and Old are created. A merge that created a second "Craft" would
	// double every memory in it. Craft is brought up to the document (it was
	// created bare here); General is identical in both stores and left alone.
	want := ImportResult{DomainsMatched: 2, DomainsCreated: 2, DomainsUpdated: 1, MemoriesCreated: 8}
	if first != want {
		t.Errorf("first merge = %+v, want %+v", first, want)
	}
	if _, err := dst.GetMemory(ctx, dst.DB(), kept.ID); err != nil {
		t.Errorf("merge destroyed an existing memory: %v", err)
	}
	if craft, _ := dst.GetDomain(ctx, dst.DB(), d.ID, false); craft.Summary != "how to write" || craft.Triggers != "file_write" {
		t.Errorf("matched Craft was not brought up to the document: %+v", craft)
	}
	before, _ := Export(ctx, dst)
	rev, err := dst.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}

	second, err := Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	// Every domain is matched, the archived "Old" included: matching only
	// active domains would re-create it, and its memory, on every merge.
	want = ImportResult{DomainsMatched: 4, MemoriesSkipped: 8}
	if second != want {
		t.Errorf("second merge = %+v, want %+v", second, want)
	}
	if archived, _ := dst.ListDomains(ctx, dst.DB(), store.StatusArchived); len(archived) != 1 {
		t.Errorf("archived domains after two merges = %d, want 1", len(archived))
	}
	if after, _ := Export(ctx, dst); !reflect.DeepEqual(before.Domains, after.Domains) {
		t.Errorf("second merge changed the store:\n before %+v\n after  %+v", before.Domains, after.Domains)
	}
	if rev2, _ := dst.StableRev(ctx); rev2 != rev {
		t.Errorf("second merge bumped stable_rev %d -> %d", rev, rev2)
	}
}

// Merging into a domain that already exists brings the domain up to the
// document — summary, stickiness, status, triggers, keyword triggers and the
// state lists — as well as adding its memories, so a document edited to fix a
// domain and merged back takes effect. The name keeps the store's spelling.
// A second merge of the same document finds nothing to change.
func TestMergeAppliesDocumentDomainFields(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t, "mf-dst.cogmem.db")
	existing, err := dst.CreateDomain(ctx, dst.DB(), store.CreateDomainParams{
		Name: "Craft", Summary: "mine", Triggers: "file_read",
		State: store.DomainState{Blockers: []string{"my blocker"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	doc := Document{
		FormatVersion: FormatVersion,
		Domains: []Domain{{
			Name:   "craft", // matched case-insensitively
			Sticky: true, Status: "active", Summary: "from the document",
			Triggers: "file_write", KeywordTriggers: "style guide",
			Blockers:    []string{"document blocker"},
			NextActions: []string{"document action"},
			Constraints: []string{"document constraint"},
			Memories:    []Memory{{Type: "fact", Status: "active", Confidence: 0.5, Origin: "user", Text: "added by merge"}},
		}},
	}
	res, err := Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := (ImportResult{DomainsMatched: 1, DomainsUpdated: 1, MemoriesCreated: 1}); res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	got, err := dst.GetDomain(ctx, dst.DB(), existing.ID, true)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Craft" || got.Summary != "from the document" || !got.Sticky() || got.Status != store.StatusActive ||
		got.Triggers != "file_write" || got.KeywordTriggers != "style guide" {
		t.Errorf("domain after merge: name=%q summary=%q sticky=%v status=%q triggers=%q keywords=%q",
			got.Name, got.Summary, got.Sticky(), got.Status, got.Triggers, got.KeywordTriggers)
	}
	wantState := store.DomainState{
		Blockers: []string{"document blocker"}, NextActions: []string{"document action"}, Constraints: []string{"document constraint"},
	}
	if !reflect.DeepEqual(got.State, wantState) {
		t.Errorf("domain state after merge = %+v, want %+v", got.State, wantState)
	}
	if len(got.Memories) != 1 || got.Memories[0].Text != "added by merge" || got.Memories[0].Origin != store.OriginUser || got.Memories[0].Confidence != 0.5 {
		t.Errorf("merged memory = %+v", got.Memories)
	}
	// One update: version 1 on create, 2 after the merge applied the fields.
	if got.Version != 2 {
		t.Errorf("domain version = %d, want 2 (one update applied)", got.Version)
	}

	// The same document again changes nothing: the fields already match, so
	// no update is written and the version stays.
	res, err = Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if want := (ImportResult{DomainsMatched: 1, MemoriesSkipped: 1}); res != want {
		t.Errorf("second merge = %+v, want %+v", res, want)
	}
	if again, _ := dst.GetDomain(ctx, dst.DB(), existing.ID, false); again.Version != 2 {
		t.Errorf("second merge bumped the version to %d", again.Version)
	}

	// Archiving through the document works too, and an archived domain is
	// still matched (never re-created) by a later merge.
	doc.Domains[0].Status = "archived"
	if res, err = Import(ctx, dst, doc, ImportMerge); err != nil || res.DomainsUpdated != 1 || res.DomainsCreated != 0 {
		t.Fatalf("archiving merge = %+v err=%v, want one domain updated", res, err)
	}
	if arch, _ := dst.GetDomain(ctx, dst.DB(), existing.ID, false); arch.Status != store.StatusArchived {
		t.Errorf("domain status after archiving merge = %q", arch.Status)
	}
	if res, err = Import(ctx, dst, doc, ImportMerge); err != nil || res.DomainsMatched != 1 || res.DomainsCreated != 0 || res.DomainsUpdated != 0 {
		t.Errorf("merge onto the archived domain = %+v err=%v, want matched and unchanged", res, err)
	}
}

// Merge dedup is by exact text after trimming, and case-sensitive: whitespace
// around a memory does not make it new, but a change of case does.
func TestMergeDedupIsTrimmedAndCaseSensitive(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t, "dd-dst.cogmem.db")
	d, _ := dst.CreateDomain(ctx, dst.DB(), store.CreateDomainParams{Name: "Craft"})
	if _, err := dst.AddMemory(ctx, dst.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "Short paragraphs.",
		Status: store.StatusActive, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	doc := Document{
		FormatVersion: FormatVersion,
		Domains: []Domain{{
			Name: "Craft", Status: "active",
			Memories: []Memory{
				{Type: "fact", Status: "active", Confidence: 0.9, Text: "   Short paragraphs.  \n"},
				{Type: "fact", Status: "active", Confidence: 0.9, Text: "short paragraphs."},
				{Type: "fact", Status: "active", Confidence: 0.9, Text: "   "}, // blank: ignored, not counted
			},
		}},
	}
	res, err := Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.MemoriesSkipped != 1 || res.MemoriesCreated != 1 {
		t.Fatalf("result = %+v, want the trimmed duplicate skipped and the case variant created", res)
	}
	mems, _ := dst.ListMemories(ctx, dst.DB(), d.ID)
	texts := map[string]bool{}
	for _, m := range mems {
		texts[m.Text] = true
	}
	if len(mems) != 2 || !texts["Short paragraphs."] || !texts["short paragraphs."] {
		t.Errorf("memories after merge = %+v, want exactly the two case variants, stored trimmed", mems)
	}
}

// A memory the document says is retired, but which is active in the store, is
// retired with the document's reason: a document edited to retire a memory
// and merged back takes effect. A second merge finds it already retired and
// skips it; a memory retired in the store but active in the document is left
// retired, since a merge never restores.
func TestMergeRetiresActiveMemoryWhenDocumentRetiresIt(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t, "ret-dst.cogmem.db")
	d, _ := dst.CreateDomain(ctx, dst.DB(), store.CreateDomainParams{Name: "Craft"})
	m, err := dst.AddMemory(ctx, dst.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "The old house style.",
		Status: store.StatusActive, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	doc := Document{
		FormatVersion: FormatVersion,
		Domains: []Domain{{
			Name: "Craft", Status: "active",
			Memories: []Memory{{
				Type: "fact", Status: "retired", RetireReason: "superseded",
				Confidence: 0.9, Text: "The old house style.",
			}},
		}},
	}
	res, err := Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if want := (ImportResult{DomainsMatched: 1, MemoriesRetired: 1}); res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	got, _ := dst.GetMemory(ctx, dst.DB(), m.ID)
	if got.Status != store.StatusRetired || got.RetireReason == nil || *got.RetireReason != "superseded" {
		t.Errorf("memory = status %q reason %v, want retired/superseded", got.Status, got.RetireReason)
	}

	res, err = Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if want := (ImportResult{DomainsMatched: 1, MemoriesSkipped: 1}); res != want {
		t.Errorf("second merge = %+v, want %+v", res, want)
	}

	doc.Domains[0].Memories[0].Status = "active"
	if res, err = Import(ctx, dst, doc, ImportMerge); err != nil || res.MemoriesSkipped != 1 || res.MemoriesCreated != 0 {
		t.Fatalf("merge of an active document memory onto a retired one = %+v err=%v, want skipped", res, err)
	}
	if got, _ = dst.GetMemory(ctx, dst.DB(), m.ID); got.Status != store.StatusRetired {
		t.Errorf("merge restored a retired memory: %+v", got)
	}
}

// Replace makes a document a true restore point: what comes back is exactly
// what was exported, and anything learned since is gone. That is the difference
// from merge, and why it has to be asked for explicitly.
func TestReplaceDiscardsWhatWasThere(t *testing.T) {
	ctx := context.Background()
	src := newStore(t, "r-src.cogmem.db")
	seed(t, src)
	doc, _ := Export(ctx, src)

	dst := newStore(t, "r-dst.cogmem.db")
	d, _ := dst.CreateDomain(ctx, dst.DB(), store.CreateDomainParams{Name: "Scratch"})
	doomed, _ := dst.AddMemory(ctx, dst.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "Learned after the export.",
		Status: store.StatusActive, Confidence: 0.9,
	})

	if _, err := Import(ctx, dst, doc, ImportReplace); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := dst.GetMemory(ctx, dst.DB(), doomed.ID); err == nil {
		t.Error("replace kept a memory that was not in the document")
	}
	after, _ := Export(ctx, dst)
	for _, dom := range after.Domains {
		if dom.Name == "Scratch" {
			t.Error("replace kept a domain that was not in the document")
		}
	}
	if len(after.Domains) != 4 {
		t.Errorf("domains after replace = %d, want exactly the document's 4", len(after.Domains))
	}
}

// An unknown mode is refused before anything is touched.
func TestImportRejectsUnknownMode(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t, "mode-dst.cogmem.db")
	doc := Document{FormatVersion: FormatVersion, Domains: []Domain{{Name: "X", Status: "active"}}}
	_, err := Import(ctx, dst, doc, ImportMode("upsert"))
	if err == nil || err.Error() != `cogmem: unknown import mode "upsert"` {
		t.Fatalf("err = %v, want the unknown-mode error", err)
	}
	if _, err := dst.DomainByName(ctx, dst.DB(), "X"); err == nil {
		t.Error("a rejected import still created the domain")
	}
}

// Ids are re-minted, which is what lets a document seed a different agent.
func TestImportReMintsIDs(t *testing.T) {
	ctx := context.Background()
	src := newStore(t, "i-src.cogmem.db")
	seed(t, src)
	doc, _ := Export(ctx, src)

	dst := newStore(t, "i-dst.cogmem.db")
	if _, err := Import(ctx, dst, doc, ImportMerge); err != nil {
		t.Fatalf("import: %v", err)
	}
	after, _ := Export(ctx, dst)

	old := map[string]bool{}
	for _, d := range doc.Domains {
		old[d.ID] = true
		for _, m := range d.Memories {
			old[m.ID] = true
		}
	}
	for _, d := range after.Domains {
		for _, m := range d.Memories {
			if old[m.ID] {
				t.Errorf("memory id %s was preserved; import must mint fresh ones", m.ID)
			}
		}
	}
}

// A file that is not a memory export is rejected rather than silently importing
// nothing, and a document from a future ClawEh says so instead of dropping the
// fields it does not understand.
func TestUnmarshalRejectsWhatItCannotRead(t *testing.T) {
	if _, err := Unmarshal([]byte("just: some yaml\n")); err == nil || err.Error() != "cogmem: not a memory export (no format_version)" {
		t.Errorf("a file with no format_version: err = %v", err)
	}
	if _, err := Unmarshal([]byte("format_version: 999\ndomains: []\n")); err == nil ||
		err.Error() != "cogmem: export is format version 999, this build understands up to 1" {
		t.Errorf("a future format version: err = %v", err)
	}
	if _, err := Unmarshal([]byte("format_version: [not, a, number\n")); err == nil ||
		!strings.HasPrefix(err.Error(), "cogmem: parse import: ") {
		t.Errorf("malformed YAML: err = %v", err)
	}
}

// An unknown type must not become an event by accident: that is the one mapping
// mistake that is invisible, because the memory would silently stop appearing
// in the prompt. Unknown statuses default to active as well, and an unknown
// origin to chat.
func TestUnknownTypeFallsBackToFactNotEvent(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t, "t-dst.cogmem.db")
	doc := Document{
		FormatVersion: FormatVersion,
		Domains: []Domain{{
			Name:   "Imported",
			Status: "paused", // unknown: becomes active
			Memories: []Memory{
				{Type: "observation", Status: "pending", Origin: "alien", Confidence: 0.5, Text: "mystery"},
				{Type: "event", Status: "active", Confidence: 0.5, Text: "a real event"},
			},
		}},
	}
	if _, err := Import(ctx, dst, doc, ImportMerge); err != nil {
		t.Fatalf("import: %v", err)
	}
	dom, err := dst.DomainByName(ctx, dst.DB(), "Imported")
	if err != nil || dom.Status != store.StatusActive {
		t.Fatalf("imported domain status = %q err=%v, want active", dom.Status, err)
	}
	mems, _ := dst.ListMemories(ctx, dst.DB(), dom.ID)
	if len(mems) != 2 {
		t.Fatalf("memories = %d, want 2", len(mems))
	}
	for _, m := range mems {
		switch m.Text {
		case "mystery":
			if m.Type != store.TypeFact || m.Status != store.StatusActive || m.Origin != store.OriginChat {
				t.Errorf("unknown type/status/origin became %q/%q/%q, want fact/active/chat", m.Type, m.Status, m.Origin)
			}
		case "a real event":
			if m.Type != store.TypeEvent {
				t.Errorf("event type was not preserved, got %q", m.Type)
			}
		}
	}
}
