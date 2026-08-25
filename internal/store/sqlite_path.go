package store

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidDatabaseLocation = errors.New("invalid database location")
	ErrInsecureDatabase        = errors.New("insecure database storage")
	ErrUnsupportedDatabase     = errors.New("database storage is unsupported")
	ErrDatabaseOpen            = errors.New("database open failed")
)

type sqliteDSN struct {
	path      string
	sqliteDSN string
	memory    bool
	create    bool
}

func parseSQLiteDSN(input string) (sqliteDSN, error) {
	if input == ":memory:" {
		return sqliteDSN{sqliteDSN: ":memory:", memory: true}, nil
	}
	if input == "" || strings.ContainsRune(input, 0) {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	if !strings.HasPrefix(input, "file:") {
		return canonicalFileLocation(input, nil)
	}
	u, err := url.Parse(input)
	if err != nil || u.Scheme != "file" || u.Host != "" || u.User != nil || u.Fragment != "" || strings.Contains(input, "#") || u.ForceQuery {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	path := u.Path
	if u.Opaque != "" {
		path, err = url.PathUnescape(u.Opaque)
	}
	if err != nil || strings.ContainsRune(path, 0) {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || hasEmptyQueryParameter(u.RawQuery) || !validSQLiteQuery(query) {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	mode, hasMode := query["mode"]
	if path == ":memory:" && !hasMode && len(query) == 1 && query.Has("cache") {
		return canonicalMemoryLocation(path, query), nil
	}
	if path == ":memory:" {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	if hasMode && mode[0] == "memory" {
		return canonicalMemoryLocation(path, query), nil
	}
	if path == "" || hasMode && mode[0] != "rwc" {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	query.Set("mode", "rwc")
	return canonicalFileLocation(path, query)
}

func hasEmptyQueryParameter(rawQuery string) bool {
	for _, parameter := range strings.Split(rawQuery, "&") {
		if rawQuery != "" && parameter == "" {
			return true
		}
	}
	return false
}

func validSQLiteQuery(query url.Values) bool {
	for key, values := range query {
		if (key != "mode" && key != "cache") || len(values) != 1 || values[0] == "" {
			return false
		}
		if key == "mode" && values[0] != "rwc" && values[0] != "memory" {
			return false
		}
		if key == "cache" && values[0] != "private" && values[0] != "shared" {
			return false
		}
	}
	return true
}

func canonicalFileLocation(path string, query url.Values) (sqliteDSN, error) {
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return sqliteDSN{}, ErrInvalidDatabaseLocation
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("mode", "rwc")
	return sqliteDSN{path: path, sqliteDSN: (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String(), create: true}, nil
}

func canonicalMemoryLocation(path string, query url.Values) sqliteDSN {
	return sqliteDSN{sqliteDSN: "file:" + url.PathEscape(path) + "?" + query.Encode(), memory: true}
}
