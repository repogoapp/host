package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		if got := Signature(json.RawMessage(schema)); got != want {
			t.Errorf("Signature(%s) = %q, want %q", schema, got, want)
		}
	}
}

func TestIndexPagePrefersThePluginsOwnIndex(t *testing.T) {
	all := []Page{
		{Method: "notes.texts", Target: ".repogo/tabs/home.md"},
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
	if got := indexPage("notes", all[:1]); got != ".repogo/tabs/home.md" {
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
    books: method(z.object({}), () => [], { effect: "read", description: "Every book" }),
    shelve: method(z.object({ id: z.string(), title: z.string().optional(), read: z.boolean().default(false) }), () => null, {
      description: "Put a book <back>", approve: "Shelve {{id}}?",
    }),
  },
});
`

// The default answer is a line per method; naming the plugin answers its
// schemas, approve and pages, and an unknown name lists the names.
func TestListAnswersSmallAndInFull(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := environment(t, s, dir, map[string]string{"shelf": shelfPlugin})
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
	want := `{"plugins":[{"name":"shelf","page":"~/.repogo/plugins/shelf/pages/index.ui","ui":"~/.repogo/plugins/shelf/pages/index.ui","description":"Books on a shelf","methods":[` +
		`{"name":"shelf.books","effect":"read","description":"Every book"},` +
		`{"name":"shelf.shelve","effect":"write","asks":true,"input":"id, title?, read?","description":"Put a book <back>"}]}]}`
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
	if got.Path != filepath.Join(root, ".repogo", "plugins", "shelf") || shelve.Approve != "Shelve {{id}}?" || !strings.Contains(string(shelve.Input), `"required":["id"]`) || len(got.Pages) != 1 {
		t.Fatalf("shelf in full = %s", full.Result)
	}
	if strings.Contains(string(full.Result), "$schema") || strings.Contains(string(full.Result), "null") {
		t.Fatalf("shelf in full repeats $schema or nulls: %s", full.Result)
	}

	if _, err := s.List(t.Context(), root, "books"); err == nil || !strings.Contains(err.Error(), "there are shelf") {
		t.Fatalf("unknown plugin: %v", err)
	}
}

// A device's listing marks only the methods HTTP really serves, and the
// detail gives each one's URLs, whether the key is made (never the key) and
// the plugin's own pages, its index first.
func TestDeviceListingAndDetail(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := project(t, dir, map[string]string{"hello": httpPlugin})
	s.cfg.Global = filepath.Join(root, ".repogo", "plugins")
	pages := filepath.Join(s.cfg.Global, "hello", "pages")
	if err := os.MkdirAll(pages, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"index.ui":   `<Router initial="a"><Route path="a" title="Hello there"/></Router>`,
		"about.md":   "# About hello\n",
		"plain.html": "<p>no title</p>",
		"notes.txt":  "not a page",
	} {
		if err := os.WriteFile(filepath.Join(pages, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.Listing(t.Context(), "")
	if err != nil || list.Error != "" || len(list.Plugins) != 1 {
		t.Fatalf("Listing: %+v %v", list, err)
	}
	if list.Plugins[0].UI != "~/.repogo/plugins/hello/pages/index.ui" {
		t.Fatalf("ui = %q", list.Plugins[0].UI)
	}
	served := map[string]string{}
	for _, m := range list.Plugins[0].Methods {
		served[m.Name] = fmt.Sprint(m.HTTP)
	}
	want := map[string]string{
		"hello.ping": "[public]", "hello.echo": "[public]", "hello.keyed": "[public]", "hello.fails": "[public]",
		"hello.local": "[device]", "hello.closed": "[]", "hello.asks": "[]", "hello.tail": "[]",
	}
	if fmt.Sprint(served) != fmt.Sprint(want) {
		t.Fatalf("http:\n got  %v\n want %v", served, want)
	}

	d, err := s.Detail(t.Context(), "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if d.Key == nil || d.Key.Set {
		t.Fatalf("key before one is made: %+v", d.Key)
	}
	keyed := d.Methods[slices.IndexFunc(d.Methods, func(m DetailMethod) bool { return m.Name == "hello.keyed" })]
	if len(keyed.URLs) != 1 || !keyed.URLs[0].Key || keyed.URLs[0].URL != "https://h-0123456789abcdef0123456789abcdef.repogo.dev/hello/keyed" {
		t.Fatalf("keyed URLs: %+v", keyed.URLs)
	}
	if got := fmt.Sprint(d.Screens); got != "[{Hello there ~/.repogo/plugins/hello/pages/index.ui} {About hello ~/.repogo/plugins/hello/pages/about.md} {plain ~/.repogo/plugins/hello/pages/plain.html}]" {
		t.Fatalf("screens: %s", got)
	}

	made, err := s.CreateKey(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	d, _ = s.Detail(t.Context(), "", "hello")
	raw, _ := json.Marshal(d)
	if !d.Key.Set || strings.Contains(string(raw), made.Key) {
		t.Fatalf("detail after a key is made: %s", raw)
	}
}

func TestListingWithoutPluginsIsEmpty(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(t.TempDir(), "plugins")
	list, err := s.Listing(t.Context(), "")
	if err != nil || list.Plugins == nil || len(list.Plugins) != 0 {
		t.Fatalf("Listing without a plugins folder: %+v %v", list, err)
	}
}
