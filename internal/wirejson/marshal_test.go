package wirejson

import (
	"encoding/json"
	"testing"
	"time"
)

type arrays struct {
	Items    []string `json:"items" wire:"array"`
	Optional []string `json:"optional"`
}

func TestRequiredArraysAreNormalizedWithoutMutation(t *testing.T) {
	child := &arrays{}
	input := struct {
		Nested   arrays          `json:"nested"`
		Children []*arrays       `json:"children"`
		Map      map[string]any  `json:"map"`
		Bytes    []byte          `json:"bytes"`
		Raw      json.RawMessage `json:"raw"`
		Time     time.Time       `json:"time"`
	}{Children: []*arrays{child}, Map: map[string]any{"child": child}, Bytes: []byte("ok"), Raw: json.RawMessage(`{"items":null}`)}
	got, err := Marshal(input)
	want := `{"nested":{"items":[],"optional":null},"children":[{"items":[],"optional":null}],"map":{"child":{"items":[],"optional":null}},"bytes":"b2s=","raw":{"items":null},"time":"0001-01-01T00:00:00Z"}`
	if err != nil || string(got) != want {
		t.Fatalf("got %s, %v; want %s", got, err, want)
	}
	if child.Items != nil || input.Nested.Items != nil {
		t.Fatal("mutated source snapshot")
	}
}

func TestInvalidJSONStillFails(t *testing.T) {
	type cycle struct {
		Next *cycle `json:"next"`
	}
	value := &cycle{}
	value.Next = value
	if _, err := Marshal(value); err == nil {
		t.Fatal("accepted cyclic value")
	}
	if _, err := Marshal(make(chan int)); err == nil {
		t.Fatal("accepted unsupported value")
	}
}
