// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

func TestSession_ObserveFeedsInbox(t *testing.T) {
	ws := t.TempDir()
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(ws, "cogmem"), Workspace: ws})
	defer s.Close()
	ctx := context.Background()
	s.Observe(ctx, 7, "user", "remember the blue door")
	s.Observe(ctx, 8, "tool", "plumbing")
	s.Observe(ctx, 9, "assistant", "noted")

	rows, err := s.Store().InboxRange(ctx, s.Store().DB(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 7 || rows[1].Seq != 9 {
		t.Fatalf("inbox = %+v, want seqs 7 and 9", rows)
	}
}

func TestSession_NilAndEphemeralAreNoOps(t *testing.T) {
	ctx := context.Background()
	var none *Session
	none.Observe(ctx, 1, "user", "x")
	none.RecordToolUse("file_read_lines")
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

func TestSession_RecallPlacesStickyInSystem(t *testing.T) {
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem")})
	defer s.Close()
	ctx := context.Background()
	st := s.Store()
	general, err := st.GeneralDomain(ctx, st.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMemory(ctx, st.DB(), store.AddMemoryParams{
		DomainID: general.ID, Type: store.TypeRule, Text: "always sign off as Alice",
		Status: store.StatusActive, Confidence: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	inj := s.Recall(ctx, "hello")
	var stable, routed int
	for _, i := range inj {
		switch i.Placement {
		case PlaceSystemStable:
			stable++
			if !strings.Contains(i.Text, "always sign off as Alice") {
				t.Fatalf("stable block missing the sticky memory: %q", i.Text)
			}
		case PlaceCurrentUser:
			routed++
		}
	}
	if stable != 1 || routed != 0 {
		t.Fatalf("injections = %+v, want one stable and no routed", inj)
	}
}

func TestSession_RecordToolUseRing(t *testing.T) {
	s := &Session{}
	for i := 0; i < maxRecentTools+3; i++ {
		s.RecordToolUse("t" + string(rune('a'+i)))
	}
	s.RecordToolUse("ta")
	got := s.RecentTools()
	if len(got) != maxRecentTools || got[0] != "ta" {
		t.Fatalf("ring = %v", got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Fatalf("duplicate %q in ring %v", n, got)
		}
		seen[n] = true
	}
}

func TestSettings_ZeroMeansDefaults(t *testing.T) {
	var s Settings
	if len(s.ComposerOptions()) != 0 || len(s.ManagerOptions()) != 0 {
		t.Fatalf("zero settings produced options: composer=%d manager=%d",
			len(s.ComposerOptions()), len(s.ManagerOptions()))
	}
	s.Consolidation.Nightly = true
	if len(s.ManagerOptions()) != 2 {
		t.Fatalf("nightly should yield the time and the jitter option, got %d", len(s.ManagerOptions()))
	}
}
