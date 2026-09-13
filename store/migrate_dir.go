// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"path/filepath"
)

// DBSuffix is the filename suffix of a cogmem database.
const DBSuffix = ".cogmem.db"

// SchemaVersion is the version Open migrates a database to.
func SchemaVersion() int { return schemaVersion }

// MigrationResult reports what opening one store did. From is the version the
// database was at beforehand (0 when it predates version tracking), To the
// version it is at afterwards.
type MigrationResult struct {
	Path string
	From int
	To   int
	Err  error
}

// Migrated reports whether this store actually changed version.
func (r MigrationResult) Migrated() bool { return r.Err == nil && r.To > r.From }

// MigrateDir opens every cogmem database in dir so any pending schema migration
// runs now, and returns one result per store.
//
// Without this, migration is lazy: a store is upgraded whenever its session
// next happens to be opened — by the agent handling a message, or by someone
// clicking that agent in the WebUI. That spreads a schema change across hours
// of ordinary use with no moment an operator can point at and call it done, and
// a store belonging to an agent nobody talks to that day stays on the old
// schema indefinitely. Doing it at agent load makes the upgrade a single
// observable event, and surfaces a database that cannot be migrated at startup
// rather than mid-conversation.
//
// A failure on one store does not stop the others: the point is to learn about
// all of them at once.
func MigrateDir(dir string) []MigrationResult {
	paths, err := filepath.Glob(filepath.Join(dir, "*"+DBSuffix))
	if err != nil || len(paths) == 0 {
		return nil
	}
	out := make([]MigrationResult, 0, len(paths))
	for _, p := range paths {
		res := MigrationResult{Path: p, From: peekVersion(p)}
		s, err := Open(p)
		if err != nil {
			res.Err = err
			out = append(out, res)
			continue
		}
		if v, verr := s.recordedVersion(context.Background()); verr == nil {
			res.To = v
		}
		_ = s.Close()
		out = append(out, res)
	}
	return out
}

// peekVersion reads a database's recorded schema version WITHOUT migrating it,
// so a caller can report what changed. Any failure reads as 0, which is also
// what a database written before the version was tracked reports.
func peekVersion(path string) int {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0
	}
	defer func() { _ = db.Close() }()
	var n sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&n); err != nil {
		return 0
	}
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}
