package repogomcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"

	"github.com/repogoapp/host/internal/screens"
)

// hostResult is a host tool's answer for the model: a screen read's
// elements as one line each (compactScreens), and a picture, {"image":
// {"mime_type", "data"}} (a screen's screenshot), as an image beside the
// rest, so the model sees the screen instead of reading base64.
func hostResult(out json.RawMessage) toolResult {
	out = compactScreens(out)
	var fields map[string]json.RawMessage
	if json.Unmarshal(out, &fields) != nil || fields["image"] == nil {
		return succeeded(out)
	}
	var image struct {
		MimeType string `json:"mime_type"`
		Data     []byte `json:"data"`
	}
	if json.Unmarshal(fields["image"], &image) != nil || len(image.Data) == 0 || image.MimeType == "" {
		return succeeded(out)
	}
	delete(fields, "image")
	res := succeeded(fields)
	res.Content = append(res.Content, content{
		Type: "image", MimeType: image.MimeType, Data: base64.StdEncoding.EncodeToString(image.Data),
	})
	return res
}

// compactScreens writes each element of a screen read (screens_elements, a
// wait, an action's screen after) as one line, as the CLI prints it: the
// same facts as the JSON objects at a third of the length, so a long screen
// isn't cut short by the agent's tool output limit.
func compactScreens(out json.RawMessage) json.RawMessage {
	// Most answers hold no screen: they aren't parsed for one.
	if !bytes.Contains(out, []byte(`"elements"`)) {
		return out
	}
	var v any
	if json.Unmarshal(out, &v) != nil {
		return out
	}
	if !compactIn(v) {
		return out
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	return b
}

// compactIn rewrites, in place, every "elements" list of screen elements
// under v, saying whether it found one.
func compactIn(v any) bool {
	found := false
	switch x := v.(type) {
	case map[string]any:
		if lines, ok := elementLines(x["elements"]); ok {
			x["elements"] = lines
			found = true
		}
		for k, child := range x {
			if k != "elements" && compactIn(child) {
				found = true
			}
		}
	case []any:
		for _, child := range x {
			if compactIn(child) {
				found = true
			}
		}
	}
	return found
}

// elementLines is a list of screen elements as screens.Element.Line writes
// each; not ok when v isn't one, every item an object with a ref and a role.
func elementLines(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	for _, item := range list {
		e, ok := item.(map[string]any)
		if _, ref := e["ref"]; !ok || !ref {
			return nil, false
		}
		if _, role := e["role"]; !role {
			return nil, false
		}
	}
	raw, err := json.Marshal(list)
	var elements []screens.Element
	if err != nil || json.Unmarshal(raw, &elements) != nil {
		return nil, false
	}
	lines := make([]string, len(elements))
	for i, e := range elements {
		lines[i] = e.Line()
	}
	return lines, true
}
