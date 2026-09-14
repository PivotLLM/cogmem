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
	for _, want := range []string{"Be concise.", "Prefers tabs.", "COGMEM domain General is sticky", "Topics (index)", "Website Redesign"} {
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
	for i := 0; i < 5; i++ {
		mustDomain(t, s, store.CreateDomainParams{Name: fmt.Sprintf("Topic %d", i)})
	}
	res, err := New(s).RoutedBlock(context.Background(), RouteRequest{})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 3 {
		t.Fatalf("loaded %d domains, want the default 3: %v", len(res.Loaded), res.Loaded)
	}
	if n := strings.Count(res.Text, "## Active Context: "); n != 3 {
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
	if !strings.Contains(txt, "COGMEM domain Always On is sticky:\n\n- (fact) pinned fact\n") {
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
	a := strings.Index(txt, "COGMEM domain Alpha is sticky")
	g := strings.Index(txt, "COGMEM domain General is sticky")
	z := strings.Index(txt, "COGMEM domain Zulu is sticky")
	if a < 0 || g < 0 || z < 0 {
		t.Fatalf("a sticky domain is missing (a=%d g=%d z=%d):\n%s", a, g, z, txt)
	}
	if !(a < g && g < z) {
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

func TestRoutedBlockLexicalMatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// "BioTech" is older/less recent; "Other" is the most recently touched.
	bio := mustDomain(t, s, store.CreateDomainParams{Name: "BioTech", Summary: "research report"})
	other := mustDomain(t, s, store.CreateDomainParams{Name: "Other", Summary: "misc"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: bio.ID, Type: store.TypeFact, Text: "The biotech report targets Q3."})
	ageDomain(t, s, bio.ID)
	mustTouch(t, s, other.ID)

	c := New(s, WithTopKDomains(1))
	res, err := c.RoutedBlock(ctx, RouteRequest{RouteText: "what's the status of the biotech report?", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	// Lexical match on "biotech" must beat the more-recent "Other" for the slot.
	if len(res.Loaded) != 1 || res.Loaded[0] != bio.ID {
		t.Fatalf("expected lexical match %s, got %v", bio.ID, res.Loaded)
	}
	if len(res.Trace) != 1 || res.Trace[0].Signal != "match:biotech" {
		t.Fatalf("trace = %+v, want match:biotech", res.Trace)
	}
}

// Lexical candidates are ranked by hit count: a domain matching two salient
// terms beats a more recent domain matching one.
func TestRoutedBlockLexicalScoreBeatsRecency(t *testing.T) {
	s := newStore(t)
	two := mustDomain(t, s, store.CreateDomainParams{Name: "Raptors", Summary: "birds of prey"})
	one := mustDomain(t, s, store.CreateDomainParams{Name: "Diary", Summary: "daily notes"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: two.ID, Type: store.TypeFact, Text: "The kestrel hunts at dawn."})
	mustMemory(t, s, store.AddMemoryParams{DomainID: two.ID, Type: store.TypeFact, Text: "The falcon nests on the tower."})
	mustMemory(t, s, store.AddMemoryParams{DomainID: one.ID, Type: store.TypeFact, Text: "Saw a kestrel today."})
	ageDomain(t, s, two.ID)
	mustTouch(t, s, one.ID) // Diary is more recent

	res, err := New(s, WithTopKDomains(1)).RoutedBlock(context.Background(),
		RouteRequest{RouteText: "tell me about the kestrel and the falcon", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != two.ID {
		t.Fatalf("two-hit domain %s should win over more-recent one-hit domain, got %v", two.ID, res.Loaded)
	}
	if res.Trace[0].Signal != "match:kestrel" {
		t.Fatalf("signal = %q, want match:kestrel (first term hit)", res.Trace[0].Signal)
	}
}

// On equal hit counts the more recently active domain wins.
func TestRoutedBlockLexicalTieBreaksOnRecency(t *testing.T) {
	s := newStore(t)
	older := mustDomain(t, s, store.CreateDomainParams{Name: "Older", Summary: "aged"})
	newer := mustDomain(t, s, store.CreateDomainParams{Name: "Newer", Summary: "fresh"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: older.ID, Type: store.TypeFact, Text: "kestrel sighting one"})
	mustMemory(t, s, store.AddMemoryParams{DomainID: newer.ID, Type: store.TypeFact, Text: "kestrel sighting two"})
	ageDomain(t, s, older.ID)
	mustTouch(t, s, newer.ID)

	res, err := New(s, WithTopKDomains(1)).RoutedBlock(context.Background(),
		RouteRequest{RouteText: "any kestrel news?", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != newer.ID {
		t.Fatalf("tie should go to the more recent %s, got %v", newer.ID, res.Loaded)
	}
}

// Event memories carry no lexical weight: a domain with five matching events
// loses to a domain with one matching fact, even when the event domain is more
// recent.
func TestRoutedBlockLexicalIgnoresEvents(t *testing.T) {
	s := newStore(t)
	logs := mustDomain(t, s, store.CreateDomainParams{Name: "Logs", Summary: "trip log"})
	facts := mustDomain(t, s, store.CreateDomainParams{Name: "Facts", Summary: "standing"})
	for i := 0; i < 5; i++ {
		mustMemory(t, s, store.AddMemoryParams{DomainID: logs.ID, Type: store.TypeEvent, Text: fmt.Sprintf("kestrel seen on day %d", i)})
	}
	mustMemory(t, s, store.AddMemoryParams{DomainID: facts.ID, Type: store.TypeFact, Text: "The kestrel is a small falcon."})
	ageDomain(t, s, facts.ID)
	mustTouch(t, s, logs.ID) // Logs is more recent

	res, err := New(s, WithTopKDomains(1)).RoutedBlock(context.Background(),
		RouteRequest{RouteText: "kestrel", Trace: true})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != facts.ID {
		t.Fatalf("fact domain %s should win; events must not score. got %v", facts.ID, res.Loaded)
	}
	if res.Trace[0].Signal != "match:kestrel" {
		t.Fatalf("signal = %q, want match:kestrel", res.Trace[0].Signal)
	}
}

func TestRoutedBlockSignalPriorityNoDuplicate(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// One domain is tool-triggered AND lexically matched AND the most recent; it
	// must appear exactly once and the strongest (tool) signal wins.
	email := mustDomain(t, s, store.CreateDomainParams{
		Name: "Email", Summary: "email handling", Triggers: "google_gmail",
	})
	mustTouch(t, s, email.ID)

	c := New(s, WithTopKDomains(8))
	res, err := c.RoutedBlock(ctx, RouteRequest{
		RouteText:   "please check my email",
		RecentTools: []string{"mcp__fusion__google_gmail_messages_list"},
		Trace:       true,
	})
	if err != nil {
		t.Fatalf("routed: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0] != email.ID {
		t.Fatalf("domain should appear exactly once, got %v", res.Loaded)
	}
	if n := strings.Count(res.Text, "Active Context: "+email.ID); n != 1 {
		t.Fatalf("domain rendered %d times, want 1:\n%s", n, res.Text)
	}
	if len(res.Trace) != 1 || res.Trace[0].Signal != "tool:google_gmail" {
		t.Fatalf("tool signal should win over lexical/recency, got %+v", res.Trace)
	}
}

func TestRouteTokensStopwordsAndLength(t *testing.T) {
	got := routeTokens("What is the STATUS of the BioTech report, please?")
	want := []string{"status", "biotech", "report"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens = %v, want exactly %v in order", got, want)
	}
}

// At most twelve salient terms are taken from a message, in order, deduped.
func TestRouteTokensCappedAtTwelve(t *testing.T) {
	words := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
		"india", "juliet", "kilo", "lima", "mike", "november", "oscar",
	}
	msg := strings.Join(words, " ") + " alpha bravo" // duplicates do not count twice
	got := routeTokens(msg)
	if len(got) != maxRouteTokens || maxRouteTokens != 12 {
		t.Fatalf("tokens = %v (%d), want the first 12", got, len(got))
	}
	if strings.Join(got, ",") != strings.Join(words[:12], ",") {
		t.Fatalf("tokens = %v, want %v", got, words[:12])
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
	// "Daily Ops" is named so it doesn't lexically collide with "morning".
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

	// Bare "morning" → no keyword (phrase-only) and no lexical hit on "Daily Ops";
	// the more-recent Other takes the slot instead.
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
