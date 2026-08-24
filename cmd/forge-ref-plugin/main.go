package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type request struct {
	Version string `json:"version"`
	Input   string `json:"input"`
}
type response struct {
	Version string `json:"version"`
	Result  string `json:"result"`
}

func main() {
	var r request
	if err := json.NewDecoder(bufio.NewReader(os.Stdin)).Decode(&r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if r.Version != "v1" {
		fmt.Fprintln(os.Stderr, "unsupported version")
		os.Exit(2)
	}
	if len(r.Input) > 65536 {
		fmt.Fprintln(os.Stderr, "input too large")
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(response{Version: "v1", Result: "FORGE: " + strings.ToUpper(r.Input)})
}
