// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// Session.Store opens the store with SessionOptions.StoreOptions, so the
// memory directory and database get the host's modes. The custom modes are
// ones a typical umask (022 or 027) leaves intact, since MkdirAll is subject
// to it.
func TestSession_StorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not supported on windows")
	}
	tests := []struct {
		name       string
		opts       []store.Option
		wantFolder os.FileMode
		wantFile   os.FileMode
	}{
		{"defaults", nil, store.DefaultFolderPermissions, store.DefaultFilePermissions},
		{"custom", []store.Option{store.WithFolderPermissions(0o750), store.WithFilePermissions(0o640)}, 0o750, 0o640},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cogmem")
			s := NewSession(SessionOptions{ID: "alice", Dir: dir, StoreOptions: tt.opts})
			defer s.Close()
			if s.Store() == nil {
				t.Fatal("store did not open")
			}
			for path, want := range map[string]os.FileMode{dir: tt.wantFolder, store.DBPath(dir): tt.wantFile} {
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat: %v", err)
				}
				if got := fi.Mode().Perm(); got != want {
					t.Errorf("%s mode = %#o, want %#o", path, got, want)
				}
			}
		})
	}
}
