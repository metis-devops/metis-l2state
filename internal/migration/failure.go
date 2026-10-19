package migration

import (
	"context"
	"sync"
)

// Preserve the original failure before cancelling siblings. Otherwise a later
// partition failure can be hidden by an earlier partition's context.Canceled.
type runFailure struct {
	once   sync.Once
	err    error
	cancel context.CancelFunc
}

func (f *runFailure) record(err error) {
	if err != nil {
		f.once.Do(func() { f.err = err; f.cancel() })
	}
}
