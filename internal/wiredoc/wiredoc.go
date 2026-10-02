// Package wiredoc reads a Go type the way encoding/json writes it, for the
// generated wire docs and the examples the client's tests decode.
package wiredoc

import (
	"encoding/json"
	"reflect"
	"strings"
)

// List renders t's JSON fields as a Markdown list, one line each.
func List(t reflect.Type) string {
	var b strings.Builder
	for _, f := range Fields(t) {
		b.WriteString("- `" + f.Name + "` " + f.Kind)
		if f.Omitted {
			b.WriteString(" (omitted when empty)")
		}
		if f.Nullable {
			b.WriteString(" (nullable)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Filled is a value of t with every field json will write set: strings to
// their field name, numbers to 1, one element per slice. A decoder that ends
// up with an empty value from this reads a key the host does not send.
func Filled(t reflect.Type) any {
	v := reflect.New(t).Elem()
	fill(v, "", map[reflect.Type]bool{})
	return v.Interface()
}

// Examples adds the files the client's tests decode for t to out:
// "<name>.json" with every field set, "<name>.zero.json" as the zero value marshals.
func Examples(out map[string][]byte, name string, t reflect.Type) error {
	for file, v := range map[string]any{name + ".json": Filled(t), name + ".zero.json": Zero(t)} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		out[file] = append(b, '\n')
	}
	return nil
}

// RawMessage is json.RawMessage's type: JSON the host passes through untyped.
var RawMessage = reflect.TypeFor[json.RawMessage]()

func fill(v reflect.Value, name string, seen map[reflect.Type]bool) {
	t := v.Type()
	if t.Kind() == reflect.Struct {
		if seen[t] {
			return
		}
		seen[t] = true
		defer delete(seen, t)
		for _, f := range Fields(t) {
			if fv := field(v, f.Index); fv.IsValid() {
				fill(fv, f.Name, seen)
			}
		}
		return
	}
	if !v.CanSet() {
		return
	}
	switch {
	case t == RawMessage:
		v.SetBytes([]byte(`{"` + name + `":"` + name + `"}`))
	case t.Kind() == reflect.Pointer:
		p := reflect.New(t.Elem())
		fill(p.Elem(), name, seen)
		v.Set(p)
	case t.Kind() == reflect.String:
		v.SetString(name)
	case t.Kind() == reflect.Bool:
		v.SetBool(true)
	case v.CanInt():
		v.SetInt(1)
	case v.CanUint():
		v.SetUint(1)
	case v.CanFloat():
		v.SetFloat(1.5)
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		v.SetBytes([]byte(name))
	case t.Kind() == reflect.Slice:
		s := reflect.MakeSlice(t, 1, 1)
		fill(s.Index(0), name, seen)
		v.Set(s)
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
		m := reflect.MakeMap(t)
		e := reflect.New(t.Elem()).Elem()
		fill(e, name, seen)
		m.SetMapIndex(reflect.ValueOf(name).Convert(t.Key()), e)
		v.Set(m)
	case t.Kind() == reflect.Interface && t.NumMethod() == 0:
		v.Set(reflect.ValueOf(name))
	}
}

// field is v's field at index, allocating embedded pointers on the way;
// invalid when one of them cannot be set.
func field(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !v.CanSet() {
					return reflect.Value{}
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

// Field is one JSON key: its name, its Go type, a readable type, and whether
// it may be omitted or nullable; wire:"array" requires a non-null slice.
type Field struct {
	Name, Kind string
	Type       reflect.Type
	Index      []int
	Omitted    bool
	Nullable   bool
}

// Fields walks the struct the way encoding/json will, flattening embedded
// structs and honouring tags, so the doc lists what actually goes over the wire.
func Fields(t reflect.Type) []Field {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []Field
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if et := f.Type; f.Anonymous && name == "" {
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for _, inner := range Fields(et) {
					inner.Index = append([]int{i}, inner.Index...)
					out = append(out, inner)
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		k := f.Type.Kind()
		omitted := strings.Contains(opts, "omitempty")
		nullable := k == reflect.Pointer || k == reflect.Map || k == reflect.Interface ||
			(k == reflect.Slice && f.Tag.Get("wire") != "array")
		out = append(out, Field{Name: name, Kind: kindOf(f.Type), Type: f.Type,
			Index: []int{i}, Omitted: omitted, Nullable: nullable})
	}
	return out
}

func kindOf(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(reflect.TypeOf((*json.Marshaler)(nil)).Elem()) {
		return "json"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return "base64" // encoding/json writes []byte as a base64 string
		}
		return "[" + kindOf(t.Elem()) + "]"
	case reflect.Map:
		return "object"
	case reflect.Struct:
		return "object{" + t.Name() + "}"
	default:
		return t.Kind().String()
	}
}

// Zero preserves required array keys as empty arrays in wire examples.
func Zero(t reflect.Type) any {
	v := reflect.New(t).Elem()
	emptyArrays(v)
	return v.Interface()
}

func emptyArrays(v reflect.Value) {
	if v.Kind() != reflect.Struct {
		return
	}
	for _, f := range Fields(v.Type()) {
		fv := field(v, f.Index)
		if !fv.IsValid() || !fv.CanSet() {
			continue
		}
		if f.Type.Kind() == reflect.Slice && !f.Nullable {
			fv.Set(reflect.MakeSlice(f.Type, 0, 0))
		}
		if fv.Kind() == reflect.Struct {
			emptyArrays(fv)
		}
	}
}
