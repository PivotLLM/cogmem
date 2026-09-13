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

Consolidation (`consolidate`) drains the store's inbox by watermark, on a
message count, after idle time and nightly, through a `ModelCaller` the host
provides. `Guidance()` returns the prompt text that teaches an assistant how
the memory works; hosts render it only for agents that have memory.

## Host obligations

- Install a logging backend with `logger.SetBackend` (silent otherwise).
- Provide a `consolidate.ModelCaller` and, if attachments are wanted, an
  `AttachmentLoader` that enforces the agent's read permissions.
- Call `Session.Observe` after every stored message with the transcript seq,
  and `Session.Recall` before every model call.

Storage is one SQLite file per session, `<workspace>/sessions/<key>.cogmem.db`,
pure Go (`modernc.org/sqlite`), migrated on open with a pre-migration snapshot.

## Origin

Extracted from [ClawEh](https://github.com/PivotLLM/ClawEh), where it was
designed and first shipped.

## License

MIT — see `LICENSE`.
