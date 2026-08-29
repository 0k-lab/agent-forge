package configjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

const MaxBytes = 1 << 20

var (
	ErrSyntax    = errors.New("invalid config: syntax")
	ErrDuplicate = errors.New("invalid config: duplicate field")
	ErrRead      = errors.New("invalid config: read")
)

type String string

func (value *String) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return ErrSyntax
	}
	var decoded string
	if err := json.Unmarshal(data, &decoded); err != nil {
		return ErrSyntax
	}
	*value = String(decoded)
	return nil
}

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
	if len(data) == 0 || len(data) > MaxBytes || !validUnicodeJSON(data) {
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

func validUnicodeJSON(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	inString := false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString {
				continue
			}
			i++
			if i >= len(data) {
				return false
			}
			if data[i] != 'u' {
				continue
			}
			if i+4 >= len(data) {
				return false
			}
			first, ok := hex4(data[i+1 : i+5])
			if !ok {
				return false
			}
			i += 4
			switch {
			case first >= 0xd800 && first <= 0xdbff:
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return false
				}
				second, ok := hex4(data[i+3 : i+7])
				if !ok || second < 0xdc00 || second > 0xdfff {
					return false
				}
				i += 6
			case first >= 0xdc00 && first <= 0xdfff:
				return false
			}
		}
	}
	return !inString
}

func hex4(data []byte) (uint16, bool) {
	var value uint16
	for _, char := range data {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
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
