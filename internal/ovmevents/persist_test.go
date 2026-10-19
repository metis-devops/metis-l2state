package ovmevents

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type faultFile struct {
	*os.File
	stage  string
	fault  error
	closed bool
}

func (f *faultFile) Write(p []byte) (int, error) {
	if f.stage == "write" {
		return 0, f.fault
	}
	return f.File.Write(p)
}
func (f *faultFile) Sync() error {
	if f.stage == "sync" {
		return f.fault
	}
	return f.File.Sync()
}
func (f *faultFile) Close() error {
	f.closed = true
	err := f.File.Close()
	if f.stage == "close" {
		return errors.Join(err, f.fault)
	}
	return err
}
func TestPersistenceFailures(t *testing.T) {
	for _, stage := range []string{"write", "sync", "close", "rename", "directory"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "events.json")
			check(t, os.WriteFile(path, []byte("original"), 0600))
			sentinel := errors.New("injected " + stage)
			ops := diskPersistence()
			var file *faultFile
			ops.create = func(dir string) (checkpointFile, error) {
				f, err := os.CreateTemp(dir, ".partial-")
				file = &faultFile{File: f, stage: stage, fault: sentinel}
				return file, err
			}
			if stage == "rename" {
				ops.rename = func(string, string) error { return sentinel }
			}
			if stage == "directory" {
				ops.syncDir = func(string) error { return sentinel }
			}
			if err := ops.save(path, sample()); !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if !file.closed {
				t.Fatal("file not closed")
			}
			data, err := os.ReadFile(path)
			check(t, err)
			if stage == "directory" {
				if string(data) == "original" {
					t.Fatal("published file lost")
				}
			} else if string(data) != "original" {
				t.Fatal("old checkpoint destroyed")
			}
			entries, err := os.ReadDir(dir)
			check(t, err)
			if len(entries) != 1 {
				t.Fatal("partial leaked")
			}
		})
	}
}
