// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "c.cogmem.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustGeneral returns the seeded sticky General domain.
func mustGeneral(t *testing.T, s *store.Store) store.Domain {
	t.Helper()
	d, err := s.GeneralDomain(context.Background(), s.DB())
	if err != nil {
		t.Fatalf("general domain: %v", err)
	}
	return d
}

// mustDomain creates a domain or fails the test.
func mustDomain(t *testing.T, s *store.Store, p store.CreateDomainParams) store.Domain {
	t.Helper()
	d, err := s.CreateDomain(context.Background(), s.DB(), p)
	if err != nil {
		t.Fatalf("create domain %q: %v", p.Name, err)
	}
	return d
}

// mustMemory adds an active memory or fails the test. Confidence defaults to
// 0.9 so a caller that does not care about the threshold gets a rendered line.
func mustMemory(t *testing.T, s *store.Store, p store.AddMemoryParams) store.Memory {
	t.Helper()
	if p.Status == "" {
		p.Status = store.StatusActive
	}
	if p.Confidence == 0 {
		p.Confidence = 0.9
	}
	m, err := s.AddMemory(context.Background(), s.DB(), p)
	if err != nil {
		t.Fatalf("add memory %q: %v", p.Text, err)
	}
	return m
}

// mustTouch marks a domain active now.
func mustTouch(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if err := s.Touch(context.Background(), s.DB(), id); err != nil {
		t.Fatalf("touch %s: %v", id, err)
	}
}

// ageDomain pushes a domain's last_active_at an hour into the past so a later
// Touch on another domain is observably more recent (the column is
// second-granular, so two writes in one test usually share a timestamp).
func ageDomain(t *testing.T, s *store.Store, id string) {
	t.Helper()
	old := time.Now().Add(-1 * time.Hour).Unix()
	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE domains SET last_active_at=? WHERE id=?`, old, id); err != nil {
		t.Fatalf("age domain %s: %v", id, err)
	}
}

func TestStableBlockEmpty(t *testing.T) {
	c := New(newStore(t))
	txt, _, err := c.StableBlock(context.Background())
	if err != nil || txt != "" {
		t.Fatalf("empty stable block: %q err=%v", txt, err)
	}
}

func TestStableBlockContent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	rev0, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	base := mustGeneral(t, s) // the seeded always-on general domain
	mustMemory(t, s, store.AddMemoryParams{DomainID: base.ID, Type: store.TypePreference, Text: "Be concise.", Confidence: 0.95})
	mustDomain(t, s, store.CreateDomainParams{Name: "Website Redesign", Summary: "CSS grid migration"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: base.ID, Type: store.TypeRule, Text: "Prefers tabs."})

	c := New(s)
	txt, rev, err := c.StableBlock(ctx)
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	// Two sticky memories and one domain each bump the generation.
	if rev != rev0+3 {
		t.Fatalf("stable_rev = %d, want %d (three stable-affecting writes after %d)", rev, rev0+3, rev0)
	}
	for _, want := range []string{"Be concise.", "Prefers tabs.", "## Injected cogmem domain: General (", "Topics (index)", "Website Redesign"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("stable block missing %q:\n%s", want, txt)
		}
	}
	// Type is rendered, so the assistant can tell a rule from a preference.
	// Without it every memory reads as an undifferentiated assertion, which is
	// why type was worth storing but not worth acting on.
	for _, want := range []string{"(preference) Be concise.", "(rule) Prefers tabs."} {
		if !strings.Contains(txt, want) {
			t.Fatalf("stable block missing type-prefixed line %q:\n%s", want, txt)
		}
	}
}

// StableRev is the composer's cache key: it must move when always-on content
// changes and stay put when only a topic domain's memories change.
func TestStableRevTracksStickyContentOnly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	gen := mustGeneral(t, s)
	topic := mustDomain(t, s, store.CreateDomainParams{Name: "Topic"})

	rev0, err := s.StableRev(ctx)
	if err != nil {
		t.Fatalf("stable rev: %v", err)
	}
	mustMemory(t, s, store.AddMemoryParams{DomainID: topic.ID, Type: store.TypeFact, Text: "topic fact"})
	rev1, _ := s.StableRev(ctx)
	if rev1 != rev0 {
		t.Fatalf("a topic memory must not bump stable_rev: %d -> %d", rev0, rev1)
	}
	mustMemory(t, s, store.AddMemoryParams{DomainID: gen.ID, Type: store.TypeFact, Text: "sticky fact"})
	rev2, _ := s.StableRev(ctx)
	if rev2 != rev1+1 {
		t.Fatalf("a sticky memory must bump stable_rev by one: %d -> %d", rev1, rev2)
	}
	if _, rev, err := New(s).StableBlock(ctx); err != nil || rev != rev2 {
		t.Fatalf("StableBlock rev = %d err=%v, want %d", rev, err, rev2)
	}
}

// Events never reach the prompt. The domain says how many it holds instead, so
// they stay out of the way without becoming invisible — the failure mode of the
// review status they replace, where 236 of 244 memories were unreachable by any
// path at all.
func TestStableBlockExcludesEventsAndReportsTheirCount(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{DomainID: base.ID, Type: store.TypeFact, Text: "Home is Ottawa."})
	for _, txt := range []string{"Drove to the KOA Sep 4.", "Drove home Sep 7."} {
		mustMemory(t, s, store.AddMemoryParams{DomainID: base.ID, Type: store.TypeEvent, Text: txt})
	}

	txt, _, err := New(s).StableBlock(ctx)
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	if !strings.Contains(txt, "Home is Ottawa.") {
		t.Fatalf("standing fact missing:\n%s", txt)
	}
	for _, ev := range []string{"Drove to the KOA", "Drove home"} {
		if strings.Contains(txt, ev) {
			t.Fatalf("event %q reached the prompt:\n%s", ev, txt)
		}
	}
	// The line must name the tool and the argument. Saying only "search to
	// retrieve" sent a live agent into four identical searches without
	// include_events before it gave up — the count advertised memories it could
	// not then find.
	if !strings.Contains(txt, "(2 event memories here — cogmem_memory_search with include_events:true to read them)") {
		t.Fatalf("event count line missing or malformed:\n%s", txt)
	}
}

// The ROUTED block carries the same count line, with the singular form for one.
func TestRoutedBlockEventCountLine(t *testing.T) {
	cases := []struct {
		name   string
		events int
		want   string
	}{
		{"none", 0, ""},
		{"one", 1, "(1 event memory here — cogmem_memory_search with include_events:true to read it)\n"},
		{"many", 3, "(3 event memories here — cogmem_memory_search with include_events:true to read them)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			d := mustDomain(t, s, store.CreateDomainParams{Name: "Trips"})
			mustMemory(t, s, store.AddMemoryParams{DomainID: d.ID, Type: store.TypeFact, Text: "Home base."})
			for i := 0; i < tc.events; i++ {
				mustMemory(t, s, store.AddMemoryParams{DomainID: d.ID, Type: store.TypeEvent, Text: fmt.Sprintf("trip %d", i)})
			}
			res, err := New(s).RoutedBlock(context.Background(), RouteRequest{})
			if err != nil {
				t.Fatalf("routed: %v", err)
			}
			if tc.want == "" {
				if strings.Contains(res.Text, "event memor") {
					t.Fatalf("no events, but a count line rendered:\n%s", res.Text)
				}
				return
			}
			if !strings.Contains(res.Text+"\n", tc.want) {
				t.Fatalf("routed block missing %q:\n%s", tc.want, res.Text)
			}
			for i := 0; i < tc.events; i++ {
				if strings.Contains(res.Text, fmt.Sprintf("trip %d", i)) {
					t.Fatalf("event text reached the routed block:\n%s", res.Text)
				}
			}
		})
	}
}

// The prompt tag says "origin", not "source". It always rendered Origin, and
// the separate source field it was named after no longer exists — leaving the
// old label would name a field that is gone.
func TestOriginTagNamesOrigin(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{
		DomainID: base.ID, Type: store.TypeFact, Text: "Typed by hand.",
		Confidence: 1.0, Origin: store.OriginUser,
	})

	txt, _, err := New(s).StableBlock(ctx)
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	if !strings.Contains(txt+"\n", "- (fact) Typed by hand. [origin: user]\n") {
		t.Fatalf("expected [origin: user] tag:\n%s", txt)
	}
	if strings.Contains(txt, "[source:") {
		t.Fatalf("stale [source:] tag still rendered:\n%s", txt)
	}
}

// The assistant's own notes (chat origin) are the unremarkable default and get
// no tag; anything else says where it came from.
func TestOriginTagPerOrigin(t *testing.T) {
	cases := []struct {
		origin store.Origin
		text   string
		want   string
	}{
		{store.OriginChat, "Chat note.", "- (fact) Chat note.\n"},
		{"", "Default note.", "- (fact) Default note.\n"},
		{store.OriginConsolidation, "Consolidated.", "- (fact) Consolidated. [origin: consolidation]\n"},
		{store.OriginUser, "By hand.", "- (fact) By hand. [origin: user]\n"},
	}
	for _, tc := range cases {
		t.Run(string(tc.origin), func(t *testing.T) {
			s := newStore(t)
			base := mustGeneral(t, s)
			mustMemory(t, s, store.AddMemoryParams{DomainID: base.ID, Type: store.TypeFact, Text: tc.text, Origin: tc.origin})
			txt, _, err := New(s).StableBlock(context.Background())
			if err != nil {
				t.Fatalf("stable block: %v", err)
			}
			if !strings.Contains(txt+"\n", tc.want) {
				t.Fatalf("want line %q in:\n%s", tc.want, txt)
			}
			if tc.origin == store.OriginChat || tc.origin == "" {
				if strings.Contains(txt, "[origin:") {
					t.Fatalf("chat-origin memory must carry no origin tag:\n%s", txt)
				}
			}
		})
	}
}

// Memories under the confidence floor are omitted from both blocks; the floor
// defaults to 0.65 and WithMinConfidence raises it.
func TestMinConfidenceFilter(t *testing.T) {
	cases := []struct {
		name    string
		opts    []Option
		keep    float64
		drop    float64
		wantMin float64
	}{
		{"default", nil, 0.7, 0.6, 0.65},
		{"raised", []Option{WithMinConfidence(0.9)}, 0.9, 0.8, 0.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			gen := mustGeneral(t, s)
			topic := mustDomain(t, s, store.CreateDomainParams{Name: "Topic"})
			mustMemory(t, s, store.AddMemoryParams{DomainID: gen.ID, Type: store.TypeFact, Text: "sticky kept", Confidence: tc.keep})
			mustMemory(t, s, store.AddMemoryParams{DomainID: gen.ID, Type: store.TypeFact, Text: "sticky dropped", Confidence: tc.drop})
			mustMemory(t, s, store.AddMemoryParams{DomainID: topic.ID, Type: store.TypeFact, Text: "routed kept", Confidence: tc.keep})
			mustMemory(t, s, store.AddMemoryParams{DomainID: topic.ID, Type: store.TypeFact, Text: "routed dropped", Confidence: tc.drop})

			c := New(s, tc.opts...)
			if c.opt.minConfidence != tc.wantMin {
				t.Fatalf("minConfidence = %v, want %v", c.opt.minConfidence, tc.wantMin)
			}
			res := composeWith(t, s, tc.opts...)
			for _, want := range []string{"sticky kept"} {
				if !strings.Contains(res.Stable, want) {
					t.Errorf("stable missing %q:\n%s", want, res.Stable)
				}
			}
			if strings.Contains(res.Stable, "sticky dropped") {
				t.Errorf("stable rendered a memory below the floor:\n%s", res.Stable)
			}
			if !strings.Contains(res.Routed, "routed kept") {
				t.Errorf("routed missing kept memory:\n%s", res.Routed)
			}
			if strings.Contains(res.Routed, "routed dropped") {
				t.Errorf("routed rendered a memory below the floor:\n%s", res.Routed)
			}
		})
	}
}

// The composer's fallback levers are the documented defaults.
func TestComposerDefaults(t *testing.T) {
	c := New(nil)
	if c.opt.topKDomains != 3 {
		t.Errorf("default topKDomains = %d, want 3", c.opt.topKDomains)
	}
	if c.opt.maxChars != 4000 {
		t.Errorf("default maxChars = %d, want 4000", c.opt.maxChars)
	}
	if c.opt.minConfidence != 0.65 {
		t.Errorf("default minConfidence = %v, want 0.65", c.opt.minConfidence)
	}
	if c.opt.fileMaxBytes != 256*1024 {
		t.Errorf("default fileMaxBytes = %d, want %d", c.opt.fileMaxBytes, 256*1024)
	}
	if c.opt.fileTotalMaxBytes != 512*1024 {
		t.Errorf("default fileTotalMaxBytes = %d, want %d", c.opt.fileTotalMaxBytes, 512*1024)
	}
	// Non-positive values leave the defaults alone.
	c = New(nil, WithTopKDomains(0), WithMaxChars(-1), WithMinConfidence(0), WithFileMaxBytes(0), WithFileTotalMaxBytes(-5))
	if c.opt.topKDomains != 3 || c.opt.maxChars != 4000 || c.opt.minConfidence != 0.65 ||
		c.opt.fileMaxBytes != 256*1024 || c.opt.fileTotalMaxBytes != 512*1024 {
		t.Errorf("non-positive options changed defaults: %+v", c.opt)
	}
}

// With five topic domains and no options, exactly three load.
func TestRoutedBlockDefaultTopK(t *testing.T) {
	s := newStore(t)
	for i := range 5 {
		mustDomain(t, s, store.CreateDomainParams{Name: fmt.Sprintf("Topic %d", i)})
	}
	res, err := New(s).RoutedBlock(context.Background(), RouteRequest{})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 3 {
		t.Fatalf("loaded %d domains, want the default 3: %v", len(res.Loaded), res.Loaded)
	}
	if n := strings.Count(res.Text, "## Injected cogmem domain: "); n != 3 {
		t.Fatalf("rendered %d sections, want 3:\n%s", n, res.Text)
	}
}

// The char budget drops later sections, but the first section always renders
// even when it alone is over budget, so a tight budget never blanks the block.
func TestRoutedBlockFirstSectionAlwaysRenders(t *testing.T) {
	s := newStore(t)
	big := mustDomain(t, s, store.CreateDomainParams{Name: "Big", Summary: strings.Repeat("summary ", 40)})
	ageDomain(t, s, big.ID)
	small := mustDomain(t, s, store.CreateDomainParams{Name: "Small"})
	ageDomain(t, s, small.ID)
	mustTouch(t, s, big.ID) // Big is most recent, so it is the first section

	res, err := New(s, WithMaxChars(50)).RoutedBlock(context.Background(), RouteRequest{})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != big.ID {
		t.Fatalf("loaded = %v, want only the over-budget first section %s", res.Loaded, big.ID)
	}
	if len(res.Text) <= 50 {
		t.Fatalf("first section should render whole even over budget, got %d chars", len(res.Text))
	}
	if strings.Contains(res.Text, "Small") {
		t.Fatalf("second section must be dropped once the budget is spent:\n%s", res.Text)
	}
}

// The topic index is one exact line per non-sticky active domain, first line
// of the summary only; sticky domains are rendered in full and never indexed.
func TestTopicIndexFormat(t *testing.T) {
	s := newStore(t)
	topic := mustDomain(t, s, store.CreateDomainParams{
		Name: "Website Redesign", Summary: "CSS grid migration\nsecond line is not shown",
	})
	sticky := mustDomain(t, s, store.CreateDomainParams{Sticky: true, Name: "Always On", Summary: "pinned"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: sticky.ID, Type: store.TypeFact, Text: "pinned fact"})

	txt, _, err := New(s).StableBlock(context.Background())
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	wantLine := "- " + topic.ID + " · Website Redesign — CSS grid migration\n"
	if !strings.Contains(txt+"\n", "## Topics (index)\n"+wantLine) {
		t.Fatalf("index missing exact line %q:\n%s", wantLine, txt)
	}
	if strings.Contains(txt, "second line is not shown") {
		t.Fatalf("index must show only the summary's first line:\n%s", txt)
	}
	if strings.Contains(txt, "- "+sticky.ID+" ·") {
		t.Fatalf("sticky domain must not be listed in the topic index:\n%s", txt)
	}
	if !strings.Contains(txt, "## Injected cogmem domain: Always On ("+sticky.ID+") — sticky, loaded every turn\n- (fact) pinned fact\n") {
		t.Fatalf("sticky domain should render in full:\n%s", txt)
	}
}

// With no topic domains there is no index heading at all.
func TestTopicIndexAbsentWithoutTopics(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{DomainID: gen.ID, Type: store.TypeFact, Text: "only sticky"})
	txt, _, err := New(s).StableBlock(context.Background())
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	if strings.Contains(txt, "Topics (index)") {
		t.Fatalf("no topics, but an index rendered:\n%s", txt)
	}
}

// Sticky domains render highest StickyPriority first, then by name. Every
// domain the store creates has the same priority, so the order is by name.
func TestStableBlockStickyOrder(t *testing.T) {
	s := newStore(t)
	gen := mustGeneral(t, s)
	zulu := mustDomain(t, s, store.CreateDomainParams{Sticky: true, Name: "Zulu"})
	alpha := mustDomain(t, s, store.CreateDomainParams{Sticky: true, Name: "Alpha"})
	for _, d := range []store.Domain{gen, zulu, alpha} {
		mustMemory(t, s, store.AddMemoryParams{DomainID: d.ID, Type: store.TypeFact, Text: d.Name + " fact"})
	}

	txt, _, err := New(s).StableBlock(context.Background())
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	a := strings.Index(txt, "## Injected cogmem domain: Alpha (")
	g := strings.Index(txt, "## Injected cogmem domain: General (")
	z := strings.Index(txt, "## Injected cogmem domain: Zulu (")
	if a < 0 || g < 0 || z < 0 {
		t.Fatalf("a sticky domain is missing (a=%d g=%d z=%d):\n%s", a, g, z, txt)
	}
	if a >= g || g >= z {
		t.Fatalf("sticky domains out of name order (Alpha@%d General@%d Zulu@%d):\n%s", a, g, z, txt)
	}
}

// A sticky domain with nothing to show (no prompt memories, no events) is
// skipped rather than rendered as an empty heading.
func TestStableBlockSkipsEmptyStickyDomain(t *testing.T) {
	s := newStore(t)
	mustDomain(t, s, store.CreateDomainParams{Sticky: true, Name: "Empty"})
	gen := mustGeneral(t, s)
	mustMemory(t, s, store.AddMemoryParams{DomainID: gen.ID, Type: store.TypeFact, Text: "one fact"})
	txt, _, err := New(s).StableBlock(context.Background())
	if err != nil {
		t.Fatalf("stable block: %v", err)
	}
	if strings.Contains(txt, "Empty") {
		t.Fatalf("empty sticky domain should not render:\n%s", txt)
	}
}

func TestRoutedBlockToolTrigger(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// Two domains; "Email" is older (less recent) but trigger-matched.
	email := mustDomain(t, s, store.CreateDomainParams{
		Name: "Email", Summary: "mail prefs",
		Triggers: "google_gmail,microsoft365_mail",
	})
	other := mustDomain(t, s, store.CreateDomainParams{Name: "Other", Summary: "misc"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: email.ID, Type: store.TypePreference, Text: "Archive newsletters."})
	// "Other" is the most recently touched, so recency alone would rank it first.
	ageDomain(t, s, email.ID)
	mustTouch(t, s, other.ID)

	c := New(s, WithTopKDomains(1))
	res, err := c.RoutedBlock(ctx, RouteRequest{
		RecentTools: []string{"mcp__fusion__system__get", "mcp__fusion__google_gmail_messages_list"},
		Trace:       true,
	})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	// The tool-triggered Email domain must win the single slot over more-recent Other.
	if len(res.Loaded) != 1 || res.Loaded[0] != email.ID {
		t.Fatalf("expected tool-triggered %s first, got %v", email.ID, res.Loaded)
	}
	if len(res.Trace) != 1 || res.Trace[0].Signal != "tool:google_gmail" {
		t.Fatalf("trace = %+v, want signal tool:google_gmail", res.Trace)
	}
	if !strings.Contains(res.Text, "Archive newsletters.") {
		t.Fatalf("routed text missing email hook:\n%s", res.Text)
	}
}

// TestRoutedBlockTouchesMatchedNotRecency verifies the recency/staleness wiring:
// a domain loaded because it genuinely matched (a tool trigger here) is Touched,
// while a domain pulled in only as recency filler is left alone — otherwise the
// recency signal would be self-reinforcing and no domain could ever go cold.
func TestRoutedBlockTouchesMatchedNotRecency(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	db := s.DB()
	email := mustDomain(t, s, store.CreateDomainParams{
		Name: "Email", Summary: "mail", Triggers: "gmail",
	})
	other := mustDomain(t, s, store.CreateDomainParams{Name: "Other", Summary: "misc"})

	// Pre-age both domains so the second-granularity touch is observable (creation
	// and the compose call would otherwise share the same wall-clock second).
	old := time.Now().Add(-48 * time.Hour).Unix()
	if _, err := db.ExecContext(ctx, `UPDATE domains SET last_active_at=? WHERE id IN (?,?)`, old, email.ID, other.ID); err != nil {
		t.Fatalf("pre-age domains: %v", err)
	}
	lastActive := func(id string) int64 {
		d, err := s.GetDomain(ctx, db, id, false)
		if err != nil {
			t.Fatalf("get domain %s: %v", id, err)
		}
		return d.LastActiveAt
	}

	c := New(s, WithTopKDomains(8)) // both fit: Email via tool, Other via recency
	res, err := c.RoutedBlock(ctx, RouteRequest{RecentTools: []string{"mcp__fusion__google_gmail_messages_list"}, Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 2 {
		t.Fatalf("expected both domains loaded, got %v", res.Loaded)
	}

	if emailAfter := lastActive(email.ID); emailAfter <= old {
		t.Errorf("tool-matched Email should be touched to now: old=%d after=%d", old, emailAfter)
	}
	if otherAfter := lastActive(other.ID); otherAfter != old {
		t.Errorf("recency-filler Other must NOT be touched: old=%d after=%d", old, otherAfter)
	}
}

func TestRoutedBlockRecency(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	d1 := mustDomain(t, s, store.CreateDomainParams{Name: "Old", Summary: "old"})
	d2 := mustDomain(t, s, store.CreateDomainParams{Name: "Recent", Summary: "recent"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: d2.ID, Type: store.TypeFact, Text: "key fact"})
	// Make d2 strictly more recent than d1 (seconds granularity): age d1 back, d2 = now.
	ageDomain(t, s, d1.ID)
	mustTouch(t, s, d2.ID)

	c := New(s, WithTopKDomains(1))
	res, err := c.RoutedBlock(ctx, RouteRequest{Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != d2.ID {
		t.Fatalf("expected most-recent %s, got %v", d2.ID, res.Loaded)
	}
	if !strings.Contains(res.Text, "Recent") || !strings.Contains(res.Text, "key fact") {
		t.Fatalf("routed text:\n%s", res.Text)
	}
	if len(res.Trace) != 1 || res.Trace[0].Signal != "recency" {
		t.Fatalf("trace = %+v", res.Trace)
	}
}

// A domain with keyword_triggers loads when its phrase appears in the message
// (beating a more-recent domain for the single slot), and not for a bare word.
func TestRoutedBlockKeywordTrigger(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	wf := mustDomain(t, s, store.CreateDomainParams{
		Name:            "Daily Ops",
		KeywordTriggers: "morning routine",
	})
	mustMemory(t, s, store.AddMemoryParams{DomainID: wf.ID, Type: store.TypeRule, Text: "Review the calendar."})
	other := mustDomain(t, s, store.CreateDomainParams{Name: "Other", Summary: "misc"})
	ageDomain(t, s, wf.ID)
	mustTouch(t, s, other.ID) // Other is the most recent

	c := New(s, WithTopKDomains(1))

	// Phrase present → keyword routes the workflow into the single slot, beating recency.
	rr, err := c.RoutedBlock(ctx, RouteRequest{RouteText: "time for your morning routine", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(rr.Loaded) != 1 || rr.Loaded[0] != wf.ID {
		t.Fatalf("keyword should route the workflow, got %v", rr.Loaded)
	}
	if len(rr.Trace) != 1 || rr.Trace[0].Signal != "keyword:morning routine" {
		t.Fatalf("trace = %+v, want keyword:morning routine", rr.Trace)
	}

	// The keyword match above marked the workflow recently-active (recency wiring).
	// Make Other strictly more recent than wf for the recency check — last_active_at
	// is second-granular, so age wf back rather than relying on same-second ordering.
	ageDomain(t, s, wf.ID)
	mustTouch(t, s, other.ID)

	// Bare "morning" → no keyword (phrase-only); the more-recent Other takes the
	// slot instead.
	rr2, err := c.RoutedBlock(ctx, RouteRequest{RouteText: "good morning everyone"})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(rr2.Loaded) != 1 || rr2.Loaded[0] != other.ID {
		t.Fatalf("bare 'morning' should not route the workflow; expected Other, got %v", rr2.Loaded)
	}
}

// Keyword phrases match on word boundaries through the composer: "git" routes
// on "git" but not on "legitimate".
func TestRoutedBlockKeywordWordBoundary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	vcs := mustDomain(t, s, store.CreateDomainParams{Name: "Source Control", Summary: "branching", KeywordTriggers: "git"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: vcs.ID, Type: store.TypeRule, Text: "Rebase before merging."})
	decoy := mustDomain(t, s, store.CreateDomainParams{Name: "Decoy", Summary: "misc"})
	ageDomain(t, s, vcs.ID)
	mustTouch(t, s, decoy.ID)

	c := New(s, WithTopKDomains(1))
	rr, err := c.RoutedBlock(ctx, RouteRequest{RouteText: "that is a legitimate concern", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(rr.Loaded) != 1 || rr.Loaded[0] != decoy.ID || rr.Trace[0].Signal != "recency" {
		t.Fatalf("'legitimate' must not trigger 'git': loaded=%v trace=%+v", rr.Loaded, rr.Trace)
	}

	rr, err = c.RoutedBlock(ctx, RouteRequest{RouteText: "push it to git now", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(rr.Loaded) != 1 || rr.Loaded[0] != vcs.ID || rr.Trace[0].Signal != "keyword:git" {
		t.Fatalf("whole word 'git' should trigger: loaded=%v trace=%+v", rr.Loaded, rr.Trace)
	}
	if !strings.Contains(rr.Text, "(rule) Rebase before merging.") {
		t.Fatalf("routed text missing the rule:\n%s", rr.Text)
	}
}

// Every routing signal becomes a plain reason in the domain's header, so the
// model can see why a domain was injected and adjust its trigger.
func TestSignalReason(t *testing.T) {
	for signal, want := range map[string]string{
		"tool:google_gmail":             `loaded by tool trigger "google_gmail"`,
		"keyword:release train":         `loaded by keyword trigger "release train"`,
		"keyword:invoice,invoices,bill": `loaded by keyword triggers "invoice", "invoices", "bill"`,
		"recent:release train":          `loaded by keyword trigger "release train" in a recent message`,
		"recent:invoice,bill":           `loaded by keyword triggers "invoice", "bill" in a recent message`,
		"recency":                       "loaded because it was recently active",
		"match:invoice":                 "loaded by match:invoice", // lexical matching is gone
		"other":                         "loaded by other",
	} {
		if got := signalReason(signal); got != want {
			t.Errorf("signalReason(%q) = %q, want %q", signal, got, want)
		}
	}
}

// One domain matched by tool trigger, keyword trigger and recency at once
// appears exactly once, under the strongest (tool) signal.
func TestRoutedBlockSignalPriorityNoDuplicate(t *testing.T) {
	s := newStore(t)
	email := mustDomain(t, s, store.CreateDomainParams{
		Name: "Email", Summary: "email handling", Triggers: "google_gmail", KeywordTriggers: "email",
	})
	mustTouch(t, s, email.ID)

	res, err := New(s, WithTopKDomains(8)).RoutedBlock(context.Background(), RouteRequest{
		RouteText:   "please check my email",
		RecentTexts: []string{"email again"},
		RecentTools: []string{"mcp__fusion__google_gmail_messages_list"},
		Trace:       true,
	})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != email.ID {
		t.Fatalf("domain should appear exactly once, got %v", res.Loaded)
	}
	if n := strings.Count(res.Text, "("+email.ID+") — "); n != 1 {
		t.Fatalf("domain rendered %d times, want 1:\n%s", n, res.Text)
	}
	if len(res.Trace) != 1 || res.Trace[0].Signal != "tool:google_gmail" {
		t.Fatalf("tool signal should win over keyword/recency, got %+v", res.Trace)
	}
}

// Lexical matching is gone: a message made of words that appear in domain
// names, summaries and memory text loads nothing by content. Every slot is
// recency filler, in recency order, and none of them is touched.
func TestRoutedBlockNoLexicalLoading(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	bio := mustDomain(t, s, store.CreateDomainParams{Name: "BioTech", Summary: "research report"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: bio.ID, Type: store.TypeFact, Text: "The biotech report targets Q3."})
	birds := mustDomain(t, s, store.CreateDomainParams{Name: "Raptors", Summary: "kestrel and falcon notes"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: birds.ID, Type: store.TypeFact, Text: "The kestrel hunts at dawn; the falcon nests on the tower."})
	recent := mustDomain(t, s, store.CreateDomainParams{Name: "Unrelated", Summary: "misc"})
	ageDomain(t, s, bio.ID)
	ageDomain(t, s, birds.ID)
	mustTouch(t, s, recent.ID)
	before := map[string]int64{}
	for _, id := range []string{bio.ID, birds.ID} {
		d, _ := s.GetDomain(ctx, s.DB(), id, false)
		before[id] = d.LastActiveAt
	}

	for _, tc := range []struct {
		name string
		topK int
		text string
	}{
		{"one slot, memory words", 1, "what's the status of the biotech report targets?"},
		{"one slot, name and summary words", 1, "raptors kestrel falcon notes research"},
		{"all slots, every word", 8, "biotech report research raptors kestrel falcon hunts dawn tower notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := New(s, WithTopKDomains(tc.topK)).RoutedBlock(ctx, RouteRequest{RouteText: tc.text, Trace: true})
			if err != nil {
				t.Fatalf("routed: %v", err)
			}
			if res.Loaded[0] != recent.ID {
				t.Fatalf("most recent %s should take the first slot, got %v", recent.ID, res.Loaded)
			}
			if len(res.Loaded) != min(tc.topK, 3) {
				t.Fatalf("loaded %v, want %d domains", res.Loaded, min(tc.topK, 3))
			}
			for _, sel := range res.Trace {
				if sel.Signal != "recency" {
					t.Fatalf("trace %+v: want only recency signals", res.Trace)
				}
			}
		})
	}
	for id, was := range before {
		if d, _ := s.GetDomain(ctx, s.DB(), id, false); d.LastActiveAt != was {
			t.Errorf("domain %s was touched by a content-free route", id)
		}
	}
}

// A domain routes on its own keywords and on those consolidation added alike.
func TestRoutedBlockKeywordUnion(t *testing.T) {
	s := newStore(t)
	d := mustDomain(t, s, store.CreateDomainParams{
		Name: "Billing", KeywordTriggers: "invoice run", AutoKeywords: "accounts payable", BlockedKeywords: "billing cycle",
	})
	decoy := mustDomain(t, s, store.CreateDomainParams{Name: "Decoy"})
	ageDomain(t, s, d.ID)
	mustTouch(t, s, decoy.ID)

	for _, tc := range []struct {
		name, text, signal string
	}{
		{"agent keyword", "start the invoice run", "keyword:invoice run"},
		{"consolidation keyword", "ask accounts payable", "keyword:accounts payable"},
		{"both", "accounts payable owns the invoice run", "keyword:invoice run,accounts payable"},
		{"blocked phrase does not route", "the billing cycle ends", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := New(s, WithTopKDomains(1)).RoutedBlock(context.Background(), RouteRequest{RouteText: tc.text, Trace: true})
			if err != nil {
				t.Fatalf("routed: %v", err)
			}
			if tc.signal == "" {
				if res.Loaded[0] != decoy.ID {
					t.Fatalf("loaded %v, want the recency decoy", res.Loaded)
				}
				return
			}
			if res.Loaded[0] != d.ID || res.Trace[0].Signal != tc.signal {
				t.Fatalf("loaded %v trace %+v, want %s with %q", res.Loaded, res.Trace, d.ID, tc.signal)
			}
			ageDomain(t, s, d.ID) // keep the decoy the most recent for the next case
		})
	}
}

// keywordFixture creates topic domains in the given recency order (first is
// the most recent) and returns their ids by name.
func keywordFixture(t *testing.T, s *store.Store, domains []store.CreateDomainParams) map[string]string {
	t.Helper()
	ids := map[string]string{}
	base := time.Now().Add(-time.Hour).Unix()
	for i, p := range domains {
		d := mustDomain(t, s, p)
		ids[p.Name] = d.ID
		if _, err := s.DB().ExecContext(context.Background(),
			`UPDATE domains SET last_active_at=? WHERE id=?`, base-int64(i*60), d.ID); err != nil {
			t.Fatalf("set recency: %v", err)
		}
	}
	return ids
}

// Keyword matches are ranked by how many distinct phrases matched, most
// first; recency breaks ties; tool triggers still come first; topK truncates
// the ranked list.
func TestRoutedBlockKeywordRanking(t *testing.T) {
	// Recency order: Newest, Middle, Oldest, Tooled.
	fixture := []store.CreateDomainParams{
		{Name: "Newest", KeywordTriggers: "alpha"},
		{Name: "Middle", KeywordTriggers: "beta"},
		{Name: "Oldest", KeywordTriggers: "alpha,beta,gamma", AutoKeywords: "delta"},
		{Name: "Tooled", Triggers: "zeta_tool"},
		{Name: "Filler"},
	}
	for _, tc := range []struct {
		name  string
		topK  int
		text  string
		tools []string
		want  []string // names, in order
	}{
		{"more phrases beat recency", 8, "alpha beta gamma", nil, []string{"Oldest", "Newest", "Middle", "Tooled", "Filler"}},
		{"consolidation phrases count", 8, "beta delta", nil, []string{"Oldest", "Middle", "Newest", "Tooled", "Filler"}},
		{"tie goes to the more recent", 8, "beta only", nil, []string{"Middle", "Oldest", "Newest", "Tooled", "Filler"}},
		{"tool trigger first", 8, "alpha beta", []string{"mcp_zeta_tool_run"}, []string{"Tooled", "Oldest", "Newest", "Middle", "Filler"}},
		{"topK truncates the ranking", 2, "alpha beta gamma", nil, []string{"Oldest", "Newest"}},
		{"topK keeps tool then best keyword", 2, "alpha beta", []string{"zeta_tool"}, []string{"Tooled", "Oldest"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ids := keywordFixture(t, s, fixture)
			res, err := New(s, WithTopKDomains(tc.topK)).RoutedBlock(context.Background(),
				RouteRequest{RouteText: tc.text, RecentTools: tc.tools, Trace: true})
			if err != nil {
				t.Fatalf("routed: %v", err)
			}
			var want []string
			for _, n := range tc.want {
				want = append(want, ids[n])
			}
			if strings.Join(res.Loaded, ",") != strings.Join(want, ",") {
				t.Fatalf("loaded %v (trace %+v), want %v", res.Loaded, res.Trace, tc.want)
			}
		})
	}
}

// The signal, the trace and the header name up to three matched phrases.
func TestRoutedBlockKeywordSignalNamesPhrases(t *testing.T) {
	s := newStore(t)
	d := mustDomain(t, s, store.CreateDomainParams{Name: "Fleet", KeywordTriggers: "tail number,hangar,fuel order", AutoKeywords: "annual inspection"})
	res, err := New(s).RoutedBlock(context.Background(), RouteRequest{
		RouteText: "Hangar fuel order and annual inspection for the tail number", Trace: true,
	})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Trace) != 1 || res.Trace[0].Signal != "keyword:tail number,hangar,fuel order" {
		t.Fatalf("trace = %+v, want the first three phrases", res.Trace)
	}
	header := "## Injected cogmem domain: Fleet (" + d.ID + `) — loaded by keyword triggers "tail number", "hangar", "fuel order"`
	if res.Text != header {
		t.Fatalf("routed text:\n%s\nwant header:\n%s", res.Text, header)
	}
}

// Keywords also match the previous few messages. A domain matched in the
// current message ranks ahead of one matched only earlier, whatever their
// phrase counts or recency; the earlier-only match has its own signal and
// header; both kinds touch the domain, recency filler does not.
func TestRoutedBlockRecentTexts(t *testing.T) {
	fixture := []store.CreateDomainParams{
		{Name: "Earlier", KeywordTriggers: "kestrel,falcon"}, // most recent, two phrases
		{Name: "Current", KeywordTriggers: "invoice run"},
		{Name: "Filler"},
	}
	for _, tc := range []struct {
		name    string
		topK    int
		text    string
		earlier []string
		want    []string
		signals []string
	}{
		{
			"current beats earlier", 8, "start the invoice run",
			[]string{"saw a kestrel", "and a falcon"},
			[]string{"Current", "Earlier", "Filler"},
			[]string{"keyword:invoice run", "recent:kestrel,falcon", "recency"},
		},
		{
			"topK keeps the current match", 1, "start the invoice run",
			[]string{"kestrel and falcon"},
			[]string{"Current"},
			[]string{"keyword:invoice run"},
		},
		{
			"current match ignores earlier texts", 8, "kestrel",
			[]string{"falcon", "invoice run"},
			[]string{"Earlier", "Current", "Filler"},
			[]string{"keyword:kestrel", "recent:invoice run", "recency"},
		},
		{
			"no earlier texts", 8, "nothing here", nil,
			[]string{"Earlier", "Current", "Filler"},
			[]string{"recency", "recency", "recency"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ids := keywordFixture(t, s, fixture)
			ctx := context.Background()
			res, err := New(s, WithTopKDomains(tc.topK)).RoutedBlock(ctx,
				RouteRequest{RouteText: tc.text, RecentTexts: tc.earlier, Trace: true})
			if err != nil {
				t.Fatalf("routed: %v", err)
			}
			var gotNames, gotSignals []string
			for _, sel := range res.Trace {
				gotNames = append(gotNames, sel.Name)
				gotSignals = append(gotSignals, sel.Signal)
			}
			if strings.Join(gotNames, ",") != strings.Join(tc.want, ",") ||
				strings.Join(gotSignals, "|") != strings.Join(tc.signals, "|") {
				t.Fatalf("trace = %+v, want %v with %v", res.Trace, tc.want, tc.signals)
			}
			// Keyword matches (current or earlier) are touched; filler is not.
			for _, sel := range res.Trace {
				d, _ := s.GetDomain(ctx, s.DB(), ids[sel.Name], false)
				touched := d.LastActiveAt > time.Now().Add(-time.Minute).Unix()
				if touched != (sel.Signal != "recency") {
					t.Errorf("%s (%s): touched=%v", sel.Name, sel.Signal, touched)
				}
			}
		})
	}
}

// Two domains matched only in earlier messages are ranked by distinct phrase
// count across those messages, ahead of recency.
func TestRoutedBlockRecentTextsRankByPhraseCount(t *testing.T) {
	s := newStore(t)
	keywordFixture(t, s, []store.CreateDomainParams{
		{Name: "One", KeywordTriggers: "alpha"},       // most recent, one phrase
		{Name: "Two", KeywordTriggers: "beta,gamma"},  // older, two phrases
		{Name: "Tie", KeywordTriggers: "delta,alpha"}, // oldest, one phrase (alpha) seen
	})
	res, err := New(s, WithTopKDomains(8)).RoutedBlock(context.Background(), RouteRequest{
		RouteText: "nothing here", RecentTexts: []string{"alpha and beta", "gamma alpha"}, Trace: true,
	})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	got := make([]string, 0, len(res.Trace))
	for _, sel := range res.Trace {
		got = append(got, sel.Name+"="+sel.Signal)
	}
	want := []string{"Two=recent:beta,gamma", "One=recent:alpha", "Tie=recent:alpha"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("trace = %v, want %v", got, want)
	}
}

// A domain matched only in an earlier message says so in its header.
func TestRoutedBlockRecentTextHeader(t *testing.T) {
	s := newStore(t)
	d := mustDomain(t, s, store.CreateDomainParams{Name: "Birds", KeywordTriggers: "kestrel"})
	res, err := New(s).RoutedBlock(context.Background(), RouteRequest{RouteText: "and then?", RecentTexts: []string{"a kestrel"}})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	want := "## Injected cogmem domain: Birds (" + d.ID + `) — loaded by keyword trigger "kestrel" in a recent message`
	if !strings.HasPrefix(res.Text, want) {
		t.Fatalf("routed text:\n%s\nwant prefix:\n%s", res.Text, want)
	}
}

// An injected domain shows its state lists (blockers, next actions,
// constraints) under their headings, in order, before its memories.
func TestRenderDomainState(t *testing.T) {
	d := store.Domain{
		ID:      "d1",
		Name:    "Launch",
		Summary: "Ship the beta.\nSecond line is dropped.",
		State: store.DomainState{
			Blockers:    []string{"waiting on legal"},
			NextActions: []string{"draft the notes", "book the room"},
			Constraints: []string{"no weekend deploys"},
		},
	}
	hooks := []store.Memory{{ID: "h1", Type: store.TypeRule, Text: "be brief"}}
	got := renderDomain(d, hooks, 0, "keyword:launch")
	want := "## Injected cogmem domain: Launch (d1) — loaded by keyword trigger \"launch\"\n" +
		"Summary: Ship the beta.\n" +
		"Blockers:\n- waiting on legal\n" +
		"Next actions:\n- draft the notes\n- book the room\n" +
		"Constraints:\n- no weekend deploys\n" +
		"- (h1) (rule) be brief\n" +
		"\n"
	if got != want {
		t.Fatalf("renderDomain =\n%s\nwant\n%s", got, want)
	}
}

// Empty state lists produce no headings.
func TestRenderDomainOmitsEmptyState(t *testing.T) {
	got := renderDomain(store.Domain{ID: "d1", Name: "Launch"}, nil, 0, "recency")
	for _, h := range []string{"Blockers:", "Next actions:", "Constraints:"} {
		if strings.Contains(got, h) {
			t.Errorf("rendered %q for empty state:\n%s", h, got)
		}
	}
}
