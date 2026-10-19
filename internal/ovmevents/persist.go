package ovmevents

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

type checkpointFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}
type persistence struct {
	create  func(string) (checkpointFile, error)
	rename  func(string, string) error
	syncDir func(string) error
	remove  func(string) error
}

func diskPersistence() persistence {
	return persistence{
		create: func(dir string) (checkpointFile, error) { return os.CreateTemp(dir, ".ovm-events.partial-*") },
		rename: os.Rename, remove: os.Remove,
		syncDir: func(dir string) error {
			f, err := os.Open(dir)
			if err != nil {
				return err
			}
			return errors.Join(f.Sync(), f.Close())
		},
	}
}

// Save atomically replaces a checkpoint. The caller must hold the output lock.
func Save(path string, d *Document) error { return diskPersistence().save(path, d) }
func (p persistence) save(path string, d *Document) (retErr error) {
	if err := d.Validate(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("checkpoint must be a regular non-symlink file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	f, err := p.create(dir)
	if err != nil {
		return err
	}
	closed, published := false, false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, f.Close())
		}
		if !published {
			retErr = errors.Join(retErr, p.remove(f.Name()))
		}
	}()
	if err := encodeValidated(f, d); err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	closed = true
	if err != nil {
		return err
	}
	if err := p.rename(f.Name(), path); err != nil {
		return err
	}
	published = true
	if err := p.syncDir(dir); err != nil {
		return errors.Join(errors.New("checkpoint published but directory sync failed; retained checkpoint"), err)
	}
	return nil
}
func lockOutput(path string) (string, *flock.Flock, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", nil, err
	}
	path = filepath.Join(parent, filepath.Base(absolute))
	lockPath := path + ".lock"
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() {
			return "", nil, errors.New("checkpoint lock must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	lock := flock.New(lockPath)
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return "", nil, errors.Join(errors.New("checkpoint is locked"), err, lock.Close())
	}
	return path, lock, nil
}
