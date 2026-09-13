// cogmem - Cognitive Memory
// License: MIT

package store

import "time"

// Identifier format (DEC-2, revised): a one-character type prefix plus
// idRandomLen Crockford base32 characters, 6 characters total (e.g. "dK3M9P",
// "hT7QX2"). Crockford base32 omits I/L/O/U so ids are unambiguous to read and
// for an LLM to echo. The store checks uniqueness on insert and retries on the
// (vanishingly rare) clash, so ids stay collision-free while unpredictable.
const (
	idRandomLen    = 5
	idMaxAttempts  = 8
	domainIDPrefix = "d"
	memoryIDPrefix = "h"
	// crockfordAlphabet has 32 symbols; 256 % 32 == 0 so byte%32 is unbiased.
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// SQLite open defaults. journalMode and synchronous are fixed (unlikely to
// change); busyTimeout is the lever, overridable via WithBusyTimeout.
const (
	defaultBusyTimeout = 5 * time.Second
	journalMode        = "WAL"
	synchronousMode    = "NORMAL"
)
