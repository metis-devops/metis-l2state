package migration

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
)

type accountPatch struct {
	Address common.Address
	Balance *uint256.Int
	Nonce   *uint64
	Code    *[]byte
	Storage map[common.Hash]common.Hash
}

func (p *accountPatch) active() bool {
	return p.Balance != nil || p.Nonce != nil || p.Code != nil || len(p.Storage) != 0
}

type inputs struct {
	events  *ovmevents.Validated
	runtime []byte
	retain  map[common.Address]struct{}
	alloc   map[common.Hash]*accountPatch
	audit   *ovmAuditSummary
	hashes  []statefile.InputDigest
}

func openInput(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("input must be a regular non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.Join(errors.New("input changed while opening"), err, f.Close())
	}
	return f, nil
}
func readRuntime(path string) (code []byte, digest common.Hash, retErr error) {
	f, err := openInput(path)
	if err != nil {
		return nil, digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, digest, err
	}
	if len(raw) > 1<<20 {
		return nil, digest, errors.New("runtime input exceeds 1 MiB")
	}
	code, err = hexutil.Decode(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, digest, err
	}
	if len(code) == 0 {
		return nil, digest, errors.New("empty wrapped runtime")
	}
	return code, common.Hash(sha256.Sum256(raw)), nil
}
func readRetain(ctx context.Context, path string) (set map[common.Address]struct{}, digest common.Hash, retErr error) {
	f, err := openInput(path)
	if err != nil {
		return nil, digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	h := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(f, h))
	scanner.Buffer(make([]byte, 4096), 4096)
	set = make(map[common.Address]struct{})
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, digest, err
		}
		text := strings.TrimSpace(scanner.Text())
		var a common.Address
		if len(text) != 42 || !strings.HasPrefix(text, "0x") {
			return nil, digest, errors.New("invalid retain address")
		}
		if err := a.UnmarshalText([]byte(text)); err != nil {
			return nil, digest, err
		}
		if a == ovmAddress {
			return nil, digest, errors.New("retain list cannot include OVM_ETH")
		}
		if _, ok := set[a]; ok {
			return nil, digest, errors.New("duplicate retain address")
		}
		set[a] = struct{}{}
	}
	return set, common.BytesToHash(h.Sum(nil)), scanner.Err()
}
func loadInputs(ctx context.Context, o Options) (*inputs, error) {
	i := &inputs{retain: make(map[common.Address]struct{}), alloc: make(map[common.Hash]*accountPatch)}
	code, d, err := readRuntime(o.WrappedEtherCode)
	if err != nil {
		return nil, err
	}
	i.hashes = append(i.hashes, statefile.InputDigest{Name: "runtime", SHA256: d})
	i.events, d, err = ovmevents.ReadValidatedFile(ctx, o.OVMEvents)
	if err != nil {
		return nil, err
	}
	i.hashes = append(i.hashes, statefile.InputDigest{Name: "ovm-events", SHA256: d})
	i.runtime = code
	if o.RetainList != "" {
		i.retain, d, err = readRetain(ctx, o.RetainList)
		if err != nil {
			return nil, err
		}
		i.hashes = append(i.hashes, statefile.InputDigest{Name: "retain", SHA256: d})
	}
	if o.Alloc != "" {
		i.alloc, d, err = readAlloc(ctx, o.Alloc)
		if err != nil {
			return nil, err
		}
		i.hashes = append(i.hashes, statefile.InputDigest{Name: "alloc", SHA256: d})
	}
	if o.AuditSummary != "" {
		i.audit, d, err = readOVMAuditSummary(ctx, o.AuditSummary)
		if err != nil {
			return nil, err
		}
		i.hashes = append(i.hashes, statefile.InputDigest{Name: "audit", SHA256: d})
	}
	slices.SortFunc(i.hashes, func(a, b statefile.InputDigest) int { return strings.Compare(a.Name, b.Name) })
	return i, nil
}

// digests describes the exact bytes parsed initially, independent of later paths.
func (i *inputs) digests() []statefile.InputDigest {
	return slices.Clone(i.hashes)
}

func (s *source) validateAudit(a *ovmAuditSummary) error {
	if a == nil {
		return nil
	}
	if err := a.matchHead(s.head); err != nil {
		return err
	}
	for _, point := range []struct {
		number     uint64
		hash, root common.Hash
	}{{0, a.Identity.Genesis, a.Genesis.Root}, {a.Identity.To, a.Identity.ToHash, a.Target.Root}} {
		h, raw, err := readHeader(s.db, point.number)
		if err != nil {
			return fmt.Errorf("audit identity requires hot canonical/header evidence: %w", err)
		}
		if h.Root != point.root || h.Hash() != point.hash || len(raw) == 0 {
			return errors.New("audit canonical identity mismatch")
		}
	}
	return nil
}
