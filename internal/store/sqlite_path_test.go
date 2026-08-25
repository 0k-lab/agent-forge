package store

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSQLiteDSNCanonicalizesAllowedLocations(t *testing.T) {
	abs := func(path string) string {
		path, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Clean(path)
	}
	fileDSN := func(path, query string) string {
		u := &url.URL{Scheme: "file", Path: abs(path), RawQuery: query}
		return u.String()
	}
	tests := []struct {
		name, input, path, dsn string
		memory                 bool
	}{
		{name: "plain", input: "data/forge.db", path: abs("data/forge.db"), dsn: fileDSN("data/forge.db", "mode=rwc")},
		{name: "plain punctuation", input: "data/literal?#.db", path: abs("data/literal?#.db"), dsn: fileDSN("data/literal?#.db", "mode=rwc")},
		{name: "relative URI", input: "file:data%20base.db?cache=shared", path: abs("data base.db"), dsn: fileDSN("data base.db", "cache=shared&mode=rwc")},
		{name: "absolute URI", input: "file:///var/lib/forge.db?mode=rwc&cache=private", path: "/var/lib/forge.db", dsn: "file:///var/lib/forge.db?cache=private&mode=rwc"},
		{name: "memory", input: ":memory:", dsn: ":memory:", memory: true},
		{name: "memory alias private", input: "file::memory:?cache=private", dsn: "file::memory:?cache=private", memory: true},
		{name: "memory alias shared", input: "file::memory:?cache=shared", dsn: "file::memory:?cache=shared", memory: true},
		{name: "named memory", input: "file:shared%20name?mode=memory&cache=shared", dsn: "file:shared%20name?cache=shared&mode=memory", memory: true},
		{name: "empty memory", input: "file:?mode=memory", dsn: "file:?mode=memory", memory: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSQLiteDSN(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.path != tt.path || got.sqliteDSN != tt.dsn || got.memory != tt.memory {
				t.Fatalf("parseSQLiteDSN() = %#v, want path=%q sqliteDSN=%q memory=%v", got, tt.path, tt.dsn, tt.memory)
			}
		})
	}
}

func TestParseSQLiteDSNRejectsEverythingOutsideAllowlist(t *testing.T) {
	inputs := []string{
		"", "file:", "file::memory:", "file://host/data.db", "file://user:secret@host/data.db",
		"file:bad%zz.db", "file:bad%00.db", "file:data.db#fragment", "file:data.db?",
		"file:data.db?mode=ro", "file:data.db?mode=rw", "file:data.db?mode=unsafe", "file:data.db?mode=",
		"file:data.db?mode=rwc&mode=rwc", "file:data.db?cache=", "file:data.db?cache=invalid",
		"file:data.db?cache=shared&", "file:data.db?&cache=shared",
		"file:data.db?cache=private&cache=shared", "file:data.db?unknown=value", "file:data.db?token",
		"file:data.db?vfs=unix-dotfile", "file:data.db?modeof=other", "file:data.db?nolock=1",
		"file:data.db?immutable=1", "file::memory:?cache=shared&unknown=value",
		"file:name?mode=memory&mode=memory", "file:name?mode=memory&cache=shared&cache=shared",
	}
	for _, input := range inputs {
		t.Run(strings.ReplaceAll(input, "/", "_"), func(t *testing.T) {
			_, err := parseSQLiteDSN(input)
			if !errors.Is(err, ErrInvalidDatabaseLocation) {
				t.Fatalf("parseSQLiteDSN(%q) error = %v, want ErrInvalidDatabaseLocation", input, err)
			}
			if input != "" && err != nil && strings.Contains(err.Error(), input) {
				t.Fatalf("error exposed input %q", input)
			}
		})
	}
}
