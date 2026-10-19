package migration

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2state/internal/version"
	"github.com/metis-devops/metis-l2state/statefile"
)

// DumpOptions configures an unmodified export of a stopped legacy source.
// OVM evidence, conversion inputs and state overlays are intentionally absent.
type DumpOptions struct {
	Source, Output            string
	Compression               string
	CodeCache                 string
	Workers, CacheMB, Handles int
	Progress                  io.Writer
}

// Dump validates and exports the canonical LastBlock state without OVM policy.
func Dump(ctx context.Context, options DumpOptions) (result Result, retErr error) {
	if ctx == nil {
		return result, errors.New("nil context")
	}
	if options.Source == "" || options.Output == "" {
		return result, errors.New("source and output are required")
	}
	if options.CodeCache == "" {
		options.CodeCache = CodeCacheNone
	}
	o, err := normalizeOptions(Options{CodeCache: options.CodeCache, Source: options.Source, Output: options.Output, Compression: options.Compression, Workers: options.Workers, CacheMB: options.CacheMB, Handles: options.Handles, Progress: options.Progress})
	if err != nil {
		return result, err
	}
	log, stop := startProgress(ctx, o.Progress)
	defer stop()
	log.Info("Dump started", "phase", "prepare", "workers", o.Workers, "code_cache", o.CodeCache)
	s, err := openSource(o.Source, o.CacheMB, o.Handles)
	if err != nil {
		return result, err
	}
	s.codeCache = o.CodeCache
	session := exportSession{source: s}
	defer func() { retErr = errors.Join(retErr, session.abort()) }()
	// Reuse one read sequence for point lookups, without retaining addresses.
	// Source close releases it on success and every subsequent error path.
	s.preimages, err = s.kv.Snapshot()
	if err != nil {
		return result, err
	}
	genesis, err := s.genesisIdentity()
	if err != nil {
		return result, err
	}
	p, err := newPublication(o.Source, o.Output)
	if err != nil {
		return result, err
	}
	session.publication = p
	l := make(limiter, o.Workers)
	metadata := statefile.Metadata{ToolVersion: version.ToolVersion, Genesis: genesis}
	w, err := statefile.NewWriter(ctx, p.file, s.header, metadata, o.Compression)
	if err != nil {
		return result, err
	}
	session.writer = w
	log.Info("Validating and streaming original state", "phase", "write_state")
	walk, err := s.writeDump(ctx, o.Workers, l, w.WriteRecord)
	if err != nil {
		return result, err
	}
	if err := s.writeCodes(ctx, nil, &walk, w); err != nil {
		return result, err
	}
	summary := statefile.Summary{
		SourceRoot: s.head.StateRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot,
		Counts: walk.counts, ConvertedCounts: walk.convertedCounts,
		Conversion: statefile.Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"},
	}
	if err := session.finish(ctx, summary); err != nil {
		return result, err
	}

	log.Info("Dump completed", "phase", "complete", "root", summary.FinalStateRoot, "accounts", summary.Counts.Accounts)
	return Result{Artifact: o.Output, Summary: summary}, nil
}

// Resolve only the current account's address, after bounded account admission.
// No index iterator or operation-wide address map is needed for raw exports.
func (s *source) dumpAddress(ctx context.Context, l limiter, hash common.Hash) (address common.Address, err error) {
	err = l.run(ctx, func() error {
		var key [len(preimageKeyPrefix) + common.HashLength]byte
		copy(key[:], preimageKeyPrefix)
		copy(key[len(preimageKeyPrefix):], hash[:])
		reader := ethdb.KeyValueReader(s.db)
		if s.preimages != nil {
			reader = s.preimages
		}
		value, err := optional(reader, key[:])
		if err != nil {
			return fmt.Errorf("account address preimage %s: %w", hash, err)
		}
		if len(value) != common.AddressLength {
			return fmt.Errorf("missing or invalid account address preimage %s: length %d", hash, len(value))
		}
		if crypto.Keccak256Hash(value) != hash {
			return fmt.Errorf("account address preimage hash mismatch %s", hash)
		}
		address = common.Address(value)
		return nil
	})
	return address, err
}
