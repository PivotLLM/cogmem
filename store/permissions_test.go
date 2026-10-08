// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// skipWithoutUnixModes skips on Windows, where a file mode carries only the
// read-only bit.
func skipWithoutUnixModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not supported on windows")
	}
}

// assertMode fails the test unless path exists with permission bits want.
func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %#o, want %#o", filepath.Base(path), got, want)
	}
}

// A fresh Open creates the directory and the database with the configured
// modes, and the -wal and -shm files SQLite creates on a write inherit the
// database's mode. The custom modes are ones a typical umask (022 or 027)
// leaves intact, since MkdirAll is subject to it.
func TestOpenCreatesWithModes(t *testing.T) {
	skipWithoutUnixModes(t)
	tests := []struct {
		name       string
		opts       []Option
		wantFolder os.FileMode
		wantFile   os.FileMode
	}{
		{"defaults", nil, DefaultFolderPermissions, DefaultFilePermissions},
		{"custom", []Option{WithFolderPermissions(0o750), WithFilePermissions(0o640)}, 0o750, 0o640},
		{"zero ignored", []Option{WithFolderPermissions(0), WithFilePermissions(0)}, DefaultFolderPermissions, DefaultFilePermissions},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "mem")
			path := DBPath(dir)
			s, err := Open(path, tt.opts...)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = s.Close() }()
			if _, err = s.CreateDomain(context.Background(), s.DB(), CreateDomainParams{Name: "Proj", Summary: "x"}); err != nil {
				t.Fatalf("create domain: %v", err)
			}
			assertMode(t, dir, tt.wantFolder)
			assertMode(t, path, tt.wantFile)
			assertMode(t, path+"-wal", tt.wantFile)
			assertMode(t, path+"-shm", tt.wantFile)
			if s.FolderPermissions() != tt.wantFolder || s.FilePermissions() != tt.wantFile {
				t.Errorf("store modes = %#o/%#o, want %#o/%#o",
					s.FolderPermissions(), s.FilePermissions(), tt.wantFolder, tt.wantFile)
			}
		})
	}
}

// An existing directory keeps its mode: Open creates directories, it does not
// manage the host's.
func TestOpenLeavesExistingDirectory(t *testing.T) {
	skipWithoutUnixModes(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := Open(DBPath(dir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
	assertMode(t, dir, 0o755)
}

// A database written with looser permissions (the driver's default, or an
// older cogmem) is repaired on Open: the database, its -wal and -shm files and
// its pre-migration snapshots all get the file mode.
func TestOpenRepairsPermissions(t *testing.T) {
	skipWithoutUnixModes(t)
	tests := []struct {
		name string
		opts []Option
		want os.FileMode
	}{
		{"defaults", nil, DefaultFilePermissions},
		{"custom", []Option{WithFilePermissions(0o640)}, 0o640},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := DBPath(t.TempDir())
			s, err := Open(path)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			_ = s.Close()
			// Stand-ins for files left by a crash or an older version. The
			// -wal and -shm are zero-length, which SQLite treats as absent.
			snap := path + ".pre-v5.db"
			for _, f := range []string{path + "-wal", path + "-shm", snap} {
				if err = os.WriteFile(f, nil, 0o600); err != nil {
					t.Fatalf("write %s: %v", f, err)
				}
			}
			other := path + ".notes"
			if err = os.WriteFile(other, nil, 0o600); err != nil {
				t.Fatalf("write %s: %v", other, err)
			}
			for _, f := range []string{path, path + "-wal", path + "-shm", snap, other} {
				if err = os.Chmod(f, 0o644); err != nil {
					t.Fatalf("chmod %s: %v", f, err)
				}
			}

			s, err = Open(path, tt.opts...)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = s.Close() }()
			for _, f := range []string{path, path + "-wal", path + "-shm", snap} {
				assertMode(t, f, tt.want)
			}
			assertMode(t, other, 0o644) // not cogmem's file: untouched
		})
	}
}

// The snapshot taken before a migration gets the file mode, even though the
// database it was taken from was looser.
func TestPreMigrationSnapshotMode(t *testing.T) {
	skipWithoutUnixModes(t)
	tests := []struct {
		name string
		opts []Option
		want os.FileMode
	}{
		{"defaults", nil, DefaultFilePermissions},
		{"custom", []Option{WithFilePermissions(0o640)}, 0o640},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := legacyV5(t, DBFileName)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			s, err := Open(path, tt.opts...)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			_ = s.Close()
			assertMode(t, path+".pre-v5.db", tt.want)
			assertMode(t, path, tt.want)
		})
	}
}

// A zero-length snapshot (the placeholder left by a crash before VACUUM INTO
// wrote it) is not a backup: the next Open takes the snapshot again rather
// than migrating with nothing to fall back on.
func TestEmptyPreMigrationSnapshotIsRetaken(t *testing.T) {
	path := legacyV5(t, DBFileName)
	snap := path + ".pre-v5.db"
	if err := os.WriteFile(snap, nil, 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()

	raw, err := sql.Open("sqlite", snap)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var status string
	if err = raw.QueryRow(`SELECT status FROM memories WHERE id='hREV'`).Scan(&status); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if status != "review" {
		t.Errorf("snapshot status = %q, want the pre-migration 'review'", status)
	}
}

// Snapshot writes its copy with the file mode, replacing a looser file
// already at the destination.
func TestSnapshotMode(t *testing.T) {
	skipWithoutUnixModes(t)
	tests := []struct {
		name string
		opts []Option
		want os.FileMode
	}{
		{"defaults", nil, DefaultFilePermissions},
		{"custom", []Option{WithFilePermissions(0o640)}, 0o640},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.db")
			s, err := Open(src)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			_ = s.Close()
			dst := filepath.Join(dir, "dst.db")
			if err = os.WriteFile(dst, []byte("stale"), 0o600); err != nil {
				t.Fatalf("write stale dst: %v", err)
			}
			if err = os.Chmod(dst, 0o644); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			if err = Snapshot(context.Background(), src, dst, tt.opts...); err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			assertMode(t, dst, tt.want)
			s2, err := Open(dst)
			if err != nil {
				t.Fatalf("open snapshot: %v", err)
			}
			_ = s2.Close()
		})
	}
}

// Migrate passes its options to Open.
func TestMigrateOptions(t *testing.T) {
	skipWithoutUnixModes(t)
	path := legacyV5(t, DBFileName)
	if r := Migrate(filepath.Dir(path), WithFilePermissions(0o640)); r.Err != nil {
		t.Fatalf("migrate: %v", r.Err)
	}
	assertMode(t, path, 0o640)
	assertMode(t, path+".pre-v5.db", 0o640)
}
