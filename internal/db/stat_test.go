package db

import (
	"database/sql"
	"os"
)

// osStat is a thin alias used by tests so we don't need an extra
// import of "os" in db_test.go on top of everything else. Keeping
// this in its own file avoids cluttering the main test file with
// test-only helpers.
func osStat(name string) (os.FileInfo, error) { return os.Stat(name) }

// openRawSQLite opens a bare sqlite connection without applying the
// schema or migrations. Used by the migration test to fabricate a
// database in the pre-fix shape so Open's in-place upgrade can be
// exercised.
func openRawSQLite(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}
