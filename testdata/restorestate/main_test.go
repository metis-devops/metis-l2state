package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/migration"
	"github.com/metis-devops/metis-l2state/internal/storageword"
	"github.com/metis-devops/metis-l2state/statefile"
)

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A fresh dump of the restored DB proves legacy code keys, header mappings,
// address preimages, and already-hashed storage keys survive the round trip.
func TestRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	address := common.HexToAddress("0x1234")
	code := []byte{0x60, 0x42, 0}
	codeHash := crypto.Keccak256Hash(code)
	slot := common.HexToHash("0x99")
	key := crypto.Keccak256Hash(slot[:])
	value := common.HexToHash("0x010203")
	storage := trie.NewStackTrie(nil)
	require(t, storageword.Update(storage, key, value))
	root := storage.Hash()
	accounts := trie.NewStackTrie(nil)
	blob, err := rlp.EncodeToBytes(&types.StateAccount{Nonce: 7, Balance: uint256.NewInt(42), Root: root, CodeHash: codeHash[:]})
	require(t, err)
	hash := crypto.Keccak256Hash(address[:])
	require(t, accounts.Update(hash[:], blob))
	header := &types.Header{Number: big.NewInt(42), Difficulty: big.NewInt(1), Root: accounts.Hash(), GasLimit: 10000000, UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
	headerRLP, err := rlp.EncodeToBytes(header)
	require(t, err)
	genesis := types.CopyHeader(header)
	genesis.Number = big.NewInt(0)
	genesisJSON, err := json.Marshal(genesis)
	require(t, err)
	genesisPath := filepath.Join(dir, "genesis.json")
	require(t, os.WriteFile(genesisPath, genesisJSON, 0600))
	preimages := filepath.Join(dir, "preimages.gz")
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	require(t, rlp.Encode(gz, address[:]))
	require(t, rlp.Encode(gz, slot[:]))
	require(t, gz.Close())
	require(t, os.WriteFile(preimages, compressed.Bytes(), 0600))
	var dump bytes.Buffer
	writer, err := statefile.NewWriter(t.Context(), &dump, headerRLP, statefile.Metadata{ToolVersion: "restore-test", Genesis: genesis.Hash()}, "zstd")
	require(t, err)
	for _, r := range []statefile.Record{
		{Kind: statefile.AccountStart, Account: statefile.Account{Address: address, CodeHash: codeHash, Balance: big.NewInt(42), Nonce: 7}},
		{Kind: statefile.StorageEntry, Storage: statefile.Storage{Key: statefile.StorageKeyHash(key), Value: value}},
		{Kind: statefile.AccountEnd, StorageRoot: root, StorageCount: 1},
		{Kind: statefile.CodeEntry, CodeHash: codeHash, Code: code},
	} {
		require(t, writer.WriteRecord(r))
	}
	counts := statefile.Counts{Accounts: 1, StorageSlots: 1, Codes: 1, CodeBytes: uint64(len(code))}
	summary := statefile.Summary{SourceRoot: header.Root, ConvertedStateRoot: header.Root, FinalStateRoot: header.Root, Counts: counts, ConvertedCounts: counts, Conversion: statefile.Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"}}
	require(t, writer.Close(summary))
	dumpPath := filepath.Join(dir, "dump")
	require(t, os.WriteFile(dumpPath, dump.Bytes(), 0600))
	targetPath := filepath.Join(dir, "target.json")
	targetJSON, err := json.Marshal(header)
	require(t, err)
	require(t, os.WriteFile(targetPath, targetJSON, 0600))
	wrong := filepath.Join(dir, "wrong-target")
	if err := restore(t.Context(), dumpPath, preimages, genesisPath, genesisPath, wrong); err == nil {
		t.Fatal("accepted wrong target header")
	}
	if _, err := os.Stat(wrong); !os.IsNotExist(err) {
		t.Fatal("created DB before target validation")
	}
	out := filepath.Join(dir, "db")
	require(t, restore(t.Context(), dumpPath, preimages, genesisPath, targetPath, out))
	roundtrip := filepath.Join(dir, "roundtrip")
	result, err := migration.Dump(t.Context(), migration.DumpOptions{Source: out, Output: roundtrip, Workers: 2, CacheMB: 16, Handles: 16, Compression: "zstd"})
	require(t, err)
	if result.Summary != summary {
		t.Fatal("roundtrip summary differs")
	}
	f, err := os.Open(roundtrip)
	require(t, err)
	defer func() { require(t, f.Close()) }()
	reader, err := statefile.NewReader(t.Context(), f)
	require(t, err)
	defer reader.Close()
	if !bytes.Equal(reader.Header().HeaderRLP, headerRLP) {
		t.Fatal("header changed")
	}
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require(t, err)
	}
	if err := restore(t.Context(), dumpPath, preimages, genesisPath, targetPath, out); err == nil {
		t.Fatal("overwrote existing DB")
	}
	// A valid preamble followed by truncation must remove its incomplete DB.
	require(t, os.WriteFile(dumpPath, dump.Bytes()[:dump.Len()-20], 0600))
	broken := filepath.Join(dir, "broken")
	if err := restore(t.Context(), dumpPath, preimages, genesisPath, "", broken); err == nil {
		t.Fatal("accepted truncated dump")
	}
	if _, err := os.Stat(broken); !os.IsNotExist(err) {
		t.Fatal("kept incomplete DB", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := restore(ctx, dumpPath, preimages, genesisPath, "", broken); err == nil {
		t.Fatal("ignored cancellation")
	}
}

// Optional direct input check also works after restoring a large dump, without
// repeating restoration or touching the resulting database.
func TestProvidedHeaders(t *testing.T) {
	dir := os.Getenv("L2STATE_FIXTURES")
	if dir == "" {
		t.Skip("set L2STATE_FIXTURES for operator-provided headers")
	}
	f, err := os.Open(filepath.Join(dir, "dump.zstd"))
	require(t, err)
	defer func() { require(t, f.Close()) }()
	r, err := statefile.NewReader(t.Context(), f)
	require(t, err)
	defer r.Close()
	target, err := blockHeader(filepath.Join(dir, "target-block-header.json"))
	require(t, err)
	genesis, err := blockHeader(filepath.Join(dir, "genesis-header.json"))
	require(t, err)
	if !bytes.Equal(target, r.Header().HeaderRLP) || crypto.Keccak256Hash(genesis) != r.Header().Metadata.Genesis {
		t.Fatal("fixture headers differ from dump")
	}
	t.Logf("header number=%d hash=%s root=%s", r.Header().Header.Number, r.Header().Header.Hash(), r.Header().Header.Root)
}
