package statefile

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"weak"

	"github.com/klauspost/compress/zstd"
)

func referenceEncoder(t testing.TB) *zstd.Encoder {
	t.Helper()
	// Independent construction freezes the original v1 options, bypassing reuse.
	e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEncoderReusePreservesBytes(t *testing.T) {
	var cache legacyEncoderCache
	random := rand.New(rand.NewPCG(11, 17))
	for _, size := range []int{0, 1, 1024, 128<<10 - 1, 128 << 10, 128<<10 + 1, blockSize} {
		for pattern := range 3 {
			data := make([]byte, size)
			for i := range data {
				switch pattern {
				case 1:
					data[i] = byte(i * 13)
				case 2:
					data[i] = byte(random.Uint32())
				}
			}
			ref := referenceEncoder(t)
			expected := ref.EncodeAll(data, nil)
			if err := ref.Close(); err != nil {
				t.Fatal(err)
			}
			e, err := cache.Acquire()
			if err != nil {
				t.Fatal(err)
			}
			// A previous unrelated frame must not affect this independent frame.
			_ = e.EncodeAll(bytes.Repeat([]byte{0x71}, blockSize), nil)
			got := e.EncodeAll(data, nil)
			cache.Release(e)
			if !bytes.Equal(got, expected) {
				t.Fatalf("changed v1 encoding: size=%d pattern=%d", size, pattern)
			}
		}
	}
}

func TestClosedCodecOwnersReleaseBuffers(t *testing.T) {
	// Closed owners may remain reachable, but neither they nor the cache should
	// keep the encoder or decoded/raw block buffers alive across GC.
	writer, reader, ptr := closedCodecOwners(t)
	runtime.GC()
	if ptr.Value() != nil {
		t.Fatal("closed owner/cache retained encoder through GC")
	}
	if writer.encoder != nil || writer.buf != nil || reader.encoder != nil || reader.decoder != nil || reader.buf != nil {
		t.Fatal("closed codec owner retained buffers")
	}
	runtime.KeepAlive(writer)
	runtime.KeepAlive(reader)
}

func closedCodecOwners(t *testing.T) (*blockWriter, *blockReader, weak.Pointer[zstd.Encoder]) {
	t.Helper()
	w, err := newBlockWriter(new(bytes.Buffer), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{1}, blockSize)); err != nil {
		t.Fatal(err)
	}
	w.abort()
	r, err := newBlockReader(bytes.NewReader(nil), true)
	if err != nil {
		t.Fatal(err)
	}
	ptr := weak.Make(r.encoder)
	r.buf = make([]byte, blockSize)
	r.close()
	r.close()
	w.abort()
	return w, r, ptr
}

func TestConcurrentCodecReuse(t *testing.T) {
	var cache legacyEncoderCache
	data := bytes.Repeat([]byte("independent frame content"), 10000)
	ref := referenceEncoder(t)
	want := ref.EncodeAll(data, nil)
	if err := ref.Close(); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 8 {
				e, err := cache.Acquire()
				if err != nil {
					errors <- err
					return
				}
				got := e.EncodeAll(data, nil)
				cache.Release(e)
				if !bytes.Equal(got, want) {
					errors <- fmt.Errorf("concurrent reuse changed frame bytes")
					return
				}
			}
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func FuzzEncoderReuse(f *testing.F) {
	f.Add([]byte{0}, []byte{1})
	f.Add(bytes.Repeat([]byte{0x41}, blockSize), bytes.Repeat([]byte{0x7f}, 128<<10+1))
	f.Fuzz(func(t *testing.T, previous, next []byte) {
		if len(previous) > blockSize || len(next) > blockSize {
			t.Skip()
		}
		var cache legacyEncoderCache
		e, err := cache.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		_ = e.EncodeAll(previous, nil)
		cache.Release(e)
		reused, err := cache.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		got := reused.EncodeAll(next, nil)
		cache.Release(reused)
		reference := referenceEncoder(t)
		want := reference.EncodeAll(next, nil)
		if err := reference.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("reuse changed v1 frame bytes")
		}
		runtime.KeepAlive(e)
	})
}

func TestEncoderReuseAcrossHistoryReset(t *testing.T) {
	var cache legacyEncoderCache
	anchor, err := cache.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	cache.Release(anchor)
	data := bytes.Repeat([]byte("frame history reset boundary"), blockSize/28)
	ref := referenceEncoder(t)
	want := ref.EncodeAll(data, nil)
	if err := ref.Close(); err != nil {
		t.Fatal(err)
	}
	// The pinned encoder advances its history offset by at least its 8 MiB
	// window per frame. 320 frames cross the signed 32-bit reset boundary.
	for range 320 {
		e, err := cache.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if e != anchor {
			t.Fatal("did not exercise one encoder across history reset")
		}
		got := e.EncodeAll(data, nil)
		cache.Release(e)
		if !bytes.Equal(got, want) {
			t.Fatal("history reset changed frame bytes")
		}
	}
	runtime.KeepAlive(anchor)
}
