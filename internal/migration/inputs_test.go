package migration

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAllocScalarRepresentations(t *testing.T) {
	const address = "0x0000000000000000000000000000000000000101"
	const canonical = `{"` + address + `":{"balance":"0x20000000000001","nonce":"0xffffffffffffffff","code":"0x6000","storage":{"0x01":"0x02"}}}`
	path := filepath.Join(t.TempDir(), "alloc")
	must(t, os.WriteFile(path, []byte(canonical), 0600))
	want, _, err := readAlloc(t.Context(), path)
	must(t, err)
	for name, body := range map[string]string{
		"numeric":                  `{"` + address + `":{"balance":9007199254740993,"nonce":18446744073709551615,"code":"0x6000","storage":{"0x01":"0x02"}}}`,
		"escaped-keys-and-storage": `{"\u0030x0000000000000000000000000000000000000101":{"bal\u0061nce":"0x20000000000001","nonce":"0xffffffffffffffff","code":"0x6000","storage":{"\u0030x01":"\u0030x02"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			must(t, os.WriteFile(path, []byte(body), 0600))
			got, digest, err := readAlloc(t.Context(), path)
			must(t, err)
			if !reflect.DeepEqual(got, want) || digest != sha256.Sum256([]byte(body)) {
				t.Fatal("alloc values or original-byte digest changed")
			}
		})
	}
	for name, fields := range map[string]string{
		"escaped-duplicate": `"nonce":1,"n\u006fnce":2`,
		"case":              `"Balance":1`,
		"overflow":          `"nonce":18446744073709551616`,
		"fraction":          `"balance":1.5`,
		"exponent":          `"nonce":1e0`,
		"container":         `"code":[]`,
		"null-storage":      `"storage":null`,
		"oversize-code":     `"code":"0x` + strings.Repeat("00", ovmAllocMaxCode+1) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			must(t, os.WriteFile(path, []byte(`{"`+address+`":{`+fields+`}}`), 0600))
			if _, _, err := readAlloc(t.Context(), path); err == nil {
				t.Fatal("accepted invalid alloc")
			}
		})
	}
}

func TestAllocRejectsInvalidJSONText(t *testing.T) {
	const address = "0x0000000000000000000000000000000000000101"
	for name, body := range map[string]string{
		"address":       "{\"\xff\":{}}",
		"field":         `{"` + address + `":{` + "\"\xff\":0}}",
		"code":          `{"` + address + `":{"code":` + "\"0x\xff\"}}",
		"storage-key":   `{"` + address + `":{"storage":{` + "\"\xff\":\"0x00\"}}}",
		"storage-value": `{"` + address + `":{"storage":{"0x00":` + "\"\xff\"}}}",
		"surrogate":     `{"` + address + `":{"code":"\ud800"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "alloc")
			must(t, os.WriteFile(path, []byte(body), 0600))
			_, _, err := readAlloc(t.Context(), path)
			if _, ok := errors.AsType[*jsontext.SyntacticError](err); !ok {
				t.Fatalf("expected JSON syntax rejection before hex decoding, got %v", err)
			}
		})
	}
}

func TestAuditJSONFieldValidation(t *testing.T) {
	f := newFixture(t, 0, 1)
	path := writeAudit(t, f, 1)
	raw, err := os.ReadFile(path)
	must(t, err)
	_, err = decodeAuditJSON(raw)
	must(t, err)
	for name, body := range map[string]string{
		"missing":           strings.Replace(string(raw), `"version":1,`, "", 1),
		"case":              strings.Replace(string(raw), `"version":`, `"Version":`, 1),
		"nested-case":       strings.Replace(string(raw), `"configHash":`, `"ConfigHash":`, 1),
		"nested-duplicate":  strings.Replace(string(raw), `"identity":{`, `"identity":{"version":1,`, 1),
		"escaped-duplicate": `{"vers\u0069on":1,` + string(raw[1:]),
		"unknown":           `{"extra":0,` + string(raw[1:]),
		"null-optional":     `{"previousDiagnosisDifferenceWei":null,` + string(raw[1:]),
		"null-required":     strings.Replace(string(raw), `"verifiedBlocks":1`, `"verifiedBlocks":null`, 1),
		"trailing":          string(raw) + `{}`,
		"null":              `null`,
		"invalid-utf8":      strings.Replace(string(raw), `"clientVersion":"test"`, "\"clientVersion\":\"\xff\"", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeAuditJSON([]byte(body)); err == nil {
				t.Fatal("accepted invalid audit schema")
			}
		})
	}
	// Invalid provenance text must fail both schema and typed decoding.
	body := strings.Replace(string(raw), `"clientVersion":"test"`, "\"clientVersion\":\"\xff\"", 1)
	must(t, os.WriteFile(path, []byte(body), 0600))
	if _, _, err := readOVMAuditSummary(t.Context(), path); err == nil {
		t.Fatal("accepted invalid UTF-8 audit provenance")
	}
}

func TestStrictAllocInputs(t *testing.T) {
	address := "0x0000000000000000000000000000000000000101"
	for _, body := range []string{"null", `{}` + `{}`, `{"` + address + `":null}`, `{"` + address + `":{"balance":null}}`, `{"` + address + `":{"nonce":"0x1","nonce":"0x2"}}`, `{"` + address + `":{"unknown":1}}`, `{"` + address + `":{"storage":{"0x01":"0x01","01":"0x02"}}}`, `{"` + address + `":{},"` + address + `":{}}`, `{"` + ovmAddress.Hex() + `":{}}`, `{"` + address + `":{"balance":"-1"}}`, `{"` + address + `":{"storage":{"0x1":"0x01"}}}`} {
		path := filepath.Join(t.TempDir(), "alloc")
		must(t, os.WriteFile(path, []byte(body), 0600))
		if _, _, err := readAlloc(t.Context(), path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	path := filepath.Join(t.TempDir(), "alloc")
	body := strings.Repeat(" \n", 100000) + `{"` + address + `":{"balance":"0x0","code":"0x","nonce":"0x0","storage":{}}}` + strings.Repeat(" \n", 100000)
	must(t, os.WriteFile(path, []byte(body), 0600))
	patches, digest, err := readAlloc(t.Context(), path)
	must(t, err)
	got := sha256.Sum256([]byte(body))
	if digest != got || len(patches) != 1 {
		t.Fatal("raw-byte hashing")
	}
	for _, p := range patches {
		if !p.active() || p.Balance == nil || p.Nonce == nil || p.Code == nil {
			t.Fatal("explicit zero lost")
		}
	}
}
func TestStrictRetainAndAudit(t *testing.T) {
	for _, body := range []string{"\n", "# comment", eoa.Hex() + "\n" + eoa.Hex(), strings.Repeat("x", 4096)} {
		path := filepath.Join(t.TempDir(), "retain")
		must(t, os.WriteFile(path, []byte(body), 0600))
		if _, _, err := readRetain(t.Context(), path); err == nil {
			t.Fatal("accepted invalid retain list")
		}
	}
	f := newFixture(t, 0, 1)
	path := writeAudit(t, f, 1)
	raw, err := os.ReadFile(path)
	must(t, err)
	for _, bad := range [][]byte{append([]byte(`{"version":1,`), raw[1:]...), []byte(strings.Replace(string(raw), `"unexplainedEvents":0`, `"unexplainedEvents":1`, 1)), []byte(strings.Replace(string(raw), `"version":1`, `"version":null`, 1))} {
		must(t, os.WriteFile(path, bad, 0600))
		if _, _, err := readOVMAuditSummary(t.Context(), path); err == nil {
			t.Fatal("accepted invalid audit")
		}
	}
}
func TestInputSymlink(t *testing.T) {
	f := newFixture(t, 0, 0)
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(f.runtime, link))
	if _, err := openInput(link); err == nil {
		t.Fatal("accepted symlink")
	}
}
func FuzzAllocWord(f *testing.F) {
	f.Add("0x00")
	f.Add("1234")
	f.Fuzz(func(t *testing.T, s string) { _, _ = allocWord(s) })
}
func FuzzAudit(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			t.Skip()
		}
		if a, err := decodeAuditJSON(b); err == nil {
			_ = a.validate()
		}
	})
}
func TestCancelledInputParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alloc")
	must(t, os.WriteFile(path, []byte(`{}`), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := readAlloc(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInputSnapshotDigests(t *testing.T) {
	f := newFixture(t, 0, 0)
	dir := t.TempDir()
	o := f.options(filepath.Join(dir, "out"))
	o.RetainList, o.Alloc = filepath.Join(dir, "retain"), filepath.Join(dir, "alloc")
	o.AuditSummary = writeAudit(t, f, 0)
	must(t, os.WriteFile(o.RetainList, []byte(eoa.Hex()+"\n"), 0600))
	must(t, os.WriteFile(o.Alloc, []byte(" \n{}\n "), 0600))
	paths := map[string]string{"ovm-events": o.OVMEvents, "runtime": o.WrappedEtherCode, "retain": o.RetainList, "alloc": o.Alloc, "audit": o.AuditSummary}
	in, err := loadInputs(t.Context(), o)
	must(t, err)
	digests := in.digests()
	if len(digests) != len(paths) {
		t.Fatal("missing input digest")
	}
	previous := ""
	for i, digest := range digests {
		data, err := os.ReadFile(paths[digest.Name])
		must(t, err)
		if digest.SHA256 != sha256.Sum256(data) || digest.Name <= previous {
			t.Fatal("digest does not represent original bytes in canonical order")
		}
		previous = digest.Name
		digests[i].SHA256[0] ^= 1
	}
	for i, digest := range in.digests() {
		if digest == digests[i] {
			t.Fatal("digest consumer mutated the input snapshot")
		}
	}
}
