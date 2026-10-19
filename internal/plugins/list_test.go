package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignatureNamesTheTopLevelFields(t *testing.T) {
	cases := map[string]string{
		`{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"},"done":{"type":"boolean"}},"required":["id"]}`: "id, text?, done?",
		`{"type":"object","properties":{}}`: "",
		`{"type":"string"}`:                 "",
		`null`:                              "",
	}
	for schema, want := range cases {
		if got := signature(json.RawMessage(schema)); got != want {
			t.Errorf("signature(%s) = %q, want %q", schema, got, want)
		}
	}
}

func TestIndexPagePrefersThePluginsOwnIndex(t *testing.T) {
	all := []Page{
		{Method: "notes.texts", Target: ".repogo/tabs/home.ui"},
		{Method: "notes.texts", Target: "~/.repogo/plugins/notes/pages/detail.ui?route=x"},
		{Method: "notes.get", Target: "~/.repogo/plugins/notes/pages/index.ui?route=note", Params: []string{"id"}},
		{Method: "notesy.list", Target: "~/.repogo/plugins/notesy/pages/index.ui"},
	}
	if got := indexPage("notes", all); got != "~/.repogo/plugins/notes/pages/index.ui" {
		t.Fatalf("index = %q", got)
	}
	if got := indexPage("notes", all[:2]); got != "~/.repogo/plugins/notes/pages/detail.ui" {
		t.Fatalf("own first = %q", got)
	}
	if got := indexPage("notes", all[:1]); got != ".repogo/tabs/home.ui" {
		t.Fatalf("any = %q", got)
	}
	if got := indexPage("other", all); got != "" {
		t.Fatalf("none = %q", got)
	}
}

const shelfPlugin = `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  description: "Books on a shelf",
  methods: {
    books: method(z.object({}), () => [], { kind: "query", description: "Every book" }),
    shelve: method(z.object({ id: z.string(), title: z.string().optional(), read: z.boolean().default(false) }), () => null, {
      description: "Put a book <back>", confirm: "Shelve {{id}}?",
    }),
  },
});
`

// The default answer is a line per method; naming the plugin answers its
// schemas, confirm and pages, and an unknown name lists the names.
func TestListAnswersSmallAndInFull(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := project(t, dir, map[string]string{"shelf": shelfPlugin})
	pages := filepath.Join(root, ".repogo", "plugins", "shelf", "pages")
	if err := os.MkdirAll(pages, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pages, "index.ui"), []byte(`<Data name="b" method="plugin:shelf.books"/>`), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(t.Context(), root, "")
	if err != nil || list.Error != nil {
		t.Fatalf("list: %+v %v", list.Error, err)
	}
	want := `{"plugins":[{"name":"shelf","dir":"project","page":".repogo/plugins/shelf/pages/index.ui","description":"Books on a shelf","methods":[` +
		`{"name":"shelf.books","kind":"query","description":"Every book"},` +
		`{"name":"shelf.shelve","kind":"mutation","asks":true,"input":"id, title?, read?","description":"Put a book <back>"}]}]}`
	if string(list.Result) != want {
		t.Fatalf("list =\n%s\nwant\n%s", list.Result, want)
	}

	full, err := s.List(t.Context(), root, "shelf")
	if err != nil || full.Error != nil {
		t.Fatalf("shelf: %+v %v", full.Error, err)
	}
	var got Detail
	if err := json.Unmarshal(full.Result, &got); err != nil {
		t.Fatal(err)
	}
	shelve := got.Methods[1]
	if got.Path != filepath.Join(root, ".repogo", "plugins", "shelf") || shelve.Confirm != "Shelve {{id}}?" || !strings.Contains(string(shelve.Input), `"required":["id"]`) || len(got.Pages) != 1 {
		t.Fatalf("shelf in full = %s", full.Result)
	}
	if strings.Contains(string(full.Result), "$schema") || strings.Contains(string(full.Result), "null") {
		t.Fatalf("shelf in full repeats $schema or nulls: %s", full.Result)
	}

	if _, err := s.List(t.Context(), root, "books"); err == nil || !strings.Contains(err.Error(), "there are shelf") {
		t.Fatalf("unknown plugin: %v", err)
	}
}
