# cogmem — cognitive memory for LLM agents

Durable, typed memory for an AI assistant: facts, preferences, rules, its own
working notes, and a searchable record of things that happened, organised into
domains, injected into the prompt when relevant, and refined in the background
by automated consolidation passes over recent conversations.

cogmem is self-contained. It keeps its own copy of every message it is shown
until it has learned from it, carries no host logging, config, or transport code,
and it exposes the same capability three ways so they cannot disagree:

| Surface | Package | For |
|---|---|---|
| Runner interface: `Session` (`Observe`, `Recall`, `RecordToolUse`) | `cogmem` | the host's agent loop |
| Typed API: domains, memories, inbox, runs, export/import | `store`, `portable` | GUI, CLI, tests |
| Tools: `toolspec` definitions built over the store | `tools` | the model, over any transport |

`Guidance()` returns the prompt text that teaches an assistant how the memory works. The host
is responsible for including it in the prompt of assistants with memory.

## How memory reaches the model

Memory is organised into **domains**, named containers of typed memories
(`fact`, `preference`, `rule`, `operational`, `event`). A domain is either
**sticky** (in every prompt) or a **topic** (loaded only when relevant). Every
domain can carry two kinds of triggers that the assistant sets itself: **tool
triggers** (substrings matched against the names of tools it has just used)
and **keyword triggers** (phrases matched, whole and at word boundaries, in the
incoming message). Memories can point at a markdown file whose full contents
are injected whenever the memory is.

There are three ways memories reach the model. The first two are
retrieval, providing RAG-like behaviour. The third is learning.

### 1. Automatic, per message (`Session.Recall`)

Before every model call, the host asks for this turn's blocks, passing the
latest user message and a ring of recently used tool names. Cogmem returns
two blocks for different placements to assist with prompt caching economics:

- **STABLE** goes into the system message, where it is part of the cached
  prompt prefix. It holds every sticky domain with its prompt memories,
  plus a **topic index**, one line per memory domain so the assistant knows
  what it could ask for.
- **ROUTED** is folded into the current user message, so it never sits ahead
  of the history, as doing so would invalidate prompt caching. It holds up to
  `top_k_domains` topic domains (default 3) within `max_chars` (default 4000),
  chosen by signal strength, in this order:

  1. **Tool triggers.** A domain whose trigger matches a tool used in
     this turn. Using `mcp_github_*` loads any domain that declared `github`.
  2. **Keyword triggers.** A domain whose declared phrase appears in the
     incoming message, including a scheduled (cron) message: "morning
     routine" loads the workflow domain that asked for it.
  3. **Lexical match.** Salient terms are taken from the message (runs of
     letters and digits of at least four characters, a small stopword list
     removed, at most twelve terms) and matched, case-insensitively, against
     memory text through the store's indexed substring search and against
     domain names and summaries. Domains are scored by hit count; recency
     breaks ties. Event memories are excluded from this scoring so a domain
     full of trip logs cannot win on volume.
  4. **Recency.** Remaining slots are filled with the most recently active
     topics.

A domain appears at most once even when several signals pick it. A domain 
loaded by a real signal (tool, keyword, lexical) is marked active; one loaded
only as recency filler is not, so the recency signal cannot reinforce
itself and a topic can go cold.

Within a rendered domain, several rules are applied:

- memories below `min_confidence` (default 0.65) are omitted
- `event` memories are never rendered (the domain shows how many it
holds)
- each memory carries its type and, when it did not come from the
assistant, its origin (`[origin: user]` outranks the assistant's own
inferences)
- Documents attached to rendered memories are loaded once each,
sticky ones with the STABLE block and routed ones with the ROUTED block, under
`file_max_bytes` and `file_total_max_bytes` budgets; a document that cannot
be read is reported in the prompt as unavailable rather than silently dropped.

The net effect is a collaborative approach in which the assistant applies tool and
keyword tags to memory domains, resulting in relevant memories being automatically
inserted into the context.

### 2. On request, by the assistant (the tools)

The assistant can access memories that did not automatically load:

- `memory_search` — case-insensitive substring search over active memory
  text. `event` memories are excluded unless `include_events` is set; when
  nothing else matches, the search retries including events and says so.
  This is the only way to reach events, which is deliberate: things that
  happened at a point in time are searched for, never carried in every prompt.
- `domain_get` — load a whole memory domain by id.
- `domain_list`, `explain`, `status` — inspect what exists and where a memory
  came from (origin, evidence range in the conversation).

It also shapes future routing: `domain_create` and `domain_update` set
stickiness, tool triggers and keyword triggers; `memory_create` records with a
`domain_hint`; `memory_attach` points a memory at a document; `memory_retire`
and `memory_forget` take things out of use. The system prompt supplied by
`Guidance()` tells it to search before answering anything that may depend on
context it cannot currently see.

In addition to directly accessing memories, this reinforces automated calls.
The assistant can make any adjustments required. 

### 3. In the background (consolidation)

Every message is handed to `Session.Observe` and kept in cogmem's own inbox.
When triggered by message count, idle time, or a nightly event, the consolidation
worker sends the curated files, the current memory state and new messages
to a model and applies the operations it returns (create or archive domains;
add, supersede or retire memories), each one citing the message range that
justifies it.

This process ensures that memories the assistant did not explicitly save are
carefully considered for possible memory content. Duplicates and stale entries
are also tidied. While cogmem's consolidation engine provides a fixed prompt,
a per-agent `COGMEM.md` allows adding agent-specific instructions.

### What cogmem is not

Cogmem is lexical and deterministic. It does not rely on embeddings, vectors,
or a full-text engine.

A domain, not a chunk, is the unit of retrieval, and the assistant can see and
edit the triggers that drive it. The assistant can also create domains as requried.
That keeps the behaviour inspectable (`explain`, the optional routing trace) and
avoids the expense of external services or hardware required for embeddings.

A host that wants semantic recall on top can implement its own `Recall` against
the same store, or add an additional retrieval provider beside cogmem.

## How consolidation calls a model

cogmem does not call a model itself and doesn't depend on any provider library.
`consolidate.ModelCaller` is a one-method interface that the host implements:

```go
// ModelRequest is one call to a model. The host owns which model answers,
// credentials, transport, retries on transport errors and cooldowns.
type ModelRequest struct {
    System     string
    User       string
    JSONObject bool     // ask for a JSON-object response where the provider supports it
    Exclude    []string // model names the caller has learned to avoid for this call
}

// ModelReply is the answer. Model names which model produced it.
type ModelReply struct {
    Content      string
    FinishReason string
    Model        string
}

type ModelCaller interface {
    Complete(ctx context.Context, req ModelRequest) (ModelReply, error)
}
```

The split is deliberate. The host owns the models: it walks its own chain,
skipping any model named in `Exclude`, and returns an error only when no model
is left to try or a transport failure could not be routed around. It does not
look at the content. cogmem owns its output format: the worker sends the
consolidation prompt and JSON payload with `JSONObject` set, parses the reply
as the strict consolidation `Output`, and if the content is empty or does not
parse it logs the reply, adds `reply.Model` to `Exclude` and asks again, up to
four attempts in total. A run that still has no usable reply is recorded as
`invalid_json` with a plain message (never a raw parser error), and the inbox
is kept for the next run; a host error is recorded as `error`. The run names
the model whose reply was accepted, or the last one tried, falling back to
`WithModelName` when a reply carries no model name.

The same request/reply shape is used by the context engine, so one host client
can serve both.

## Host obligations

- Install a logging backend with `logger.SetBackend` (silent otherwise).
- Provide a `consolidate.ModelCaller` (see above) and, if attachments are
  wanted, an `AttachmentLoader` that enforces the agent's read permissions.
- Call `Session.Observe` after every stored message with the transcript seq,
  and `Session.Recall` before every model call.

## Storage

The host gives cogmem a directory and cogmem owns everything in it: one SQLite
database, `cogmem.db` (pure Go, `modernc.org/sqlite`, migrated on open with a
pre-migration snapshot kept beside it), plus the WAL and shared-memory files
SQLite needs. The directory is passed as `SessionOptions.Dir` for the runner
interface and `tools.Host.Dir` for the tools; `store.DBPath(dir)` names the
file for anything that opens it directly, and `store.Migrate(dir)` upgrades it
eagerly at load.

One directory is one memory. cogmem does not know or care what a session is:
a host that wants one memory per assistant passes one directory per assistant,
and a host that ever wants several memories for one assistant passes several
directories. An identifier (`SessionOptions.ID`) travels with the memory only
so log lines and consolidation run records can say which one they are about.
Because the directory is self-contained, it can be backed up, copied to a new
assistant, or kept while everything else about the assistant is deleted.

## Copyright and license

Copyright (c) 2026 Tenebris Technologies Inc.

This software is licensed under the MIT License. Please see LICENSE for details.

## Trademarks

Any trademarks referenced are the property of their respective owners, used for identification only, and do not imply sponsorship, endorsement, or affiliation.

## No Warranty

**(zilch, none, void, nil, null, "", {}, 0x00, 0b00000000, EOF)**

THIS SOFTWARE IS PROVIDED “AS IS,” WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND NON-INFRINGEMENT. IN NO EVENT SHALL THE COPYRIGHT HOLDERS OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

Made in Canada with internationally sourced components.
