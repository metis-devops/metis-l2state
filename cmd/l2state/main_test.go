package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"help"}, {"migrate", "-h"}, {"dump", "-h"}} {
		var out, errout bytes.Buffer
		if err := run(t.Context(), args, &out, &errout); err != nil {
			t.Fatal(err)
		}
		if args[0] == "version" && !json.Valid(out.Bytes()) {
			t.Fatal("version is not JSON")
		}
	}
	for _, args := range [][]string{{"export"}, {"import"}, {"verify"}, {"prune"}, {"migrate", "--scheme", "hash"}, {"migrate", "--source-ancient", "x"}, {"migrate", "--ovm-state-witness", "x"}, {"migrate", "--temp-db", "memory"}, {"migrate", "--migrate-ovm-eth"}, {"migrate", "unexpected"}, {"migrate"}} {
		var out, errout bytes.Buffer
		if err := run(t.Context(), args, &out, &errout); err == nil {
			t.Fatalf("accepted retired command/flag: %v", args)
		}
		if strings.TrimSpace(out.String()) != "" {
			t.Fatal("error leaked to stdout")
		}
	}
}

func TestDumpArguments(t *testing.T) {
	for _, flag := range []string{"ovm-events", "wrapped-ether-code", "ovm-erc20-retain-list", "ovm-genesis-alloc", "ovm-audit-summary", "source-ancient", "scheme", "temp-db"} {
		var out, stderr bytes.Buffer
		err := run(t.Context(), []string{"dump", "--" + flag, "value"}, &out, &stderr)
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") || out.Len() != 0 {
			t.Fatalf("accepted dump flag %s or leaked stdout: %v", flag, err)
		}
	}
	for _, args := range [][]string{{"dump"}, {"dump", "--source-chaindata", "missing"}, {"dump", "--out", "file"}, {"dump", "unexpected"}} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), args, &out, &stderr); err == nil || out.Len() != 0 {
			t.Fatalf("accepted arguments: %v", args)
		}
	}
	var out, stderr bytes.Buffer
	if err := run(t.Context(), []string{"dump", "--help"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source-chaindata", "out", "workers", "cache-mb", "handles", "compression", "quiet"} {
		if !strings.Contains(stderr.String(), "-"+name) {
			t.Fatalf("missing dump flag %s", name)
		}
	}
	if strings.Contains(stderr.String(), "ovm-") {
		t.Fatal("dump advertises OVM flags")
	}
}

func TestCodeCacheFlag(t *testing.T) {
	for _, command := range []string{"migrate", "dump"} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), []string{command, "--help"}, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		expected := "memory"
		if command == "dump" {
			expected = "none"
		}
		if !strings.Contains(stderr.String(), "-code-cache") || !strings.Contains(stderr.String(), "(default \""+expected+"\")") {
			t.Fatal("missing code-cache default")
		}
		for _, value := range []string{"invalid", "MEMORY", "auto"} {
			args := []string{command, "--source-chaindata", "missing", "--out", "unused", "--code-cache", value}
			if command == "migrate" {
				args = append(args, "--wrapped-ether-code", "missing", "--ovm-events", "missing")
			}
			out.Reset()
			stderr.Reset()
			if err := run(t.Context(), args, &out, &stderr); err == nil || !strings.Contains(err.Error(), "code-cache must be memory or none") || out.Len() != 0 {
				t.Fatalf("wrong invalid-cache rejection: %v", err)
			}
		}
	}
}
