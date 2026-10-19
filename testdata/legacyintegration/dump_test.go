package legacyintegration

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/core"
	"github.com/MetisProtocol/mvm/l2geth/core/rawdb"
	"github.com/MetisProtocol/mvm/l2geth/core/types"
	"github.com/MetisProtocol/mvm/l2geth/params"
	"github.com/MetisProtocol/mvm/l2geth/rlp"
	"github.com/MetisProtocol/mvm/l2geth/rollup/rcfg"
)

func TestDumpLegacyState(t *testing.T) {
	dir := t.TempDir()
	binary := build(t, "./cmd/l2state", dir)
	reader := build(t, "./testdata/readercheck", dir)
	previous := rcfg.UsingOVM
	rcfg.UsingOVM = false
	defer func() { rcfg.UsingOVM = previous }()
	for _, withOVM := range []bool{false, true} {
		name := "native"
		if withOVM {
			name = "ovm-storage"
		}
		t.Run(name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			header, alloc := dumpLegacySource(t, source, withOVM)
			for _, compression := range []string{"none", "zstd"} {
				var first []byte
				for _, workers := range []string{"2", "8"} {
					out := filepath.Join(t.TempDir(), "state")
					args := []string{"dump", "--source-chaindata", source, "--out", out, "--compression", compression, "--workers", workers, "--cache-mb", "16", "--handles", "16"}
					quiet := workers == "2"
					if quiet {
						args = append(args, "--quiet")
					}
					cmd := exec.Command(binary, args...)
					var stderr bytes.Buffer
					cmd.Stderr = &stderr
					result, err := cmd.Output()
					if err != nil {
						t.Fatalf("dump: %s %s %v", result, stderr.String(), err)
					}
					if !json.Valid(result) {
						t.Fatal("stdout is not standalone JSON")
					}
					if quiet && stderr.Len() != 0 {
						t.Fatal("quiet emitted progress")
					}
					if !quiet && !bytes.Contains(stderr.Bytes(), []byte("Dump completed")) {
						t.Fatal("missing stderr progress")
					}
					data, err := os.ReadFile(out)
					check(t, err)
					if first == nil {
						first = data
					} else if !bytes.Equal(first, data) {
						t.Fatal("workers changed dump")
					}
					raw, err := exec.Command(reader, out).Output()
					check(t, err)
					var d document
					check(t, json.Unmarshal(raw, &d))
					original, err := rlp.EncodeToBytes(header)
					check(t, err)
					if !bytes.Equal(d.Header, original) || d.Root != header.Root || len(d.Accounts) != len(alloc) {
						t.Fatal("dump changed source header, root or accounts")
					}
					st := rebuildLegacyState(t, d)
					for address, want := range alloc {
						if st.GetBalance(address).Cmp(want.Balance) != 0 || st.GetNonce(address) != want.Nonce || !bytes.Equal(st.GetCode(address), want.Code) {
							t.Fatalf("changed legacy account %s", address)
						}
						if len(d.Accounts[address].Storage) != len(want.Storage) {
							t.Fatal("changed storage count")
						}
						for slot, value := range want.Storage {
							if st.GetState(address, slot) != value {
								t.Fatalf("changed legacy storage %s/%s", address, slot)
							}
						}
					}
				}
			}
		})
	}
}

func dumpLegacySource(t *testing.T, path string, withOVM bool) (*types.Header, core.GenesisAlloc) {
	t.Helper()
	db, err := rawdb.NewLevelDBDatabase(path, 16, 16, "dump-integration")
	check(t, err)
	storage := map[common.Hash]common.Hash{common.HexToHash("0x99"): common.HexToHash("0x2345")}
	alloc := core.GenesisAlloc{
		holder:    {Balance: big.NewInt(123456), Nonce: 3},
		retained:  {Balance: big.NewInt(789), Nonce: 7, Code: []byte{0x60, 0x42, 0}, Storage: storage},
		converted: {Balance: new(big.Int), Code: []byte{0x60, 0x42, 0}},
	}
	if withOVM {
		alloc[ovm] = core.GenesisAccount{Balance: big.NewInt(987), Code: []byte{0}, Storage: map[common.Hash]common.Hash{
			common.HexToHash("0x02"): common.HexToHash("0x01"),
			balanceSlot(holder):      common.HexToHash("0x42"),
			common.HexToHash("0x99"): common.HexToHash("0xabc"),
		}}
	}
	g := &core.Genesis{Config: params.AllEthashProtocolChanges, GasLimit: 10000000, Difficulty: big.NewInt(1), Alloc: alloc}
	block := g.MustCommit(db)
	check(t, db.Close())
	return block.Header(), alloc
}
