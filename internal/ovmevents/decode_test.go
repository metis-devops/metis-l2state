package ovmevents

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ethereum/go-ethereum/common"
)

// Keep v1 as an independent typed-decoding reference, including geth's
// rejection of JSON escapes inside address and hash strings.
func TestScalarDecodingParity(t *testing.T) {
	inputs := []string{
		`0`, `1`, `9007199254740993`, `18446744073709551615`,
		`18446744073709551616`, `-1`, `-0`, `1.0`, `1e0`,
		`"1"`, `""`, `"1088"`, `"10\u00388"`, `"a\"b\\c\n"`,
		`true`, `false`, `null`, `[]`, `{}`,
	}
	for _, width := range []int{20, 32} {
		hex := strings.Repeat("aB", width)
		inputs = append(inputs,
			`"0x`+hex+`"`,
			`"0X`+hex+`"`,
			`"\u0030x`+hex+`"`,
			`"0x`+hex[:len(hex)-1]+`\u0042"`,
			`"`+hex+`"`,
			`"0x`+hex[:len(hex)-1]+`"`,
			`"0x`+hex+`00"`,
			`"0x`+hex[:len(hex)-1]+`g"`,
		)
	}
	for _, raw := range inputs {
		t.Run(raw, func(t *testing.T) {
			checkScalarParity[uint64](t, raw)
			checkScalarParity[string](t, raw)
			checkScalarParity[common.Address](t, raw)
			checkScalarParity[common.Hash](t, raw)
		})
	}
}

func checkScalarParity[T uint64 | string | common.Address | common.Hash](t *testing.T, raw string) {
	t.Helper()
	var want, got T
	wantErr := jsonv1.Unmarshal([]byte(raw), &want)
	wantRejected := raw == "null" || wantErr != nil

	decoder := jsontext.NewDecoder(strings.NewReader("[" + raw + ",true]"))
	check(t, delim(decoder, jsontext.KindBeginArray))
	gotErr := scalar(decoder, &got)()
	if (gotErr != nil) != wantRejected {
		t.Fatalf("%T input %s: error %v, want rejected=%v", got, raw, gotErr, wantRejected)
	}
	if wantRejected {
		return
	}
	if got != want {
		t.Fatalf("%T input %s: got %v, want %v", got, raw, got, want)
	}
	// A scalar read must leave the next array element available.
	next, err := decoder.ReadToken()
	check(t, err)
	if next.Kind() != jsontext.KindTrue {
		t.Fatalf("scalar consumed the next element: %v", next)
	}
	check(t, delim(decoder, jsontext.KindEndArray))
}

func TestDecodeRequiredFields(t *testing.T) {
	raw, err := json.Marshal(sample())
	check(t, err)
	for _, path := range []string{
		"version", "chainId", "genesis", "progress", "allowances", "members",
		"progress.blockNumber", "progress.blockHash",
	} {
		for _, missing := range []bool{false, true} {
			name := path + "/null"
			if missing {
				name = path + "/missing"
			}
			t.Run(name, func(t *testing.T) {
				var fields map[string]jsontext.Value
				check(t, json.Unmarshal(raw, &fields))
				target := fields
				key := path
				if field, nested := strings.CutPrefix(path, "progress."); nested {
					key = field
					target = nil
					check(t, json.Unmarshal(fields["progress"], &target))
				}
				if missing {
					delete(target, key)
				} else {
					target[key] = jsontext.Value("null")
				}
				if key != path {
					fields["progress"], err = json.Marshal(target)
					check(t, err)
				}
				invalid, err := json.Marshal(fields)
				check(t, err)
				if _, err := Decode(t.Context(), strings.NewReader(string(invalid))); err == nil {
					t.Fatal("accepted missing or null field")
				}
			})
		}
	}
}

func TestDecodeNestedFieldStrictness(t *testing.T) {
	raw, err := json.Marshal(sample())
	check(t, err)
	for _, replacement := range []string{
		`"BlockNumber":2`,
		`"other":2`,
		`"blockNumber":2,"blockNumber":2`,
		`"blockNumber":2,"block\u004eumber":2`,
	} {
		t.Run(replacement, func(t *testing.T) {
			invalid := strings.Replace(string(raw), `"blockNumber":2`, replacement, 1)
			if _, err := Decode(t.Context(), strings.NewReader(invalid)); err == nil {
				t.Fatal("accepted non-canonical or duplicate nested field")
			}
		})
	}
}

func TestDecodeFragmentedAndBounded(t *testing.T) {
	raw, err := json.Marshal(sample())
	check(t, err)
	got, err := Decode(t.Context(), iotest.OneByteReader(strings.NewReader(string(raw))))
	check(t, err)
	if got.IndexDigest() != sample().IndexDigest() {
		t.Fatal("fragmented reads changed checkpoint")
	}
	for _, prefix := range []string{
		`{"genesis":`,
		`{"progress":{"blockHash":`,
		`{"allowances":[[`,
		`{"members":[`,
	} {
		t.Run(prefix, func(t *testing.T) {
			// An oversized container of small tokens must be rejected before
			// geth's UnmarshalJSON can buffer the whole value.
			input := strings.NewReader(prefix + `[` + strings.Repeat(`0,`, 10000) + `0]`)
			size := input.Len()
			if _, err := Decode(t.Context(), input); err == nil || !strings.Contains(err.Error(), "expected scalar") {
				t.Fatalf("wrong container rejection: %v", err)
			}
			if consumed := size - input.Len(); consumed > 4096 {
				t.Fatalf("buffered %d bytes before rejecting container", consumed)
			}
		})
	}
}
