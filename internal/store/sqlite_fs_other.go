//go:build !unix

package store

func prepareSQLiteFile(sqliteDSN) (func() error, error) {
	return nil, ErrUnsupportedDatabase
}
