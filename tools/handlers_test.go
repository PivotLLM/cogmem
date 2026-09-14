// cogmem - Cognitive Memory
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PivotLLM/cogmem/portable"
	"github.com/PivotLLM/toolspec"
)

const testSession = "chan:123"

// buildHandlers maps bare tool names to their handlers, built against a temp
// workspace with no attachment checker and no consolidation worker.
func buildHandlers(t *testing.T) (map[string]toolspec.ToolHandler, string) {
	t.Helper()
	ws := t.TempDir()
	defs := Definitions(Host{Dir: filepath.Join(ws, "cogmem"), Workspace: ws})
	m := make(map[string]toolspec.ToolHandler, len(defs))
	for _, d := range defs {
		m[d.Name] = d.Handler
	}
	return m, ws
}

func newCall(session string, args map[string]any) *toolspec.ToolCall {
	if args == nil {
		args = map[string]any{}
	}
	return &toolspec.ToolCall{Ctx: context.Background(), Args: args, AgentID: "alice", Session: session}
}

func run(t *testing.T, h toolspec.ToolHandler, call *toolspec.ToolCall) *toolspec.Result {
	t.Helper()
	res, err := h(call)
	if err != nil {
		t.Fatalf("handler returned go error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	return res
}

func TestCreateRememberGetUpdateRetire(t *testing.T) {
	h, _ := buildHandlers(t)

	// create_domain returns an id.
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{
		"name": "Bob's project", "summary": "demo",
	}))
	if res.IsError {
		t.Fatalf("create_domain error: %s", res.ForLLM)
	}
	domainID := extractID(t, res.ForLLM, "d")

	// remember adds a hook to the domain.
	res = run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "fact", "text": "the sky is blue",
	}))
	if res.IsError {
		t.Fatalf("remember error: %s", res.ForLLM)
	}
	memoryID := extractID(t, res.ForLLM, "h")

	// get_domain shows the hook id and text.
	res = run(t, h["domain_get"], newCall(testSession, map[string]any{"id": domainID}))
	if res.IsError {
		t.Fatalf("get_domain error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, memoryID) || !strings.Contains(res.ForLLM, "the sky is blue") {
		t.Fatalf("get_domain missing hook: %s", res.ForLLM)
	}

	// update_domain is a patch — no version needed; only provided fields change.
	res = run(t, h["domain_update"], newCall(testSession, map[string]any{
		"id": domainID, "set_summary": "updated",
		"set_blockers": []any{"waiting on review"},
	}))
	if res.IsError {
		t.Fatalf("update_domain error: %s", res.ForLLM)
	}

	// A second patch (e.g. set_sticky) also succeeds — there is no optimistic lock.
	res = run(t, h["domain_update"], newCall(testSession, map[string]any{
		"id": domainID, "set_sticky": true,
	}))
	if res.IsError {
		t.Fatalf("second update_domain (set_sticky) error: %s", res.ForLLM)
	}

	// retire_hook removes it from active memory.
	res = run(t, h["memory_retire"], newCall(testSession, map[string]any{
		"id": memoryID, "reason": "no longer true",
	}))
	if res.IsError {
		t.Fatalf("retire_hook error: %s", res.ForLLM)
	}

	// search no longer finds it (retired != active).
	res = run(t, h["memory_search"], newCall(testSession, map[string]any{"query": "sky"}))
	if res.IsError {
		t.Fatalf("search error: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, memoryID) {
		t.Fatalf("retired hook still found in search: %s", res.ForLLM)
	}
}

func TestRememberWithDomainHint(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_hint": "new project", "type": "preference", "text": "use tabs",
	}))
	if res.IsError {
		t.Fatalf("remember(hint) error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Stored memory h") {
		t.Fatalf("unexpected result: %s", res.ForLLM)
	}
}

// Search excludes events unless asked, which is the whole retrieval path for
// them: an event is never loaded into the prompt, so if search cannot reach it
// nothing can.
func TestSearchExcludesEventsUnlessAsked(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "p"}))
	domainID := extractID(t, res.ForLLM, "d")

	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "fact", "text": "standing widget",
	}))
	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "event", "text": "widget shipped Sep 4",
	}))

	res = run(t, h["memory_search"], newCall(testSession, map[string]any{"query": "widget"}))
	if !strings.Contains(res.ForLLM, "standing widget") {
		t.Fatalf("search missed the standing memory: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "widget shipped") {
		t.Fatalf("event returned without include_events: %s", res.ForLLM)
	}

	res = run(t, h["memory_search"], newCall(testSession, map[string]any{
		"query": "widget", "include_events": true,
	}))
	if !strings.Contains(res.ForLLM, "widget shipped") {
		t.Fatalf("include_events did not reach the event: %s", res.ForLLM)
	}
}

// When nothing else matches, events are searched anyway.
//
// Events are held back so a routine lookup is not buried under hundreds of
// recurring notes — but with no other results there is nothing to bury, and
// excluding them only turns a findable memory into "not found". Asked when a
// trip happened, a live agent called this tool four times with identical
// arguments, never added include_events, and gave up while the answer sat in
// the store. Retrieval must not depend on the model remembering a flag.
func TestSearchFallsBackToEventsWhenNothingElseMatches(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "trips"}))
	domainID := extractID(t, res.ForLLM, "d")
	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "event", "text": "drove to the KOA on Sep 4",
	}))

	// No standing memory mentions the KOA, so the fallback is the only way this
	// is ever found — and the caller is told why it is seeing events.
	res = run(t, h["memory_search"], newCall(testSession, map[string]any{"query": "KOA"}))
	if res.IsError {
		t.Fatalf("search error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "drove to the KOA") {
		t.Fatalf("fallback did not reach the event: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "no standing memories matched") {
		t.Fatalf("result does not say why events were searched: %s", res.ForLLM)
	}
}

// The fallback must not fire when a standing memory DID match: that is the case
// the exclusion exists for, and quietly appending events would defeat it.
func TestSearchDoesNotFallBackWhenSomethingMatched(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "trips"}))
	domainID := extractID(t, res.ForLLM, "d")
	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "fact", "text": "the KOA is near Gananoque",
	}))
	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "event", "text": "drove to the KOA on Sep 4",
	}))

	res = run(t, h["memory_search"], newCall(testSession, map[string]any{"query": "KOA"}))
	if !strings.Contains(res.ForLLM, "near Gananoque") {
		t.Fatalf("search missed the standing memory: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "drove to the KOA") {
		t.Fatalf("events leaked in although a standing memory matched: %s", res.ForLLM)
	}
}

// Creating an event tells the assistant it will not be in context, because the
// alternative is a memory it believes it stored and then never sees again.
func TestCreateEventExplainsItIsSearchOnly(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["memory_create"], newCall(testSession, map[string]any{
		"type": "event", "text": "oversight run at 07:40",
	}))
	if res.IsError {
		t.Fatalf("memory_create(event) error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "not loaded into your context") {
		t.Fatalf("event creation should say it is search-only: %s", res.ForLLM)
	}
}

// An unrecognised type is rejected rather than coerced. Type decides whether a
// memory is in the prompt at all, so a silent fallback would either hide it or
// expose it, both invisibly.
func TestCreateRejectsUnknownType(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["memory_create"], newCall(testSession, map[string]any{
		"type": "observation", "text": "x",
	}))
	if !res.IsError {
		t.Fatalf("unknown type accepted: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "operational") || !strings.Contains(res.ForLLM, "event") {
		t.Fatalf("rejection should name the valid types: %s", res.ForLLM)
	}
}

// memory_confirm is gone with the review status it promoted from.
func TestConfirmToolIsGone(t *testing.T) {
	h, _ := buildHandlers(t)
	if _, ok := h["memory_confirm"]; ok {
		t.Error("memory_confirm still registered: the review status it promoted from no longer exists")
	}
}

func TestForgetRetiresMatches(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "p"}))
	domainID := extractID(t, res.ForLLM, "d")
	for _, txt := range []string{"forget me one", "forget me two", "keep this"} {
		run(t, h["memory_create"], newCall(testSession, map[string]any{
			"domain_id": domainID, "type": "fact", "text": txt,
		}))
	}
	res = run(t, h["memory_forget"], newCall(testSession, map[string]any{"query": "forget me"}))
	if res.IsError {
		t.Fatalf("forget error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Retired 2 memories") {
		t.Fatalf("expected 2 retired, got: %s", res.ForLLM)
	}
	// "keep this" should still be searchable.
	res = run(t, h["memory_search"], newCall(testSession, map[string]any{"query": "keep this"}))
	if !strings.Contains(res.ForLLM, "keep this") {
		t.Fatalf("forget removed the wrong hook: %s", res.ForLLM)
	}
}

func TestArchiveAndListDomains(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "Bob proj"}))
	domainID := extractID(t, res.ForLLM, "d")

	res = run(t, h["domain_list"], newCall(testSession, map[string]any{"status": "active"}))
	if !strings.Contains(res.ForLLM, domainID) {
		t.Fatalf("active list missing domain: %s", res.ForLLM)
	}

	if r := run(t, h["domain_archive"], newCall(testSession, map[string]any{"id": domainID})); r.IsError {
		t.Fatalf("archive error: %s", r.ForLLM)
	}
	res = run(t, h["domain_list"], newCall(testSession, map[string]any{"status": "active"}))
	if strings.Contains(res.ForLLM, domainID) {
		t.Fatalf("archived domain still active: %s", res.ForLLM)
	}
}

func TestDomainMigrate(t *testing.T) {
	h, _ := buildHandlers(t)
	fromID := extractID(t, run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "From"})).ForLLM, "d")
	toID := extractID(t, run(t, h["domain_create"], newCall(testSession, map[string]any{"name": "To"})).ForLLM, "d")
	run(t, h["memory_create"], newCall(testSession, map[string]any{"domain_id": fromID, "type": "fact", "text": "migrate me"}))

	res := run(t, h["domain_migrate"], newCall(testSession, map[string]any{"from": fromID, "to": toID}))
	if res.IsError {
		t.Fatalf("domain_migrate error: %s", res.ForLLM)
	}
	// The source domain is gone; its memory now lives under the target.
	if r := run(t, h["domain_get"], newCall(testSession, map[string]any{"id": fromID})); !r.IsError {
		t.Fatalf("source domain survived migrate: %s", r.ForLLM)
	}
	r := run(t, h["domain_get"], newCall(testSession, map[string]any{"id": toID}))
	if r.IsError || !strings.Contains(r.ForLLM, "migrate me") {
		t.Fatalf("target domain missing migrated memory: %s", r.ForLLM)
	}
}

func TestStatusAndConsolidate(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["status"], newCall(testSession, nil))
	if res.IsError {
		t.Fatalf("status error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Last consolidation run: none") {
		t.Fatalf("unexpected status: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "Pending") {
		t.Fatalf("status still reports pending memories, which no longer exist: %s", res.ForLLM)
	}

	res = run(t, h["consolidate"], newCall(testSession, nil))
	if res.IsError || !strings.Contains(res.ForLLM, "no background memory worker") {
		t.Fatalf("unexpected consolidate result: %s", res.ForLLM)
	}
}

// A host that runs a worker gets the request forwarded with the agent and
// session from the call, and status reports the worker as active.
func TestConsolidateForwardsToHost(t *testing.T) {
	ws := t.TempDir()
	var gotAgent, gotSession string
	defs := Definitions(Host{Dir: filepath.Join(ws, "cogmem"), Workspace: ws, Consolidate: func(agentID, sessionKey string) {
		gotAgent, gotSession = agentID, sessionKey
	}})
	h := map[string]toolspec.ToolHandler{}
	for _, d := range defs {
		h[d.Name] = d.Handler
	}
	res := run(t, h["consolidate"], newCall(testSession, nil))
	if res.IsError || !strings.Contains(res.ForLLM, "requested") {
		t.Fatalf("consolidate: %s", res.ForLLM)
	}
	if gotAgent != "alice" || gotSession != testSession {
		t.Fatalf("forwarded (%q, %q)", gotAgent, gotSession)
	}
	res = run(t, h["status"], newCall(testSession, nil))
	if !strings.Contains(res.ForLLM, "Consolidation worker: active") {
		t.Fatalf("status: %s", res.ForLLM)
	}
}

// Without an attachment checker a file reference is refused up front rather
// than stored and failing in every later prompt.
func TestAttachmentRefusedWithoutChecker(t *testing.T) {
	h, _ := buildHandlers(t)
	res := run(t, h["memory_create"], newCall(testSession, map[string]any{
		"type": "rule", "text": "Write in my voice.", "file": "files/voice.md",
	}))
	if !res.IsError || !strings.Contains(res.ForLLM, "not supported") {
		t.Fatalf("expected refusal, got: %s", res.ForLLM)
	}
}

// The export is YAML that can be read back, not Markdown that could only be
// looked at. This test round-trips it: what comes out of the tool must parse as
// a memory document and still contain what went in.
func TestExportMemoryRoundTrips(t *testing.T) {
	h, ws := buildHandlers(t)
	res := run(t, h["domain_create"], newCall(testSession, map[string]any{
		"name":             "BioTech",
		"triggers":         "google_gmail",
		"keyword_triggers": []any{"biotech report"},
	}))
	domainID := extractID(t, res.ForLLM, "d")
	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "fact", "text": "the report targets Q3",
	}))
	run(t, h["memory_create"], newCall(testSession, map[string]any{
		"domain_id": domainID, "type": "event", "text": "results published Sep 4",
	}))

	res = run(t, h["export"], newCall(testSession, nil))
	if res.IsError || !strings.Contains(res.ForLLM, "MEMORY_EXPORT.yaml") {
		t.Fatalf("unexpected export result: %s", res.ForLLM)
	}
	data, err := os.ReadFile(filepath.Join(ws, "files", exportFilename))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	doc, err := portable.Unmarshal(data)
	if err != nil {
		t.Fatalf("export does not parse as a memory document: %v\n%s", err, data)
	}
	if doc.FormatVersion != portable.FormatVersion {
		t.Errorf("format_version = %d, want %d", doc.FormatVersion, portable.FormatVersion)
	}

	var found, foundEvent bool
	for _, d := range doc.Domains {
		if d.Name != "BioTech" {
			continue
		}
		if d.Triggers != "google_gmail" {
			t.Errorf("triggers = %q, want google_gmail", d.Triggers)
		}
		if !strings.Contains(d.KeywordTriggers, "biotech report") {
			t.Errorf("keyword_triggers = %q, want it to include the phrase", d.KeywordTriggers)
		}
		for _, m := range d.Memories {
			switch m.Text {
			case "the report targets Q3":
				found = true
			case "results published Sep 4":
				foundEvent = true
				if m.Type != "event" {
					t.Errorf("event exported with type %q", m.Type)
				}
			}
		}
	}
	if !found {
		t.Errorf("export lost the standing memory:\n%s", data)
	}
	// An export is a backup, so it holds everything — including the memories
	// that are deliberately absent from the prompt.
	if !foundEvent {
		t.Errorf("export omitted the event memory:\n%s", data)
	}
}

// A host that configured no memory directory (a deps-free catalogue
// enumeration, or an agent without memory) gets an error result from every
// tool, never a store created somewhere by accident.
//
// This never reaches a handler: wrap() refuses before opening anything, so
// the arguments here are irrelevant and handler validation is NOT exercised
// by this test — see TestMissingRequiredArgs for that. Every registered tool,
// consolidate and export included, is refused with the same message.
func TestEmptyDirErrors(t *testing.T) {
	defs := Definitions(Host{})
	if len(defs) != 15 {
		t.Fatalf("registered tools = %d, want 15", len(defs))
	}
	const want = "cognitive memory is unavailable (no memory directory configured)"
	for _, d := range defs {
		res := run(t, d.Handler, newCall(testSession, map[string]any{"id": "dXXXXX", "query": "x", "type": "fact", "text": "t", "name": "n"}))
		if !res.IsError {
			t.Fatalf("%s: expected error with no memory directory, got: %s", d.Name, res.ForLLM)
		}
		if res.ForLLM != want {
			t.Errorf("%s: error = %q, want %q", d.Name, res.ForLLM, want)
		}
		if res.Err != nil {
			t.Errorf("%s: Err = %v, want nil (refusal is not a Go error)", d.Name, res.Err)
		}
	}
}

// extractID pulls the first whitespace-delimited token beginning with prefix
// followed by Crockford chars (e.g. "d3K9P") from text.
func extractID(t *testing.T, text, prefix string) string {
	t.Helper()
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '(' || r == ')' || r == ',' || r == '.'
	}) {
		if len(tok) == 6 && strings.HasPrefix(tok, prefix) && isCrockfordTail(tok[1:]) {
			return tok
		}
	}
	t.Fatalf("no %s-id found in: %s", prefix, text)
	return ""
}

// isCrockfordTail reports whether s is all uppercase Crockford base32 digits
// (the id tail), distinguishing a real id from an English word like "domain".
func isCrockfordTail(s string) bool {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, c := range s {
		if !strings.ContainsRune(alphabet, c) {
			return false
		}
	}
	return true
}
