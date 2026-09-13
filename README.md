# cogmem — cognitive memory for LLM agents

Durable, typed memory for an assistant: facts, preferences, rules, its own
working notes, and a searchable record of things that happened — organised into
domains, injected into the prompt when relevant, and refined in the background
by a consolidation pass over the conversation.

cogmem is self-contained. It keeps its own copy of every message it is shown
until it has learned from it, so it depends on nothing else for its input; it
carries no host logging, config or transport code; and it exposes the same
capability three ways so they cannot disagree:

| Surface | Package | For |
|---|---|---|
| Runner interface: `Session` (`Observe`, `Recall`, `RecordToolUse`) | `cogmem` | the host's agent loop |
| Typed API: domains, memories, inbox, runs, export/import | `store`, `portable` | a GUI, a CLI, tests |
| Tools: `toolspec` definitions built over the store | `tools` | the model, over any transport |

`Guidance()` returns the prompt text that teaches an assistant how the memory
works; hosts render it only for agents that have memory.

## How memory reaches the model

Memory is organised into **domains**: named containers of typed memories
(`fact`, `preference`, `rule`, `operational`, `event`). A domain is either
**sticky** (in every prompt) or a **topic** (loaded only when relevant). Every
domain can carry two kinds of trigger the assistant sets itself: **tool
triggers** (substrings matched against the names of tools it has just used)
and **keyword triggers** (phrases matched, whole and at word boundaries, in the
incoming message). Memories can point at a markdown file whose full contents
are injected whenever the memory is.

There are three ways memory gets in front of the model. The first two are
retrieval, and together they are the "RAG-like" behaviour; the third is
learning.

### 1. Automatic, per message (`Session.Recall`)

Before every model call the host asks for this turn's blocks, passing the
latest user message and the ring of recently used tool names. The composer
returns two blocks with different placements, because they have different
cache economics:

- **STABLE** goes into the system message, where it is part of the cached
  prompt prefix and paid for once per session. It holds every sticky domain
  with its prompt memories, plus a **topic index**: one line per topic domain
  (`id · name — summary`) so the assistant knows what it could ask for.
- **ROUTED** is folded into the current user message, so it never sits ahead
  of the history and never invalidates the cached prefix. It holds up to
  `top_k_domains` topic domains (default 3) within `max_chars` (default 4000),
  chosen by signal strength, in this order:

  1. **Tool triggers.** A domain whose trigger matches a tool used earlier in
     this turn. Using `mcp_github_*` loads the domain that declared `github`.
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
  itself and a cold topic can go cold.

Within a rendered domain, memories below `min_confidence` (default 0.65) are
omitted, `event` memories are never rendered (the domain shows how many it
holds), and each memory carries its type and, when it did not come from the
assistant, its origin (`[origin: user]` outranks the assistant's own
inferences). Documents attached to rendered memories are loaded once each,
sticky ones with the STABLE block and routed ones with the ROUTED block, under
`file_max_bytes` and `file_total_max_bytes` budgets; a document that cannot
be read is reported in the prompt as unavailable rather than silently dropped.

### 2. On request, by the assistant (the tools)

The assistant can reach anything routing did not load:

- `memory_search` — case-insensitive substring search over active memory
  text. `event` memories are excluded unless `include_events` is set; when
  nothing else matches, the search retries including events and says so.
  This is the only way to reach events, which is deliberate: things that
  happened at a point in time are searched for, never carried in every prompt.
- `domain_get` — load a whole domain by id, memories included, with ids.
- `domain_list`, `explain`, `status` — inspect what exists and where a memory
  came from (origin, evidence range in the conversation).

It also shapes future routing: `domain_create` and `domain_update` set
stickiness, tool triggers and keyword triggers; `memory_create` records with a
`domain_hint`; `memory_attach` points a memory at a document; `memory_retire`
and `memory_forget` take things out of use. The system prompt text from
`Guidance()` tells it to search before answering anything that may depend on
context it cannot currently see.

### 3. In the background (consolidation)

Every stored message is handed to `Session.Observe` and kept in the store's
own inbox. On a message count, after idle time and nightly, the consolidation
worker sends the curated files, the current memory state and the new messages
to a model and applies the operations it returns (create or archive domains;
add, supersede or retire memories), each one citing the message range that
justifies it. This is where memories the assistant did not think to save
come from, and where duplicates and stale entries are tidied. The engine
prompt is fixed; a per-agent `COGMEM.md` adds instructions to it.

### What it is not

Retrieval is lexical and deterministic: no embeddings, no vector index, no
full-text engine. A domain, not a chunk, is the unit of retrieval, and the
assistant can see and edit the triggers that drive it. That keeps the
behaviour inspectable (`explain`, the optional routing trace) and cheap, and
it is what the tools and the guidance describe to the model. A host that wants
semantic recall on top can implement its own `Recall` against the same store,
or add a separate retrieval provider beside cogmem.

## How consolidation calls a model

cogmem never calls a model itself and has no dependency on any provider
library. `consolidate.ModelCaller` is a one-method interface the host
implements:

```go
type ModelCaller interface {
    Consolidate(ctx context.Context, systemPrompt, userJSON string) (raw string, model string, err error)
}
```

The worker hands it the engine prompt and a JSON payload and expects back raw
text that parses as the strict consolidation `Output`, together with the name
of the model that produced it. The contract requires the host to walk its own
model chain until it has a usable, parseable reply, so the worker never
records a raw parser error from a flaky model. Everything about which model,
which credentials, which transport, retries, cooldowns and structured-output
options is the host's.

In ClawEh the caller is the agent's summarization chain: the agent's
`summarization_models`, then the global `summarization.models`, then the
agent's primary model, resolved through the same dispatcher and cooldown
tracker as context compaction, requesting a JSON-object response where the
provider supports it. Those clients are spawnllm provider clients, so the call
ultimately flows through spawnllm to the HTTP API or CLI, but that is ClawEh's
choice, not cogmem's.

## Host obligations

- Install a logging backend with `logger.SetBackend` (silent otherwise).
- Provide a `consolidate.ModelCaller` (see above) and, if attachments are
  wanted, an `AttachmentLoader` that enforces the agent's read permissions.
- Call `Session.Observe` after every stored message with the transcript seq,
  and `Session.Recall` before every model call.

Storage is one SQLite file per session, `<workspace>/sessions/<key>.cogmem.db`,
pure Go (`modernc.org/sqlite`), migrated on open with a pre-migration snapshot.

## Origin

Extracted from [ClawEh](https://github.com/PivotLLM/ClawEh), where it was
designed and first shipped.

## License

MIT — see `LICENSE`.
