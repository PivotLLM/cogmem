// cogmem - Cognitive Memory
// License: MIT

// Package cogmem composes per-turn prompt memory from a session's .cogmem.db.
// It produces two blocks:
//
//   - StableBlock: always-on learned memory (the "general" domain), the
//     pending-confirmation digest, and the domain index. Cacheable; it carries
//     the store's stable_rev so the caller invalidates only on change. The
//     caller (the agent context layer) prepends the verbatim curated .md files.
//   - RoutedBlock: the full state + active hooks of the most-recently-active
//     domain(s), pre-loaded by recency. The only per-turn-varying memory.
package cogmem

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/PivotLLM/cogmem/store"
)

// options tune composition (mirrors memory.prompt config); set via functional
// options on New.
type options struct {
	topKDomains   int     // routed domains to pre-load
	maxChars      int     // routed block budget
	minConfidence float64 // hide active hooks below this

	// File attachments (memories carrying a FileRef). Budgets are independent of
	// maxChars: a referenced document is injected whole, not squeezed into the
	// routed block's line budget.
	loadAttachment    AttachmentLoader
	fileMaxBytes      int // per attachment
	fileTotalMaxBytes int // all attachments in one turn
}

// Option configures a Composer (functional options pattern, per dev standards).
type Option func(*options)

// WithTopKDomains sets how many routed domains to pre-load per turn.
func WithTopKDomains(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.topKDomains = n
		}
	}
}

// WithMaxChars caps the routed block size.
func WithMaxChars(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxChars = n
		}
	}
}

// WithMinConfidence hides active hooks below the given confidence.
func WithMinConfidence(c float64) Option {
	return func(o *options) {
		if c > 0 {
			o.minConfidence = c
		}
	}
}

// Composer reads a session's cogmem store. A Composer instance is scoped to a
// single session (see agent/memory_wiring.go).
type Composer struct {
	st  *store.Store
	opt options
}

// New returns a Composer over st with the given options applied over defaults.
func New(st *store.Store, opts ...Option) *Composer {
	o := options{
		topKDomains:       defaultTopKDomains,
		maxChars:          defaultMaxChars,
		minConfidence:     defaultMinConfidence,
		fileMaxBytes:      defaultFileMaxBytes,
		fileTotalMaxBytes: defaultFileTotalMaxBytes,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return &Composer{st: st, opt: o}
}

// RouteRequest carries the per-turn routing inputs.
type RouteRequest struct {
	RouteText   string   // latest user message (reserved for future signals)
	RecentTools []string // recently-invoked tool names (newest-first) for tool-trigger activation
	Trace       bool
}

// DomainSelection is one trace entry explaining a routed pre-load.
type DomainSelection struct {
	ID     string
	Name   string
	Signal string // "tool:<token>" (tool-trigger activation) or "recency"
}

// RoutedResult is the per-turn block plus optional trace.
type RoutedResult struct {
	Text   string
	Loaded []string
	Trace  []DomainSelection
}

// StableBlock renders the always-on learned memory and returns it with the
// store's stable_rev for cache keying. Returns "" (and the rev) when there is no
// learned content yet.
func (c *Composer) StableBlock(ctx context.Context) (string, int64, error) {
	text, rev, _, err := c.stableBlock(ctx)
	return text, rev, err
}

// stableBlock is StableBlock plus the file references carried by the memories it
// rendered, for the attachments block (see Compose).
func (c *Composer) stableBlock(ctx context.Context) (string, int64, []refSite, error) {
	db := c.st.DB()
	var refs []refSite
	rev, err := c.st.StableRev(ctx)
	if err != nil {
		return "", 0, nil, err
	}
	var b strings.Builder

	// Sticky domains: always injected. Each is labeled so the model knows which
	// domain it's seeing and why; higher StickyPriority first, then name.
	active, err := c.st.ListDomains(ctx, db, store.StatusActive)
	if err != nil {
		return "", rev, nil, err
	}
	var sticky []store.Domain
	for _, d := range active {
		if d.Sticky() {
			sticky = append(sticky, d)
		}
	}
	sort.SliceStable(sticky, func(i, j int) bool {
		if sticky[i].StickyPriority != sticky[j].StickyPriority {
			return sticky[i].StickyPriority > sticky[j].StickyPriority
		}
		return sticky[i].Name < sticky[j].Name
	})
	for _, d := range sticky {
		hooks, err := c.st.ListPromptMemories(ctx, db, d.ID)
		if err != nil {
			return "", rev, nil, err
		}
		hooks = filterConfidence(hooks, c.opt.minConfidence)
		events, err := c.st.CountEvents(ctx, db, d.ID)
		if err != nil {
			return "", rev, nil, err
		}
		if len(hooks) == 0 && events == 0 {
			continue
		}
		fmt.Fprintf(&b, "COGMEM domain %s is sticky:\n\n", d.Name)
		for _, h := range hooks {
			fmt.Fprintf(&b, "- %s %s%s\n", typePrefix(h.Type), h.Text, originSuffix(h.Origin))
			refs = collectRef(refs, memoryRef{memoryID: h.ID, text: h.Text, ref: h.FileRef})
		}
		if line := eventLine(events); line != "" {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	// Domain index (routed, active non-sticky topic domains), stable sort by id.
	var index []store.Domain
	for _, d := range active {
		if !d.Sticky() {
			index = append(index, d)
		}
	}
	if len(index) > 0 {
		b.WriteString("## Topics (index)\n")
		for _, d := range index {
			fmt.Fprintf(&b, "- %s · %s — %s\n", d.ID, d.Name, oneLine(d.Summary))
		}
		b.WriteString("\n")
	}

	out := strings.TrimRight(b.String(), "\n")
	if out != "" {
		out = "# Learned Memory\n\n" + out
	}
	return out, rev, refs, nil
}

// RoutedBlock renders the most-recently-active topic domain(s), up to TopKDomains
// and within MaxChars.
func (c *Composer) RoutedBlock(ctx context.Context, req RouteRequest) (RoutedResult, error) {
	res, _, err := c.routedBlock(ctx, req)
	return res, err
}

// routedBlock is RoutedBlock plus the file references carried by the memories it
// rendered, for the attachments block (see Compose).
func (c *Composer) routedBlock(ctx context.Context, req RouteRequest) (RoutedResult, []refSite, error) {
	db := c.st.DB()
	var refs []refSite
	active, err := c.st.ListDomains(ctx, db, store.StatusActive)
	if err != nil {
		return RoutedResult{}, nil, err
	}
	var topics []store.Domain
	for _, d := range active {
		if !d.Sticky() {
			topics = append(topics, d)
		}
	}
	// Most-recently-active first; nil last_active sorts last.
	sort.SliceStable(topics, func(i, j int) bool {
		return lastActive(topics[i]) > lastActive(topics[j])
	})

	// Order the candidates by signal strength: (1) domains activated by a
	// recently-used tool, (2) domains whose keyword_triggers appear in the latest
	// message, (3) domains lexically matching the latest message, (4) the rest by
	// recency. A single shared seen-set guarantees every domain appears at most
	// once, so a domain matched by more than one signal is never duplicated.
	seen := make(map[string]bool, len(topics))
	var ordered []candidate
	for _, d := range topics {
		if tok, ok := matchAnyTool(d, req.RecentTools); ok {
			seen[d.ID] = true
			ordered = append(ordered, candidate{d: d, signal: "tool:" + tok})
		}
	}
	// Author-defined keyword triggers: activate when a phrase appears (whole-
	// phrase, word-boundary) in the latest message — e.g. a workflow domain that
	// should load when a cron message or user mentions "morning routine".
	for _, d := range topics {
		if seen[d.ID] {
			continue
		}
		if kw, ok := d.MatchKeyword(req.RouteText); ok {
			seen[d.ID] = true
			ordered = append(ordered, candidate{d: d, signal: "keyword:" + kw})
		}
	}
	for _, cand := range c.lexicalCandidates(ctx, topics, req.RouteText, seen) {
		seen[cand.d.ID] = true
		ordered = append(ordered, cand)
	}
	for _, d := range topics {
		if !seen[d.ID] {
			seen[d.ID] = true
			ordered = append(ordered, candidate{d: d, signal: "recency"})
		}
	}
	if len(ordered) > c.opt.topKDomains {
		ordered = ordered[:c.opt.topKDomains]
	}

	var b strings.Builder
	var res RoutedResult
	for _, cand := range ordered {
		d := cand.d
		hooks, err := c.st.ListPromptMemories(ctx, db, d.ID)
		if err != nil {
			return RoutedResult{}, nil, err
		}
		hooks = filterConfidence(hooks, c.opt.minConfidence)
		events, err := c.st.CountEvents(ctx, db, d.ID)
		if err != nil {
			return RoutedResult{}, nil, err
		}
		section := renderDomain(d, hooks, events)
		if b.Len()+len(section) > c.opt.maxChars && b.Len() > 0 {
			break
		}
		b.WriteString(section)
		// Only memories in a domain that actually made it into the block get
		// their documents attached — a domain dropped by the char budget must not
		// smuggle a 250 KB file into the prompt.
		for _, h := range hooks {
			refs = collectRef(refs, memoryRef{memoryID: h.ID, text: h.Text, ref: h.FileRef})
		}
		res.Loaded = append(res.Loaded, d.ID)
		// Mark the domain active when it was loaded because it was genuinely
		// relevant (a tool, keyword, or lexical match) — not when it was only
		// filler chosen by recency itself, which would make the recency/staleness
		// signal self-reinforcing and never let a domain go cold. Best-effort.
		if cand.signal != "recency" {
			_ = c.st.Touch(ctx, db, d.ID)
		}
		if req.Trace {
			res.Trace = append(res.Trace, DomainSelection{ID: d.ID, Name: d.Name, Signal: cand.signal})
		}
	}
	res.Text = strings.TrimRight(b.String(), "\n")
	return res, refs, nil
}

// Result is one turn's composed memory: the cacheable stable block, the
// per-turn routed block, and the attachments block holding the full contents of
// any markdown files the rendered memories point at.
type Result struct {
	Stable string
	Rev    int64
	Routed string
	// Attachments holds documents owned by STABLE memories: they belong with the
	// stable block, wherever the caller places it.
	Attachments string
	// RoutedAttachments holds documents owned only by ROUTED memories, so they
	// can travel with the routed block rather than being stranded away from the
	// memory whose id their header cites.
	RoutedAttachments string
	Loaded            []string
	Trace             []DomainSelection
}

// Compose builds all three blocks for one turn. It is the entry point the agent
// wiring uses; StableBlock/RoutedBlock remain for callers that want one block.
// Attachment loading happens once here, after both blocks are rendered, so a
// file referenced by several in-context memories is injected exactly once.
//
// Block errors are returned, but a composed-so-far Result is still returned with
// them: a failure to read one part of memory should degrade the prompt, not
// blank it.
func (c *Composer) Compose(ctx context.Context, req RouteRequest) (Result, error) {
	var res Result
	stable, rev, stableRefs, sErr := c.stableBlock(ctx)
	res.Stable, res.Rev = stable, rev

	routed, routedRefs, rErr := c.routedBlock(ctx, req)
	res.Routed, res.Loaded, res.Trace = routed.Text, routed.Loaded, routed.Trace

	// Sticky memories first: their documents get first claim on the shared budget.
	// The two partitions travel to different places in the request (stable stays
	// in the cached prompt, routed rides with the current turn), so each document
	// is rendered alongside the memory that names it.
	res.Attachments, res.RoutedAttachments = c.attachmentsBlock(stableRefs, routedRefs)

	if sErr != nil {
		return res, sErr
	}
	return res, rErr
}

// candidate is one routed-block selection: a domain plus the signal that chose it.
type candidate struct {
	d      store.Domain
	signal string // "tool:<token>", "match:<term>", or "recency"
}

// matchAnyTool reports the first trigger token of d that matches any recently
// used tool name, and whether one matched.
func matchAnyTool(d store.Domain, recentTools []string) (string, bool) {
	if d.Triggers == "" {
		return "", false
	}
	for _, name := range recentTools {
		if tok, ok := d.MatchTrigger(name); ok {
			return tok, true
		}
	}
	return "", false
}

// lexicalCandidates returns topic domains whose name, summary, or active hooks
// contain a salient term from the latest user message, ordered by match count
// (recency breaks ties). Domains already in `seen` (e.g. tool-triggered) are
// skipped so the caller's dedup holds. Returns nil when routeText has no usable
// terms — keeping behavior identical to before for empty/tool-result turns.
func (c *Composer) lexicalCandidates(ctx context.Context, topics []store.Domain, routeText string, seen map[string]bool) []candidate {
	terms := routeTokens(routeText)
	if len(terms) == 0 {
		return nil
	}
	db := c.st.DB()
	// Restrict scoring to the routed topic set (general is handled in StableBlock).
	inTopics := make(map[string]bool, len(topics))
	for _, d := range topics {
		inTopics[d.ID] = true
	}
	type hit struct {
		score int
		term  string
	}
	hits := make(map[string]*hit)
	bump := func(domainID, term string) {
		if !inTopics[domainID] || seen[domainID] {
			return
		}
		h := hits[domainID]
		if h == nil {
			h = &hit{term: term}
			hits[domainID] = h
		}
		h.score++
	}
	for _, t := range terms {
		// Memory-text matches via the indexed substring scan (returns DomainID).
		// Events are excluded: routing decides which domain is relevant to the
		// turn, and a domain full of trip logs would win on volume alone.
		rows, err := c.st.SearchMemories(ctx, db, t, lexicalSearchLimit, false)
		if err == nil {
			for _, h := range rows {
				bump(h.DomainID, t)
			}
		}
		// Name / summary matches, scored in-memory over the topic set.
		for _, d := range topics {
			if strings.Contains(strings.ToLower(d.Name), t) || strings.Contains(strings.ToLower(d.Summary), t) {
				bump(d.ID, t)
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	// Walk topics in recency order, keep those with a hit; stable-sort by score
	// so a stronger lexical match wins, recency breaking ties.
	var out []candidate
	for _, d := range topics {
		if h := hits[d.ID]; h != nil {
			out = append(out, candidate{d: d, signal: "match:" + h.term})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return hits[out[i].d.ID].score > hits[out[j].d.ID].score
	})
	return out
}

// routeTokens extracts salient, deduped lowercase terms from the user message for
// lexical routing: alphanumeric runs of at least minRouteTokenLen, minus a small
// stopword set, capped at maxRouteTokens.
func routeTokens(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]bool, len(fields))
	var out []string
	for _, f := range fields {
		if len(f) < minRouteTokenLen || routeStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) >= maxRouteTokens {
			break
		}
	}
	return out
}

// routeStopwords are common 4+ char words excluded from lexical routing so they
// don't pull in unrelated domains on ordinary phrasing.
var routeStopwords = map[string]bool{
	"what": true, "when": true, "where": true, "which": true, "this": true,
	"that": true, "with": true, "have": true, "will": true, "from": true,
	"they": true, "them": true, "then": true, "here": true, "there": true,
	"about": true, "would": true, "could": true, "should": true, "please": true,
	"need": true, "want": true, "like": true, "your": true, "ours": true,
	"into": true, "just": true, "also": true, "make": true, "does": true,
	"done": true, "been": true, "were": true, "than": true, "very": true,
}

// originSuffix tags a memory line with where it came from, so the assistant
// knows its provenance. Chat-origin (the agent's own notes) is the unremarkable
// default and gets no tag; user- and consolidation-origin are flagged.
//
// The tag says "origin", not "source": it has always rendered Origin, and the
// separate source field it was named after no longer exists.
func originSuffix(o store.Origin) string {
	switch o {
	case store.OriginUser:
		return " [origin: user]"
	case store.OriginConsolidation:
		return " [origin: consolidation]"
	default:
		return ""
	}
}

// typePrefix labels a memory line with its type, so the assistant can tell a
// standing rule from a preference or its own bookkeeping. Without it every
// memory reads as an undifferentiated assertion, which is why type was worth
// storing but not worth acting on.
func typePrefix(t store.MemoryType) string {
	if t == "" {
		return "(fact)"
	}
	return "(" + string(t) + ")"
}

// eventLine reports how many event memories a domain holds. Events never load
// into the prompt — they go stale and accumulate without bound — so the count
// is how the assistant learns they exist and how to reach them.
//
// It names the tool AND the argument on purpose. An earlier version said only
// "search to retrieve", and a live agent asked about an event called
// cogmem_memory_search four times with identical arguments, never adding
// include_events, and gave up. Search excludes events by default, so without
// the flag the count line advertises memories the assistant then cannot find —
// worse than not mentioning them at all.
func eventLine(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "(1 event memory here — cogmem_memory_search with include_events:true to read it)\n"
	default:
		return fmt.Sprintf(
			"(%d event memories here — cogmem_memory_search with include_events:true to read them)\n", n)
	}
}

func renderDomain(d store.Domain, hooks []store.Memory, events int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Active Context: %s · %s\n", d.ID, d.Name)
	if s := oneLine(d.Summary); s != "" {
		fmt.Fprintf(&b, "Summary: %s\n", s)
	}
	if len(d.State.Blockers) > 0 {
		b.WriteString("Blockers:\n")
		for _, x := range d.State.Blockers {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	if len(d.State.NextActions) > 0 {
		b.WriteString("Next actions:\n")
		for _, x := range d.State.NextActions {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	if len(d.State.Constraints) > 0 {
		b.WriteString("Constraints:\n")
		for _, x := range d.State.Constraints {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	for _, h := range hooks {
		fmt.Fprintf(&b, "- (%s) %s %s%s\n", h.ID, typePrefix(h.Type), h.Text, originSuffix(h.Origin))
	}
	if line := eventLine(events); line != "" {
		b.WriteString(line)
	}
	b.WriteString("\n")
	return b.String()
}

func filterConfidence(hooks []store.Memory, min float64) []store.Memory {
	out := hooks[:0:0]
	for _, h := range hooks {
		if h.Confidence >= min {
			out = append(out, h)
		}
	}
	return out
}

func lastActive(d store.Domain) int64 { return d.LastActiveAt }

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}
