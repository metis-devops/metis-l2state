package migration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type progressHook func([]byte)

func (h progressHook) Write(p []byte) (int, error) { h(p); return len(p), nil }

// Initial parsed values and exact-byte digests survive replacement/deletion of
// their paths. Compare the entire artifact, then independently read it back.
func TestMigrationInputSnapshot(t *testing.T) {
	for _, action := range []string{"overwrite", "replace", "remove"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t, 3, 0)
			dir := t.TempDir()
			o := f.options(filepath.Join(dir, "before"))
			o.RetainList, o.Alloc = filepath.Join(dir, "retain"), filepath.Join(dir, "alloc")
			o.AuditSummary = writeAudit(t, f, 0)
			must(t, os.WriteFile(o.RetainList, []byte(manual.Hex()+"\n"), 0600))
			must(t, os.WriteFile(o.Alloc, fmt.Appendf(nil, `{"%s":{"balance":"0x05"}}`, eoa.Hex()), 0600))
			before, err := Migrate(t.Context(), o)
			must(t, err)
			want, err := os.ReadFile(o.Output)
			must(t, err)
			o.Output = filepath.Join(dir, "after")
			called := false
			var hookErr error
			o.Progress = progressHook(func(p []byte) {
				if !bytes.Contains(p, []byte("phase=write_state")) {
					return
				}
				called = true
				for _, path := range []string{o.WrappedEtherCode, o.RetainList, o.Alloc, o.AuditSummary, o.OVMEvents} {
					switch action {
					case "remove":
						hookErr = errors.Join(hookErr, os.Remove(path))
					case "replace":
						replacement := path + ".replacement"
						hookErr = errors.Join(hookErr, os.WriteFile(replacement, []byte("changed input"), 0600))
						hookErr = errors.Join(hookErr, os.Rename(replacement, path))
					case "overwrite":
						hookErr = errors.Join(hookErr, os.WriteFile(path, []byte("changed input"), 0600))
					}
				}
			})
			after, err := Migrate(t.Context(), o)
			must(t, hookErr)
			must(t, err)
			if !called {
				t.Fatal("snapshot hook not called")
			}
			got, err := os.ReadFile(o.Output)
			must(t, err)
			_, _, verified := readArtifact(t, o.Output)
			if !bytes.Equal(want, got) || before.Summary != after.Summary || after.Summary != verified {
				t.Fatal("input path change affected captured migration")
			}
		})
	}
}
