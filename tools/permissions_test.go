// cogmem - Cognitive Memory
// License: MIT

package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// The handlers open the store with Host.StoreOptions, and the export tool
// writes files/ and its document with the same modes. The custom modes are
// ones a typical umask (022 or 027) leaves intact, since MkdirAll is subject
// to it.
func TestPermissions(t *testing.T) {
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
			hs := newHarness(t, func(h *Host) { h.StoreOptions = tt.opts })
			hs.ok(t, "export", nil)
			for path, want := range map[string]os.FileMode{
				hs.dir:                        tt.wantFolder,
				store.DBPath(hs.dir):          tt.wantFile,
				filepath.Join(hs.ws, "files"): tt.wantFolder,
				filepath.Join(hs.ws, "files", exportFilename): tt.wantFile,
			} {
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
