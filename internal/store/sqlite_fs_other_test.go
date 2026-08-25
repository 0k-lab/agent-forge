//go:build !unix

package store

import (
	"errors"
	"testing"
)

func TestOpenPlatformStorageBoundary(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open("forge.db"); !errors.Is(err, ErrUnsupportedDatabase) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("file Open error = %v, want ErrUnsupportedDatabase", err)
	}
}
