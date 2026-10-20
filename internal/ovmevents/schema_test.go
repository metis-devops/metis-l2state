package ovmevents

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestFrozenCheckpointV1(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1.json")
	check(t, err)
	d, err := Decode(t.Context(), bytes.NewReader(raw))
	check(t, err)
	var out bytes.Buffer
	check(t, Encode(&out, sample()))
	if !bytes.Equal(raw, out.Bytes()) {
		t.Fatal("v1 checkpoint encoding changed; review the frozen fixture explicitly")
	}
	out.Reset()
	check(t, Encode(&out, d))
	if !bytes.Equal(raw, out.Bytes()) {
		t.Fatal("checkpoint round trip changed bytes")
	}
	// Independently calculated from the v1 length-framed records, with an empty
	// value for each member address.
	if d.IndexDigest() != common.HexToHash("0x86dd1a56c01a01a1e81c5c7e6c06535f619ceead2e72a198c3d61f97c18711ab") {
		t.Fatal("normalized index digest changed")
	}
	a := d.Allowances[0]
	if AllowanceKey(a.Owner, a.Spender) != common.HexToHash("0xbd5f1d126b3cc81f576b3819604a11afeec618a5780643f64f54c80b19f2f88e") {
		t.Fatal("derived secure trie key changed")
	}
}

func TestCheckpointRejectsRemovedFields(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1.json")
	check(t, err)
	for field, value := range map[string]any{
		"source":         "rpc-logs",
		"contract":       Contract,
		"allowanceSlot":  1,
		"transferTopic":  TransferTopic,
		"approvalTopic":  ApprovalTopic,
		"storageKeyHash": AllowanceKey(sample().Allowances[0].Owner, sample().Allowances[0].Spender),
	} {
		t.Run(field, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			check(t, err)
			target := `"version":`
			hostile := strings.Replace(string(raw), target, `"`+field+`":`+string(encoded)+`,`+target, 1)
			if _, err := Decode(t.Context(), strings.NewReader(hostile)); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("removed field accepted or rejected for wrong reason: %v", err)
			}
		})
	}
}

func TestAllowanceTupleStrictness(t *testing.T) {
	d := sample()
	a := d.Allowances[0]
	for name, value := range map[string]any{
		"null":            nil,
		"empty":           []any{},
		"missing-spender": []any{a.Owner},
		"extra-address":   []any{a.Owner, a.Spender, a.Owner},
		"extra-null":      []any{a.Owner, a.Spender, nil},
		"null-owner":      []any{nil, a.Spender},
		"null-spender":    []any{a.Owner, nil},
		"short-owner":     []any{"0x01", a.Spender},
		"short-spender":   []any{a.Owner, "0x02"},
		"numeric-owner":   []any{1, a.Spender},
		"numeric-spender": []any{a.Owner, 2},
		"nested-owner":    []any{[]any{a.Owner}, a.Spender},
		"nested-spender":  []any{a.Owner, []any{a.Spender}},
		"address":         a.Owner,
		"old-object":      map[string]any{"owner": a.Owner, "spender": a.Spender},
		"old-derived-key": map[string]any{"owner": a.Owner, "spender": a.Spender, "storageKeyHash": AllowanceKey(a.Owner, a.Spender)},
	} {
		t.Run(name, func(t *testing.T) {
			doc := map[string]any{"version": d.Version, "chainId": d.ChainID, "genesis": d.Genesis, "progress": d.Progress, "allowances": []any{value}, "members": d.Members}
			raw, err := json.Marshal(doc)
			check(t, err)
			if _, err := Decode(t.Context(), bytes.NewReader(raw)); err == nil {
				t.Fatal("accepted malformed allowance tuple")
			}
		})
	}
}

func TestAllowanceDerivedOrdering(t *testing.T) {
	d := sample()
	a, b := d.Allowances[0].Owner, d.Allowances[0].Spender
	d.Allowances = []Allowance{{a, b}, {b, a}, {a, a}, {b, b}}
	original := slices.Clone(d.Allowances)
	d.Sort()
	check(t, d.Validate())
	for _, v := range original {
		if !slices.Contains(d.Allowances, v) {
			t.Fatal("sort detached a relationship from its key")
		}
	}
	var canonical bytes.Buffer
	check(t, Encode(&canonical, d))
	digest := d.IndexDigest()
	slices.Reverse(d.Allowances)
	raw, err := json.Marshal(d)
	check(t, err)
	if _, err := Decode(t.Context(), bytes.NewReader(raw)); err == nil {
		t.Fatal("accepted allowances out of derived-key order")
	}
	d.Sort()
	var reordered bytes.Buffer
	check(t, Encode(&reordered, d))
	if !bytes.Equal(canonical.Bytes(), reordered.Bytes()) || d.IndexDigest() != digest {
		t.Fatal("input ordering changed checkpoint or digest")
	}
	d.Allowances = append(d.Allowances, d.Allowances[0])
	d.Sort()
	raw, err = json.Marshal(d)
	check(t, err)
	if _, err := Decode(t.Context(), bytes.NewReader(raw)); err == nil {
		t.Fatal("accepted duplicate relationship")
	}
}

func TestMemberAddressSet(t *testing.T) {
	a, b := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	d := sample()
	d.Members = []common.Address{b, a, {}}
	d.Sort()
	check(t, d.Validate())
	if !slices.Equal(d.Members, []common.Address{{}, a, b}) {
		t.Fatal("members are not sorted by address")
	}
	var encoded bytes.Buffer
	check(t, Encode(&encoded, d))
	got, err := Decode(t.Context(), bytes.NewReader(encoded.Bytes()))
	check(t, err)
	if !slices.Equal(got.Members, d.Members) || got.IndexDigest() != d.IndexDigest() {
		t.Fatal("address set round trip changed evidence")
	}
	for name, members := range map[string]any{
		"duplicate":       []common.Address{a, a},
		"unordered":       []common.Address{b, a},
		"null-array":      nil,
		"null-address":    []any{nil},
		"short-address":   []string{"0x01"},
		"numeric-address": []int{1},
		"address-object":  []any{map[string]any{"address": a}},
		"old-member":      []any{map[string]any{"address": a, "blockNumber": 1, "blockHash": common.HexToHash("0x34")}},
	} {
		t.Run(name, func(t *testing.T) {
			doc := map[string]any{"version": d.Version, "chainId": d.ChainID, "genesis": d.Genesis, "progress": d.Progress, "allowances": d.Allowances, "members": members}
			raw, err := json.Marshal(doc)
			check(t, err)
			if _, err := Decode(t.Context(), bytes.NewReader(raw)); err == nil {
				t.Fatal("accepted malformed member set")
			}
		})
	}
}

func TestMemberSetDigestBindsIdentityAndAddresses(t *testing.T) {
	for name, modify := range map[string]func(*Document){
		"genesis":        func(d *Document) { d.Genesis = common.HexToHash("0x1235") },
		"height":         func(d *Document) { d.Progress.BlockNumber++ },
		"head-hash":      func(d *Document) { d.Progress.BlockHash = common.HexToHash("0xabce") },
		"member-address": func(d *Document) { d.Members[0] = common.HexToAddress("0x02") },
		"empty-set":      func(d *Document) { d.Members = []common.Address{} },
	} {
		t.Run(name, func(t *testing.T) {
			d := sample()
			before := d.IndexDigest()
			modify(d)
			check(t, d.Validate())
			if d.IndexDigest() == before {
				t.Fatal("digest did not bind changed evidence")
			}
		})
	}
}

func TestCheckpointProgressRemainsStrict(t *testing.T) {
	for name, change := range map[string]func(*Document){
		"empty-hash":       func(d *Document) { d.Progress.BlockHash = common.Hash{} },
		"genesis-mismatch": func(d *Document) { d.Progress.BlockNumber = 0 },
		"empty-genesis":    func(d *Document) { d.Genesis = common.Hash{} },
	} {
		t.Run(name, func(t *testing.T) {
			d := sample()
			change(d)
			raw, err := json.Marshal(d)
			check(t, err)
			if _, err := Decode(t.Context(), bytes.NewReader(raw)); err == nil {
				t.Fatal("accepted invalid checkpoint binding")
			}
		})
	}
}
