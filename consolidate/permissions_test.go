// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PivotLLM/cogmem/store"
)

// The debug dump directory and files are created with the store's configured
// modes unless worker options override them. The custom modes are ones a
// typical umask (022 or 027) leaves intact, since MkdirAll and WriteFile are
// subject to it.
func TestDebugDumpPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not supported on windows")
	}
	storeCustom := []store.Option{store.WithFolderPermissions(0o750), store.WithFilePermissions(0o640)}
	tests := []struct {
		name       string
		storeOpts  []store.Option
		opts       []Option
		wantFolder os.FileMode
		wantFile   os.FileMode
	}{
		{"defaults", nil, nil, store.DefaultFolderPermissions, store.DefaultFilePermissions},
		{"store modes", storeCustom, nil, 0o750, 0o640},
		{"worker overrides", storeCustom, []Option{WithFolderPermissions(0o710), WithFilePermissions(0o400)}, 0o710, 0o400},
		{"zero ignored", storeCustom, []Option{WithFolderPermissions(0), WithFilePermissions(0)}, 0o750, 0o640},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(t.TempDir(), "test.cogmem.db"), tt.storeOpts...)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			domainID, memoryID := seedDomain(t, s)
			seedInbox(t, s, sampleMessages())
			dir := filepath.Join(t.TempDir(), "dump")

			raw := supersedeOutput(domainID, memoryID, "Run gofmt and tests.")
			opts := append([]Option{WithDebugDump(dir)}, tt.opts...)
			w := NewWorker(s, &fakeModel{raw: raw}, opts...)
			if _, err = w.RunOnce(context.Background(), params()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat dump dir: %v", err)
			}
			if got := fi.Mode().Perm(); got != tt.wantFolder {
				t.Errorf("dump dir mode = %#o, want %#o", got, tt.wantFolder)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("dump files = %d (err %v), want 1", len(entries), err)
			}
			fi, err = os.Stat(filepath.Join(dir, entries[0].Name()))
			if err != nil {
				t.Fatalf("stat dump: %v", err)
			}
			if got := fi.Mode().Perm(); got != tt.wantFile {
				t.Errorf("dump file mode = %#o, want %#o", got, tt.wantFile)
			}
		})
	}
}
