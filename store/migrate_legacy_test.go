// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// execAll runs each statement against a raw database at path and closes it.
func execAll(t *testing.T, path string, stmts []string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	for _, q := range stmts {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("seed (%.60s...): %v", q, err)
		}
	}
}

// legacyVersioned builds a database in the shape written at schema version 6
// or 7, with domains and memories that mirror legacyV5's: General plus a topic
// domain with triggers and state, an archived domain, memories of every status
// and origin with evidence, an audit event, a run record, and the per-version
// consolidation state (archive-path rows at v6, the single inbox row plus an
// inbox table at v7). Both still carry domains.agent_id/session_key, which v8
// drops.
func legacyVersioned(t *testing.T, version int, name string) string {
	t.Helper()
	if version != 6 && version != 7 {
		t.Fatalf("legacyVersioned: unsupported version %d", version)
	}
	path := filepath.Join(t.TempDir(), name)
	stmts := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(5, 1), (6, 2)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta(key,value) VALUES('stable_rev','12'), ('seeded_general','1')`,
		`CREATE TABLE domains (
		   id TEXT PRIMARY KEY, agent_id TEXT NOT NULL DEFAULT '', session_key TEXT NOT NULL DEFAULT '',
		   type TEXT NOT NULL DEFAULT '0', name TEXT NOT NULL, status TEXT NOT NULL,
		   version INTEGER NOT NULL DEFAULT 1, summary TEXT NOT NULL DEFAULT '',
		   state_json TEXT NOT NULL DEFAULT '{}', schema_name TEXT NOT NULL DEFAULT 'domain',
		   schema_version INTEGER NOT NULL DEFAULT 1, last_active_at INTEGER,
		   triggers TEXT NOT NULL DEFAULT '', keyword_triggers TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, archived_at INTEGER)`,
		`INSERT INTO domains(id,agent_id,session_key,type,name,status,version,summary,state_json,triggers,keyword_triggers,created_at,updated_at,archived_at) VALUES
		   ('dGEN','alice','main','1','General','active',1,'Global rules, preferences, and standing facts.','{}','','',1,1,NULL),
		   ('dPROJ','alice','main','0','BioTech','active',4,'the report','{"blockers":["waiting on data"],"next_actions":["send draft"]}','google_gmail','biotech report',2,2,NULL),
		   ('dARCH','alice','main','0','Old','archived',2,'finished','{}','','',3,3,4)`,
		`CREATE TABLE memories (
		   id TEXT PRIMARY KEY, domain_id TEXT NOT NULL REFERENCES domains(id), type TEXT NOT NULL,
		   text TEXT NOT NULL, status TEXT NOT NULL, confidence REAL NOT NULL,
		   origin TEXT NOT NULL DEFAULT 'chat', source_session TEXT,
		   source_seq_start INTEGER, source_seq_end INTEGER, supersedes_memory_id TEXT,
		   retire_reason TEXT, file_ref TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO memories(id,domain_id,type,text,status,confidence,origin,source_session,source_seq_start,source_seq_end,supersedes_memory_id,retire_reason,file_ref,created_at,updated_at) VALUES
		   ('hFACT','dPROJ','fact','the report targets Q3','active',0.9,'consolidation','chan:1',10,12,'hRET',NULL,'',5,5),
		   ('hEVT','dPROJ','event','results published Sep 4','active',0.9,'chat',NULL,NULL,NULL,NULL,NULL,'',6,6),
		   ('hRET','dPROJ','fact','the report targets Q2','retired',0.9,'consolidation','chan:1',1,3,NULL,'superseded','',4,5),
		   ('hGEN','dGEN','rule','Reply in English.','active',1.0,'user',NULL,NULL,NULL,NULL,NULL,'files/voice.md',1,1),
		   ('hOLD','dARCH','fact','shipped in 2025','active',0.8,'chat',NULL,NULL,NULL,NULL,NULL,'',3,3)`,
		`CREATE TABLE memory_events (
		   id TEXT PRIMARY KEY, event_type TEXT NOT NULL, domain_id TEXT, memory_id TEXT,
		   old_json TEXT, new_json TEXT, reason TEXT NOT NULL DEFAULT '',
		   evidence_json TEXT NOT NULL DEFAULT '{}', actor TEXT NOT NULL, model TEXT,
		   prompt_hash TEXT, created_at INTEGER NOT NULL)`,
		`INSERT INTO memory_events(id,event_type,domain_id,memory_id,new_json,reason,evidence_json,actor,model,created_at)
		   VALUES('evt1','create','dPROJ','hFACT','{"text":"the report targets Q3"}','','{"seq_start":10,"seq_end":12}','sleep_cycle','m1',5)`,
		`CREATE TABLE consolidation_runs (
		   id TEXT PRIMARY KEY, trigger TEXT NOT NULL, model TEXT NOT NULL, seq_start INTEGER,
		   seq_end INTEGER, input_tokens INTEGER, output_tokens INTEGER, status TEXT NOT NULL,
		   ops_applied INTEGER NOT NULL DEFAULT 0, error TEXT, note TEXT, prompt_hash TEXT,
		   started_at INTEGER NOT NULL, finished_at INTEGER)`,
		`INSERT INTO consolidation_runs(id,trigger,model,seq_start,seq_end,status,ops_applied,started_at,finished_at)
		   VALUES('run1','idle','m1',10,12,'ok',2,5,6)`,
		`CREATE TABLE worker_leases (
		   name TEXT PRIMARY KEY, owner TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE consolidation_state (
		   archive_path TEXT PRIMARY KEY, consolidated_seq INTEGER NOT NULL DEFAULT 0,
		   last_seen_seq INTEGER NOT NULL DEFAULT 0, meaningful_count INTEGER NOT NULL DEFAULT 0,
		   last_run_at INTEGER, updated_at INTEGER NOT NULL)`,
	}
	switch version {
	case 6:
		stmts = append(stmts,
			`INSERT INTO consolidation_state VALUES
			   ('/old/sessions/a.archive.db', 40, 41, 3, NULL, 1),
			   ('/new/sessions/a.archive.db', 120, 125, 7, 5, 2)`)
	case 7:
		stmts = append(stmts,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(7, 3)`,
			`INSERT INTO consolidation_state VALUES('inbox', 120, 125, 2, 5, 2)`,
			`CREATE TABLE inbox (seq INTEGER PRIMARY KEY, role TEXT NOT NULL, text TEXT NOT NULL, created_at INTEGER NOT NULL)`,
			`INSERT INTO inbox VALUES(126,'user','what is the status?',7), (127,'assistant','on track',7)`,
			`INSERT INTO meta(key,value) VALUES('inbox_backfilled','1')`)
	}
	execAll(t, path, stmts)
	return path
}

// assertVersionedDataSurvived checks everything legacyVersioned seeded is
// intact after the migration to the current schema.
func assertVersionedDataSurvived(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	db := s.DB()

	if v, err := s.recordedVersion(ctx); err != nil || v != schemaVersion {
		t.Errorf("recorded version = %d (err %v), want %d", v, err, schemaVersion)
	}
	cols, _ := s.columnSet(ctx, "domains")
	for _, c := range []string{"agent_id", "session_key"} {
		if cols[c] {
			t.Errorf("domains.%s survived the v8 migration", c)
		}
	}

	doms, err := s.ListDomains(ctx, db)
	if err != nil || len(doms) != 3 {
		t.Fatalf("domains = %d err=%v, want the 3 seeded (General not re-seeded)", len(doms), err)
	}
	gen, _ := s.GetDomain(ctx, db, "dGEN", false)
	if !gen.Sticky() || gen.Name != "General" || gen.Status != StatusActive {
		t.Errorf("General = %+v", gen)
	}
	proj, _ := s.GetDomain(ctx, db, "dPROJ", true)
	if proj.Sticky() || proj.Name != "BioTech" || proj.Version != 4 || proj.Summary != "the report" ||
		proj.Triggers != "google_gmail" || proj.KeywordTriggers != "biotech report" {
		t.Errorf("BioTech = %+v", proj)
	}
	if len(proj.State.Blockers) != 1 || proj.State.Blockers[0] != "waiting on data" ||
		len(proj.State.NextActions) != 1 || proj.State.NextActions[0] != "send draft" {
		t.Errorf("BioTech state = %+v", proj.State)
	}
	if len(proj.Memories) != 2 {
		t.Errorf("BioTech active memories = %d, want 2 (fact + event)", len(proj.Memories))
	}
	arch, _ := s.GetDomain(ctx, db, "dARCH", false)
	if arch.Status != StatusArchived || arch.ArchivedAt == nil || arch.ArchivedAt.Unix() != 4 {
		t.Errorf("Old = %+v", arch)
	}

	fact, err := s.GetMemory(ctx, db, "hFACT")
	if err != nil {
		t.Fatalf("hFACT: %v", err)
	}
	if fact.Type != TypeFact || fact.Status != StatusActive || fact.Origin != OriginConsolidation ||
		fact.Confidence != 0.9 || fact.Text != "the report targets Q3" ||
		fact.SourceSession == nil || *fact.SourceSession != "chan:1" ||
		fact.SourceSeqStart == nil || *fact.SourceSeqStart != 10 ||
		fact.SourceSeqEnd == nil || *fact.SourceSeqEnd != 12 ||
		fact.SupersedesMemoryID == nil || *fact.SupersedesMemoryID != "hRET" {
		t.Errorf("hFACT = %+v", fact)
	}
	ret, _ := s.GetMemory(ctx, db, "hRET")
	if ret.Status != StatusRetired || ret.RetireReason == nil || *ret.RetireReason != "superseded" {
		t.Errorf("hRET = %+v", ret)
	}
	evt, _ := s.GetMemory(ctx, db, "hEVT")
	if evt.Type != TypeEvent || evt.Origin != OriginChat {
		t.Errorf("hEVT = %+v", evt)
	}
	rule, _ := s.GetMemory(ctx, db, "hGEN")
	if rule.Type != TypeRule || rule.Origin != OriginUser || rule.FileRef != "files/voice.md" || rule.Confidence != 1 {
		t.Errorf("hGEN = %+v", rule)
	}
	if old, err := s.GetMemory(ctx, db, "hOLD"); err != nil || old.DomainID != "dARCH" {
		t.Errorf("hOLD = %+v err=%v", old, err)
	}

	events, err := s.ListEvents(ctx, db)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %d err=%v, want 1", len(events), err)
	}
	if e := events[0]; e.ID != "evt1" || e.MemoryID != "hFACT" || e.DomainID != "dPROJ" ||
		e.Evidence != `{"seq_start":10,"seq_end":12}` || e.Actor != "sleep_cycle" || e.Model != "m1" {
		t.Errorf("event = %+v", e)
	}
	run, ok, err := s.LastRun(ctx, db)
	if err != nil || !ok || run.ID != "run1" || run.SeqStart != 10 || run.SeqEnd != 12 || run.OpsApplied != 2 || run.FinishedAt == nil {
		t.Errorf("run = %+v ok=%v err=%v", run, ok, err)
	}
	if rev, _ := s.StableRev(ctx); rev != 12 {
		t.Errorf("stable_rev = %d, want the seeded 12 (migration must not reset it)", rev)
	}
	st, _ := s.GetState(ctx, db, InboxStateKey)
	if st.ConsolidatedSeq != 120 || st.LastSeenSeq != 125 {
		t.Errorf("inbox state = %+v, want consolidated 120 / last seen 125", st)
	}
	var rows int
	_ = db.QueryRow(`SELECT COUNT(*) FROM consolidation_state`).Scan(&rows)
	if rows != 1 {
		t.Errorf("consolidation_state rows = %d, want 1", rows)
	}
}

// A v6 database (post column drop, pre inbox) migrates to the current schema
// with every domain and memory intact, its archive-keyed watermarks collapsed
// into the inbox row, and a pre-v6 snapshot beside it.
func TestMigrationV6ToCurrentKeepsData(t *testing.T) {
	path := legacyVersioned(t, 6, "v6.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open v6: %v", err)
	}
	defer func() { _ = s.Close() }()
	assertVersionedDataSurvived(t, s)

	ctx := context.Background()
	if n, err := s.InboxCount(ctx, s.DB()); err != nil || n != 0 {
		t.Errorf("inbox after v6 migration = %d err=%v, want an empty table", n, err)
	}
	if done, _ := s.InboxBackfilled(ctx); done {
		t.Error("v6 migration must leave the backfill flag unset")
	}
	if _, err := os.Stat(path + ".pre-v6.db"); err != nil {
		t.Errorf("pre-v6 snapshot missing: %v", err)
	}
	if _, err := os.Stat(path + ".pre-v7.db"); err == nil {
		t.Error("a pre-v7 snapshot was written; only the version found on open is snapshotted")
	}
}

// A v7 database (inbox present, owner columns still on domains) migrates to
// the current schema with its data, its inbox rows and its watermark intact,
// and a pre-v7 snapshot beside it.
func TestMigrationV7ToCurrentKeepsData(t *testing.T) {
	path := legacyVersioned(t, 7, "v7.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open v7: %v", err)
	}
	defer func() { _ = s.Close() }()
	assertVersionedDataSurvived(t, s)

	ctx := context.Background()
	rows, err := s.InboxRange(ctx, s.DB(), 0, 1000)
	if err != nil || len(rows) != 2 || rows[0].Seq != 126 || rows[1].Text != "on track" {
		t.Errorf("inbox rows = %+v err=%v, want the two seeded", rows, err)
	}
	if done, _ := s.InboxBackfilled(ctx); !done {
		t.Error("v7 migration lost the backfill flag")
	}
	if _, err := os.Stat(path + ".pre-v7.db"); err != nil {
		t.Errorf("pre-v7 snapshot missing: %v", err)
	}
}

// legacyHooks builds a database from before the hook→memory rename and before
// the schema version was recorded: a hooks table with kind and
// supersedes_hook_id, memory_events keyed by hook_id, the idx_hooks_* indexes,
// domains typed by string with no trigger columns, and memories carrying
// source (but no origin) and priority.
func legacyHooks(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	execAll(t, path, []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta(key,value) VALUES('stable_rev','3')`,
		`CREATE TABLE domains (
		   id TEXT PRIMARY KEY, agent_id TEXT NOT NULL DEFAULT '', session_key TEXT NOT NULL DEFAULT '',
		   type TEXT NOT NULL, name TEXT NOT NULL, status TEXT NOT NULL,
		   version INTEGER NOT NULL DEFAULT 1, summary TEXT NOT NULL DEFAULT '',
		   state_json TEXT NOT NULL DEFAULT '{}', schema_name TEXT NOT NULL DEFAULT 'domain',
		   schema_version INTEGER NOT NULL DEFAULT 1, last_active_at INTEGER,
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, archived_at INTEGER)`,
		`INSERT INTO domains(id,type,name,status,created_at,updated_at) VALUES
		   ('dGEN','general','General','active',1,1),
		   ('dPROJ','project','Website','active',2,2)`,
		`CREATE TABLE hooks (
		   id TEXT PRIMARY KEY, domain_id TEXT NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL,
		   status TEXT NOT NULL, confidence REAL NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
		   source TEXT NOT NULL, source_session TEXT, source_seq_start INTEGER, source_seq_end INTEGER,
		   supersedes_hook_id TEXT, retire_reason TEXT,
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE INDEX idx_hooks_domain ON hooks(domain_id, status)`,
		`CREATE INDEX idx_hooks_status ON hooks(status)`,
		`INSERT INTO hooks(id,domain_id,kind,text,status,confidence,priority,source,source_seq_start,source_seq_end,supersedes_hook_id,retire_reason,created_at,updated_at) VALUES
		   ('hOLD','dPROJ','fact','old wording','retired',0.8,0,'tool_write',NULL,NULL,NULL,'superseded',1,2),
		   ('hNEW','dPROJ','fact','new wording','active',0.9,2,'user_explicit',5,6,'hOLD',NULL,2,2),
		   ('hMIG','dGEN','lesson','a lesson learned','active',0.9,0,'migration',NULL,NULL,NULL,NULL,1,1),
		   ('hINF','dGEN','rule','an inferred rule','active',0.7,0,'assistant_inferred',NULL,NULL,NULL,NULL,1,1),
		   ('hUNK','dGEN','workflow','a workflow note','active',0.7,0,'something_else',NULL,NULL,NULL,NULL,1,1)`,
		`CREATE TABLE memory_events (
		   id TEXT PRIMARY KEY, event_type TEXT NOT NULL, domain_id TEXT, hook_id TEXT,
		   old_json TEXT, new_json TEXT, reason TEXT NOT NULL DEFAULT '',
		   evidence_json TEXT NOT NULL DEFAULT '{}', actor TEXT NOT NULL, model TEXT,
		   prompt_hash TEXT, created_at INTEGER NOT NULL)`,
		`INSERT INTO memory_events(id,event_type,domain_id,hook_id,new_json,actor,created_at)
		   VALUES('e1','create','dPROJ','hNEW','{"text":"new wording"}','sleep_cycle',2)`,
	})
	return path
}

// Opening a pre-rename database renames hooks→memories and the three legacy
// columns, drops the hooks indexes so the schema can recreate the memories
// ones, adds every column introduced since, backfills origin from source, and
// keeps every row.
func TestMigrationRenamesLegacyHooksTables(t *testing.T) {
	path := legacyHooks(t, "hooks.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-rename database: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	db := s.DB()

	if have, _ := s.tableExists(ctx, "hooks"); have {
		t.Error("hooks table still exists after the rename")
	}
	if have, _ := s.tableExists(ctx, "memories"); !have {
		t.Fatal("memories table missing after the rename")
	}
	mcols, _ := s.columnSet(ctx, "memories")
	for _, want := range []string{"type", "supersedes_memory_id", "origin", "file_ref"} {
		if !mcols[want] {
			t.Errorf("memories.%s missing after migration", want)
		}
	}
	for _, gone := range []string{"kind", "supersedes_hook_id", "source", "priority"} {
		if mcols[gone] {
			t.Errorf("memories.%s survived migration", gone)
		}
	}
	ecols, _ := s.columnSet(ctx, "memory_events")
	if !ecols["memory_id"] || ecols["hook_id"] {
		t.Errorf("memory_events columns = %v, want memory_id and no hook_id", ecols)
	}
	dcols, _ := s.columnSet(ctx, "domains")
	if !dcols["triggers"] || !dcols["keyword_triggers"] {
		t.Errorf("domains trigger columns missing: %v", dcols)
	}
	if dcols["agent_id"] || dcols["session_key"] {
		t.Errorf("domains owner columns survived: %v", dcols)
	}

	// The renamed table's indexes went with it and were recreated under the
	// memories names; the old names are gone.
	idx := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND name LIKE 'idx_%'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		idx[n] = true
	}
	_ = rows.Close()
	for _, want := range []string{"idx_memories_domain", "idx_memories_status"} {
		if !idx[want] {
			t.Errorf("index %s missing; have %v", want, idx)
		}
	}
	for _, gone := range []string{"idx_hooks_domain", "idx_hooks_status"} {
		if idx[gone] {
			t.Errorf("legacy index %s survived", gone)
		}
	}

	// Domains: the legacy type strings became sticky flags, nothing re-seeded.
	doms, _ := s.ListDomains(ctx, db)
	if len(doms) != 2 {
		t.Fatalf("domains = %d, want 2", len(doms))
	}
	if g, _ := s.GetDomain(ctx, db, "dGEN", false); !g.Sticky() || g.Triggers != "" {
		t.Errorf("General = %+v, want sticky with empty triggers", g)
	}
	if p, _ := s.GetDomain(ctx, db, "dPROJ", false); p.Sticky() {
		t.Errorf("'project' domain became sticky")
	}

	// Memories: every row, with the renamed columns readable and origin
	// backfilled from source (tool_write→chat, migration→user,
	// user_explicit/assistant_inferred→consolidation, anything else→chat),
	// and retired types folded (lesson/workflow→fact).
	for _, tc := range []struct {
		id     string
		typ    MemoryType
		status Status
		origin Origin
	}{
		{"hOLD", TypeFact, StatusRetired, OriginChat},
		{"hNEW", TypeFact, StatusActive, OriginConsolidation},
		{"hMIG", TypeFact, StatusActive, OriginUser},
		{"hINF", TypeRule, StatusActive, OriginConsolidation},
		{"hUNK", TypeFact, StatusActive, OriginChat},
	} {
		m, err := s.GetMemory(ctx, db, tc.id)
		if err != nil {
			t.Errorf("%s lost in migration: %v", tc.id, err)
			continue
		}
		if m.Type != tc.typ || m.Status != tc.status || m.Origin != tc.origin || m.FileRef != "" {
			t.Errorf("%s = type %q status %q origin %q file %q, want %q/%q/%q/''",
				tc.id, m.Type, m.Status, m.Origin, m.FileRef, tc.typ, tc.status, tc.origin)
		}
	}
	nw, _ := s.GetMemory(ctx, db, "hNEW")
	if nw.SupersedesMemoryID == nil || *nw.SupersedesMemoryID != "hOLD" {
		t.Errorf("supersedes link lost in the column rename: %v", nw.SupersedesMemoryID)
	}
	if nw.SourceSeqStart == nil || *nw.SourceSeqStart != 5 || nw.SourceSeqEnd == nil || *nw.SourceSeqEnd != 6 {
		t.Errorf("evidence lost: %v..%v", nw.SourceSeqStart, nw.SourceSeqEnd)
	}
	if old, _ := s.GetMemory(ctx, db, "hOLD"); old.RetireReason == nil || *old.RetireReason != "superseded" {
		t.Errorf("retire reason lost: %+v", old)
	}
	if prompt, _ := s.ListPromptMemories(ctx, db, "dPROJ"); len(prompt) != 1 || prompt[0].ID != "hNEW" {
		t.Errorf("prompt memories for dPROJ = %+v, want just hNEW", prompt)
	}

	events, err := s.ListEvents(ctx, db)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %d err=%v, want 1", len(events), err)
	}
	if e := events[0]; e.MemoryID != "hNEW" || e.DomainID != "dPROJ" || e.NewJSON != `{"text":"new wording"}` {
		t.Errorf("event after hook_id rename = %+v", e)
	}
	if v, _ := s.recordedVersion(ctx); v != schemaVersion {
		t.Errorf("recorded version = %d, want %d", v, schemaVersion)
	}
	if rev, _ := s.StableRev(ctx); rev != 3 {
		t.Errorf("stable_rev = %d, want the seeded 3", rev)
	}
	// The whole thing is idempotent: a second open is a no-op.
	_ = s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if m, err := s2.GetMemory(ctx, s2.DB(), "hNEW"); err != nil || m.Origin != OriginConsolidation {
		t.Errorf("after reopen hNEW = %+v err=%v", m, err)
	}
}

// A memories table with source but no origin gets origin added and backfilled
// by the documented mapping (store.go ensureMemoryColumns), then loses source.
func TestMigrationBackfillsOriginFromSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "origin.cogmem.db")
	execAll(t, path, []string{
		`CREATE TABLE domains (
		   id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT '0', name TEXT NOT NULL, status TEXT NOT NULL,
		   version INTEGER NOT NULL DEFAULT 1, summary TEXT NOT NULL DEFAULT '',
		   state_json TEXT NOT NULL DEFAULT '{}', schema_name TEXT NOT NULL DEFAULT 'domain',
		   schema_version INTEGER NOT NULL DEFAULT 1, last_active_at INTEGER,
		   triggers TEXT NOT NULL DEFAULT '', keyword_triggers TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, archived_at INTEGER)`,
		`INSERT INTO domains(id,name,status,created_at,updated_at) VALUES('dONE','One','active',1,1)`,
		`CREATE TABLE memories (
		   id TEXT PRIMARY KEY, domain_id TEXT NOT NULL, type TEXT NOT NULL, text TEXT NOT NULL,
		   status TEXT NOT NULL, confidence REAL NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
		   source TEXT NOT NULL, source_session TEXT, source_seq_start INTEGER, source_seq_end INTEGER,
		   supersedes_memory_id TEXT, retire_reason TEXT, file_ref TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO memories(id,domain_id,type,text,status,confidence,source,created_at,updated_at) VALUES
		   ('h1','dONE','fact','tool','active',0.9,'tool_write',1,1),
		   ('h2','dONE','fact','migrated','active',0.9,'migration',1,1),
		   ('h3','dONE','fact','explicit','active',0.9,'user_explicit',1,1),
		   ('h4','dONE','fact','inferred','active',0.9,'assistant_inferred',1,1),
		   ('h5','dONE','fact','unknown source','active',0.9,'',1,1)`,
	})
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	for id, want := range map[string]Origin{
		"h1": OriginChat,          // tool_write
		"h2": OriginUser,          // migration
		"h3": OriginConsolidation, // user_explicit
		"h4": OriginConsolidation, // assistant_inferred
		"h5": OriginChat,          // anything else: the column default
	} {
		m, err := s.GetMemory(ctx, s.DB(), id)
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if m.Origin != want {
			t.Errorf("%s origin = %q, want %q", id, m.Origin, want)
		}
	}
	cols, _ := s.columnSet(ctx, "memories")
	if cols["source"] || cols["priority"] || !cols["origin"] {
		t.Errorf("columns after migration = %v, want origin present and source/priority gone", cols)
	}
}

// A domains table from before triggers existed gets both trigger columns added,
// empty, and the domain is then fully usable — including setting triggers.
func TestMigrationAddsTriggerColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "triggers.cogmem.db")
	execAll(t, path, []string{
		`CREATE TABLE domains (
		   id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT '0', name TEXT NOT NULL, status TEXT NOT NULL,
		   version INTEGER NOT NULL DEFAULT 1, summary TEXT NOT NULL DEFAULT '',
		   state_json TEXT NOT NULL DEFAULT '{}', schema_name TEXT NOT NULL DEFAULT 'domain',
		   schema_version INTEGER NOT NULL DEFAULT 1, last_active_at INTEGER,
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, archived_at INTEGER)`,
		`INSERT INTO domains(id,name,status,summary,created_at,updated_at) VALUES('dOLD','Mail','active','pre-trigger',1,1)`,
		`CREATE TABLE memories (
		   id TEXT PRIMARY KEY, domain_id TEXT NOT NULL, type TEXT NOT NULL, text TEXT NOT NULL,
		   status TEXT NOT NULL, confidence REAL NOT NULL, origin TEXT NOT NULL DEFAULT 'chat',
		   source_session TEXT, source_seq_start INTEGER, source_seq_end INTEGER,
		   supersedes_memory_id TEXT, retire_reason TEXT, file_ref TEXT NOT NULL DEFAULT '',
		   created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	})
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	cols, _ := s.columnSet(ctx, "domains")
	if !cols["triggers"] || !cols["keyword_triggers"] {
		t.Fatalf("trigger columns not added: %v", cols)
	}
	d, err := s.GetDomain(ctx, s.DB(), "dOLD", false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if d.Triggers != "" || d.KeywordTriggers != "" || d.Summary != "pre-trigger" || len(d.TriggerTokens()) != 0 {
		t.Errorf("migrated domain = %+v, want empty triggers and the old summary", d)
	}
	trig, kw := "Gmail, *Mail*", "Morning Routine"
	if err := s.UpdateDomain(ctx, s.DB(), d.ID, UpdateDomainParams{Triggers: &trig, KeywordTriggers: &kw}); err != nil {
		t.Fatalf("update triggers on a migrated domain: %v", err)
	}
	d, _ = s.GetDomain(ctx, s.DB(), d.ID, false)
	if d.Triggers != "gmail,mail" || d.KeywordTriggers != "morning routine" {
		t.Errorf("after update: triggers=%q keywords=%q", d.Triggers, d.KeywordTriggers)
	}
	if _, ok := d.MatchTrigger("google_gmail_send"); !ok {
		t.Error("trigger set on a migrated domain does not match")
	}
	// The domain count is what was seeded: no General added to a migrated store.
	doms, _ := s.ListDomains(ctx, s.DB())
	if len(doms) != 1 {
		t.Errorf("domains = %d, want 1", len(doms))
	}
	if _, err := s.GeneralDomain(ctx, s.DB()); !errors.Is(err, ErrNotFound) {
		t.Errorf("General err = %v, want ErrNotFound on a migrated store", err)
	}
}
