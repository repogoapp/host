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

type schemaParams struct {
	Inner
	ID     string            `json:"id" doc:"Which one"`
	Count  int               `json:"count"`
	Flag   *bool             `json:"flag"`
	Data   []byte            `json:"data"`
	Rows   []Inner           `json:"rows"`
	Labels map[string]string `json:"labels"`
	Raw    json.RawMessage   `json:"raw"`
}

// Schema reads params as encoding/json does: embedded fields flattened,
// pointers as their value, bytes as base64, raw JSON as anything.
func TestSchemaFollowsTheWire(t *testing.T) {
	got, _ := json.Marshal(Schema(reflect.TypeFor[schemaParams]()))
	want := `{"properties":{"count":{"type":"integer"},"data":{"contentEncoding":"base64","type":"string"},` +
		`"deep":{"type":"string"},"flag":{"type":"boolean"},"id":{"description":"Which one","type":"string"},` +
		`"labels":{"additionalProperties":{"type":"string"},"type":"object"},"raw":{},` +
		`"rows":{"items":{"properties":{"deep":{"type":"string"}},"type":"object"},"type":"array"}},"type":"object"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
