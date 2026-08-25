package configjson

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadRejectsEmptyOversizedAndFailedInput(t *testing.T) {
	private := "private-config-value"
	for name, reader := range map[string]readerCase{
		"empty":     {strings.NewReader(""), ErrSyntax},
		"oversized": {strings.NewReader(strings.Repeat(private, MaxBytes/len(private)+2)), ErrSyntax},
		"failure":   {failingReader{}, ErrRead},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := read(reader.reader)
			if !errors.Is(err, reader.want) {
				t.Fatalf("error = %v, want %v", err, reader.want)
			}
			if strings.Contains(err.Error(), private) {
				t.Fatalf("error leaked content: %q", err)
			}
		})
	}

	data, err := read(bytes.NewReader(bytes.Repeat([]byte{'x'}, MaxBytes)))
	if err != nil || len(data) != MaxBytes {
		t.Fatalf("maximum-sized input: len=%d err=%v", len(data), err)
	}
}

type readerCase struct {
	reader io.Reader
	want   error
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("private read failure") }
