package statefile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// Reader validates order, roots, code and checksum while streaming. An early
// Close does not verify the unread suffix. The caller owns the source reader.
type Reader struct {
	ctx          context.Context
	header       FileHeader
	blocks       *blockReader
	digest       hash.Hash
	state        *validator
	summary      Summary
	done, closed bool
	err          error
	recordHeader [6]byte
	payload      [maxFixedPayload]byte
}

// NewReader validates the bounded preamble without taking ownership of in.
func NewReader(ctx context.Context, in io.Reader) (*Reader, error) {
	if ctx == nil || in == nil {
		return nil, errors.New("nil reader context or source")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var prefix [13]byte
	if _, err := io.ReadFull(in, prefix[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(prefix[:8], magic[:]) || prefix[8] > 1 {
		return nil, errors.New("unsupported state file format")
	}
	n := binary.BigEndian.Uint32(prefix[9:])
	if n == 0 || n > MaxMetadataSize {
		return nil, errors.New("invalid header length")
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(in, p); err != nil {
		return nil, err
	}
	var wire headerWire
	if err := rlp.DecodeBytes(p, &wire); err != nil {
		return nil, err
	}
	h, err := validateHeader(wire.HeaderRLP, wire.Metadata)
	if err != nil {
		return nil, err
	}
	blocks, err := newBlockReader(in, prefix[8] == 1)
	if err != nil {
		return nil, err
	}
	d := sha256.New()
	_, _ = d.Write(prefix[:])
	_, _ = d.Write(p)
	c := CompressionNone
	if prefix[8] == 1 {
		c = CompressionZstd
	}
	return &Reader{ctx: ctx, header: FileHeader{wire.HeaderRLP, h, wire.Metadata, c}, blocks: blocks, digest: d, state: newValidator()}, nil
}

// Header returns a defensive copy of the source identity and provenance.
func (r *Reader) Header() FileHeader {
	h := r.header
	h.HeaderRLP = bytes.Clone(h.HeaderRLP)
	h.Header = types.CopyHeader(h.Header)
	h.Metadata.Inputs = append([]InputDigest(nil), h.Metadata.Inputs...)
	return h
}

// Next returns one owned record; only EOF after Footer means complete verification.
func (r *Reader) Next() (rec Record, err error) {
	if r.err != nil {
		return rec, r.err
	}
	if r.closed {
		return rec, errors.New("reader closed")
	}
	if r.done {
		return rec, io.EOF
	}
	defer func() {
		// EOF is reserved for the already-verified done branch above. A block
		// boundary (including a premature terminator) can otherwise hide a
		// missing record payload behind an ordinary end-of-stream signal.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			r.err = err
		}
	}()
	if err = r.ctx.Err(); err != nil {
		return rec, err
	}
	k, n, h, err := readRecordHeader(r.blocks, &r.recordHeader)
	if err != nil {
		return rec, err
	}
	limit := payloadLimit(k)
	if limit == 0 || n == 0 || n > limit {
		return rec, errors.New("invalid record length or type")
	}
	p, err := r.readPayload(k, n)
	if err != nil {
		return rec, err
	}
	if k == Footer {
		return r.readFooter(p)
	}
	_, _ = r.digest.Write(h[:])
	_, _ = r.digest.Write(p)
	rec, err = decodeRecord(k, p)
	if err == nil {
		err = r.state.accept(rec)
	}
	return rec, err
}

// Fixed records decode into owned values. Variable records (including short
// code) need their own buffer because Record.Code survives subsequent Next calls.
func (r *Reader) readPayload(k Kind, n uint32) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if k == AccountStart || k == StorageEntry || k == AccountEnd {
		p := r.payload[:n]
		_, err := io.ReadFull(r.blocks, p)
		return p, err
	}
	// Grow only as bytes arrive; forged code lengths cannot reserve MaxCodeSize.
	p := make([]byte, 0, min(n, blockSize))
	for uint32(len(p)) < n {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		offset := len(p)
		chunk := min(int(n)-offset, blockSize)
		p = append(p, make([]byte, chunk)...)
		if _, err := io.ReadFull(r.blocks, p[offset:]); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (r *Reader) readFooter(p []byte) (Record, error) {
	if len(p) <= 32 {
		return Record{}, errors.New("invalid footer")
	}
	body, sum := p[:len(p)-32], p[len(p)-32:]
	_, _ = r.digest.Write(body)
	if !bytes.Equal(sum, r.digest.Sum(nil)) {
		return Record{}, errors.New("checksum mismatch")
	}
	var s Summary
	if err := rlp.DecodeBytes(body, &s); err != nil {
		return Record{}, err
	}
	if err := r.state.finish(s, r.header.Header.Root); err != nil {
		return Record{}, err
	}
	if r.header.Metadata.IndexVersion == 0 {
		if err := validateDumpSummary(s); err != nil {
			return Record{}, err
		}
	}
	var tail [1]byte
	n, err := io.ReadFull(r.blocks, tail[:])
	if n != 0 || !errors.Is(err, io.EOF) || !r.blocks.ended {
		if err != nil && !errors.Is(err, io.EOF) {
			return Record{}, err
		}
		return Record{}, errors.New("trailing records")
	}
	r.done = true
	r.summary = s
	return Record{Kind: Footer, Summary: s}, nil
}

// Result returns the summary only after successful full-file verification.
func (r *Reader) Result() (Summary, error) {
	if !r.done || r.err != nil {
		return Summary{}, errors.New("file has not been completely verified")
	}
	return r.summary, nil
}

// Close releases codec resources without closing the underlying source.
func (r *Reader) Close() {
	if !r.closed {
		r.closed = true
		r.blocks.close()
	}
}

// Verify independently rebuilds roots and checks the complete file inventory.
func Verify(ctx context.Context, in io.Reader) (Summary, error) {
	r, err := NewReader(ctx, in)
	if err != nil {
		return Summary{}, err
	}
	defer r.Close()
	for {
		_, err = r.Next()
		if errors.Is(err, io.EOF) {
			return r.Result()
		}
		if err != nil {
			return Summary{}, err
		}
	}
}
