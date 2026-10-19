package migration

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/metis-devops/metis-l2state/statefile"
)

func stringWord(length uint64) common.Hash {
	n := new(big.Int).SetUint64(length)
	return common.BigToHash(n.Add(n.Lsh(n, 1), big.NewInt(1)))
}

func TestProductionWalkEntrypointsRejectInvalidPlans(t *testing.T) {
	s := &source{}
	write := func(statefile.Record) error { t.Fatal("invalid plan reached writer"); return nil }
	if _, err := s.writeMigration(t.Context(), 2, make(limiter, 2), nil, nil, write); err == nil {
		t.Fatal("accepted missing validation")
	}
	if _, err := s.writeDump(t.Context(), 2, make(limiter, 2), nil); err == nil {
		t.Fatal("accepted missing writer")
	}
	for _, invalid := range []string{"source", "head", "policy"} {
		prepared := &source{memory: newStateSnapshot()}
		proof := &sourceValidation{source: prepared, head: prepared.head, policy: ovmSource}
		switch invalid {
		case "source":
			proof.source = s
		case "head":
			proof.head.BlockNumber++
		case "policy":
			proof.policy = rawSource
		}
		_, err := prepared.writeMigration(t.Context(), 2, make(limiter, 2), &indexes{validation: proof}, nil, write)
		if err == nil || !strings.Contains(err.Error(), "source validation") {
			t.Fatalf("accepted unrelated %s proof: %v", invalid, err)
		}
	}

	for _, r := range []walkRequest{
		{plan: walkPlan{mode: convertState}, write: write},
		{plan: walkPlan{mode: validateSource}, write: write},
		{plan: walkPlan{mode: overlayState, policy: rawSource}, alloc: map[common.Hash]*accountPatch{{}: {}}, write: write},
		{plan: walkPlan{mode: convertState, policy: rawSource, reuseSource: true}, write: write},
	} {
		if _, err := s.runWalk(t.Context(), 2, make(limiter, 2), r); err == nil {
			t.Fatal("accepted invalid execution plan")
		}
	}
}

func TestAccountRootErrorHasIdentityAndStage(t *testing.T) {
	f := newFixture(t, 3, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	_, err = s.prepareSnapshot(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	hash := crypto.Keccak256Hash(ordinary[:])
	a, ok := s.memory.find(hash)
	if !ok {
		t.Fatal("missing account")
	}
	original := s.memory.account(a)
	a.storage.Set(sortedHashes(treeMap(&a.storage))[0], common.HexToHash("0xffff"))
	err = s.processAccount(t.Context(), accountJob{hash: hash, original: original}, make(limiter, 2), nil, walkPlan{mode: validateSource}, func(accountEvent) error { return nil })
	for _, text := range []string{hash.Hex(), "source validation", "source storage root mismatch", "expected " + a.storageRoot().Hex(), "actual "} {
		if err == nil || !strings.Contains(err.Error(), text) {
			t.Fatalf("missing %q: %v", text, err)
		}
	}
}

func TestExportSessionSourceCloseFailurePreventsPublication(t *testing.T) {
	f, _ := newDumpFixture(t, 3)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	p, err := newPublication(f.source, filepath.Join(t.TempDir(), "out"))
	must(t, err)
	fault := errors.New("source close failed")
	db := &sourceFaultDB{Database: s.db, closeErr: fault}
	s.db = db
	genesis, err := s.genesisIdentity()
	must(t, err)
	w, err := statefile.NewWriter(t.Context(), p.file, s.header, statefile.Metadata{ToolVersion: "test", Genesis: genesis}, "none")
	must(t, err)
	e := exportSession{source: s, publication: p, writer: w}
	defer func() { must(t, e.abort()) }()
	walk, err := s.writeDump(t.Context(), 2, make(limiter, 2), w.WriteRecord)
	must(t, err)
	must(t, s.writeCodes(t.Context(), nil, &walk, w))
	summary := statefile.Summary{SourceRoot: s.head.StateRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot, Counts: walk.counts, ConvertedCounts: walk.convertedCounts, Conversion: statefile.Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"}}
	if err := e.finish(t.Context(), summary); !errors.Is(err, fault) {
		t.Fatalf("lost close failure: %v", err)
	}
	if p.file != nil || e.source != nil || p.published || db.closes != 1 {
		t.Fatal("invalid finalization order/ownership")
	}
	must(t, e.abort())
	if db.closes != 1 {
		t.Fatal("source closed twice")
	}
	if _, err := os.Stat(p.final); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published despite close failure")
	}
}
