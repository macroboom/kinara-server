//go:build !nosqlite

package store

// The pure-Go SQLite driver keeps cross-compiling simple (no cgo).
// Build with -tags nosqlite to compile without it (used for quick checks).
import _ "modernc.org/sqlite"
