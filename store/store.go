// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DBTX is the subset of *sql.DB / *sql.Tx used by store methods, so the same
// code path runs standalone or inside a transaction (e.g. the consolidation
// worker applies all ops in one tx).
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store owns one .cogmem.db.
type Store struct {
	db   *sql.DB
	path string
}

// Option configures Open (functional options pattern, per dev standards).
type Option func(*openConfig)

type openConfig struct {
	busyTimeout time.Duration
}

// WithBusyTimeout overrides the SQLite busy_timeout (default defaultBusyTimeout).
func WithBusyTimeout(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.busyTimeout = d
		}
	}
}

// Open opens (or creates) a cogmem database at path with WAL mode and runs
// migrations. Pure Go (modernc.org/sqlite), no CGO.
func Open(path string, opts ...Option) (*Store, error) {
	cfg := openConfig{busyTimeout: defaultBusyTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("cogmem: open %s: %w", path, err)
	}
	pragmas := []string{
		"PRAGMA journal_mode=" + journalMode,
		fmt.Sprintf("PRAGMA busy_timeout=%d", cfg.busyTimeout.Milliseconds()),
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=" + synchronousMode,
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(context.Background(), p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("cogmem: %q: %w", p, err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying handle for callers needing direct access (e.g. the
// composer's read path). Treat as read-only outside store methods.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	// What this database was last migrated to. 0 for a database written before
	// the version was recorded, and for one that does not exist yet.
	from, err := s.recordedVersion(ctx)
	if err != nil {
		return fmt.Errorf("cogmem: read schema version: %w", err)
	}
	// Snapshot before touching anything. A no-op for a database that is being
	// created, or one already at the current version, so the cost is paid once
	// per upgrade rather than on every open.
	if from < schemaVersion {
		if err := s.snapshotBeforeMigration(ctx, from); err != nil {
			return fmt.Errorf("cogmem: pre-migration snapshot: %w", err)
		}
	}
	// Legacy rename (hook→memory, kind→type) must run BEFORE the schema DDL, or
	// CREATE TABLE IF NOT EXISTS memories would make a fresh empty table beside
	// the existing data. Idempotent and a no-op on fresh databases.
	if err := s.renameLegacyTables(ctx); err != nil {
		return fmt.Errorf("cogmem: legacy rename: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("cogmem: schema: %w", err)
	}
	// Seed bookkeeping rows if absent. IDs are random (constants.go), so no
	// counters are needed — only the stable-block generation.
	seeds := map[string]string{"stable_rev": "0"}
	for k, v := range seeds {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO meta(key,value) VALUES(?,?)`, k, v); err != nil {
			return fmt.Errorf("cogmem: seed meta %q: %w", k, err)
		}
	}
	if err := s.ensureDomainColumns(ctx); err != nil {
		return fmt.Errorf("cogmem: ensure domain columns: %w", err)
	}
	if err := s.ensureRunColumns(ctx); err != nil {
		return err
	}
	if err := s.ensureMemoryColumns(ctx); err != nil {
		return fmt.Errorf("cogmem: ensure memory columns: %w", err)
	}
	if err := s.normalizeMemoryTypes(ctx); err != nil {
		return fmt.Errorf("cogmem: normalize memory types: %w", err)
	}
	if err := s.normalizeStickyColumn(ctx); err != nil {
		return fmt.Errorf("cogmem: normalize sticky column: %w", err)
	}
	// v6: the review status and the source/priority columns go. Both steps are
	// idempotent and probe rather than trusting the recorded version, because
	// databases written before the version was recorded report 0 regardless of
	// what they actually contain.
	if err := s.retireReviewStatus(ctx); err != nil {
		return fmt.Errorf("cogmem: retire review status: %w", err)
	}
	if err := s.dropLegacyMemoryColumns(ctx); err != nil {
		return fmt.Errorf("cogmem: drop legacy memory columns: %w", err)
	}
	if err := s.dropLegacyDomainColumns(ctx); err != nil {
		return fmt.Errorf("cogmem: drop legacy domain columns: %w", err)
	}
	if err := s.seedGeneralOnce(ctx); err != nil {
		return fmt.Errorf("cogmem: seed general domain: %w", err)
	}
	if err := s.collapseConsolidationState(ctx); err != nil {
		return fmt.Errorf("cogmem: collapse consolidation state: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?,?)`,
		schemaVersion, now()); err != nil {
		return fmt.Errorf("cogmem: record migration: %w", err)
	}
	return nil
}

// recordedVersion returns the highest version in schema_migrations, or 0 when
// the table does not exist yet (a database being created, or one written before
// the version was tracked).
func (s *Store) recordedVersion(ctx context.Context) (int, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&n)
	if err != nil {
		// No such table: nothing has been recorded, which is what 0 means.
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

// snapshotBeforeMigration writes a consistent copy of the database to
// <path>.pre-v<from>.db before a migration runs, so an upgrade is recoverable
// without the operator having prepared for it.
//
// VACUUM INTO rather than a file copy: it takes a read transaction and writes a
// single self-contained file, so it captures anything still sitting in the WAL.
// A plain copy of the .db alone can miss committed data entirely.
//
// No-op when there is nothing to lose (no memories table yet — a database being
// created) and when the snapshot already exists (VACUUM INTO refuses to
// overwrite, which is the behaviour we want: the first snapshot at a given
// version is the one taken before any changes).
func (s *Store) snapshotBeforeMigration(ctx context.Context, from int) error {
	have, err := s.tableExists(ctx, "memories")
	if err != nil || !have {
		return err
	}
	dst := fmt.Sprintf("%s.pre-v%d.db", s.path, from)
	if _, err := os.Stat(dst); err == nil {
		return nil // already snapshotted at this version
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("VACUUM INTO %s: %w", dst, err)
	}
	return nil
}

// retireReviewStatus promotes every memory left in the removed "review" status
// to active. Review meant "written but unconfirmed", and the confirmation it
// waited for never came: those memories were excluded from the prompt, from
// search and from the WebUI, so they were unreachable by anything. Making them
// active is what removing the gate means.
func (s *Store) retireReviewStatus(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE memories SET status='active' WHERE status='review'`)
	return err
}

// dropLegacyMemoryColumns removes memories.source and memories.priority.
//
// Neither drove behaviour. source recorded whether the model believed a memory
// came from the user or was inferred — it gated only the review rule, which is
// gone, and never reached the prompt (the [source: ...] tag renders origin).
// priority was written and returned by the API and read by nothing.
//
// Must run AFTER ensureMemoryColumns, which backfills origin FROM source.
func (s *Store) dropLegacyMemoryColumns(ctx context.Context) error {
	have, err := s.columnSet(ctx, "memories")
	if err != nil {
		return err
	}
	for _, col := range []string{"source", "priority"} {
		if !have[col] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE memories DROP COLUMN `+col); err != nil {
			return fmt.Errorf("drop %s: %w", col, err)
		}
	}
	return nil
}

// collapseConsolidationState (v7) folds the archive-path-keyed watermark rows
// into the single InboxStateKey row. The highest consolidated seq wins: it is
// the point up to which memory already reflects the conversation, and the
// host's one-time inbox backfill starts just past it. Idempotent: a store that
// already has the row is left alone, and a fresh store has nothing to fold.
func (s *Store) collapseConsolidationState(ctx context.Context) error {
	// Databases from before the version was recorded can carry a
	// consolidation_state table without the columns later code writes; add them
	// so the single-row shape below (and SetWatermark) always has a home.
	cols, err := s.columnSet(ctx, "consolidation_state")
	if err != nil {
		return err
	}
	for col, ddl := range map[string]string{
		"last_seen_seq": "INTEGER NOT NULL DEFAULT 0",
		"updated_at":    "INTEGER NOT NULL DEFAULT 0",
	} {
		if cols[col] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE consolidation_state ADD COLUMN `+col+` `+ddl); err != nil {
			return fmt.Errorf("add %s: %w", col, err)
		}
	}
	var have int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM consolidation_state WHERE archive_path = ?`, InboxStateKey).Scan(&have); err != nil {
		return err
	}
	if have > 0 {
		return nil
	}
	var seq, seen sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(consolidated_seq), MAX(last_seen_seq) FROM consolidation_state`).Scan(&seq, &seen); err != nil {
		return err
	}
	if !seq.Valid {
		return nil // nothing was ever consolidated
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO consolidation_state(archive_path, consolidated_seq, last_seen_seq, meaningful_count, last_run_at, updated_at)
		VALUES(?,?,?,0,NULL,?)`, InboxStateKey, seq.Int64, seen.Int64, now()); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM consolidation_state WHERE archive_path != ?`, InboxStateKey)
	return err
}

// dropLegacyDomainColumns (v8) removes domains.agent_id and domains.session_key.
// A store is one memory for one agent, in a directory the host names, so the
// columns only repeated what the file's location already said. Idempotent:
// probes the column set rather than trusting the recorded version.
func (s *Store) dropLegacyDomainColumns(ctx context.Context) error {
	have, err := s.columnSet(ctx, "domains")
	if err != nil {
		return err
	}
	for _, col := range []string{"agent_id", "session_key"} {
		if !have[col] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE domains DROP COLUMN `+col); err != nil {
			return fmt.Errorf("drop %s: %w", col, err)
		}
	}
	return nil
}

// DedupeActiveMemories retires active memories that exactly duplicate an earlier
// active memory in the same domain (same domain_id + text). The earliest by
// created_at (then id) is kept; the rest are retired with reason "duplicate".
// Returns how many were retired. Idempotent: a no-op once there are no dups.
func (s *Store) DedupeActiveMemories(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id FROM memories m
		WHERE m.status = ?
		  AND EXISTS (
		    SELECT 1 FROM memories e
		    WHERE e.domain_id = m.domain_id AND e.text = m.text AND e.status = ?
		      AND (e.created_at < m.created_at OR (e.created_at = m.created_at AND e.id < m.id))
		  )`, string(StatusActive), string(StatusActive))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, string(StatusRetired), now())
	for _, id := range ids {
		args = append(args, id)
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE memories SET status=?, retire_reason='duplicate', updated_at=? WHERE id IN (`+
				placeholders(len(ids))+`)`, args...); err != nil {
			return err
		}
		return bumpStableRev(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// PurgeStats reports how many rows a purge removed (or would remove on a dry run).
type PurgeStats struct {
	Memories int64
	Domains  int64
}

// PurgeNonActive hard-deletes everything that is not current active memory: every
// domain whose status is not active (and all of its memories, regardless of their
// status), and every non-active memory in the surviving active domains. Only
// active memories in active domains remain. When apply is false it counts what
// would be removed without deleting. Returns the counts either way.
func (s *Store) PurgeNonActive(ctx context.Context, apply bool) (PurgeStats, error) {
	var st PurgeStats
	// Memories removed: non-active, or belonging to a non-active domain.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memories
		WHERE status != ? OR domain_id IN (SELECT id FROM domains WHERE status != ?)`,
		string(StatusActive), string(StatusActive)).Scan(&st.Memories); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM domains WHERE status != ?`, string(StatusActive)).Scan(&st.Domains); err != nil {
		return st, err
	}
	if !apply || (st.Memories == 0 && st.Domains == 0) {
		return st, nil
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		// Delete memories first (FK: memories reference domains). This clears every
		// non-active memory plus all memories under soon-to-be-deleted domains.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM memories
			WHERE status != ? OR domain_id IN (SELECT id FROM domains WHERE status != ?)`,
			string(StatusActive), string(StatusActive)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM domains WHERE status != ?`, string(StatusActive)); err != nil {
			return err
		}
		// Pending digest / always-on content may have changed; force a rebuild.
		return bumpStableRev(ctx, tx)
	})
	return st, err
}

// Vacuum reclaims disk space after deletions. Must run outside a transaction.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// Snapshot writes a clean, consistent copy of the cogmem DB at srcPath to
// dstPath using SQLite "VACUUM INTO" (captures committed WAL data, no partial
// pages). dstPath must not already exist. Used to give a spawned sub-agent its
// own private copy of the primary's memory, deleted when the sub-agent finishes.
func Snapshot(ctx context.Context, srcPath, dstPath string) error {
	_ = os.Remove(dstPath) // VACUUM INTO requires the target not exist
	db, err := sql.Open("sqlite", "file:"+srcPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("cogmem snapshot: open source: %w", err)
	}
	defer func() { _ = db.Close() }()
	// dstPath is workspace-derived (no user quotes); escape single quotes defensively.
	target := strings.ReplaceAll(dstPath, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+target+"'"); err != nil {
		return fmt.Errorf("cogmem snapshot: vacuum into %s: %w", dstPath, err)
	}
	return nil
}

// normalizeMemoryTypes folds retired memory type values into the current set:
// project_state/workflow/lesson → fact. Idempotent.
func (s *Store) normalizeMemoryTypes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE memories SET type='fact' WHERE type IN ('project_state','workflow','lesson')`)
	return err
}

// normalizeStickyColumn migrates the legacy domain "type" column to a sticky
// priority int: the old always-on "general" → "1" (sticky); every other legacy
// string value (project/workflow/repo/empty) → "0" (not sticky). Numeric values
// are left as-is. Idempotent.
func (s *Store) normalizeStickyColumn(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE domains SET type='1' WHERE type='general'`); err != nil {
		return err
	}
	// Any remaining non-numeric value (CAST→0) that isn't already "0" → "0".
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET type='0' WHERE CAST(type AS INTEGER)=0 AND type<>'0'`)
	return err
}

// renameLegacyTables migrates a pre-rename database (hooks→memories, kind→type,
// supersedes_hook_id→supersedes_memory_id, memory_events.hook_id→memory_id) in
// place. Every step is guarded so it is idempotent and a no-op on fresh DBs.
func (s *Store) renameLegacyTables(ctx context.Context) error {
	hooksExists, err := s.tableExists(ctx, "hooks")
	if err != nil {
		return err
	}
	memExists, err := s.tableExists(ctx, "memories")
	if err != nil {
		return err
	}
	if hooksExists && !memExists {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE hooks RENAME TO memories`); err != nil {
			return err
		}
		// Indexes follow the renamed table but keep their idx_hooks_* names; drop
		// them so the schema DDL can recreate idx_memories_* without duplication.
		for _, idx := range []string{"idx_hooks_domain", "idx_hooks_status"} {
			if _, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+idx); err != nil {
				return err
			}
		}
	}
	renames := []struct{ table, old, new string }{
		{"memories", "kind", "type"},
		{"memories", "supersedes_hook_id", "supersedes_memory_id"},
		{"memory_events", "hook_id", "memory_id"},
	}
	for _, r := range renames {
		cols, err := s.columnSet(ctx, r.table)
		if err != nil {
			return err
		}
		if cols[r.old] && !cols[r.new] {
			if _, err := s.db.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, r.table, r.old, r.new)); err != nil {
				return err
			}
		}
	}
	return nil
}

// tableExists reports whether a table of the given name exists.
func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var x string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ensureDomainColumns adds columns introduced after a database was first created.
// CREATE TABLE IF NOT EXISTS does not add columns to an existing table, so each
// additive column gets an idempotent ALTER guarded by a table_info check.
func (s *Store) ensureDomainColumns(ctx context.Context) error {
	have, err := s.columnSet(ctx, "domains")
	if err != nil {
		return err
	}
	if !have["triggers"] {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE domains ADD COLUMN triggers TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !have["keyword_triggers"] {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE domains ADD COLUMN keyword_triggers TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// ensureRunColumns adds the note column to a pre-existing consolidation_runs
// table and moves the notes already stranded in `error` across.
//
// Notes describe a run that SUCCEEDED (an auto-repair, say) and used to be
// written into `error` for want of anywhere else, which made the memory page
// render a success in red. Adding the column alone would leave every historical
// note where it is, so the red text would persist until the next consolidation
// run — the complaint that prompted this.
//
// The backfill is deliberately narrow: only rows that are BOTH status='ok' AND
// prefixed 'auto-repaired:'. That pair is unambiguous — it is the exact string
// the worker wrote, on runs it recorded as successful — so no genuine failure
// can be swept up by it. Anything else in `error` is left alone.
//
// Idempotent: the ALTER is guarded, and the UPDATE matches nothing once run.
func (s *Store) ensureRunColumns(ctx context.Context) error {
	have, err := s.columnSet(ctx, "consolidation_runs")
	if err != nil {
		return err
	}
	if have["note"] {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE consolidation_runs ADD COLUMN note TEXT`); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE consolidation_runs
		   SET note = error, error = NULL
		 WHERE status = 'ok' AND error LIKE 'auto-repaired:%'`)
	return err
}

// ensureMemoryColumns adds the origin column to a pre-existing memories table and
// backfills it from the legacy source value: tool_write→chat, migration→user,
// user_explicit/assistant_inferred→consolidation. Idempotent; a no-op once the
// column exists.
func (s *Store) ensureMemoryColumns(ctx context.Context) error {
	have, err := s.columnSet(ctx, "memories")
	if err != nil {
		return err
	}
	if !have["file_ref"] {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE memories ADD COLUMN file_ref TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if have["origin"] {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE memories ADD COLUMN origin TEXT NOT NULL DEFAULT 'chat'`); err != nil {
		return err
	}
	// Backfill from source (rows default to 'chat' from the ALTER above). A
	// database already migrated past v6 has no source column, and nothing to
	// backfill from.
	if !have["source"] {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET origin='consolidation' WHERE source IN ('user_explicit','assistant_inferred')`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET origin='user' WHERE source='migration'`); err != nil {
		return err
	}
	return nil
}

// columnSet returns the set of column names on a table.
func (s *Store) columnSet(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		set[name] = true
	}
	return set, rows.Err()
}

// seedGeneralOnce creates a sticky "General" domain on a brand-new DB only. A
// meta flag (seeded_general) records that the one-time seed has happened, so a
// General the user later deletes is never re-created. Existing (migrated) DBs
// that already have domains are not seeded.
func (s *Store) seedGeneralOnce(ctx context.Context) error {
	seeded, err := getMetaInt(ctx, s.db, "seeded_general")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if seeded != 0 {
		return nil // already seeded once (or explicitly skipped) — respect deletions
	}
	// Mark seeded first so the one-time seed never repeats, even across upgrades.
	if err := setMetaInt(ctx, s.db, "seeded_general", 1); err != nil {
		return err
	}
	// Only seed a truly fresh DB; a migrated DB already has its domains.
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM domains`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err = s.CreateDomain(ctx, s.db, CreateDomainParams{
		Name:    "General",
		Sticky:  true,
		Status:  StatusActive,
		Summary: "Global rules, preferences, and standing facts.",
	})
	return err
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on error or panic.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// --- meta / counters / stable_rev ---

func getMetaInt(ctx context.Context, q DBTX, key string) (int64, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func setMetaInt(ctx context.Context, q DBTX, key string, val int64) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, strconv.FormatInt(val, 10))
	return err
}

// StableRev returns the current stable-block generation. The composer caches its
// stable block and rebuilds only when this changes.
func (s *Store) StableRev(ctx context.Context) (int64, error) {
	return getMetaInt(ctx, s.db, "stable_rev")
}

// bumpStableRev increments the generation counter; call within the same tx as
// any change to always-on content (always-on domains, the index, or pending).
func bumpStableRev(ctx context.Context, q DBTX) error {
	_, err := q.ExecContext(ctx,
		`UPDATE meta SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT) WHERE key='stable_rev'`)
	return err
}

// genID returns a candidate id: prefix + idRandomLen Crockford base32 chars.
func genID(prefix string) (string, error) {
	buf := make([]byte, idRandomLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, idRandomLen)
	for i, b := range buf {
		out[i] = crockfordAlphabet[int(b)%32] // 256%32==0 → unbiased
	}
	return prefix + string(out), nil
}

// freshID generates a unique id for the given table (an internal constant, never
// user input), retrying on the rare collision.
func freshID(ctx context.Context, q DBTX, prefix, table string) (string, error) {
	for i := 0; i < idMaxAttempts; i++ {
		id, err := genID(prefix)
		if err != nil {
			return "", err
		}
		var x int
		err = q.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE id=?", id).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("cogmem: id allocation failed after retries")
}

// now returns the current unix time in seconds. Centralized so tests can reason
// about timestamps; cogmem rarely needs sub-second precision.
func now() int64 { return time.Now().Unix() }

func derefOr0(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func unixPtr(t *int64) *time.Time {
	if t == nil {
		return nil
	}
	tt := time.Unix(*t, 0)
	return &tt
}

func timeUnix(t int64) time.Time { return time.Unix(t, 0) }

// placeholders returns "?,?,..." with n entries for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
