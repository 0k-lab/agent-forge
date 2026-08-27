package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIRequiresOnlyConfigAndStrictPublication(t *testing.T) {
	for _, tt := range []struct {
		args  []string
		input string
	}{
		{nil, `{}`},
		{[]string{"-config", "config.json", "extra"}, `{}`},
		{[]string{"-config", "config.json"}, `{"version":1,"unknown":true}`},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(tt.args, strings.NewReader(tt.input), &stdout, &stderr); err == nil || stdout.Len() != 0 || strings.Contains(stderr.String(), tt.input) {
			t.Fatalf("args=%v err=%v stdout=%q stderr=%q", tt.args, err, stdout.String(), stderr.String())
		}
	}
}
