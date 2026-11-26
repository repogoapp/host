package plugins

import (
	"cmp"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Screen is one page file in a plugin's own pages folder, for a device to list
// and open with page:. Unlike Page, it needs no Data showing a method.
type Screen struct {
	Title string `json:"title"`
	// Target opens it, as page: takes it.
	Target string `json:"target"`
}

var (
	routeTitle = regexp.MustCompile(`<Route\b[^>]*\btitle="([^"{]+)"`)
	htmlTitle  = regexp.MustCompile(`(?is)<title>\s*([^<]+?)\s*</title>`)
	mdHeading  = regexp.MustCompile(`(?m)^#\s+(.+?)\s*$`)
)

// screens is every page a plugin's folder holds, its index first, then by name.
func screens(plugin, dir string) []Screen {
	files, _ := filepath.Glob(filepath.Join(dir, "pages", "*"))
	var out []Screen
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if ext != ".ui" && ext != ".md" && ext != ".html" {
			continue
		}
		base := filepath.Base(file)
		out = append(out, Screen{Title: screenTitle(file, ext, plugin), Target: "~/.repogo/plugins/" + plugin + "/pages/" + base})
	}
	slices.SortStableFunc(out, func(a, b Screen) int {
		return cmp.Or(cmp.Compare(rank(a.Target), rank(b.Target)), cmp.Compare(a.Target, b.Target))
	})
	return out
}

// ownIndex is the target of a plugin's pages/index page, or "" without one.
func ownIndex(plugin, dir string) string {
	if all := screens(plugin, dir); len(all) > 0 && rank(all[0].Target) == 0 {
		return all[0].Target
	}
	return ""
}

// rank puts a plugin's index page first.
func rank(target string) int {
	if strings.TrimSuffix(filepath.Base(target), filepath.Ext(target)) == "index" {
		return 0
	}
	return 1
}

// screenTitle is the title a page gives itself: a .ui's first fixed Route
// title, a .md's first heading, an .html's <title>; else the plugin's name for
// its index and the file's name for the rest.
func screenTitle(file, ext, plugin string) string {
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	text, err := os.ReadFile(file)
	if err == nil {
		pattern := map[string]*regexp.Regexp{".ui": routeTitle, ".md": mdHeading, ".html": htmlTitle}[ext]
		if m := pattern.FindSubmatch(text); m != nil {
			if title := strings.TrimSpace(html.UnescapeString(string(m[1]))); title != "" {
				return title
			}
		}
	}
	if name == "index" {
		return plugin
	}
	return name
}
