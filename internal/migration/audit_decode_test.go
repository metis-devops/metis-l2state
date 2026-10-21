package migration

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAuditRequiredFields(t *testing.T) {
	f := newFixture(t, 0, 1)
	path := writeAudit(t, f, 1)
	raw, err := os.ReadFile(path)
	must(t, err)
	var original map[string]jsontext.Value
	must(t, json.Unmarshal(raw, &original))
	// The independent audit fixture omits both optional fields. Check every
	// emitted field instead of duplicating the decoder's required-field list.
	for _, object := range []string{"", "identity", "genesisState", "targetState", "totals"} {
		body := jsontext.Value(raw)
		if object != "" {
			body = original[object]
		}
		var fields map[string]jsontext.Value
		must(t, json.Unmarshal(body, &fields))
		for name, value := range fields {
			for _, mutation := range []string{"missing", "null", "type", "case", "duplicate", "escaped-duplicate"} {
				t.Run(object+"/"+name+"/"+mutation, func(t *testing.T) {
					var changed map[string]jsontext.Value
					must(t, json.Unmarshal(body, &changed))
					switch mutation {
					case "missing":
						delete(changed, name)
					case "null":
						changed[name] = jsontext.Value("null")
					case "type":
						changed[name] = jsontext.Value("[]")
					case "case":
						delete(changed, name)
						changed[strings.ToUpper(name[:1])+name[1:]] = value
					}
					invalid, err := json.Marshal(changed)
					must(t, err)
					if mutation == "duplicate" || mutation == "escaped-duplicate" {
						key := name
						if mutation == "escaped-duplicate" {
							key = fmt.Sprintf(`\u%04x%s`, name[0], name[1:])
						}
						invalid = []byte(`{"` + key + `":` + string(value) + `,` + string(invalid[1:]))
					}
					if object != "" {
						// Splice invalid JSON without re-encoding it: v2 itself
						// would reject the deliberately duplicated names.
						invalid = []byte(strings.Replace(string(raw), string(body), string(invalid), 1))
					}
					if _, err := decodeAuditJSON(invalid); err == nil {
						t.Fatal("accepted invalid audit fields")
					}
				})
			}
		}
		t.Run(object+"/unknown", func(t *testing.T) {
			invalid := `{"unknown":0,` + string(body[1:])
			if object != "" {
				invalid = strings.Replace(string(raw), string(body), invalid, 1)
			}
			if _, err := decodeAuditJSON([]byte(invalid)); err == nil {
				t.Fatal("accepted unknown audit field")
			}
		})
	}
}

func TestAuditOptionalFieldsAndDigest(t *testing.T) {
	f := newFixture(t, 0, 1)
	path := writeAudit(t, f, 1)
	raw, err := os.ReadFile(path)
	must(t, err)
	for _, fields := range []string{
		``, `"previousDiagnosisDifferenceWei":"0",`, `"matchesPreviousDiagnosis":false,`,
		`"previousDiagnosisDifferenceWei":"0","matchesPreviousDiagnosis":true,`,
	} {
		t.Run(fields, func(t *testing.T) {
			body := []byte(" \n{" + fields + string(raw[1:]) + "\n ")
			must(t, os.WriteFile(path, body, 0600))
			got, digest, err := readOVMAuditSummary(t.Context(), path)
			must(t, err)
			var want ovmAuditSummary
			must(t, json.Unmarshal(body, &want))
			if !reflect.DeepEqual(*got, want) || digest != sha256.Sum256(body) {
				t.Fatal("audit values or original-byte digest changed")
			}
		})
	}
	for _, name := range []string{"previousDiagnosisDifferenceWei", "matchesPreviousDiagnosis"} {
		for _, value := range []string{"null", "[]"} {
			if _, err := decodeAuditJSON([]byte(`{"` + name + `":` + value + `,` + string(raw[1:]))); err == nil {
				t.Fatalf("accepted optional field %s with value %s", name, value)
			}
		}
	}
}

func TestAuditNullCheckedDuringDecode(t *testing.T) {
	// Each input stops after null. Reject it at the field being decoded,
	// without first parsing the rest of the (deliberately incomplete) document.
	for _, tc := range []struct{ input, path string }{
		{`null`, ""},
		{`{"version":null,`, "/version"},
		{`{"identity":null,`, "/identity"},
		{`{"identity":{"genesis":null,`, "/identity/genesis"},
		{`{"identity":{"firstSequencer":null,`, "/identity/firstSequencer"},
		{`{"genesisState":{"balancesWei":null,`, "/genesisState/balancesWei"},
		{`{"targetState":{"balanceSlots":null,`, "/targetState/balanceSlots"},
		{`{"totals":{"gasShortfallWei":null,`, "/totals/gasShortfallWei"},
		{`{"previousDiagnosisDifferenceWei":null,`, "/previousDiagnosisDifferenceWei"},
		{`{"matchesPreviousDiagnosis":null,`, "/matchesPreviousDiagnosis"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, err := decodeAuditJSON([]byte(tc.input))
			want := "OVM audit field " + tc.path + " must not be null"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %s", err, want)
			}
		})
	}
}

func TestAuditPresentZeroValues(t *testing.T) {
	var zero ovmAuditSummary
	raw, err := json.Marshal(zero)
	must(t, err)
	got, err := decodeAuditJSON(raw)
	must(t, err)
	if !reflect.DeepEqual(got, zero) {
		t.Fatal("schema decoding changed zero values")
	}
	if err := got.validate(); err == nil {
		t.Fatal("semantic validation accepted an empty audit")
	}
}

func TestAuditEscapedFieldNames(t *testing.T) {
	f := newFixture(t, 0, 1)
	path := writeAudit(t, f, 1)
	raw, err := os.ReadFile(path)
	must(t, err)
	want, err := decodeAuditJSON(raw)
	must(t, err)
	body := []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), `"version":`, `"vers\u0069on":`), `"configHash":`, `"config\u0048ash":`))
	must(t, os.WriteFile(path, body, 0600))
	got, digest, err := readOVMAuditSummary(t.Context(), path)
	must(t, err)
	if !reflect.DeepEqual(*got, want) || digest != sha256.Sum256(body) {
		t.Fatal("escaped field names changed audit values or digest")
	}
}
