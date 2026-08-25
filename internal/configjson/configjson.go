package configjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const MaxBytes = 1 << 20

var (
	ErrSyntax    = errors.New("invalid config: syntax")
	ErrDuplicate = errors.New("invalid config: duplicate field")
	ErrRead      = errors.New("invalid config: read")
)

func ReadFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrRead
	}
	defer file.Close()
	return read(file)
}

func read(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxBytes+1))
	if err != nil {
		return nil, ErrRead
	}
	if len(data) == 0 || len(data) > MaxBytes {
		return nil, ErrSyntax
	}
	return data, nil
}

func Decode(data []byte, value any) error {
	if len(data) == 0 || len(data) > MaxBytes {
		return ErrSyntax
	}
	if err := rejectDuplicates(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return ErrSyntax
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrSyntax
	}
	return nil
}

func rejectDuplicates(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return ErrSyntax
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return ErrSyntax
				}
				name, ok := key.(string)
				if !ok {
					return ErrSyntax
				}
				if _, ok := seen[name]; ok {
					return ErrDuplicate
				}
				seen[name] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return ErrSyntax
		}
		if _, err := decoder.Token(); err != nil {
			return ErrSyntax
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrSyntax
	}
	return nil
}
