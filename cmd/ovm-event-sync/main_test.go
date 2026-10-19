package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCLI(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"version"}, {"--help"}} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if args[0] == "version" && !json.Valid(out.Bytes()) {
			t.Fatal("invalid result")
		}
	}
	for _, args := range [][]string{{"bad"}, {"version", "bad"}, {"--rpc-url", "x"}, {"--to-block", "-1"}, {"--rpc-url", "x", "--out", "x", "--batch-size", "0"}, {"--rpc-url", "x", "--out", "x", "--poll-interval", "0s"}} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), args, &out, &stderr); err == nil {
			t.Fatal("accepted", args)
		}
		if out.Len() != 0 {
			t.Fatal("error leaked to stdout")
		}
	}
}
