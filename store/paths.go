// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"path/filepath"
	"strings"
)

// SanitizeSessionKey converts a session key to a safe filename component, using
// the same rule as memory.sanitizeKey (':' '/' '\' → '_') so a session's
// .cogmem.db sits beside its .archive.db.
func SanitizeSessionKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// SessionDBPath returns the per-session cogmem database path:
// <workspace>/sessions/<sanitized-key>.cogmem.db
func SessionDBPath(workspace, sessionKey string) string {
	return filepath.Join(workspace, "sessions", SanitizeSessionKey(sessionKey)+".cogmem.db")
}
