// Package wirejson enforces required arrays at the host's JSON boundaries.
package wirejson

import (
	"encoding"
	"encoding/json"
	"reflect"
)

// Marshal copies payloads so normalization cannot mutate a service's snapshot.
func Marshal(value any) ([]byte, error) {
	v := normalize(reflect.ValueOf(value), make(map[reference]reflect.Value))
	if !v.IsValid() {
		return json.Marshal(nil)
	}
	return json.Marshal(v.Interface())
}

type reference struct {
	typ     reflect.Type
	pointer uintptr
	length  int
}

func normalize(v reflect.Value, seen map[reference]reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	t := v.Type()
	if t.Implements(reflect.TypeFor[json.Marshaler]()) || reflect.PointerTo(t).Implements(reflect.TypeFor[json.Marshaler]()) ||
		t.Implements(reflect.TypeFor[encoding.TextMarshaler]()) || reflect.PointerTo(t).Implements(reflect.TypeFor[encoding.TextMarshaler]()) {
		return v
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if v.IsNil() {
			return v
		}
		key := reference{typ: t, pointer: uintptr(v.UnsafePointer())}
		if v.Kind() == reflect.Slice {
			key.length = v.Len()
		}
		if previous, ok := seen[key]; ok {
			return previous
		}
		switch v.Kind() {
		case reflect.Pointer:
			out := reflect.New(t.Elem())
			seen[key] = out
			out.Elem().Set(normalize(v.Elem(), seen))
			return out
		case reflect.Map:
			out := reflect.MakeMapWithSize(t, v.Len())
			seen[key] = out
			iter := v.MapRange()
			for iter.Next() {
				out.SetMapIndex(iter.Key(), normalize(iter.Value(), seen))
			}
			return out
		case reflect.Slice:
			if t.Elem().Kind() == reflect.Uint8 {
				return v
			}
			out := reflect.MakeSlice(t, v.Len(), v.Len())
			seen[key] = out
			for i := 0; i < v.Len(); i++ {
				out.Index(i).Set(normalize(v.Index(i), seen))
			}
			return out
		}
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		out.Set(normalize(v.Elem(), seen))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Tag.Get("json") == "-" {
				continue
			}
			field := v.Field(i)
			if f.Tag.Get("wire") == "array" && field.Kind() == reflect.Slice && field.IsNil() {
				out.Field(i).Set(reflect.MakeSlice(field.Type(), 0, 0))
			} else {
				out.Field(i).Set(normalize(field, seen))
			}
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(normalize(v.Index(i), seen))
		}
		return out
	}
	return v
}
