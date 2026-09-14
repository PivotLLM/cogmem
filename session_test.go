// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/consolidate"
	"github.com/PivotLLM/cogmem/store"
)

// injectionText returns the concatenated text of every injection with the
// given placement and how many there were.
func injectionText(inj []Injection, p Placement) (string, int) {
	var parts []string
	for _, i := range inj {
		if i.Placement == p {
			parts = append(parts, i.Text)
		}
	}
	return strings.Join(parts, "\n"), len(parts)
}

func TestSession_ObserveFeedsInbox(t *testing.T) {
	ws := t.TempDir()
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(ws, "cogmem"), Workspace: ws})
	defer s.Close()
	ctx := context.Background()
	s.Observe(ctx, 7, "user", "remember the blue door")
	s.Observe(ctx, 8, "tool", "plumbing")
	s.Observe(ctx, 9, "assistant", "noted")
	s.Observe(ctx, 0, "user", "seq zero is dropped")

	rows, err := s.Store().InboxRange(ctx, s.Store().DB(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 7 || rows[1].Seq != 9 {
		t.Fatalf("inbox = %+v, want seqs 7 and 9", rows)
	}
	if rows[0].Role != "user" || rows[0].Text != "remember the blue door" || rows[1].Role != "assistant" || rows[1].Text != "noted" {
		t.Fatalf("inbox rows = %+v", rows)
	}
}

func TestSession_NilAndEphemeralAreNoOps(t *testing.T) {
	ctx := context.Background()
	var none *Session
	none.Observe(ctx, 1, "user", "x")
	none.RecordToolUse("file_read_lines")
	if none.RecentTools() != nil || none.Store() != nil {
		t.Fatal("nil session should have no tools and no store")
	}
	if inj := none.Recall(ctx, "anything"); inj != nil {
		t.Fatalf("nil session recalled %+v", inj)
	}
	none.Close()

	opened := false
	sub := NewSession(SessionOptions{ID: "alice/sub", Dir: filepath.Join(t.TempDir(), "snap"), Ephemeral: true,
		OnOpen: func(context.Context, *store.Store) { opened = true }})
	defer sub.Close()
	sub.Observe(ctx, 1, "user", "x")
	if n, _ := sub.Store().InboxCount(ctx, sub.Store().DB()); n != 0 {
		t.Fatalf("ephemeral session wrote %d inbox rows", n)
	}
	if opened {
		t.Fatal("OnOpen must not run for an ephemeral session")
	}
}

func TestSession_OnOpenRunsOnce(t *testing.T) {
	calls := 0
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem"),
		OnOpen: func(context.Context, *store.Store) { calls++ }})
	defer s.Close()
	s.Store()
	s.Store()
	s.Recall(context.Background(), "hi")
	if calls != 1 {
		t.Fatalf("OnOpen ran %d times, want 1", calls)
	}
}

// Many goroutines racing to open the store see one store and one OnOpen.
func TestSession_OnOpenOnceUnderConcurrentStore(t *testing.T) {
	var calls int32
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem"),
		OnOpen: func(context.Context, *store.Store) { atomic.AddInt32(&calls, 1) }})
	defer s.Close()

	const n = 16
	got := make([]*store.Store, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i] = s.Store()
		}(i)
	}
	close(start)
	wg.Wait()

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("OnOpen ran %d times, want 1", calls)
	}
	for i, st := range got {
		if st == nil || st != got[0] {
			t.Fatalf("goroutine %d saw store %p, want the single handle %p", i, st, got[0])
		}
	}
}

func TestSession_RecallPlacesStickyInSystem(t *testing.T) {
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem")})
	defer s.Close()
	ctx := context.Background()
	st := s.Store()
	general := mustGeneral(t, st)
	mustMemory(t, st, store.AddMemoryParams{DomainID: general.ID, Type: store.TypeRule, Text: "always sign off as Alice"})

	inj := s.Recall(ctx, "hello")
	stable, ns := injectionText(inj, PlaceSystemStable)
	_, nr := injectionText(inj, PlaceCurrentUser)
	if ns != 1 || nr != 0 {
		t.Fatalf("injections = %+v, want one stable and no routed", inj)
	}
	want := "# Learned Memory\n\nCOGMEM domain General is sticky:\n\n- (rule) always sign off as Alice"
	if stable != want {
		t.Fatalf("stable block:\n%q\nwant:\n%q", stable, want)
	}
}

// A tool the agent just used routes a topic domain into the current-user
// block, beating a more recently active domain for the single slot.
func TestSession_ToolTriggerRoutesToCurrentUser(t *testing.T) {
	s := NewSession(SessionOptions{
		ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem"),
		Settings: Settings{Prompt: PromptSettings{TopKDomains: 1}},
	})
	defer s.Close()
	ctx := context.Background()
	st := s.Store()
	email := mustDomain(t, st, store.CreateDomainParams{Name: "Email", Summary: "mail prefs", Triggers: "google_gmail"})
	mem := mustMemory(t, st, store.AddMemoryParams{DomainID: email.ID, Type: store.TypePreference, Text: "Archive newsletters."})
	decoy := mustDomain(t, st, store.CreateDomainParams{Name: "Decoy", Summary: "misc"})
	ageDomain(t, st, email.ID)
	mustTouch(t, st, decoy.ID)

	// No tool used yet: recency fills the slot with the decoy.
	routed, n := injectionText(s.Recall(ctx, "anything else"), PlaceCurrentUser)
	if n != 1 || !strings.Contains(routed, "## Active Context: "+decoy.ID+" · Decoy\n") || strings.Contains(routed, "Archive newsletters.") {
		t.Fatalf("before the tool use, recency should load the decoy (n=%d):\n%s", n, routed)
	}

	s.RecordToolUse("mcp__fusion__google_gmail_messages_list")
	inj := s.Recall(ctx, "anything else")
	routed, n = injectionText(inj, PlaceCurrentUser)
	if n != 1 {
		t.Fatalf("want one routed injection, got %d in %+v", n, inj)
	}
	want := "## Active Context: " + email.ID + " · Email\nSummary: mail prefs\n- (" + mem.ID + ") (preference) Archive newsletters."
	if routed != want {
		t.Fatalf("routed block:\n%q\nwant:\n%q", routed, want)
	}
	if _, ns := injectionText(inj, PlaceSystemStable); ns != 1 {
		t.Fatalf("the topic index should still produce a stable injection: %+v", inj)
	}
}

// Observe nudges the manager once per meaningful message; tool plumbing is
// never counted. The factory returns an error, which the manager logs and
// skips, so no worker or model is needed.
func TestSession_ObserveNudgesManager(t *testing.T) {
	jobs := make(chan consolidate.Job, 8)
	factory := func(j consolidate.Job) (*consolidate.Worker, error) {
		jobs <- j
		return nil, errors.New("no worker in this test")
	}
	m := consolidate.NewManager(factory, consolidate.WithEveryNMessages(2))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	ws := t.TempDir()
	dir := filepath.Join(ws, "cogmem")
	s := NewSession(SessionOptions{ID: "alice", Dir: dir, Workspace: ws, Manager: m})
	defer s.Close()

	s.Observe(ctx, 1, "user", "one")
	s.Observe(ctx, 2, "tool", "plumbing")
	s.Observe(ctx, 3, "tool", "more plumbing")
	s.Observe(ctx, 4, "assistant", "two") // second meaningful message: threshold reached

	select {
	case j := <-jobs:
		want := consolidate.Job{ID: "alice", Dir: dir, Workspace: ws}
		if j != want {
			t.Fatalf("job = %+v, want %+v", j, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager was never nudged")
	}
	m.Stop() // waits for in-flight runs, so any extra enqueue would be visible now
	if len(jobs) != 0 {
		t.Fatalf("%d extra runs were triggered; tool messages must not count", len(jobs))
	}
}

// The recent-tool ring is newest-first, deduped, and capped: the oldest entry
// is evicted and a repeated name moves to the front.
func TestSession_RecordToolUseRing(t *testing.T) {
	s := &Session{}
	if s.RecentTools() != nil {
		t.Fatal("empty ring should be nil")
	}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		s.RecordToolUse(n)
	}
	want := []string{"j", "i", "h", "g", "f", "e", "d", "c"}
	if got := s.RecentTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ring = %v, want %v (cap %d, oldest evicted)", got, want, maxRecentTools)
	}

	s.RecordToolUse("d") // repeat: moves to the front, no duplicate
	want = []string{"d", "j", "i", "h", "g", "f", "e", "c"}
	if got := s.RecentTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ring after repeat = %v, want %v", got, want)
	}

	s.RecordToolUse("x", "y") // several at once: the last named is the newest
	want = []string{"y", "x", "d", "j", "i", "h", "g", "f"}
	if got := s.RecentTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ring after batch = %v, want %v", got, want)
	}

	s.RecordToolUse("")
	s.RecordToolUse()
	if got := s.RecentTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty names changed the ring: %v", got)
	}
	// The copy handed out is detached from the ring.
	got := s.RecentTools()
	got[0] = "mutated"
	if s.RecentTools()[0] != "y" {
		t.Fatal("RecentTools must return a copy")
	}
}

// Concurrent use of every method, with a Close in the middle, is race-free and
// leaves the session inert afterwards.
func TestSession_ConcurrentUseAndClose(t *testing.T) {
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem")})
	ctx := context.Background()
	st := s.Store()
	if st == nil {
		t.Fatal("store did not open")
	}
	email := mustDomain(t, st, store.CreateDomainParams{Name: "Email", Triggers: "gmail"})
	mustMemory(t, st, store.AddMemoryParams{DomainID: email.ID, Type: store.TypeFact, Text: "mail fact"})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 25; j++ {
				s.RecordToolUse(fmt.Sprintf("mcp_gmail_%d_%d", i, j))
				_ = s.RecentTools()
				s.Observe(ctx, int64(i*100+j+1), "user", "message")
				_ = s.Recall(ctx, "check my email")
				_ = s.Store()
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		s.Close()
	}()
	close(start)
	wg.Wait()

	if s.Store() != nil {
		t.Fatal("a closed session must not hand out a store")
	}
	if inj := s.Recall(ctx, "check my email"); inj != nil {
		t.Fatalf("a closed session recalled %+v", inj)
	}
	s.Observe(ctx, 999, "user", "after close") // must not panic
	s.Close()                                  // idempotent
	if len(s.RecentTools()) != maxRecentTools {
		t.Fatalf("ring = %v, want %d entries", s.RecentTools(), maxRecentTools)
	}
}

// A directory that cannot be created (its parent is a regular file) leaves the
// session inert: Observe is a no-op and Recall returns nil, without panics.
func TestSession_UnopenableStoreIsInert(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(parent, "cogmem"),
		OnOpen: func(context.Context, *store.Store) { t.Error("OnOpen must not run when the store cannot open") }})
	ctx := context.Background()
	s.Observe(ctx, 1, "user", "hello")
	if s.Store() != nil {
		t.Fatal("store should be nil under a regular file")
	}
	if inj := s.Recall(ctx, "hello"); inj != nil {
		t.Fatalf("recall = %+v, want nil", inj)
	}
	s.RecordToolUse("mcp_gmail")
	if got := s.RecentTools(); !reflect.DeepEqual(got, []string{"mcp_gmail"}) {
		t.Fatalf("the ring works without a store: %v", got)
	}
	s.Close()
}

// Documents follow their owner: a sticky memory's file rides in the stable
// injection, a routed memory's file in the current-user injection.
func TestSession_RecallAttachmentsFollowPlacement(t *testing.T) {
	fl := &fakeLoader{files: map[string]string{
		"files/voice.md": "VOICEBODY",
		"files/topic.md": "TOPICBODY",
	}}
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem"), Loader: fl.load})
	defer s.Close()
	ctx := context.Background()
	st := s.Store()
	gen := mustGeneral(t, st)
	sticky := mustMemory(t, st, store.AddMemoryParams{DomainID: gen.ID, Type: store.TypeRule, Text: "Use the house voice.", FileRef: "files/voice.md"})
	topic := mustDomain(t, st, store.CreateDomainParams{Name: "Writing"})
	routedMem := mustMemory(t, st, store.AddMemoryParams{DomainID: topic.ID, Type: store.TypeRule, Text: "Drafts follow the guide.", FileRef: "files/topic.md"})

	inj := s.Recall(ctx, "hello")
	stable, ns := injectionText(inj, PlaceSystemStable)
	routed, nr := injectionText(inj, PlaceCurrentUser)
	if ns != 1 || nr != 1 {
		t.Fatalf("want one stable and one routed injection, got %+v", inj)
	}

	stableDoc := "\n\n---\n\n# Attached Documents\n\n"
	if !strings.Contains(stable, stableDoc) {
		t.Fatalf("stable injection should join the block and its documents with the separator:\n%s", stable)
	}
	if !strings.Contains(stable, "### Attached: files/voice.md\nFrom memory "+sticky.ID+" (\"Use the house voice.\"), 9 bytes, current as of this turn.\n\nVOICEBODY") {
		t.Fatalf("sticky document missing from the stable injection:\n%s", stable)
	}
	if strings.Contains(stable, "TOPICBODY") {
		t.Fatalf("routed document leaked into the stable injection:\n%s", stable)
	}
	if !strings.Contains(routed, "## Active Context: "+topic.ID+" · Writing\n- ("+routedMem.ID+") (rule) Drafts follow the guide.") {
		t.Fatalf("routed block missing the topic memory:\n%s", routed)
	}
	if !strings.Contains(routed, "### Attached: files/topic.md\nFrom memory "+routedMem.ID+" (\"Drafts follow the guide.\"), 9 bytes, current as of this turn.\n\nTOPICBODY") {
		t.Fatalf("routed document missing from the current-user injection:\n%s", routed)
	}
	if strings.Contains(routed, "VOICEBODY") {
		t.Fatalf("sticky document leaked into the routed injection:\n%s", routed)
	}
	if got := fmt.Sprint(fl.calls); got != "[files/voice.md files/topic.md]" {
		t.Fatalf("loads = %s, want each document once, sticky first", got)
	}
}
