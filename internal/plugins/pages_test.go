package plugins

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const vercelPage = "```ui\n" + `<State name="project" value=""/>
<State name="deployment" value=""/>
<State name="route" value="projects"/>
<Router id="vercel" initial="{{route}}">
  <Route path="projects">
    <Data name="list" method="plugin:vercel.projects">
      <Each in="list.projects" as="p">
        <Item label="{{p.name}}" action="set:project={{p.name}}" then="push:project"/>
      </Each>
    </Data>
  </Route>
  <Route path="project" title="{{project}}">
    <Tabs name="view" options="Deployments|Logs" value="Deployments"/>
    <Data name="deployments" method="plugin:vercel.deployments" show="view=Deployments">
      <Param name="project" value="{{project}}"/>
      <Each in="deployments" as="d">
        <Badge show="d.status=ready" color="success">Ready</Badge>
      </Each>
    </Data>
    <Data name="logs" method="plugin:vercel.logs" show="view=Logs">
      <Param name="project" value="{{project}}"/>
      <Param name="kind" value="runtime"/>
    </Data>
  </Route>
  <Route path="deployment" title="Deployment">
    <Data name="build" method="plugin:vercel.logs">
      <Param name="project" value="{{project}}"/>
      <Param name="kind" value="build"/>
      <Param name="deployment" value="{{deployment}}"/>
    </Data>
  </Route>
</Router>
` + "```\n"

func writePage(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPagesRouteAware(t *testing.T) {
	root := t.TempDir()
	writePage(t, filepath.Join(root, ".repogo", "tabs", "vercel.md"), "# Vercel\n\n"+vercelPage)

	got := pages(root, "")
	want := []Page{
		{Method: "vercel.projects", Target: ".repogo/tabs/vercel.md?route=projects"},
		{Method: "vercel.deployments", Target: ".repogo/tabs/vercel.md?route=project&view=Deployments", Params: []string{"project"}},
		{Method: "vercel.logs", Target: ".repogo/tabs/vercel.md?route=project&view=Logs", Params: []string{"project"}},
		{Method: "vercel.logs", Target: ".repogo/tabs/vercel.md?route=deployment", Params: []string{"project", "deployment"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages:\n got %+v\nwant %+v", got, want)
	}
}

// A Router opening on a fixed route lists only that route's Data, which no
// query is needed to reach.
func TestPagesLiteralRouterAndNoRouter(t *testing.T) {
	root := t.TempDir()
	writePage(t, filepath.Join(root, ".repogo", "tabs", "plans.md"), "````ui\n"+`<Router initial="list">
  <Route path="list"><Data name="c" method="plugin:plans.counts"/></Route>
  <Route path="plan"><Data name="p" method="plugin:plans.plans.get"><Param name="id" value="{{plan}}"/></Data></Route>
</Router>`+"\n````\n")
	writePage(t, filepath.Join(root, ".repogo", "home.md"), "```ui\n<Data name=\"s\" method=\"plugin:github.status\"/>\n```\n")

	got := pages(root, "")
	want := []Page{
		{Method: "github.status", Target: ".repogo/home.md"},
		{Method: "plans.counts", Target: ".repogo/tabs/plans.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages:\n got %+v\nwant %+v", got, want)
	}
}

// An environment plugin's pages are listed by their ~ path, unless the project
// has a plugin of that name; markdown outside a pages folder isn't a page.
func TestPagesEnvironmentAndShadowing(t *testing.T) {
	root, global := t.TempDir(), t.TempDir()
	page := "```ui\n<Data name=\"n\" method=\"plugin:notes.open\"/>\n```\n"
	writePage(t, filepath.Join(global, "notes", "pages", "index.md"), page)
	writePage(t, filepath.Join(global, "vercel", "pages", "index.md"), page)
	writePage(t, filepath.Join(root, ".repogo", "plugins", "vercel", "app.ts"), "")
	writePage(t, filepath.Join(root, ".repogo", "plugins", "node_modules", "x", "README.md"), page)
	writePage(t, filepath.Join(root, ".repogo", "plugins", "vercel", "guide.src.md"), page)
	writePage(t, filepath.Join(root, ".repogo", "plugins", "vercel", "pages", "index.md"), page)

	got := pages(root, global)
	want := []Page{
		{Method: "notes.open", Target: ".repogo/plugins/vercel/pages/index.md"},
		{Method: "notes.open", Target: "~/.repogo/plugins/notes/pages/index.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages:\n got %+v\nwant %+v", got, want)
	}
}

// A card that isn't well-formed keeps the Data read before the break, and a
// page without ui fences or plugin Data adds nothing.
func TestPagesMalformedAndEmpty(t *testing.T) {
	root := t.TempDir()
	writePage(t, filepath.Join(root, ".repogo", "a.md"), "```ui\n<Data name=\"a\" method=\"plugin:a.list\"></Data>\n<Text>1 < 2 && </Wrong>\n```\n")
	writePage(t, filepath.Join(root, ".repogo", "b.md"), "# Notes\n\n```swift\n<Data method=\"plugin:b.list\"/>\n```\n")

	got := pages(root, "")
	want := []Page{{Method: "a.list", Target: ".repogo/a.md"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages:\n got %+v\nwant %+v", got, want)
	}
	if got := pages(t.TempDir(), ""); got != nil {
		t.Fatalf("no .repogo: %+v", got)
	}
}

// A .ui page is card markup with no fence, in a project and in the
// environment's plugins alike; other files beside them aren't pages.
func TestPagesUIFiles(t *testing.T) {
	root, global := t.TempDir(), t.TempDir()
	writePage(t, filepath.Join(root, ".repogo", "tabs", "home.ui"), `<Data name="s" method="plugin:github.status"/>`)
	writePage(t, filepath.Join(root, ".repogo", "tabs", "layout.xml"), `<Data name="x" method="plugin:github.other"/>`)
	writePage(t, filepath.Join(global, "notes", "pages", "index.ui"), `<Data name="n" method="plugin:notes.list"/>`)
	writePage(t, filepath.Join(global, "notes", "pages", "notes.json"), `{}`)

	got := pages(root, global)
	want := []Page{
		{Method: "github.status", Target: ".repogo/tabs/home.ui"},
		{Method: "notes.list", Target: "~/.repogo/plugins/notes/pages/index.ui"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages:\n got %+v\nwant %+v", got, want)
	}
}
