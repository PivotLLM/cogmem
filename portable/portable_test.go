// cogmem - Cognitive Memory
// License: MIT

package portable

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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

// seed builds a store with one topic domain holding one memory of every type,
// plus a retired one, so a round trip has something of each shape to lose.
func seed(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{
		Name: "Craft", Summary: "how to write",
		Triggers: "file_write", KeywordTriggers: "style guide",
		State: store.DomainState{Blockers: []string{"waiting on the brief"}},
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	for _, m := range []struct {
		typ  store.MemoryType
		text string
	}{
		{store.TypeFact, "The house style is Oxford commas."},
		{store.TypePreference, "Short paragraphs."},
		{store.TypeRule, "Do not use the word thuddy."},
		{store.TypeOperational, "Craft rules live at files/craft.md."},
		{store.TypeEvent, "Chapter 1 delivered Sep 4."},
	} {
		if _, err := s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
			DomainID: d.ID, Type: m.typ, Text: m.text,
			Status: store.StatusActive, Confidence: 0.9, Origin: store.OriginChat,
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
	data, err := Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dst := newStore(t, "dst.cogmem.db")
	if _, err := Import(ctx, dst, parsed, ImportReplace); err != nil {
		t.Fatalf("import: %v", err)
	}

	got, err := Export(ctx, dst)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	compare(t, doc, got)
}

// compare checks two documents hold the same memories, ignoring ids and
// timestamps — which import deliberately does not preserve.
func compare(t *testing.T, want, got Document) {
	t.Helper()
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

// Merge adds what is missing and touches nothing else, so importing the same
// document twice is a no-op and an import can never destroy anything.
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
	if first.MemoriesCreated == 0 {
		t.Fatal("merge created nothing")
	}
	// Craft and the auto-seeded General both already exist, so both are matched
	// by name and nothing is created. A merge that created a second "Craft"
	// would double every memory in it.
	if first.DomainsCreated != 0 {
		t.Errorf("domains created = %d, want existing domains reused by name", first.DomainsCreated)
	}
	if _, err := dst.GetMemory(ctx, dst.DB(), kept.ID); err != nil {
		t.Errorf("merge destroyed an existing memory: %v", err)
	}

	second, err := Import(ctx, dst, doc, ImportMerge)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if second.MemoriesCreated != 0 {
		t.Errorf("re-importing created %d duplicates, want none", second.MemoriesCreated)
	}
	if second.MemoriesSkipped != first.MemoriesCreated {
		t.Errorf("skipped %d on re-import, want the %d it created the first time",
			second.MemoriesSkipped, first.MemoriesCreated)
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
	if _, err := Unmarshal([]byte("just: some yaml\n")); err == nil {
		t.Error("a file with no format_version was accepted as an export")
	}
	if _, err := Unmarshal([]byte("format_version: 999\ndomains: []\n")); err == nil {
		t.Error("a future format version was accepted")
	}
	if _, err := Unmarshal([]byte("format_version: [not, a, number\n")); err == nil {
		t.Error("malformed YAML was accepted")
	}
}

// An unknown type must not become an event by accident: that is the one mapping
// mistake that is invisible, because the memory would silently stop appearing
// in the prompt.
func TestUnknownTypeFallsBackToFactNotEvent(t *testing.T) {
	ctx := context.Background()
	dst := newStore(t, "t-dst.cogmem.db")
	doc := Document{
		FormatVersion: FormatVersion,
		Domains: []Domain{{
			Name:   "Imported",
			Status: "active",
			Memories: []Memory{
				{Type: "observation", Status: "active", Confidence: 0.5, Text: "mystery"},
				{Type: "event", Status: "active", Confidence: 0.5, Text: "a real event"},
			},
		}},
	}
	if _, err := Import(ctx, dst, doc, ImportMerge); err != nil {
		t.Fatalf("import: %v", err)
	}
	after, _ := Export(ctx, dst)
	for _, d := range after.Domains {
		for _, m := range d.Memories {
			switch m.Text {
			case "mystery":
				if m.Type != string(store.TypeFact) {
					t.Errorf("unknown type became %q, want fact", m.Type)
				}
			case "a real event":
				if m.Type != string(store.TypeEvent) {
					t.Errorf("event type was not preserved, got %q", m.Type)
				}
			}
		}
	}
}
