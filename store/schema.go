// cogmem - Cognitive Memory
// License: MIT

package store

// schemaVersion is the current migration version. Bump when DDL changes, and
// add the corresponding step to migrateFrom in store.go — the recorded version
// drives which steps run, so a bump without a step silently does nothing.
//
// 6: dropped memories.source and memories.priority, and retired the "review"
//
//	status. See docs/cogmem-redesign-plan.md.
//
// 7: added the inbox table. Consolidation reads the store's own copy of the
// conversation instead of the session archive, so the store depends on nothing
// outside itself; consolidation_state collapses to the single InboxStateKey row.
//
// 8: dropped domains.agent_id and domains.session_key. A store is one memory
// for one agent, in a directory the host owns, so neither column said anything
// the store's location did not.
const schemaVersion = 8

// schema is the full DDL for a .cogmem.db. All statements are idempotent so
// migrate() can run it on every open. No FTS, no vector columns.
const schema = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);

-- Single-row-per-key bookkeeping: stable_rev (cache-invalidation generation),
-- next_domain_seq / next_hook_seq (monotonic id counters).
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS domains (
  id             TEXT PRIMARY KEY,
  type           TEXT NOT NULL,
  name           TEXT NOT NULL,
  status         TEXT NOT NULL,
  version        INTEGER NOT NULL,
  summary        TEXT NOT NULL DEFAULT '',
  state_json     TEXT NOT NULL DEFAULT '{}',
  schema_name    TEXT NOT NULL DEFAULT 'domain',
  schema_version INTEGER NOT NULL DEFAULT 1,
  last_active_at INTEGER,
  triggers       TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  archived_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_domains_status ON domains(status);

CREATE TABLE IF NOT EXISTS memories (
  id                 TEXT PRIMARY KEY,
  domain_id          TEXT NOT NULL REFERENCES domains(id),
  type               TEXT NOT NULL,
  text               TEXT NOT NULL,
  status             TEXT NOT NULL,
  confidence         REAL NOT NULL,
  origin             TEXT NOT NULL DEFAULT 'chat',
  source_session     TEXT,
  source_seq_start   INTEGER,
  source_seq_end     INTEGER,
  supersedes_memory_id TEXT,
  retire_reason      TEXT,
  -- Optional pointer to a markdown file whose full contents are injected into
  -- the prompt whenever this memory is rendered. Path is agent-relative and is
  -- resolved (and permission-checked) by the caller, not here.
  file_ref           TEXT NOT NULL DEFAULT '',
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_domain ON memories(domain_id, status);
CREATE INDEX IF NOT EXISTS idx_memories_status ON memories(status);

CREATE TABLE IF NOT EXISTS consolidation_state (
  archive_path     TEXT PRIMARY KEY,
  consolidated_seq INTEGER NOT NULL DEFAULT 0,
  last_seen_seq    INTEGER NOT NULL DEFAULT 0,
  meaningful_count INTEGER NOT NULL DEFAULT 0,
  last_run_at      INTEGER,
  updated_at       INTEGER NOT NULL
);

-- The store's own copy of conversation messages not yet consolidated. Written
-- by the host as each message is spoken (AppendInbox), drained by the
-- consolidation worker once a run has covered them. seq is the host's
-- transcript number, shared with the evidence ranges memories cite.
CREATE TABLE IF NOT EXISTS inbox (
  seq        INTEGER PRIMARY KEY,
  role       TEXT NOT NULL,
  text       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS memory_events (
  id          TEXT PRIMARY KEY,
  event_type  TEXT NOT NULL,
  domain_id   TEXT,
  memory_id     TEXT,
  old_json    TEXT,
  new_json    TEXT,
  reason      TEXT NOT NULL DEFAULT '',
  evidence_json TEXT NOT NULL DEFAULT '{}',
  actor       TEXT NOT NULL,
  model       TEXT,
  prompt_hash TEXT,
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_created ON memory_events(created_at);

CREATE TABLE IF NOT EXISTS worker_leases (
  name       TEXT PRIMARY KEY,
  owner      TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS consolidation_runs (
  id            TEXT PRIMARY KEY,
  trigger       TEXT NOT NULL,
  model         TEXT NOT NULL,
  seq_start     INTEGER,
  seq_end       INTEGER,
  input_tokens  INTEGER,
  output_tokens INTEGER,
  status        TEXT NOT NULL,
  ops_applied   INTEGER NOT NULL DEFAULT 0,
  error         TEXT,
  note          TEXT,
  prompt_hash   TEXT,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_runs_started ON consolidation_runs(started_at);
`
