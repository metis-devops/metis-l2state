package migration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/metis-devops/metis-l2state/internal/version"
	"github.com/metis-devops/metis-l2state/statefile"
)

// Options configures OVM conversion to a state file.
type Options struct {
	Source, Output                                    string
	WrappedEtherCode, RetainList, Alloc, AuditSummary string
	OVMEvents                                         string
	Compression                                       string
	CodeCache                                         string
	Workers, CacheMB, Handles                         int
	Progress                                          io.Writer
}

// Result reports the Writer-validated, synced and published file.
type Result struct {
	Artifact string            `json:"artifact"`
	Summary  statefile.Summary `json:"summary"`
}

// Migrate converts a stopped OVM source and atomically publishes a state file.
func Migrate(ctx context.Context, o Options) (result Result, retErr error) {
	if ctx == nil {
		return result, errors.New("nil context")
	}
	if o.Source == "" || o.Output == "" || o.WrappedEtherCode == "" || o.OVMEvents == "" {
		return result, errors.New("source, output, wrapped-ether-code and ovm-events are required")
	}
	var err error
	o, err = normalizeOptions(o)
	if err != nil {
		return result, err
	}
	log, stop := startProgress(ctx, o.Progress)
	defer stop()
	log.Info("Migration started", "phase", "prepare", "workers", o.Workers, "code_cache", o.CodeCache)
	in, err := loadInputs(ctx, o)
	if err != nil {
		return result, err
	}
	s, err := openSource(o.Source, o.CacheMB, o.Handles)
	if err != nil {
		return result, err
	}
	s.codeCache = o.CodeCache
	session := exportSession{source: s}
	defer func() { retErr = errors.Join(retErr, session.abort()) }()
	if err := s.validateAudit(in.audit); err != nil {
		return result, err
	}
	p, err := newPublication(o.Source, o.Output)
	if err != nil {
		return result, err
	}
	session.publication = p
	l := make(limiter, o.Workers)
	log.Info("Validating source and loading RPC event indexes", "phase", "validate_source")
	x, err := s.prepareSnapshot(ctx, o.Workers, l, in.events)
	if err != nil {
		return result, err
	}
	in.events = nil
	x.metadata.ToolVersion = version.ToolVersion
	x.metadata.Inputs = in.digests()
	log.Info("Converting OVM balances", "phase", "convert_ovm")
	evidence, err := s.convert(ctx, x, in, o.Workers, l)
	if err != nil {
		return result, err
	}
	w, err := statefile.NewWriter(ctx, p.file, s.header, x.metadata, o.Compression)
	if err != nil {
		return result, err
	}
	session.writer = w
	log.Info("Streaming transformed state", "phase", "write_state")
	walk, err := s.writeMigration(ctx, o.Workers, l, x, in.alloc, w.WriteRecord)
	if err != nil {
		return result, err
	}
	if err := s.writeCodes(ctx, in, &walk, w); err != nil {
		return result, err
	}
	summary := statefile.Summary{SourceRoot: s.head.StateRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot, Counts: walk.counts, ConvertedCounts: walk.convertedCounts, Conversion: evidence}
	if err := session.finish(ctx, summary); err != nil {
		return result, err
	}

	log.Info("Migration completed", "phase", "complete", "root", summary.FinalStateRoot, "accounts", summary.Counts.Accounts)
	return Result{Artifact: o.Output, Summary: summary}, nil
}
func codeOverrides(in *inputs) map[common.Hash][]byte {
	m := make(map[common.Hash][]byte)
	if in == nil {
		return m
	}
	m[crypto.Keccak256Hash(in.runtime)] = in.runtime
	for _, p := range in.alloc {
		if p.Code != nil && len(*p.Code) > 0 {
			m[crypto.Keccak256Hash(*p.Code)] = *p.Code
		}
	}
	return m
}

// Source code keeps account-discovery order. Runtime follows, then new alloc
// code in ascending account-key order. Stage membership never changes these IDs.
func (s *source) writeCodes(ctx context.Context, in *inputs, walk *walkResult, w *statefile.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	overrides := codeOverrides(in)
	emit := func(h common.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, ok := walk.codes.ids[h]
		if !ok {
			return nil
		}
		r := walk.codes.items.ptr(id)
		if r.use&emittedCode != 0 {
			return nil
		}
		if err := s.writeCode(h, r.use, overrides, walk, w); err != nil {
			return err
		}
		r.use |= emittedCode
		return nil
	}
	if s.codes != nil {
		for i := uint64(0); i < s.codes.codes.length; i++ {
			if err := emit(s.codes.codes.at(i).hash); err != nil {
				return err
			}
		}
	}
	if in != nil {
		if err := emit(crypto.Keccak256Hash(in.runtime)); err != nil {
			return err
		}
		for _, h := range sortedHashes(in.alloc) {
			p := in.alloc[h]
			if p.Code != nil && len(*p.Code) != 0 {
				if err := emit(crypto.Keccak256Hash(*p.Code)); err != nil {
					return err
				}
			}
		}
	}
	return walk.codes.Visit(func(_ common.Hash, use codeUse) error {
		if use&emittedCode == 0 {
			return errors.New("unregistered output code")
		}
		return nil
	})
}

func (s *source) writeCode(h common.Hash, use codeUse, overrides map[common.Hash][]byte, walk *walkResult, w *statefile.Writer) error {
	code, supplied := overrides[h]
	var length uint64
	if supplied {
		length = uint64(len(code))
	} else {
		if s.codes == nil {
			return errors.New("missing source code directory")
		}
		id, ok := s.codes.codeIDs[h]
		if !ok {
			return errors.New("missing source code reference")
		}
		length = s.codes.codes.at(id - 1).length
		if length == 0 {
			return errors.New("unvalidated source code")
		}
	}
	if use&finalCode != 0 {
		if !supplied {
			var err error
			code, err = s.code(h)
			if err != nil {
				return err
			}
		}
		if err := w.WriteRecord(statefile.Record{Kind: statefile.CodeEntry, CodeHash: h, Code: code}); err != nil {
			return err
		}
		walk.counts.Codes++
		walk.counts.CodeBytes += uint64(len(code))
	}
	if use&convertedCode != 0 {
		walk.convertedCounts.Codes++
		walk.convertedCounts.CodeBytes += length
	}
	return nil
}
func startProgress(ctx context.Context, out io.Writer) (*slog.Logger, func()) {
	if out == nil {
		out = io.Discard
	}
	logger := slog.New(slog.NewTextHandler(out, nil))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				logger.Info("Export resources", "heap_bytes", m.HeapAlloc, "system_bytes", m.Sys, "goroutines", runtime.NumGoroutine())
			}
		}
	})
	return logger, func() { close(done); wg.Wait() }
}

func normalizeOptions(o Options) (Options, error) {
	if o.Workers == 0 {
		o.Workers = 2
	}
	if o.Workers < 2 || o.Workers > 16 {
		return o, errors.New("workers must be between 2 and 16")
	}
	if o.CacheMB == 0 {
		o.CacheMB = 512
	}
	if o.Handles == 0 {
		o.Handles = 256
	}
	if o.CacheMB < 1 || o.Handles < 1 {
		return o, errors.New("cache and handles must be positive")
	}
	if o.Compression == "" {
		o.Compression = statefile.CompressionZstd
	}
	if o.Compression != statefile.CompressionZstd && o.Compression != statefile.CompressionNone {
		return o, errors.New("compression must be zstd or none")
	}
	if o.CodeCache == "" {
		o.CodeCache = "memory"
	}
	if o.CodeCache != "memory" && o.CodeCache != "none" {
		return o, errors.New("code-cache must be memory or none")
	}
	return o, nil
}
