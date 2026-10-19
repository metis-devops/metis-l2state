package strictio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// TokenReader bounds JSON tokens and collapses outside-string whitespace.
// Hash original bytes upstream. The caller chooses a positive token limit and
// read chunk size; decoded objects and arrays remain the parser's responsibility.
func TokenReader(ctx context.Context, r io.Reader, limit, chunk int, label string) io.Reader {
	if ctx == nil || r == nil || limit < 1 || chunk < 1 {
		return &tokenReader{ctx: context.Background(), err: errors.New("invalid JSON token reader configuration")}
	}
	return &tokenReader{ctx: ctx, r: r, limit: limit, chunk: chunk, label: label}
}

// tokenReader bounds a single JSON token before encoding/json buffers it and
// collapses each run of whitespace outside strings to one space. Keeping that
// separator preserves token boundaries without letting the decoder buffer an
// arbitrarily long whitespace run. Hash raw bytes upstream of this reader.
type tokenReader struct {
	ctx     context.Context
	r       io.Reader
	limit   int
	chunk   int
	label   string
	length  int
	quoted  bool
	escaped bool
	space   bool
	err     error
}

func (r *tokenReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.err != nil {
			return 0, r.err
		}
		n, err := r.r.Read(p[:min(len(p), r.chunk)])
		r.err = err // Retain errors even when a decoder consumes bytes first.
		n, filterErr := r.filter(p[:n])
		if filterErr != nil {
			r.err = errors.Join(filterErr, r.err)
		}
		if n != 0 || r.err != nil {
			return n, r.err
		}
		// A chunk containing only an already-started whitespace run produces no
		// output. Continue reading with cancellation checks and the same buffer.
	}
}

func (r *tokenReader) filter(p []byte) (int, error) {
	written := 0
	for _, b := range p {
		if !r.quoted && (b == ' ' || b == '\t' || b == '\r' || b == '\n') {
			r.length = 0
			if !r.space {
				p[written] = ' '
				written++
				r.space = true
			}
			continue
		}
		r.space = false
		if err := r.tokenByte(b); err != nil {
			return written, err
		}
		p[written] = b
		written++
	}
	return written, nil
}

func (r *tokenReader) tokenByte(b byte) error {
	r.length++
	if r.length > r.limit {
		return fmt.Errorf("%s JSON token exceeds limit %d", r.label, r.limit)
	}
	if r.quoted {
		switch {
		case r.escaped:
			r.escaped = false
		case b == '\\':
			r.escaped = true
		case b == '"':
			r.quoted, r.length = false, 0
		}
	} else if b == '"' {
		r.quoted, r.length = true, 1
	} else if strings.ContainsRune("{}[],:", rune(b)) {
		r.length = 0
	}
	return nil
}
