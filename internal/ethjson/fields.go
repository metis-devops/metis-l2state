// Package ethjson decodes geth wire types with streaming JSON v2 validation.
package ethjson

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// objectFields records required (true) and optional or observed (false) fields.
// It is local to one decoding operation; jsontext checks duplicate names.
type objectFields map[string]bool

// Unknown values are validated and skipped without retaining their JSON bodies.
type discardedField struct{}

func (fields objectFields) unmarshalers() *json.Unmarshalers {
	return json.UnmarshalFromFunc(func(d *jsontext.Decoder, value any) error {
		path := d.StackPointer()
		if path == "" && d.PeekKind() != jsontext.KindBeginObject {
			return errors.New("expected JSON object")
		}
		if path != "" && path.Parent() == "" {
			name := path.LastToken()
			if fields[name] {
				if d.PeekKind() == jsontext.KindNull {
					return fmt.Errorf("null field %s", name)
				}
				fields[name] = false
			}
			if _, unknown := value.(*discardedField); unknown {
				for canonical := range fields {
					if strings.EqualFold(name, canonical) {
						return fmt.Errorf("non-canonical field %q; expected %q", name, canonical)
					}
				}
				return d.SkipValue()
			}
		}
		return errors.ErrUnsupported
	})
}

func (fields objectFields) check() error {
	for name, missing := range fields {
		if missing {
			return fmt.Errorf("missing field %s", name)
		}
	}
	return nil
}
