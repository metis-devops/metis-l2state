package migration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestRetainCannotRepairMissingSourceIdentity(t *testing.T) {
	f := newFixture(t, 0, 0)
	mutate(t, f, func(db *leveldb.DB) {
		h := crypto.Keccak256Hash(ordinary[:])
		must(t, db.Delete(append([]byte(preimageKeyPrefix), h[:]...), nil))
	})
	dir := t.TempDir()
	retain := filepath.Join(dir, "retain")
	must(t, os.WriteFile(retain, []byte(ordinary.Hex()+"\n"), 0600))
	o := f.options(filepath.Join(dir, "state"))
	o.RetainList = retain
	if _, err := Migrate(t.Context(), o); err == nil {
		t.Fatal("retain-list supplied missing source address identity")
	}
	if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published invalid source")
	}
	partials, err := filepath.Glob(filepath.Join(dir, ".*.partial-*"))
	must(t, err)
	if len(partials) != 0 {
		t.Fatal("leaked partial output")
	}
}
