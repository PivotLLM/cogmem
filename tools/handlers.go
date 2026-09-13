// cogmem - Cognitive Memory
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PivotLLM/cogmem/store"
	"github.com/PivotLLM/toolspec"
)

// fileSuffix tags a rendered memory with the markdown file attached to it, so a
// listing shows which memories carry a document.
func fileSuffix(ref string) string {
	if ref = strings.TrimSpace(ref); ref == "" {
		return ""
	}
	return " [file: " + ref + "]"
}

// handlerFunc is the inner handler shape: it receives the opened store plus the
// call, and returns the model-facing text or an error. wrap() handles store
// lifecycle and turns errors into error Results.
type handlerFunc func(s *store.Store, call *toolspec.ToolCall) (string, error)

// wrap builds a toolspec.ToolHandler that resolves the per-session database from
// the workspace + ToolCall.Session, opens it (ensuring the parent dir exists),
// runs h, and closes the store. A missing session yields an error Result.
func wrap(workspace string, h handlerFunc) toolspec.ToolHandler {
	return func(call *toolspec.ToolCall) (*toolspec.Result, error) {
		if call.Session == "" {
			return errResult("cognitive memory requires an active session", nil), nil
		}
		if workspace == "" {
			return errResult("cognitive memory is unavailable (no workspace configured)", nil), nil
		}
		dbPath := store.SessionDBPath(workspace, call.Session)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return errResult("failed to prepare memory store directory", err), nil
		}
		s, err := store.Open(dbPath)
		if err != nil {
			return errResult("failed to open memory store", err), nil
		}
		defer func() { _ = s.Close() }()

		text, err := h(s, call)
		if err != nil {
			return errResult(err.Error(), err), nil
		}
		return &toolspec.Result{ForLLM: text}, nil
	}
}

func errResult(msg string, err error) *toolspec.Result {
	return &toolspec.Result{IsError: true, ForLLM: msg, Err: err}
}

// --- argument helpers ---

func argStr(call *toolspec.ToolCall, key string) string {
	if v, ok := call.Args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func argBool(call *toolspec.ToolCall, key string, def bool) bool {
	if v, ok := call.Args[key].(bool); ok {
		return v
	}
	return def
}

func argInt(call *toolspec.ToolCall, key string, def int) int {
	switch v := call.Args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

func argFloat(call *toolspec.ToolCall, key string, def float64) float64 {
	switch v := call.Args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return def
}

func argStrSlice(call *toolspec.ToolCall, key string) ([]string, bool) {
	raw, ok := call.Args[key]
	if !ok {
		return nil, false
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// --- handlers ---

func getDomain(s *store.Store, call *toolspec.ToolCall) (string, error) {
	id := argStr(call, "id")
	if id == "" {
		return "", errors.New("id is required")
	}
	d, err := s.GetDomain(call.Ctx, s.DB(), id, true)
	if err != nil {
		return "", mapErr(err, id)
	}
	// The agent actively retrieved this domain — mark it used (recency/staleness
	// signal). Best-effort; never fail the read on a recency-bookkeeping error.
	_ = s.Touch(call.Ctx, s.DB(), id)
	var b strings.Builder
	fmt.Fprintf(&b, "Domain %s %q (sticky=%t, status=%s, version=%d)\n", d.ID, d.Name, d.Sticky(), d.Status, d.Version)
	if d.Summary != "" {
		fmt.Fprintf(&b, "Summary: %s\n", d.Summary)
	}
	if d.Triggers != "" {
		fmt.Fprintf(&b, "Tool triggers (auto-load on tool use): %s\n", d.Triggers)
	}
	if d.KeywordTriggers != "" {
		fmt.Fprintf(&b, "Keyword triggers (auto-load when mentioned): %s\n", d.KeywordTriggers)
	}
	writeStateLine(&b, "Blockers", d.State.Blockers)
	writeStateLine(&b, "Next actions", d.State.NextActions)
	writeStateLine(&b, "Constraints", d.State.Constraints)
	if len(d.Memories) == 0 {
		b.WriteString("Memories: (none active)\n")
	} else {
		fmt.Fprintf(&b, "Memories (%d active):\n", len(d.Memories))
		for _, h := range d.Memories {
			fmt.Fprintf(&b, "  %s [%s] (conf=%.2f) %s%s\n", h.ID, h.Type, h.Confidence, h.Text, fileSuffix(h.FileRef))
		}
	}
	return b.String(), nil
}

func search(s *store.Store, call *toolspec.ToolCall) (string, error) {
	query := argStr(call, "query")
	if query == "" {
		return "", errors.New("query is required")
	}
	limit := argInt(call, "limit", 20)
	includeEvents := argBool(call, "include_events", false)
	hooks, err := s.SearchMemories(call.Ctx, s.DB(), query, limit, includeEvents)
	if err != nil {
		return "", err
	}
	// Nothing matched, and events were held back. Events are excluded by default
	// so a routine lookup is not buried under hundreds of recurring notes — but
	// with no other results there is nothing to bury, so excluding them can only
	// turn a findable memory into "not found".
	//
	// This is not a nicety. Asked when a trip happened, a live agent called this
	// tool four times with identical arguments, never added include_events, and
	// gave up — with the answer sitting in the store the whole time. Retrieval
	// must not depend on the model remembering a flag.
	fellBack := false
	if len(hooks) == 0 && !includeEvents {
		hooks, err = s.SearchMemories(call.Ctx, s.DB(), query, limit, true)
		if err != nil {
			return "", err
		}
		fellBack = len(hooks) > 0
	}
	if len(hooks) == 0 {
		return fmt.Sprintf("No active memories match %q.", query), nil
	}
	var b strings.Builder
	if fellBack {
		fmt.Fprintf(&b, "%d event memories matching %q (no standing memories matched, "+
			"so event memories were searched too):\n", len(hooks), query)
	} else {
		fmt.Fprintf(&b, "%d active memories matching %q:\n", len(hooks), query)
	}
	for _, h := range hooks {
		fmt.Fprintf(&b, "  %s [%s] (domain=%s, conf=%.2f) %s%s\n", h.ID, h.Type, h.DomainID, h.Confidence, h.Text, fileSuffix(h.FileRef))
	}
	return b.String(), nil
}

func listDomains(s *store.Store, call *toolspec.ToolCall) (string, error) {
	var statuses []store.Status
	if st := argStr(call, "status"); st != "" {
		statuses = append(statuses, store.Status(st))
	}
	domains, err := s.ListDomains(call.Ctx, s.DB(), statuses...)
	if err != nil {
		return "", err
	}
	var out []store.Domain
	out = append(out, domains...)
	if len(out) == 0 {
		return "No domains.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d domain(s):\n", len(out))
	for _, d := range out {
		summary := d.Summary
		if summary == "" {
			summary = "(no summary)"
		}
		marker := ""
		if d.Sticky() {
			marker = " [sticky]"
		}
		fmt.Fprintf(&b, "  %s · %s%s · %s · %s\n", d.ID, d.Name, marker, summary, d.Status)
	}
	return b.String(), nil
}

func explain(s *store.Store, call *toolspec.ToolCall) (string, error) {
	id := argStr(call, "id")
	if id == "" {
		return "", errors.New("id is required")
	}
	// Memory ids carry the "h" prefix; domain ids "d". Try the matching lookup
	// first, then fall back to the other so a mis-typed prefix still resolves.
	if strings.HasPrefix(id, "h") {
		if h, err := s.GetMemory(call.Ctx, s.DB(), id); err == nil {
			return explainMemory(h), nil
		}
	}
	if d, err := s.GetDomain(call.Ctx, s.DB(), id, false); err == nil {
		return explainDomain(d), nil
	}
	if h, err := s.GetMemory(call.Ctx, s.DB(), id); err == nil {
		return explainMemory(h), nil
	}
	return "", mapErr(store.ErrNotFound, id)
}

func explainDomain(d store.Domain) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Domain %s %q\n", d.ID, d.Name)
	fmt.Fprintf(&b, "  sticky=%t status=%s version=%d\n", d.Sticky(), d.Status, d.Version)
	fmt.Fprintf(&b, "  created=%s updated=%s\n", d.CreatedAt.Format("2006-01-02"), d.UpdatedAt.Format("2006-01-02"))
	if t, ok := d.LastActive(); ok {
		fmt.Fprintf(&b, "  last used=%s\n", t.Format("2006-01-02"))
	}
	if d.Summary != "" {
		fmt.Fprintf(&b, "  summary: %s\n", d.Summary)
	}
	return b.String()
}

func explainMemory(h store.Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Memory %s (domain %s)\n", h.ID, h.DomainID)
	fmt.Fprintf(&b, "  type=%s status=%s confidence=%.2f origin=%s\n", h.Type, h.Status, h.Confidence, h.Origin)
	fmt.Fprintf(&b, "  text: %s\n", h.Text)
	if h.FileRef != "" {
		fmt.Fprintf(&b, "  attached file: %s (full contents load into context with this memory)\n", h.FileRef)
	}
	if h.SourceSeqStart != nil && h.SourceSeqEnd != nil {
		fmt.Fprintf(&b, "  evidence: seq %d..%d\n", *h.SourceSeqStart, *h.SourceSeqEnd)
	}
	if h.SupersedesMemoryID != nil {
		fmt.Fprintf(&b, "  supersedes: %s\n", *h.SupersedesMemoryID)
	}
	if h.RetireReason != nil && *h.RetireReason != "" {
		fmt.Fprintf(&b, "  retire reason: %s\n", *h.RetireReason)
	}
	return b.String()
}

// rememberWith builds the memory_create handler, closing over what it needs to
// validate an attached markdown file against the agent's read permissions.
func rememberWith(host Host) handlerFunc {
	return func(s *store.Store, call *toolspec.ToolCall) (string, error) {
		return remember(s, call, host)
	}
}

func remember(s *store.Store, call *toolspec.ToolCall, host Host) (string, error) {
	mtype := argStr(call, "type")
	text := argStr(call, "text")
	if mtype == "" || text == "" {
		return "", errors.New("type and text are required")
	}
	// Type decides whether the memory is in the prompt every turn or reachable
	// only by search, so an unrecognised one is rejected rather than coerced.
	if !store.ValidMemoryTypes(store.MemoryType(mtype)) {
		return "", fmt.Errorf("unknown memory type %q: use fact, preference, rule, "+
			"operational (your own housekeeping) or event (something that happened "+
			"at a point in time)", mtype)
	}

	// Validate the attachment before storing anything: a pointer the agent cannot
	// read must fail here, in front of the user, rather than degrade every later
	// prompt into an "attachment unavailable" note.
	fileRef := argStr(call, "file")
	var fileSize int64
	if fileRef != "" {
		size, err := host.checkAttachment(fileRef)
		if err != nil {
			return "", fmt.Errorf("attachment rejected: %w", err)
		}
		fileSize = size
	}

	domainID := argStr(call, "domain_id")
	if domainID == "" {
		hint := argStr(call, "domain_hint")
		if hint == "" {
			// No domain specified → the sticky "General" domain. Re-create it if the
			// user previously deleted it, so there's always a default home.
			g, err := s.GeneralDomain(call.Ctx, s.DB())
			if errors.Is(err, store.ErrNotFound) {
				g, err = s.CreateDomain(call.Ctx, s.DB(), store.CreateDomainParams{
					AgentID: call.AgentID, SessionKey: call.Session,
					Name: "General", Sticky: true, Status: store.StatusActive,
					Summary: "Global rules, preferences, and standing facts.",
				})
			}
			if err != nil {
				return "", err
			}
			domainID = g.ID
		} else {
			// Reuse an existing domain with that name, else create a new (non-sticky) one.
			d, err := s.DomainByName(call.Ctx, s.DB(), hint)
			if errors.Is(err, store.ErrNotFound) {
				d, err = s.CreateDomain(call.Ctx, s.DB(), store.CreateDomainParams{
					AgentID:    call.AgentID,
					SessionKey: call.Session,
					Name:       hint,
					Status:     store.StatusActive,
				})
			}
			if err != nil {
				return "", err
			}
			domainID = d.ID
		}
	}

	// Status is not an argument. A memory the assistant chooses to write is
	// active; retiring one is a separate, deliberate act through memory_retire.
	// Letting it be set here is what allowed every tool-written memory to land
	// active by default with no thought given to it.
	h, err := s.AddMemory(call.Ctx, s.DB(), store.AddMemoryParams{
		DomainID:   domainID,
		Type:       store.MemoryType(mtype),
		Text:       text,
		Status:     store.StatusActive,
		Confidence: argFloat(call, "confidence", 0.9),
		Origin:     store.OriginChat,
		FileRef:    fileRef,
	})
	if err != nil {
		return "", mapErr(err, domainID)
	}
	msg := fmt.Sprintf("Stored memory %s in domain %s (type=%s).", h.ID, domainID, h.Type)
	if h.Type == store.TypeEvent {
		msg += " Event memories are not loaded into your context — retrieve it with" +
			" memory_search using include_events."
	}
	if fileRef != "" {
		msg += fmt.Sprintf(" Attached %s (%d bytes); its full contents load into context whenever this memory does.", fileRef, fileSize)
	}
	return msg, nil
}

func updateDomain(s *store.Store, call *toolspec.ToolCall) (string, error) {
	id := argStr(call, "id")
	if id == "" {
		return "", errors.New("id is required")
	}
	cur, err := s.GetDomain(call.Ctx, s.DB(), id, false)
	if err != nil {
		return "", mapErr(err, id)
	}
	p := store.UpdateDomainParams{}
	if _, ok := call.Args["set_name"]; ok {
		v := argStr(call, "set_name")
		p.Name = &v
	}
	if v := argStr(call, "set_summary"); v != "" {
		p.Summary = &v
	}
	if _, ok := call.Args["set_sticky"]; ok {
		v := argBool(call, "set_sticky", false)
		p.Sticky = &v
	}
	state := cur.State
	changedState := false
	if v, ok := argStrSlice(call, "set_blockers"); ok {
		state.Blockers = v
		changedState = true
	}
	if v, ok := argStrSlice(call, "set_next_actions"); ok {
		state.NextActions = v
		changedState = true
	}
	if v, ok := argStrSlice(call, "set_constraints"); ok {
		state.Constraints = v
		changedState = true
	}
	if changedState {
		p.State = &state
	}
	// set_triggers present (even empty) replaces the trigger list; empty clears it.
	if _, ok := call.Args["set_triggers"]; ok {
		v := argStr(call, "set_triggers")
		p.Triggers = &v
	}
	// set_keyword_triggers present (even empty) replaces the keyword list.
	if _, ok := call.Args["set_keyword_triggers"]; ok {
		kw, _ := argStrSlice(call, "set_keyword_triggers")
		v := strings.Join(kw, ",")
		p.KeywordTriggers = &v
	}
	if p.Name == nil && p.Summary == nil && p.State == nil && p.Triggers == nil && p.KeywordTriggers == nil && p.Sticky == nil {
		return "", errors.New("nothing to update (set_name, set_summary, set_sticky, set_triggers, set_keyword_triggers, or a list field)")
	}
	if err := s.UpdateDomain(call.Ctx, s.DB(), id, p); err != nil {
		if errors.Is(err, store.ErrDuplicateName) {
			return "", fmt.Errorf("a domain named that already exists — pick a unique name")
		}
		return "", mapErr(err, id)
	}
	return fmt.Sprintf("Updated domain %s.", id), nil
}

// exportMemory writes the agent's entire active memory as one Markdown document
// to files/MEMORY_EXPORT.yaml (its writable area) and reports the path and counts.
func exportMemory(s *store.Store, call *toolspec.ToolCall) (string, error) {
	doc, nDomains, nMemories, err := renderFullExport(call.Ctx, s)
	if err != nil {
		return "", err
	}
	// The store lives at <workspace>/sessions/<key>.cogmem.db; recover <workspace>
	// to write the export into the agent's read/write files/ directory.
	workspace := filepath.Dir(filepath.Dir(s.Path()))
	outDir := filepath.Join(workspace, "files")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to prepare export directory: %w", err)
	}
	outPath := filepath.Join(outDir, exportFilename)
	if err := os.WriteFile(outPath, []byte(doc), 0o644); err != nil {
		return "", fmt.Errorf("failed to write export: %w", err)
	}
	return fmt.Sprintf("Exported %d domain(s) and %d memory(ies) to files/%s.",
		nDomains, nMemories, exportFilename), nil
}

func retireHook(s *store.Store, call *toolspec.ToolCall) (string, error) {
	id := argStr(call, "id")
	reason := argStr(call, "reason")
	if id == "" || reason == "" {
		return "", errors.New("id and reason are required")
	}
	if err := s.RetireMemory(call.Ctx, s.DB(), id, reason); err != nil {
		return "", mapErr(err, id)
	}
	return fmt.Sprintf("Retired memory %s.", id), nil
}

// attachFileWith builds the memory_attach handler, closing over what it needs to
// validate the file against the agent's read permissions.
func attachFileWith(h Host) handlerFunc {
	return func(s *store.Store, call *toolspec.ToolCall) (string, error) {
		id := argStr(call, "id")
		if id == "" {
			return "", errors.New("id is required")
		}
		// An absent "file" argument is a detach, same as an empty one: there is
		// nothing else this tool could mean without it.
		ref := argStr(call, "file")

		var size int64
		if ref != "" {
			n, err := h.checkAttachment(ref)
			if err != nil {
				return "", fmt.Errorf("attachment rejected: %w", err)
			}
			size = n
		}

		m, err := s.SetMemoryFileRef(call.Ctx, s.DB(), id, ref)
		if err != nil {
			return "", mapErr(err, id)
		}
		if ref == "" {
			return fmt.Sprintf("Detached the file from memory %s; it no longer loads a document into context.", m.ID), nil
		}
		return fmt.Sprintf("Attached %s (%d bytes) to memory %s; its full contents load into context whenever this memory does.", m.FileRef, size, m.ID), nil
	}
}

func createDomain(s *store.Store, call *toolspec.ToolCall) (string, error) {
	name := argStr(call, "name")
	if name == "" {
		return "", errors.New("name is required")
	}
	kw, _ := argStrSlice(call, "keyword_triggers")
	d, err := s.CreateDomain(call.Ctx, s.DB(), store.CreateDomainParams{
		AgentID:         call.AgentID,
		SessionKey:      call.Session,
		Sticky:          argBool(call, "sticky", false),
		Name:            name,
		Status:          store.StatusActive,
		Summary:         argStr(call, "summary"),
		Triggers:        argStr(call, "triggers"),
		KeywordTriggers: strings.Join(kw, ","),
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicateName) {
			return "", fmt.Errorf("a domain named %q already exists — use it, rename it, or pick a unique name", name)
		}
		return "", err
	}
	return fmt.Sprintf("Created domain %s (name=%q, sticky=%t).", d.ID, d.Name, d.Sticky()), nil
}

func migrateDomain(s *store.Store, call *toolspec.ToolCall) (string, error) {
	from := argStr(call, "from")
	to := argStr(call, "to")
	if from == "" || to == "" {
		return "", errors.New("from and to domain ids are required")
	}
	n, err := s.MigrateDomain(call.Ctx, s.DB(), from, to)
	if err != nil {
		return "", mapErr(err, from)
	}
	return fmt.Sprintf("Migrated %d mem(s) from domain %s into %s; %s deleted.", n, from, to, from), nil
}

func archiveDomain(s *store.Store, call *toolspec.ToolCall) (string, error) {
	id := argStr(call, "id")
	if id == "" {
		return "", errors.New("id is required")
	}
	if err := s.ArchiveDomain(call.Ctx, s.DB(), id); err != nil {
		return "", mapErr(err, id)
	}
	return fmt.Sprintf("Archived domain %s.", id), nil
}

func forget(s *store.Store, call *toolspec.ToolCall) (string, error) {
	query := argStr(call, "query")
	if query == "" {
		return "", errors.New("query is required")
	}
	domainFilter := argStr(call, "domain_id")
	// Events included: forgetting is about removing something the user asked to
	// be rid of, and an event is as forgettable as anything else.
	hooks, err := s.SearchMemories(call.Ctx, s.DB(), query, 100, true)
	if err != nil {
		return "", err
	}
	var retired []string
	for _, h := range hooks {
		if domainFilter != "" && h.DomainID != domainFilter {
			continue
		}
		if err := s.RetireMemory(call.Ctx, s.DB(), h.ID, "forget: "+query); err != nil {
			return "", err
		}
		retired = append(retired, h.ID)
	}
	if len(retired) == 0 {
		return fmt.Sprintf("No active memories matched %q; nothing retired.", query), nil
	}
	sort.Strings(retired)
	return fmt.Sprintf("Retired %d memories: %s.", len(retired), strings.Join(retired, ", ")), nil
}

func consolidateWith(h Host) handlerFunc {
	return func(_ *store.Store, call *toolspec.ToolCall) (string, error) {
		if h.Consolidate != nil {
			h.Consolidate(call.AgentID, call.Session)
			return "Consolidation requested.", nil
		}
		return "Consolidation is not available: the host runs no background memory worker.", nil
	}
}

func statusWith(h Host) handlerFunc {
	return func(s *store.Store, call *toolspec.ToolCall) (string, error) {
		return status(s, call, h)
	}
}

func status(s *store.Store, call *toolspec.ToolCall, h Host) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Cognitive memory database: %s (healthy)\n", s.Path())

	run, ok, err := s.LastRun(call.Ctx, s.DB())
	if err != nil {
		return "", err
	}
	if !ok {
		b.WriteString("Last consolidation run: none\n")
	} else {
		fmt.Fprintf(&b, "Last consolidation run: %s (trigger=%s, status=%s, ops=%d) at %s\n",
			run.ID, run.Trigger, run.Status, run.OpsApplied, run.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if h.Consolidate == nil {
		b.WriteString("Consolidation worker: not running\n")
	} else {
		b.WriteString("Consolidation worker: active\n")
	}
	return b.String(), nil
}

// --- small helpers ---

func writeStateLine(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s: %s\n", label, strings.Join(items, "; "))
}

// mapErr turns store sentinel errors into user-facing messages naming the id.
func mapErr(err error, id string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s not found", id)
	}
	return err
}
