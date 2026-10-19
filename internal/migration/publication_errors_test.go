package migration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/ethdb"
)

// Initial identity validation must propagate database read errors.
type sourceFaultDB struct {
	ethdb.Database
	key      []byte
	fault    error
	closeErr error
	closes   int
}

func (db *sourceFaultDB) Get(key []byte) ([]byte, error) {
	if bytes.Equal(key, db.key) {
		return nil, db.fault
	}
	return db.Database.Get(key)
}

func (*sourceFaultDB) NewIterator([]byte, []byte) ethdb.Iterator {
	panic("identity validation scanned index contents")
}

func (db *sourceFaultDB) Close() error {
	db.closes++
	return errors.Join(db.Database.Close(), db.closeErr)
}

func TestInitialIdentityReadErrors(t *testing.T) {
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	fault := errors.New("injected identity read error")
	db := &sourceFaultDB{Database: s.db, fault: fault}
	s.db = db
	for _, key := range [][]byte{numberKey(0, []byte{'n'})} {
		db.key = key
		if err := s.confirmGenesis(f.genesis.Hash()); !errors.Is(err, fault) {
			t.Fatalf("marker %x: %v", key, err)
		}
	}
	for _, key := range [][]byte{[]byte("LastBlock"), append([]byte{'H'}, s.head.BlockHash[:]...), numberKey(s.head.BlockNumber, []byte{'n'}), numberKey(s.head.BlockNumber, s.head.BlockHash[:])} {
		db.key = key
		if _, _, err := readHead(s.db); !errors.Is(err, fault) {
			t.Fatalf("head %x: %v", key, err)
		}
	}
	db.key = nil
	must(t, s.confirmGenesis(f.genesis.Hash()))
	_, _, err = readHead(s.db)
	must(t, err)
}

func TestSourceCloseError(t *testing.T) {
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	fault := errors.New("injected source close error")
	db := &sourceFaultDB{Database: s.db, closeErr: fault}
	s.db = db
	if err := s.close(); !errors.Is(err, fault) || db.closes != 1 {
		t.Fatalf("source close lost error or closed repeatedly: %v, %d", err, db.closes)
	}
}

func TestPublicationFileCloseFailure(t *testing.T) {
	p, err := newPublication(t.TempDir(), filepath.Join(t.TempDir(), "out"))
	must(t, err)
	must(t, p.file.Close())
	// Both Sync and Close now fail. close must still relinquish the handle so
	// deferred abort can remove the partial without retrying a closed descriptor.
	if err := p.close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("lost sync/close error: %v", err)
	}
	if p.file != nil {
		t.Fatal("failed close retained file handle")
	}
	must(t, p.abort())
	if _, err := os.Stat(p.partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial survived close failure: %v", err)
	}
}

func TestPublicationDestinationRace(t *testing.T) {
	p, err := newPublication(t.TempDir(), filepath.Join(t.TempDir(), "out"))
	must(t, err)
	_, err = p.file.Write([]byte("new"))
	must(t, err)
	must(t, p.close())
	// The destination appears after initial validation, immediately before rename.
	must(t, os.WriteFile(p.final, []byte("existing"), 0600))
	synced := false
	err = p.publish(renameNoReplace, func(string) error { synced = true; return nil })
	if err == nil || p.published || synced {
		t.Fatalf("accepted competing destination: %v", err)
	}
	must(t, p.abort())
	got, err := os.ReadFile(p.final)
	must(t, err)
	if string(got) != "existing" {
		t.Fatal("competing destination modified")
	}
	if _, err := os.Stat(p.partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial survived failed rename: %v", err)
	}
}

func TestMigrationCancellationCleansPublication(t *testing.T) {
	f := newFixture(t, 3, 0)
	dir := t.TempDir()
	o := f.options(filepath.Join(dir, "out"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o.Progress = progressHook(func(p []byte) {
		if bytes.Contains(p, []byte("phase=write_state")) {
			cancel()
		}
	})
	if _, err := Migrate(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("cancellation left output or partial: %v", entries)
	}
	// A fresh invocation can reopen the source and reuse the destination.
	o.Progress = nil
	_, err = Migrate(t.Context(), o)
	must(t, err)
	readArtifact(t, o.Output)
}

func TestPublicationParentIdentity(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	moved := filepath.Join(dir, "original")
	must(t, os.Mkdir(parent, 0700))
	p, err := newPublication(t.TempDir(), filepath.Join(parent, "out"))
	must(t, err)
	must(t, p.close())
	must(t, os.Rename(parent, moved))
	must(t, os.Mkdir(parent, 0700))
	called := false
	err = p.publish(func(string, string) error { called = true; return nil }, func(string) error { called = true; return nil })
	if err == nil || called || p.published {
		t.Fatalf("accepted replaced parent: %v", err)
	}
	// Restore the original directory so abort can clean its partial normally.
	must(t, os.Remove(parent))
	must(t, os.Rename(moved, parent))
	must(t, p.abort())
}
