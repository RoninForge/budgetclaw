package db

import "os"

// osStat is a thin alias used by tests so we don't need an extra
// import of "os" in db_test.go on top of everything else. Keeping
// this in its own file avoids cluttering the main test file with
// test-only helpers.
func osStat(name string) (os.FileInfo, error) { return os.Stat(name) }
