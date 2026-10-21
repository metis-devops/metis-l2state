package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
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

func TestBlockHeaderRepresentations(t *testing.T) {
	h := &types.Header{
		Number: big.NewInt(42), Difficulty: big.NewInt(1),
		Root: common.HexToHash("0x1234"), GasLimit: 10000000,
	}
	raw, err := json.Marshal(h)
	require(t, err)
	want, err := rlp.EncodeToBytes(h)
	require(t, err)
	var fields map[string]jsontext.Value
	require(t, json.Unmarshal(raw, &fields))
	for key, number := range map[string]string{
		"number": "42", "difficulty": "1", "gasLimit": "10000000", "gasUsed": "0", "timestamp": "0",
	} {
		fields[key] = jsontext.Value(number)
	}
	numeric, err := json.Marshal(fields)
	require(t, err)
	for name, data := range map[string]string{
		"hex":       string(raw),
		"numeric":   string(numeric),
		"extension": string(raw[:len(raw)-1]) + `,"extra":{"x":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "header.json")
			require(t, os.WriteFile(path, []byte(data), 0600))
			got, err := blockHeader(path)
			require(t, err)
			if !bytes.Equal(got, want) {
				t.Fatal("fixture representation changed header RLP")
			}
		})
	}
	for name, data := range map[string]string{
		"case":              strings.Replace(string(raw), `"number":`, `"Number":`, 1),
		"case-alias":        `{"Number":"0x2a",` + string(raw[1:]),
		"optional-case":     `{"Miner":null,` + string(raw[1:]),
		"hash-alias":        `{"Hash":"` + h.Hash().Hex() + `",` + string(raw[1:]),
		"duplicate":         `{"number":"0x2a",` + string(raw[1:]),
		"escaped-duplicate": `{"n\u0075mber":"0x2a",` + string(raw[1:]),
		"nested-duplicate":  string(raw[:len(raw)-1]) + `,"extra":{"x":1,"x":2}}`,
		"invalid-utf8":      string(raw[:len(raw)-1]) + ",\"extra\":\"\xff\"}",
		"trailing":          string(raw) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "header.json")
			require(t, os.WriteFile(path, []byte(data), 0600))
			if _, err := blockHeader(path); err == nil {
				t.Fatal("accepted invalid header JSON")
			}
		})
	}
}

func TestBlockHeaderForkFields(t *testing.T) {
	h := &types.Header{
		ParentHash: common.HexToHash("0x01"), UncleHash: common.HexToHash("0x02"),
		Coinbase: common.HexToAddress("0x03"), Root: common.HexToHash("0x04"),
		TxHash: common.HexToHash("0x05"), ReceiptHash: common.HexToHash("0x06"),
		Bloom: types.Bloom{7, 8}, Difficulty: big.NewInt(9), Number: big.NewInt(10),
		GasLimit: 11, GasUsed: 12, Time: 13, Extra: []byte{0x60, 0xff},
		MixDigest: common.HexToHash("0x14"), Nonce: types.BlockNonce{15, 16},
		BaseFee:         new(big.Int).Lsh(big.NewInt(1), 200),
		WithdrawalsHash: new(common.HexToHash("0x17")),
		BlobGasUsed:     new(uint64(18)), ExcessBlobGas: new(uint64(19)),
		ParentBeaconRoot: new(common.HexToHash("0x20")), RequestsHash: new(common.HexToHash("0x21")),
		BlockAccessListHash: new(common.HexToHash("0x22")), SlotNumber: new(^uint64(0)),
	}
	for _, absent := range []bool{false, true} {
		if absent {
			h.BaseFee, h.WithdrawalsHash, h.BlobGasUsed, h.ExcessBlobGas = nil, nil, nil, nil
			h.ParentBeaconRoot, h.RequestsHash, h.BlockAccessListHash, h.SlotNumber = nil, nil, nil, nil
		}
		raw, err := json.Marshal(h)
		require(t, err)
		path := filepath.Join(t.TempDir(), "header.json")
		require(t, os.WriteFile(path, raw, 0600))
		got, err := blockHeader(path)
		require(t, err)
		want, err := rlp.EncodeToBytes(h)
		require(t, err)
		if !bytes.Equal(got, want) {
			t.Fatal("header fields changed during JSON decoding")
		}
	}
}

func TestBlockHeaderNumericPrecision(t *testing.T) {
	h := &types.Header{
		Number: new(big.Int).SetUint64(^uint64(0)), Difficulty: new(big.Int).Lsh(big.NewInt(1), 255),
		GasLimit: ^uint64(0), GasUsed: 1<<53 + 1, Time: ^uint64(0),
	}
	raw, err := json.Marshal(h)
	require(t, err)
	var fields map[string]jsontext.Value
	require(t, json.Unmarshal(raw, &fields))
	fields["number"], fields["difficulty"] = jsontext.Value(h.Number.String()), jsontext.Value(h.Difficulty.String())
	fields["gasLimit"], fields["timestamp"] = jsontext.Value(h.Number.String()), jsontext.Value(h.Number.String())
	fields["gasUsed"] = jsontext.Value("9007199254740993")
	raw, err = json.Marshal(fields)
	require(t, err)
	path := filepath.Join(t.TempDir(), "header.json")
	require(t, os.WriteFile(path, raw, 0600))
	got, err := blockHeader(path)
	require(t, err)
	want, err := rlp.EncodeToBytes(h)
	require(t, err)
	if !bytes.Equal(got, want) {
		t.Fatal("numeric header lost integer precision")
	}
}

func TestBlockHeaderRequiredFields(t *testing.T) {
	h := &types.Header{Number: big.NewInt(42), Difficulty: big.NewInt(1)}
	raw, err := json.Marshal(h)
	require(t, err)
	for _, key := range []string{
		"parentHash", "sha3Uncles", "stateRoot", "transactionsRoot", "receiptsRoot",
		"logsBloom", "difficulty", "number", "gasLimit", "gasUsed", "timestamp", "extraData", "hash",
	} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%t", key, missing), func(t *testing.T) {
				var fields map[string]jsontext.Value
				require(t, json.Unmarshal(raw, &fields))
				if missing {
					delete(fields, key)
				} else {
					fields[key] = jsontext.Value("null")
				}
				data, err := json.Marshal(fields)
				require(t, err)
				path := filepath.Join(t.TempDir(), "header.json")
				require(t, os.WriteFile(path, data, 0600))
				if _, err := blockHeader(path); err == nil {
					t.Fatal("accepted missing or null required field")
				}
			})
		}
	}
}

func TestBlockHeaderOptionalFields(t *testing.T) {
	h := &types.Header{Number: big.NewInt(42), Difficulty: big.NewInt(1)}
	raw, err := json.Marshal(h)
	require(t, err)
	want, err := rlp.EncodeToBytes(h)
	require(t, err)
	optional := []string{
		"miner", "mixHash", "nonce", "baseFeePerGas", "withdrawalsRoot", "blobGasUsed",
		"excessBlobGas", "parentBeaconBlockRoot", "requestsHash", "blockAccessListHash", "slotNumber",
	}
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			var fields map[string]jsontext.Value
			require(t, json.Unmarshal(raw, &fields))
			for _, key := range optional {
				if missing {
					delete(fields, key)
				} else {
					fields[key] = jsontext.Value("null")
				}
			}
			data, err := json.Marshal(fields)
			require(t, err)
			path := filepath.Join(t.TempDir(), "header.json")
			require(t, os.WriteFile(path, data, 0600))
			got, err := blockHeader(path)
			require(t, err)
			if !bytes.Equal(got, want) {
				t.Fatal("absent or null optional fields changed header RLP")
			}
		})
	}
	// Decimal fixture syntax must not leak into optional RPC quantities.
	for _, key := range []string{"baseFeePerGas", "blobGasUsed", "excessBlobGas", "slotNumber"} {
		t.Run(key+"/decimal", func(t *testing.T) {
			var fields map[string]jsontext.Value
			require(t, json.Unmarshal(raw, &fields))
			fields[key] = jsontext.Value("1")
			data, err := json.Marshal(fields)
			require(t, err)
			path := filepath.Join(t.TempDir(), "header.json")
			require(t, os.WriteFile(path, data, 0600))
			if _, err := blockHeader(path); err == nil || strings.Contains(err.Error(), "hash mismatch") {
				t.Fatalf("expected rejection before hash check, got %v", err)
			}
		})
	}
}

func TestBlockHeaderDeclaredHash(t *testing.T) {
	h := &types.Header{Number: big.NewInt(42), Difficulty: big.NewInt(1)}
	raw, err := json.Marshal(h)
	require(t, err)
	var fields map[string]jsontext.Value
	require(t, json.Unmarshal(raw, &fields))
	fields["hash"] = jsontext.Value(`"` + (common.Hash{}).Hex() + `"`)
	raw, err = json.Marshal(fields)
	require(t, err)
	path := filepath.Join(t.TempDir(), "header.json")
	require(t, os.WriteFile(path, raw, 0600))
	if _, err := blockHeader(path); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected declared hash mismatch, got %v", err)
	}
}

func TestBlockHeaderInvalidQuantities(t *testing.T) {
	h := &types.Header{Number: big.NewInt(42), Difficulty: big.NewInt(1)}
	raw, err := json.Marshal(h)
	require(t, err)
	for _, key := range []string{"number", "difficulty", "gasLimit", "gasUsed", "timestamp", "baseFeePerGas", "blobGasUsed", "excessBlobGas", "slotNumber"} {
		for _, value := range []string{`-1`, `1.5`, `1e0`, `"01"`, `"\u0030x01"`, `true`, `[]`, `{}`, strings.Repeat("9", 80), `"0x1` + strings.Repeat("0", 64) + `"`} {
			t.Run(key+"/"+value, func(t *testing.T) {
				var fields map[string]jsontext.Value
				require(t, json.Unmarshal(raw, &fields))
				fields[key] = jsontext.Value(value)
				data, err := json.Marshal(fields)
				require(t, err)
				path := filepath.Join(t.TempDir(), "header.json")
				require(t, os.WriteFile(path, data, 0600))
				if _, err := blockHeader(path); err == nil || strings.Contains(err.Error(), "hash mismatch") {
					t.Fatalf("expected quantity rejection before hash check, got %v", err)
				}
			})
		}
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
