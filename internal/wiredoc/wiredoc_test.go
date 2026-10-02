package wiredoc

import (
	"encoding/json"
	"reflect"
	"testing"
)

type Inner struct {
	Deep string `json:"deep"`
}

type outer struct {
	*Inner
	Name  string            `json:"name"`
	Tags  []string          `json:"tags"`
	Meta  map[string]string `json:"meta"`
	Note  string            `json:"note,omitempty"`
	Skip  string            `json:"-"`
	quiet string
}

// Fields preserves presence independently of nullability.
func TestFieldsReadTheWireShape(t *testing.T) {
	got := map[string][2]bool{}
	for _, f := range Fields(reflect.TypeFor[outer]()) {
		got[f.Name] = [2]bool{f.Omitted, f.Nullable}
	}
	want := map[string][2]bool{"deep": {false, false}, "name": {false, false},
		"tags": {false, true}, "meta": {false, true}, "note": {true, false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
}

// Filled sets every key, through an embedded pointer too.
func TestFilledSetsEveryKey(t *testing.T) {
	b, err := json.Marshal(Filled(reflect.TypeFor[outer]()))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"deep":"deep","name":"name","tags":["tags"],"meta":{"meta":"meta"},"note":"note"}`
	if string(b) != want {
		t.Fatalf("filled = %s, want %s", b, want)
	}
}
