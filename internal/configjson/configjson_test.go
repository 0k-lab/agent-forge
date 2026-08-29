package configjson

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type decodeFixture struct {
	Value string `json:"value"`
}

func TestDecodeRejectsMalformedUnicodeWithoutReplacingIt(t *testing.T) {
	invalidByte := append([]byte(`{"value":"`), 0xff)
	invalidByte = append(invalidByte, []byte(`"}`)...)
	for name, body := range map[string][]byte{
		"invalid UTF-8":       invalidByte,
		"lone high surrogate": []byte(`{"value":"\ud800"}`),
		"lone low surrogate":  []byte(`{"value":"\udc00"}`),
	} {
		t.Run(name, func(t *testing.T) {
			var value decodeFixture
			if err := Decode(body, &value); !errors.Is(err, ErrSyntax) {
				t.Fatalf("Decode error = %v, value = %q", err, value.Value)
			}
		})
	}
}

func TestDecodeAcceptsWellFormedUnicodeAndEscapedLiterals(t *testing.T) {
	for name, body := range map[string][]byte{
		"surrogate pair":       []byte(`{"value":"\ud83d\ude00"}`),
		"escaped literal":      []byte(`{"value":"\\ud800"}`),
		"escaped replacement":  []byte(`{"value":"\ufffd"}`),
		"literal replacement":  []byte(`{"value":"�"}`),
		"ordinary UTF-8 value": []byte(`{"value":"Джерело"}`),
	} {
		t.Run(name, func(t *testing.T) {
			var value decodeFixture
			if err := Decode(body, &value); err != nil {
				t.Fatalf("Decode error = %v", err)
			}
		})
	}
}

func TestStringRejectsNullButAcceptsOmittedOrEmpty(t *testing.T) {
	type fixture struct {
		Value String `json:"value"`
	}
	var rejected fixture
	if err := Decode([]byte(`{"value":null}`), &rejected); !errors.Is(err, ErrSyntax) {
		t.Fatalf("null error = %v", err)
	}
	for _, body := range [][]byte{[]byte(`{}`), []byte(`{"value":""}`)} {
		var accepted fixture
		if err := Decode(body, &accepted); err != nil || accepted.Value != "" {
			t.Fatalf("body %s: value=%q err=%v", body, accepted.Value, err)
		}
	}
}

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
