package migration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PublicationDurabilityError means the final file exists but syncing its parent
// failed. It must be retained; operators decide whether to reuse it after verify.
type PublicationDurabilityError struct {
	Path string
	Err  error
}

func (e *PublicationDurabilityError) Error() string {
	return fmt.Sprintf("published %s but parent sync failed: %v", e.Path, e.Err)
}
func (e *PublicationDurabilityError) Unwrap() error { return e.Err }

type publication struct {
	file                   *os.File
	partial, final, parent string
	parentInfo             os.FileInfo
	published              bool
}

func newPublication(sourcePath, output string) (*publication, error) {
	sourcePath, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return nil, err
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return nil, err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(parent, filepath.Base(output))
	rel, err := filepath.Rel(sourcePath, final)
	if err != nil {
		return nil, err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return nil, errors.New("output must be outside source")
	}
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, errors.New("output already exists")
		}
		return nil, err
	}
	info, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(parent, "."+filepath.Base(output)+".partial-")
	if err != nil {
		return nil, err
	}
	return &publication{file: f, partial: f.Name(), final: final, parent: parent, parentInfo: info}, nil
}
func (p *publication) abort() error {
	var err error
	if p.file != nil {
		err = p.file.Close()
		p.file = nil
	}
	if !p.published {
		removeErr := os.Remove(p.partial)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}
func (p *publication) close() error {
	if p.file == nil {
		return nil
	}
	err := p.file.Sync()
	err = errors.Join(err, p.file.Close())
	p.file = nil
	return err
}
func syncDir(path string) (retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	return f.Sync()
}
func (p *publication) publish(rename func(string, string) error, syncParent func(string) error) error {
	info, err := os.Stat(p.parent)
	if err != nil {
		return err
	}
	if !os.SameFile(p.parentInfo, info) {
		return errors.New("output parent changed")
	}
	if err := rename(p.partial, p.final); err != nil {
		return err
	}
	p.published = true
	if err := syncParent(p.parent); err != nil {
		return &PublicationDurabilityError{p.final, err}
	}
	return nil
}
