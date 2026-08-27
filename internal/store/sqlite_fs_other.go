//go:build !unix

package store

import "io"

func prepareSQLiteFile(sqliteDSN) (func() error, error) {
	return nil, ErrUnsupportedDatabase
}

func acquireSQLiteLock(string) (io.Closer, error) { return nil, ErrUnsupportedDatabase }
