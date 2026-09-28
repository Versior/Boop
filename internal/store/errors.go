package store

import (
	"errors"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint failure,
// so callers can turn it into a domain conflict without parsing error strings.
func IsUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
