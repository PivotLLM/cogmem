// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PivotLLM/cogmem/consolidate"
	"github.com/PivotLLM/cogmem/logger"
	"github.com/PivotLLM/cogmem/store"
)

// Placement says where a recalled block belongs in the model request.
type Placement int

const (
	// PlaceSystemStable: append to the system message. The block is the same on
	// every turn of the session, so it joins the cached prompt prefix.
	PlaceSystemStable Placement = iota
	// PlaceCurrentUser: fold into the latest user message. The block is
	// selected per turn and must not sit ahead of the history, or it
	// invalidates the cached prefix for all of it.
	PlaceCurrentUser
)

// Injection is one block Recall asks the host to place in the request.
type Injection struct {
	Placement Placement
	Text      string
}

// maxRecentTools bounds the recent-tool ring used for tool-trigger routing.
const maxRecentTools = 8

// SessionOptions describes one memory and how the host wants it driven.
type SessionOptions struct {
	// ID labels the memory in logs and run records; an agent id, typically.
	ID string
	// Dir is the directory cogmem owns for this memory. The store is
	// store.DBPath(Dir); WAL files and pre-migration snapshots sit beside it.
	// One memory per directory: a host that wants several memories for one
	// agent passes several directories.
	Dir string
	// Workspace is where consolidation reads the curated files and COGMEM.md.
	Workspace string
	// Ephemeral marks a throwaway memory (a sub-agent working on a snapshot):
	// nothing is observed into the inbox and no consolidation is scheduled.
	Ephemeral bool
	Settings  Settings
	// Loader resolves memory file attachments. Nil renders references as
	// plain markers with no content.
	Loader AttachmentLoader
	// Manager, when set, is nudged after every observed message so its
	// message-count trigger can fire.
	Manager *consolidate.Manager
	// OnOpen runs once, right after the store is first opened, before any
	// observe or recall. Hosts use it for one-time work such as backfilling
	// the inbox from an older record of the conversation.
	OnOpen func(ctx context.Context, st *store.Store)
}

// Session is one agent session's view of cognitive memory: the host hands it a
// copy of every message (Observe), tells it which tools ran (RecordToolUse),
// and asks it for the blocks to place in the next request (Recall). It knows
// nothing about the host's context engine, and the engine knows nothing about
// it; the host's turn loop is the only thing that holds both.
//
// A nil *Session is valid and does nothing, so hosts need no guard for agents
// without cognitive memory.
type Session struct {
	opt    SessionOptions
	dbPath string

	mu     sync.Mutex
	st     *store.Store
	comp   *Composer
	opened bool

	recentMu    sync.Mutex
	recentTools []string
}

// NewSession builds a session. The store is opened lazily on first use.
func NewSession(opt SessionOptions) *Session {
	return &Session{opt: opt, dbPath: store.DBPath(opt.Dir)}
}

// Store returns the session's store, opening it on first call. Nil when the
// open failed (logged once) or the receiver is nil.
func (s *Session) Store() *store.Store {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened {
		return s.st
	}
	s.opened = true
	// cogmem owns Dir; create it so a brand-new memory does not fail its first
	// write.
	if err := os.MkdirAll(filepath.Dir(s.dbPath), 0o755); err != nil {
		logger.WarnCF("cogmem", "create session store directory failed", map[string]any{
			"id": s.opt.ID, "path": s.dbPath, "error": err.Error(),
		})
		return nil
	}
	st, err := store.Open(s.dbPath)
	if err != nil {
		logger.WarnCF("cogmem", "open session store failed", map[string]any{
			"id":    s.opt.ID,
			"path":  s.dbPath,
			"error": err.Error(),
		})
		return nil
	}
	s.st = st
	s.comp = New(st, append(s.opt.Settings.ComposerOptions(), WithAttachmentLoader(s.opt.Loader))...)
	if s.opt.OnOpen != nil && !s.opt.Ephemeral {
		s.opt.OnOpen(context.Background(), st)
	}
	return st
}

// Observe hands memory a copy of a message the host stored under seq. It
// lands in the inbox for the next consolidation run and nudges the run's
// message-count trigger. Best-effort: a failure is logged and never affects
// the host's turn.
func (s *Session) Observe(ctx context.Context, seq int64, role, text string) {
	if s == nil || s.opt.Ephemeral || seq <= 0 {
		return
	}
	st := s.Store()
	if st == nil {
		return
	}
	stored, err := consolidate.Observe(ctx, st, seq, role, text, s.opt.Settings.Consolidation.PerMessageChars)
	if err != nil {
		logger.WarnCF("cogmem", "observe message failed", map[string]any{
			"id": s.opt.ID, "seq": seq, "error": err.Error(),
		})
		return
	}
	if stored && s.opt.Manager != nil {
		s.opt.Manager.OnMessage(consolidate.Job{
			ID:        s.opt.ID,
			Dir:       s.opt.Dir,
			Workspace: s.opt.Workspace,
		})
	}
}

// RecordToolUse feeds the recent-tool ring (newest-first, deduped, capped) so
// Recall can auto-load domains whose triggers match a tool the agent just used.
func (s *Session) RecordToolUse(names ...string) {
	if s == nil || len(names) == 0 {
		return
	}
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	for _, n := range names {
		if n == "" {
			continue
		}
		out := s.recentTools[:0]
		for _, e := range s.recentTools {
			if e != n {
				out = append(out, e)
			}
		}
		s.recentTools = append([]string{n}, out...)
	}
	if len(s.recentTools) > maxRecentTools {
		s.recentTools = s.recentTools[:maxRecentTools]
	}
}

// RecentTools returns a copy of the recent-tool ring, newest first.
func (s *Session) RecentTools() []string {
	if s == nil {
		return nil
	}
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	if len(s.recentTools) == 0 {
		return nil
	}
	out := make([]string, len(s.recentTools))
	copy(out, s.recentTools)
	return out
}

// Recall returns the blocks to place in the next request: the STABLE block
// (sticky domains, topic index, their attached documents) for the system
// message, and the ROUTED block (domains selected from routeText and the recent
// tools, with their documents) for the current turn. Nil when there is nothing
// to inject or the store could not be opened.
func (s *Session) Recall(ctx context.Context, routeText string) []Injection {
	if s == nil || s.Store() == nil {
		return nil
	}
	res, err := s.comp.Compose(ctx, RouteRequest{
		RecentTools: s.RecentTools(),
		RouteText:   routeText,
		Trace:       s.opt.Settings.Prompt.IncludeDebugTrace,
	})
	if err != nil {
		logger.WarnCF("cogmem", "memory compose failed", map[string]any{
			"id": s.opt.ID, "error": err.Error(),
		})
	}
	if res.Attachments != "" || res.RoutedAttachments != "" {
		// Split by provenance: sticky bytes ride in the cached prompt and are
		// paid for once, routed bytes ride with the turn and are paid for
		// every time. One combined figure hides which is which.
		logger.DebugCF("cogmem", "attached documents injected", map[string]any{
			"id":           s.opt.ID,
			"sticky_bytes": len(res.Attachments),
			"routed_bytes": len(res.RoutedAttachments),
		})
	}
	// Sticky documents belong with the stable block; routed documents belong
	// with the routed block, whose memory ids their headers cite.
	var out []Injection
	if stable := joinBlocks(res.Stable, res.Attachments); stable != "" {
		out = append(out, Injection{Placement: PlaceSystemStable, Text: stable})
	}
	if routed := joinBlocks(res.Routed, res.RoutedAttachments); routed != "" {
		out = append(out, Injection{Placement: PlaceCurrentUser, Text: routed})
	}
	return out
}

// Close releases the store handle. Safe on nil and when never opened.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		_ = s.st.Close()
		s.st = nil
		s.comp = nil
	}
}

// joinBlocks concatenates non-empty prompt blocks with the separator used
// throughout a system prompt.
func joinBlocks(blocks ...string) string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n\n---\n\n")
}
