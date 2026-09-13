// cogmem - Cognitive Memory
// License: MIT

package store

import "path/filepath"

// DBFileName is the database file cogmem keeps inside the directory the host
// gives it. The host owns the choice of directory; cogmem owns everything in
// it: the database, its WAL and shared-memory files, and the pre-migration
// snapshots taken on upgrade.
const DBFileName = "cogmem.db"

// DBPath returns the database path for a cogmem directory.
func DBPath(dir string) string { return filepath.Join(dir, DBFileName) }
