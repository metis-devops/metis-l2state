// Historical encoder-cache oracle. Production codecs own their encoders
// directly; these helpers retain the existing reuse and byte-parity tests.
package statefile

import (
	"context"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// legacyEncoderCache holds at most one idle encoder. Each acquired encoder has one owner;
// concurrent callers allocate separate encoders rather than waiting for a lease.
// A nil legacyEncoderCache disables reuse (the standalone statefile API's default).
type legacyEncoderCache struct {
	mu     sync.Mutex
	idle   *zstd.Encoder
	closed bool
}

type contextKey struct{}

// legacyWithCache creates an operation-local cache. Release it on success and failure.
// Release is idempotent and does not invalidate encoders still owned by callers.
func legacyWithCache(ctx context.Context) (context.Context, func()) {
	cache := new(legacyEncoderCache)
	return context.WithValue(ctx, contextKey{}, cache), cache.close
}

// legacyFromContext returns the optional operation-local cache, or nil.
func legacyFromContext(ctx context.Context) *legacyEncoderCache {
	cache, _ := ctx.Value(contextKey{}).(*legacyEncoderCache)
	return cache
}

// Acquire returns an exclusively owned encoder with the unchanged v1 options.
func (c *legacyEncoderCache) Acquire() (*zstd.Encoder, error) {
	if c != nil {
		c.mu.Lock()
		encoder := c.idle
		c.idle = nil
		c.mu.Unlock()
		if encoder != nil {
			return encoder, nil
		}
	}
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true), zstd.WithEncoderLevel(zstd.SpeedDefault))
}

// Release relinquishes an encoder after its synchronous EncodeAll calls finish.
// EncodeAll resets each frame; Close permits reuse but does not free its history.
func (c *legacyEncoderCache) Release(encoder *zstd.Encoder) {
	if encoder == nil {
		return
	}
	_ = encoder.Close()
	if c != nil {
		c.mu.Lock()
		if !c.closed && c.idle == nil {
			c.idle = encoder
		}
		c.mu.Unlock()
	}
}

func (c *legacyEncoderCache) close() {
	c.mu.Lock()
	c.closed = true
	c.idle = nil
	c.mu.Unlock()
}
