package statefile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

const blockSize = 256 << 10

// Independently framed blocks bound decompression memory and make trailing
// frames unambiguous. The final block is always short (unless empty).
type blockWriter struct {
	out     io.Writer
	encoder *zstd.Encoder
	buf     []byte
	err     error
}

func newBlockWriter(out io.Writer, compressed bool) (*blockWriter, error) {
	w := &blockWriter{out: out, buf: make([]byte, 0, blockSize)}
	if compressed {
		var err error
		w.encoder, err = newBlockEncoder()
		if err != nil {
			return nil, err
		}
	}
	return w, nil
}
func (w *blockWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n := len(p)
	for len(p) > 0 {
		k := min(blockSize-len(w.buf), len(p))
		w.buf = append(w.buf, p[:k]...)
		p = p[k:]
		if len(w.buf) == blockSize {
			if err := w.flush(); err != nil {
				w.err = err
				return n - len(p), err
			}
		}
	}
	return n, nil
}
func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}
func (w *blockWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	data := w.buf
	if w.encoder != nil {
		data = w.encoder.EncodeAll(w.buf, nil)
	}
	var h [8]byte
	binary.BigEndian.PutUint32(h[:4], uint32(len(w.buf)))
	binary.BigEndian.PutUint32(h[4:], uint32(len(data)))
	if err := writeAll(w.out, h[:]); err != nil {
		return err
	}
	if err := writeAll(w.out, data); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}
func (w *blockWriter) close() error {
	defer w.abort()
	if w.err != nil {
		return w.err
	}
	if err := w.flush(); err != nil {
		return err
	}
	return writeAll(w.out, make([]byte, 8))
}
func (w *blockWriter) abort() {
	w.buf = nil
	if w.encoder != nil {
		encoder := w.encoder
		w.encoder = nil
		_ = encoder.Close()
	}
}

type blockReader struct {
	in           io.Reader
	decoder      *zstd.Decoder
	encoder      *zstd.Encoder
	buf          []byte
	ended, short bool
}

func newBlockReader(in io.Reader, compressed bool) (*blockReader, error) {
	r := &blockReader{in: in}
	if compressed {
		var err error
		r.decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<20), zstd.WithDecodeAllCapLimit(true))
		if err != nil {
			return nil, err
		}
		r.encoder, err = newBlockEncoder()
		if err != nil {
			r.decoder.Close()
			return nil, err
		}
	}
	return r, nil
}
func (r *blockReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.buf) == 0 {
		if r.ended {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
func (r *blockReader) next() error {
	var h [8]byte
	if _, err := io.ReadFull(r.in, h[:]); err != nil {
		return err
	}
	raw, encoded := binary.BigEndian.Uint32(h[:4]), binary.BigEndian.Uint32(h[4:])
	if raw == 0 && encoded == 0 {
		r.ended = true
		var tail [1]byte
		n, err := io.ReadFull(r.in, tail[:])
		if n != 0 || !errors.Is(err, io.EOF) {
			return errors.New("trailing physical data")
		}
		return io.EOF
	}
	if r.short || raw == 0 || raw > blockSize || encoded == 0 || encoded > 2*blockSize {
		return errors.New("invalid block lengths or ordering")
	}
	r.short = raw < blockSize
	data := make([]byte, encoded)
	if _, err := io.ReadFull(r.in, data); err != nil {
		return err
	}
	if r.decoder != nil {
		decoded, err := r.decoder.DecodeAll(data, make([]byte, 0, raw))
		if err != nil {
			return err
		}
		if len(decoded) != int(raw) || !bytes.Equal(r.encoder.EncodeAll(decoded, nil), data) {
			return errors.New("noncanonical compressed block")
		}
		r.buf = decoded
	} else {
		if raw != encoded {
			return errors.New("invalid raw block length")
		}
		r.buf = data
	}
	return nil
}
func (r *blockReader) close() {
	if r.decoder != nil {
		r.decoder.Close()
		r.decoder = nil
	}
	encoder := r.encoder
	r.encoder = nil
	r.buf = nil
	if encoder != nil {
		_ = encoder.Close()
	}
}

// Both directions use exactly the same canonical v1 encoder options.
func newBlockEncoder() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true), zstd.WithEncoderLevel(zstd.SpeedDefault))
}
