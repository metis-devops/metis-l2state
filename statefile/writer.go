package statefile

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// Writer does not own its destination. Close finalizes the file; Abort releases
// codec resources without publishing a valid footer. Errors permanently poison it.
type Writer struct {
	ctx          context.Context
	blocks       *blockWriter
	digest       hash.Hash
	state        *validator
	source       common.Hash
	dump         bool
	err          error
	closed       bool
	recordHeader [6]byte
	payload      [maxFixedPayload]byte
}

// NewWriter writes a preamble and creates a deterministic bounded encoder.
func NewWriter(ctx context.Context, out io.Writer, headerRLP []byte, metadata Metadata, compression string) (*Writer, error) {
	if ctx == nil || out == nil {
		return nil, errors.New("nil writer context or destination")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if compression == "" {
		compression = CompressionZstd
	}
	if compression != CompressionNone && compression != CompressionZstd {
		return nil, errors.New("unsupported compression")
	}
	h, err := validateHeader(headerRLP, metadata)
	if err != nil {
		return nil, err
	}
	p, err := rlp.EncodeToBytes(headerWire{headerRLP, metadata})
	if err != nil {
		return nil, err
	}
	if len(p) > MaxMetadataSize {
		return nil, errors.New("header too large")
	}
	prefix := make([]byte, 13)
	copy(prefix, magic[:])
	if compression == CompressionZstd {
		prefix[8] = 1
	}
	binary.BigEndian.PutUint32(prefix[9:], uint32(len(p)))
	digest := sha256.New()
	for _, b := range [][]byte{prefix, p} {
		if err := writeAll(out, b); err != nil {
			return nil, err
		}
		_, _ = digest.Write(b)
	}
	blocks, err := newBlockWriter(out, compression == CompressionZstd)
	if err != nil {
		return nil, err
	}
	return &Writer{ctx: ctx, blocks: blocks, digest: digest, state: newValidator(), source: h.Root, dump: metadata.IndexVersion == 0}, nil
}

// WriteRecord validates and appends an account, storage or code record.
func (w *Writer) WriteRecord(r Record) error {
	if w.closed {
		return errors.New("writer closed")
	}
	if w.err != nil {
		return w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return err
	}
	p, err := encodeRecord(r, &w.payload)
	if err == nil {
		err = w.state.accept(r)
	}
	if err == nil {
		err = w.write(r.Kind, p, true)
	}
	w.err = err
	return err
}
func (w *Writer) write(k Kind, p []byte, hashed bool) error {
	h := w.recordHeader[:]
	h[0] = byte(k)
	n := binary.PutUvarint(h[1:], uint64(len(p)))
	h = h[:1+n]
	for _, b := range [][]byte{h[:], p} {
		if hashed {
			_, _ = w.digest.Write(b)
		}
		for len(b) > 0 {
			if err := w.ctx.Err(); err != nil {
				return err
			}
			n := min(len(b), blockSize)
			if err := writeAll(w.blocks, b[:n]); err != nil {
				return err
			}
			b = b[n:]
		}
	}
	return nil
}

// Close validates the summary and writes the checksum and stream terminator.
func (w *Writer) Close(summary Summary) error {
	if w.closed {
		return errors.New("writer closed")
	}
	w.closed = true
	defer w.blocks.abort()
	if w.err != nil {
		return w.err
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := w.state.finish(summary, w.source); err != nil {
		return err
	}
	if w.dump {
		if err := validateDumpSummary(summary); err != nil {
			return err
		}
	}
	p, err := rlp.EncodeToBytes(summary)
	if err != nil {
		return err
	}
	if len(p) > MaxMetadataSize {
		return errors.New("summary too large")
	}
	_, _ = w.digest.Write(p)
	p = append(p, w.digest.Sum(nil)...)
	if err := w.write(Footer, p, false); err != nil {
		return err
	}
	return w.blocks.close()
}

// Abort releases encoder resources without finishing the file.
func (w *Writer) Abort() {
	if !w.closed {
		w.closed = true
		w.blocks.abort()
	}
}
