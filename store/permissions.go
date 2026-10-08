// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// prepareFiles readies path for opening: it creates the parent directory with
// folderMode when missing, creates the database file with fileMode when
// missing, and gives the database, its -wal and -shm files and its
// pre-migration snapshots fileMode where they have another.
//
// Creating the file here means it never exists with the driver's default mode:
// SQLite treats a zero-length file as a new database, and gives the -wal and
// -shm files it creates the main file's mode.
func prepareFiles(path string, folderMode, fileMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), folderMode); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	if err := createFile(path, fileMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create database file: %w", err)
	}
	files := []string{path, path + "-wal", path + "-shm"}
	snaps, err := snapshotsOf(path)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	for _, f := range append(files, snaps...) {
		if err := fixPermissions(f, fileMode); err != nil {
			return fmt.Errorf("set permissions of %s: %w", f, err)
		}
	}
	return nil
}

// snapshotsOf returns the pre-migration snapshots snapshotBeforeMigration
// wrote for the database at path (<path>.pre-v<N>.db).
func snapshotsOf(path string) ([]string, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		rest, ok := strings.CutPrefix(name, base)
		if !ok || e.IsDir() {
			continue
		}
		// The suffix must be exactly one snapshot suffix: a match starting
		// later is a snapshot of a snapshot, not of this database.
		if loc := snapshotName.FindStringIndex(rest); loc == nil || loc[0] != 0 {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// createFile creates an empty path with mode perm (subject to the umask;
// callers follow with fixPermissions), failing with os.ErrExist when it is
// already there.
func createFile(path string, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // G304: path is the host-configured database or snapshot path; creating it is the purpose.
	if err != nil {
		return err
	}
	return f.Close()
}

// fixPermissions sets path's mode to perm when the file exists with another.
// A missing file is not an error.
func fixPermissions(path string, perm os.FileMode) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().Perm() == perm {
		return nil
	}
	return os.Chmod(path, perm)
}

// vacuumInto writes a consistent copy of db to dst, which must not exist, with
// mode perm. dst is created empty first (VACUUM INTO accepts an empty file) so
// the copy never exists with the driver's default mode; it is removed again if
// the copy fails. A zero-length dst is therefore never a finished copy.
func vacuumInto(ctx context.Context, db *sql.DB, dst string, perm os.FileMode) error {
	if err := createFile(dst, perm); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("VACUUM INTO %s: %w", dst, err)
	}
	if err := fixPermissions(dst, perm); err != nil {
		return fmt.Errorf("set permissions of %s: %w", dst, err)
	}
	return nil
}
