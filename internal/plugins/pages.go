package plugins

import (
	"cmp"
	"encoding/xml"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Page is one place a page shows a plugin method's answer, so an agent can link
// it rather than draw the same card again.
type Page struct {
	// Method is the query its Data reads, such as `vercel.deployments`.
	Method string `json:"method"`
	// Target opens the page where that Data shows, for `page:` or `<Page path>`.
	Target string `json:"target"`
	// Params are the page's states the Data reads; the agent adds them to Target.
	Params []string `json:"params,omitempty"`
}

// pages is every plugin Data a link can reach in a project's pages (.repogo,
// .repogo/tabs, each plugin's pages folder), then in the environment plugins'
// pages, except a plugin the project shadows. A page is a .ui or .md file.
func pages(root, global string) []Page {
	var out []Page
	_ = filepath.WalkDir(filepath.Join(root, ".repogo"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isPageFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if dir := filepath.ToSlash(filepath.Dir(rel)); isPagesDir(dir) {
			out = append(out, pageData(path, filepath.ToSlash(rel))...)
		}
		return nil
	})
	if global == "" {
		return dedupe(out)
	}
	entries, _ := os.ReadDir(global)
	for _, e := range entries {
		if !e.IsDir() || isFile(filepath.Join(root, filepath.FromSlash(Folder), e.Name(), "app.ts")) {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(global, e.Name(), "pages", "*"))
		for _, file := range files {
			if !isPageFile(file) {
				continue
			}
			out = append(out, pageData(file, "~/.repogo/plugins/"+e.Name()+"/pages/"+filepath.Base(file))...)
		}
	}
	return dedupe(out)
}

var pluginPages = regexp.MustCompile(`^\.repogo/plugins/[^/]+/pages$`)

func isPagesDir(dir string) bool {
	return dir == ".repogo" || dir == ".repogo/tabs" || strings.HasPrefix(dir, ".repogo/tabs/") || pluginPages.MatchString(dir)
}

// isPageFile is a page whose cards can show a plugin's Data: card markup
// (.ui) or Markdown with ui fences (.md).
func isPageFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".ui" || ext == ".md"
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// pageData reads one page file's cards: all of a .ui file, or each ui fence
// of a .md file; a file it can't read has none.
func pageData(file, target string) []Page {
	text, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	if strings.ToLower(filepath.Ext(file)) == ".ui" {
		return fenceData(string(text), target)
	}
	var out []Page
	for _, fence := range uiFences(string(text)) {
		out = append(out, fenceData(fence, target)...)
	}
	return out
}

var fenceOpen = regexp.MustCompile("^(`{3,})ui\\s*$")

// uiFences is the body of each ```ui (or longer) fence in a markdown page.
func uiFences(text string) []string {
	var fences []string
	var body []string
	closing := ""
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if closing == "" {
			if m := fenceOpen.FindStringSubmatch(trimmed); m != nil {
				closing = m[1]
				body = nil
			}
			continue
		}
		if trimmed == closing {
			fences = append(fences, strings.Join(body, "\n"))
			closing = ""
			continue
		}
		body = append(body, line)
	}
	return fences
}

// stateTags are the elements that declare a page state by their name.
var stateTags = []string{"State", "Tabs", "Input", "TextArea", "Select", "Radio", "Toggle", "Slider"}

var binding = regexp.MustCompile(`\{\{\s*([A-Za-z][A-Za-z0-9_-]*)`)

// found is one plugin Data while its card is read: the route it sits in, the
// show conditions around it, and the names its Params read.
type found struct {
	method string
	route  string
	shows  []string
	reads  []string
}

// fenceData is each plugin Data in one card. Its target lands on its route and
// meets the show conditions around it; its params are the states its Params read.
func fenceData(fence, target string) []Page {
	states := map[string]bool{}
	var routeState, initial string
	var datas []*found

	type open struct {
		route, show string
		data        *found
	}
	var stack []open

	d := xml.NewDecoder(strings.NewReader("<card>" + fence + "</card>"))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			tag := t.Name.Local
			o := open{show: attr(t, "show")}
			switch tag {
			case "Router":
				initial = attr(t, "initial")
				if m := binding.FindStringSubmatch(initial); m != nil {
					routeState = m[1]
				}
			case "Route":
				o.route = attr(t, "path")
			case "Data":
				if method, ok := strings.CutPrefix(attr(t, "method"), "plugin:"); ok {
					o.data = &found{method: method}
					for _, a := range stack {
						o.data.route = cmp.Or(a.route, o.data.route)
						o.data.shows = appendNonEmpty(o.data.shows, a.show)
					}
					o.data.shows = appendNonEmpty(o.data.shows, o.show)
					datas = append(datas, o.data)
				}
			case "Param":
				if n := len(stack); n > 0 && stack[n-1].data != nil {
					for _, m := range binding.FindAllStringSubmatch(attr(t, "value"), -1) {
						stack[n-1].data.reads = append(stack[n-1].data.reads, m[1])
					}
				}
			}
			if slices.Contains(stateTags, tag) && attr(t, "name") != "" {
				states[attr(t, "name")] = true
			}
			stack = append(stack, o)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	var out []Page
	for _, f := range datas {
		// A Router that opens on a fixed route can't be linked past it.
		if routeState == "" && initial != "" && f.route != "" && f.route != initial {
			continue
		}
		query := url.Values{}
		if routeState != "" && f.route != "" && f.route != initial {
			query.Set(routeState, f.route)
		}
		for _, show := range f.shows {
			if name, value, ok := strings.Cut(show, "="); ok && states[name] {
				query.Set(name, value)
			}
		}
		page := Page{Method: f.method, Target: target}
		if len(query) > 0 {
			page.Target += "?" + query.Encode()
		}
		// Only declared states are the page's to set; the rest are Data answers and Each rows.
		for _, name := range f.reads {
			if states[name] && !query.Has(name) && !slices.Contains(page.Params, name) {
				page.Params = append(page.Params, name)
			}
		}
		out = append(out, page)
	}
	return out
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func appendNonEmpty(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}

func dedupe(pages []Page) []Page {
	var out []Page
	for _, p := range pages {
		if !slices.ContainsFunc(out, func(q Page) bool {
			return q.Method == p.Method && q.Target == p.Target && slices.Equal(q.Params, p.Params)
		}) {
			out = append(out, p)
		}
	}
	return out
}
