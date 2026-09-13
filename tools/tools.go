// cogmem - Cognitive Memory
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// Package tools exposes the cognitive-memory store as toolspec tool definitions
// with BARE names ("domain_get", "memory_create", ...). A host mounts them under
// its own namespace (ClawEh publishes "cogmem_domain_get" and so on); the names
// follow object_verb: domain_* (containers), memory_* (items), plus the
// subsystem tools status/explain/consolidate/export.
//
// Every tool is session-scoped: it operates on the per-session .cogmem.db
// resolved from Host.Workspace and ToolCall.Session. The tools are a thin
// layer over the store package — the typed API a GUI or CLI uses directly — so
// the two cannot disagree.
package tools

import (
	"errors"

	"github.com/PivotLLM/toolspec"
)

// Host is what a tool needs from the application embedding cognitive memory.
type Host struct {
	// Workspace is the agent workspace; the per-session store lives under
	// <Workspace>/sessions. Empty during a deps-free catalogue enumeration, in
	// which case handlers refuse to touch disk.
	Workspace string
	// CheckAttachment validates that ref is a markdown file the agent may read
	// and returns its size. Nil means attachments are not supported: a memory
	// with a file reference is rejected at create time rather than silently
	// failing in every later prompt.
	CheckAttachment func(ref string) (int64, error)
	// Consolidate asks the host's background worker to run for a session now.
	// Nil means the tool reports that no worker is available.
	Consolidate func(agentID, sessionKey string)
}

func (h Host) checkAttachment(ref string) (int64, error) {
	if h.CheckAttachment == nil {
		return 0, errors.New("file attachments are not supported by this host")
	}
	return h.CheckAttachment(ref)
}

// Definitions returns the cognitive-memory tool definitions bound to h.
func Definitions(h Host) []toolspec.ToolDefinition {
	workspace := h.Workspace

	def := func(name, desc string, params []toolspec.Parameter, allow bool, hf handlerFunc) toolspec.ToolDefinition {
		return toolspec.ToolDefinition{
			Name:          name,
			Description:   desc,
			Parameters:    params,
			Category:      "memory",
			SessionScoped: true,
			DefaultAllow:  toolspec.Allow(allow),
			Handler:       wrap(workspace, hf),
		}
	}

	defs := []toolspec.ToolDefinition{
		def("domain_get",
			"Load a domain by id, with its active memories as readable text (each memory's id included).",
			[]toolspec.Parameter{
				{Name: "id", Type: "string", Required: true, Description: "Domain id (e.g. d3K9P)."},
			}, true, getDomain),

		def("memory_search",
			"Search your active memories by a word or phrase in their text (case-insensitive). Use it to look something up when the answer may be in memory but is not currently shown in your context. Event memories are excluded unless you pass include_events — they are never in your context, so search is the ONLY way to reach them.",
			[]toolspec.Parameter{
				{Name: "query", Type: "string", Required: true, Description: "Word or phrase to match against memory text."},
				{Name: "limit", Type: "integer", Required: false, Description: "Max results (default 20)."},
				{Name: "include_events", Type: "boolean", Required: false, Description: "Include event memories (things that happened at a point in time: trips, deliveries, scheduled runs). Default false. Set true when the question is about the past — \"when did we last…\", \"what happened on…\"."},
			}, true, search),

		def("domain_list",
			"List memory domains (id, name, summary, status; sticky ones are marked). Optionally filter by status. To see everything you're tracking, list with no filter.",
			[]toolspec.Parameter{
				{
					Name: "status", Type: "string", Required: false, Description: "Filter by status.",
					Enum: []any{"active", "archived"},
				},
			}, true, listDomains),

		def("explain",
			"Summarize the status, origin, and evidence of a domain or memory id.",
			[]toolspec.Parameter{
				{Name: "id", Type: "string", Required: true, Description: "A domain id (d…) or memory id (h…)."},
			}, true, explain),

		def("memory_create",
			"Record a memory. With NO domain_id and NO domain_hint it records to your sticky 'General' domain (always in context). Give a domain_hint to use (or create) a topic domain by name, or a domain_id to target a specific one. Choose the type carefully — it decides whether the memory is in your context every turn or only reachable by search.",
			[]toolspec.Parameter{
				{Name: "domain_id", Type: "string", Required: false, Description: "Target domain id. If omitted and no domain_hint is given, records to the sticky General domain."},
				{Name: "domain_hint", Type: "string", Required: false, Description: "A domain name: an existing domain with that name is reused, otherwise a new (non-sticky) one is created. Omit to use General."},
				{
					Name: "type", Type: "string", Required: true, Description: "fact = something true that stays true. preference = how the user likes things done. rule = a hard directive governing your output or behaviour toward the user. operational = your OWN housekeeping: where you file things, how you work, a rule you set for your own method. event = something that happened at a point in time (a trip, a delivery, a scheduled run, a status as of a date). Use event for anything with a timestamp or that will be stale next week — events are NEVER loaded into your context and are reached only by memory_search, which is what keeps recurring notes from crowding out everything else.",
					Enum: []any{"fact", "preference", "rule", "operational", "event"},
				},
				{Name: "text", Type: "string", Required: true, Description: "The memory content to store."},
				{Name: "confidence", Type: "number", Required: false, Description: "Confidence 0..1 (default 0.9)."},
				{Name: "file", Type: "string", Required: false, Description: "Optional path to a markdown file (e.g. \"files/voice.md\", \"maestro/style-guide.md\") whose FULL contents are injected into your context whenever this memory is in context. Use it for reference material too long to put in the memory text, such as a writing-voice description; keep the memory text as a one-line description of what the document is and when to use it. The path must be one you can read with the file tools, and the file is read fresh each turn."},
			}, true, rememberWith(h)),

		def("domain_update",
			"Update a domain — a patch: pass only the fields you want to change (rename, summary, state, sticky, triggers). No version needed.",
			[]toolspec.Parameter{
				{Name: "id", Type: "string", Required: true, Description: "Domain id."},
				{Name: "set_name", Type: "string", Required: false, Description: "Rename the domain. Names must be unique; rejected if another domain already uses it."},
				{Name: "set_sticky", Type: "boolean", Required: false, Description: "true = always inject this domain into context every turn; false = routed only when relevant. Use sticky sparingly."},
				{Name: "set_summary", Type: "string", Required: false, Description: "Replace the domain summary."},
				{Name: "set_blockers", Type: "array", Items: "string", Required: false, Description: "Replace the blockers list."},
				{Name: "set_next_actions", Type: "array", Items: "string", Required: false, Description: "Replace the next-actions list."},
				{Name: "set_constraints", Type: "array", Items: "string", Required: false, Description: "Replace the constraints list."},
				{Name: "set_triggers", Type: "string", Required: false, Description: "Replace tool triggers: comma-separated name fragments that auto-load this domain whenever you use a tool whose name contains one (substring, case-insensitive). E.g. \"mail,github\"; for an MCP server use its name (\"github\" matches mcp_github_*). Empty string clears."},
				{Name: "set_keyword_triggers", Type: "array", Items: "string", Required: false, Description: "Replace keyword triggers: words/phrases that load this domain when they appear in the incoming message (whole-phrase, word-boundary). Prefer multi-word phrases like \"morning routine\" over common single words. Empty list clears."},
			}, true, updateDomain),

		def("memory_attach",
			"Attach a markdown file to an existing memory, or detach the current one. The attached file's FULL contents load into your context whenever that memory does. Use this to point a memory at a different document (e.g. the file moved or was rewritten) or to remove the document while keeping the memory — memory text itself is not editable; retire and re-create for that.",
			[]toolspec.Parameter{
				{Name: "id", Type: "string", Required: true, Description: "Memory id."},
				{Name: "file", Type: "string", Required: false, Description: "Path to a markdown file you can read with the file tools (e.g. \"files/voice.md\", \"maestro/style-guide.md\"). Omit or pass an empty string to detach the current file."},
			}, true, attachFileWith(h)),

		def("memory_retire",
			"Retire a memory so it is no longer used (it stays in the audit history). To change a memory, retire the old one and create a new one.",
			[]toolspec.Parameter{
				{Name: "id", Type: "string", Required: true, Description: "Memory id."},
				{Name: "reason", Type: "string", Required: true, Description: "Why it is being retired."},
			}, true, retireHook),

		def("domain_create",
			"Create a new memory domain and return its assigned id. A domain groups related memories; register each ongoing project/topic as its own domain. Names must be unique — creating one with an existing name returns an error (reuse or rename instead).",
			[]toolspec.Parameter{
				{Name: "name", Type: "string", Required: true, Description: "Domain name (must be unique)."},
				{Name: "sticky", Type: "boolean", Required: false, Description: "true = always inject this domain into context every turn (use sparingly). Default false: loaded only when relevant."},
				{Name: "summary", Type: "string", Required: false, Description: "Optional one-line summary."},
				{Name: "triggers", Type: "string", Required: false, Description: "Optional tool triggers: comma-separated name fragments that auto-load this domain whenever you use a tool whose name contains one (substring, case-insensitive). E.g. \"mail,github\"; for an MCP server use its name (\"github\" matches mcp_github_*)."},
				{Name: "keyword_triggers", Type: "array", Items: "string", Required: false, Description: "Optional keyword triggers: words/phrases that load this domain when they appear in the incoming message (whole-phrase, word-boundary). Prefer multi-word phrases like \"morning routine\". Use this to pull a workflow's context up when a cron job fires."},
			}, true, createDomain),

		def("domain_archive",
			"Archive a domain so it is no longer used in prompting.",
			[]toolspec.Parameter{
				{Name: "id", Type: "string", Required: true, Description: "Domain id."},
			}, true, archiveDomain),

		def("domain_migrate",
			"Merge two domains: move every memory from the 'from' domain into the 'to' domain, then permanently delete the 'from' domain. Use this to consolidate duplicate/overlapping domains into one.",
			[]toolspec.Parameter{
				{Name: "from", Type: "string", Required: true, Description: "Source domain id — its memories are moved out and the domain is deleted."},
				{Name: "to", Type: "string", Required: true, Description: "Destination domain id — receives the moved memories."},
			}, true, migrateDomain),

		def("memory_forget",
			"Retire all active memories matching a query (optionally limited to one domain). Reports how many were retired.",
			[]toolspec.Parameter{
				{Name: "query", Type: "string", Required: true, Description: "Substring to match against active memory text."},
				{Name: "domain_id", Type: "string", Required: false, Description: "Restrict to this domain."},
			}, true, forget),

		def("consolidate",
			"Ask the background process to review the recent conversation and update memory now. Returns immediately; the work runs in the background.",
			[]toolspec.Parameter{
				{Name: "scope", Type: "string", Required: false, Description: "Optional scope hint (currently advisory)."},
			}, true, consolidateWith(h)),

		def("export",
			"Dump the agent's entire memory — every domain and memory, with all their fields — to files/MEMORY_EXPORT.yaml, and report the path and counts. The format can be read back in, so it works as a backup or to hand your memory to another assistant.",
			nil, true, exportMemory),

		def("status",
			"Report memory status: database path, the last background update, and how many domains and memories are held.",
			nil, true, statusWith(h)),
	}

	// A host that runs sub-agents on a snapshot of the primary's store can hand
	// them the full toolset, including writes: the isolation, not tool
	// withholding, is what protects the primary's memory.
	return defs
}
