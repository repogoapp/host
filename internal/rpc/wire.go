package rpc

import (
	"reflect"
	"sync"
)

// Types that cross the wire outside a method's params and result, such as a
// stream's messages, declared in init so the clients' contracts include them.
// In is what a client sends, Out what it receives.

var (
	wireMu       sync.Mutex
	wireIn       []reflect.Type
	wireOut      []reflect.Type
	wireDeclared = map[reflect.Type]bool{}
)

// WireIn declares types a client sends outside any method.
func WireIn(values ...any) { declareWire(&wireIn, values) }

// WireOut declares types a client receives outside any method or event.
func WireOut(values ...any) { declareWire(&wireOut, values) }

func declareWire(into *[]reflect.Type, values []any) {
	wireMu.Lock()
	defer wireMu.Unlock()
	for _, v := range values {
		t := reflect.TypeOf(v)
		if wireDeclared[t] {
			panic("rpc: wire type " + t.String() + " declared twice")
		}
		wireDeclared[t] = true
		*into = append(*into, t)
	}
}

// WireTypes are the declared types, in declaration order.
func WireTypes() (in, out []reflect.Type) {
	wireMu.Lock()
	defer wireMu.Unlock()
	return append([]reflect.Type(nil), wireIn...), append([]reflect.Type(nil), wireOut...)
}

// Enum is a named string type that lists its values: the clients get an
// enum with each, and a case for a value a newer host adds.
type Enum interface {
	Values() []string
}

// Union marks a struct as a tagged union: a `type` string naming the case,
// and one pointer field per case whose JSON name is that case's type.
// Exactly one case is set; the clients get an enum with an associated value.
type Union interface {
	WireUnion()
}

// ErrorData is an error that carries data for the reply's error.data: a
// domain code a client branches on, beside the numeric JSON-RPC code.
type ErrorData interface {
	error
	ErrorData() any
}
