package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/clientprompt"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/repogomcp"
)

func TestInspectMatchesHostAssembly(t *testing.T) {
	for _, child := range []bool{false, true} {
		for _, tools := range []bool{false, true} {
			req := request{Features: map[string]int{"markdown": 1}, Tools: tools, Child: child}
			input, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := inspect(bytes.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			var got result
			if err := json.Unmarshal(output.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tools {
				want = repogomcp.ChatsPrompt(child) + clientprompt.For(device.Client{Features: req.Features}, req.Plugins, req.Servers)
			}
			if got.Text != want {
				t.Errorf("tools=%v child=%v: inspector differs from host assembly", tools, child)
			}
			for _, section := range got.Sections {
				if !tools && section.Included {
					t.Errorf("tools disabled but %s is included", section.ID)
				}
			}
		}
	}
}

func TestInspectRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{`{`, `{"plugins":3}`, `{"unknown":true}`} {
		var output bytes.Buffer
		if err := inspect(strings.NewReader(input), &output); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}
