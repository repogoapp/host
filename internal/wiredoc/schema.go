package wiredoc

import (
	"encoding"
	"encoding/json"
	"reflect"
)

// Schema is t as JSON Schema, for an agent filling in a method's params: the
// keys Fields walks, each with its doc tag. Nothing is marked required; the
// method itself says what is missing.
func Schema(t reflect.Type) map[string]any {
	return schema(t, map[reflect.Type]bool{})
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

func schema(t reflect.Type, seen map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler):
		return map[string]any{}
	case t.Implements(textMarshaler):
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": schema(t.Elem(), seen)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schema(t.Elem(), seen)}
	case reflect.Struct:
		if seen[t] {
			return map[string]any{"type": "object"}
		}
		seen[t] = true
		defer delete(seen, t)
		properties := map[string]any{}
		for _, f := range Fields(t) {
			p := schema(f.Type, seen)
			if f.Doc != "" {
				p["description"] = f.Doc
			}
			properties[f.Name] = p
		}
		return map[string]any{"type": "object", "properties": properties}
	}
	return map[string]any{}
}
