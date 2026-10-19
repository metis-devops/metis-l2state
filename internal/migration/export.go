package migration

import (
	"context"
	"errors"

	"github.com/metis-devops/metis-l2state/statefile"
)

// exportSession owns the destination lifecycle and the open source. Both
// commands finalize Writer, sync/close output, close source, then publish.
type exportSession struct {
	source      *source
	publication *publication
	writer      *statefile.Writer
}

func (e *exportSession) closeSource() error {
	if e.source == nil {
		return nil
	}
	s := e.source
	e.source = nil // Close failures must not trigger a second close.
	return s.close()
}
func (e *exportSession) abort() error {
	if e.writer != nil {
		e.writer.Abort()
	}
	var err error
	if e.publication != nil {
		err = e.publication.abort()
	}
	return errors.Join(err, e.closeSource())
}
func (e *exportSession) finish(ctx context.Context, summary statefile.Summary) error {
	if err := e.writer.Close(summary); err != nil {
		return err
	}
	if err := e.publication.close(); err != nil {
		return err
	}
	if err := e.closeSource(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.publication.publish(renameNoReplace, syncDir)
}
