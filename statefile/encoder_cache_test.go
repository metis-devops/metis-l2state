package statefile

import (
	"bytes"
	"runtime"
	"testing"
	"weak"

	"github.com/klauspost/compress/zstd"
)

func TestExclusiveSingleIdleSlot(t *testing.T) {
	var cache legacyEncoderCache
	first, err := cache.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("one encoder leased to two owners")
	}
	cache.Release(first)
	cache.Release(second)
	if cache.idle != first {
		t.Fatal("idle cache exceeded or replaced its one slot")
	}
	reused, err := cache.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if reused != first || cache.idle != nil {
		t.Fatal("idle slot not transferred to owner")
	}
	cache.close()
	// Closing the scope does not break an active owner's independent frame.
	if len(reused.EncodeAll([]byte("still owned"), nil)) == 0 {
		t.Fatal("invalid frame")
	}
	cache.Release(reused)
	if cache.idle != nil {
		t.Fatal("late release retained encoder after scope ended")
	}
	cache.close()
}

func cachedHistory(t *testing.T, cache *legacyEncoderCache) weak.Pointer[zstd.Encoder] {
	t.Helper()
	encoder, err := cache.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	_ = encoder.EncodeAll(bytes.Repeat([]byte{1}, 256<<10), nil)
	ref := weak.Make(encoder)
	cache.Release(encoder)
	return ref
}

func TestScopeKeepsScratchUntilRelease(t *testing.T) {
	if legacyFromContext(t.Context()) != nil {
		t.Fatal("standalone caller unexpectedly has a cache")
	}
	ctx, release := legacyWithCache(t.Context())
	cache := legacyFromContext(ctx)
	ref := cachedHistory(t, cache)
	runtime.GC()
	if ref.Value() == nil {
		t.Fatal("GC discarded scratch before independent readback")
	}
	release()
	release()
	runtime.GC()
	if ref.Value() != nil {
		t.Fatal("ended scope retained compression history")
	}
	runtime.KeepAlive(ctx)
}

func TestIndependentScopes(t *testing.T) {
	ctx, release := legacyWithCache(t.Context())
	defer release()
	nested, releaseNested := legacyWithCache(ctx)
	defer releaseNested()
	if legacyFromContext(ctx) == legacyFromContext(nested) {
		t.Fatal("nested operations shared a codec scope")
	}
}
