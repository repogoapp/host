package repogomcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// A screenshot in a host tool's answer reaches the model as an image, and
// the JSON beside it keeps the rest without the base64.
func TestHostResultShowsAnImage(t *testing.T) {
	res := hostResult(json.RawMessage(`{"path":"/tmp/a.png","image":{"mime_type":"image/jpeg","data":"AAEC"}}`))
	if len(res.Content) != 2 || res.Content[1].Type != "image" || res.Content[1].MimeType != "image/jpeg" ||
		res.Content[1].Data != "AAEC" {
		t.Fatalf("content = %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].Text, "/tmp/a.png") || strings.Contains(res.Content[0].Text, "AAEC") {
		t.Fatalf("text = %s", res.Content[0].Text)
	}
	plain := hostResult(json.RawMessage(`{"chats":[]}`))
	if len(plain.Content) != 1 || plain.Content[0].Type != "text" {
		t.Fatalf("plain = %+v", plain.Content)
	}
}

// A screen read reaches the model as one line per element, wherever it sits
// in the answer (a wait's screen, an action's).
func TestHostResultWritesElementsAsLines(t *testing.T) {
	res := hostResult(json.RawMessage(`{"met":true,"read":{"width":951,"height":669,"unit":"pt","elements":[` +
		`{"ref":"e3.1","role":"RadioButton","label":"Files","value":"","id":"doc.text","x":881,"y":585,"w":44,"h":58,"selected":true},` +
		`{"ref":"e3.2","role":"TextField","label":"","value":"Ask RepoGo...","x":34,"y":831,"w":571,"h":22}]}}`))
	text := res.Content[0].Text
	for _, want := range []string{`e3.1 RadioButton \"Files\" selected #doc.text @881,585 44x58`, `e3.2 TextField \"\" =\"Ask RepoGo...\" @34,831 571x22`, `"met": true`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
}
