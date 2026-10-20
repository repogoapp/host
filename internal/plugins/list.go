package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/outcome"
)

// The `plugins` tool answers small by default, a line per method, since most
// turns touching a plugin call it; naming a plugin answers it in full.

// Listing is the default answer: each plugin with a line per method, and the
// lines of crons.json with how each is going.
type Listing struct {
	Plugins []Listed `json:"plugins"`
	Crons   []Cron   `json:"crons,omitempty"`
}

// Listed is one plugin in the default answer. Dir is `project` or `env`, and
// Page is the page to open it by, when one shows a method of it.
type Listed struct {
	Name        string         `json:"name"`
	Dir         string         `json:"dir"`
	Errors      int            `json:"errors,omitempty"`
	Page        string         `json:"page,omitempty"`
	Description string         `json:"description,omitempty"`
	Methods     []ListedMethod `json:"methods"`
}

// ListedMethod is one method in a line: Asks when it has a confirm, and Input
// as its top-level fields, optional ones ending in `?`: `id, text?, done?`.
type ListedMethod struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Asks        bool   `json:"asks,omitempty"`
	Input       string `json:"input,omitempty"`
	Description string `json:"description,omitempty"`
}

// Detail is one plugin in full: its folder, each method's schemas and confirm,
// its storage, the pages that show its methods and the cron lines naming it.
type Detail struct {
	Name        string          `json:"name"`
	Dir         string          `json:"dir"`
	Path        string          `json:"path"`
	Errors      int             `json:"errors,omitempty"`
	Description string          `json:"description,omitempty"`
	Methods     []DetailMethod  `json:"methods"`
	Storage     json.RawMessage `json:"storage,omitempty"`
	Pages       []Page          `json:"pages,omitempty"`
	Crons       []Cron          `json:"crons,omitempty"`
}

// DetailMethod is one method with its input's JSON Schema, and its output's
// when it declares one.
type DetailMethod struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Description string          `json:"description,omitempty"`
	Confirm     string          `json:"confirm,omitempty"`
	Input       json.RawMessage `json:"input"`
	Output      json.RawMessage `json:"output,omitempty"`
}

// manifestPlugin is one plugin as its Worker lists it.
type manifestPlugin struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Methods     []struct {
		Name        string          `json:"name"`
		Kind        string          `json:"kind"`
		Description string          `json:"description"`
		Confirm     string          `json:"confirm"`
		HTTP        string          `json:"http"`
		Input       json.RawMessage `json:"input"`
		Output      json.RawMessage `json:"output"`
	} `json:"methods"`
	Storage json.RawMessage `json:"storage"`
}

// listedPlugin is a plugin with the folder it came from.
type listedPlugin struct {
	manifestPlugin
	target Target
}

// dir is `env` for the environment's folder, `project` for the project's.
func (p listedPlugin) dir() string {
	if p.target.Root != "" {
		return "env"
	}
	return "project"
}

// List answers the `plugins` tool for a folder: every plugin in a line per
// method, or with plugin set, that one in full. crons.json is read again
// first, so an edit shows.
func (s *Service) List(ctx context.Context, path, plugin string) (outcome.Answer, error) {
	if s.cfg.Crons != "" {
		s.reconcile(ctx)
	}
	found, failure, err := s.manifests(ctx, path)
	if err != nil || failure != nil {
		return outcome.Answer{Error: failure}, err
	}
	errors := s.recentErrors(ctx)
	all := pages(checkout(path), s.cfg.Global)
	if plugin == "" {
		return encode(Listing{Plugins: listedAll(found, errors, all), Crons: s.Crons()})
	}
	i := slices.IndexFunc(found, func(p listedPlugin) bool { return p.Name == plugin })
	if i < 0 {
		names := make([]string, len(found))
		for i, p := range found {
			names[i] = p.Name
		}
		return outcome.Answer{}, errkind.ErrNotFound.Errorf("plugins: there's no plugin %q here; there are %s", plugin, strings.Join(names, ", "))
	}
	p := found[i]
	return encode(detail(p, errors[errorsKey(p.target.Dir, p.Name)], all, s.cronsOf(p)))
}

// Plugins is every plugin a folder can use with a line per method, as the
// `plugins` tool lists them; a Worker's own failure comes back as an error.
func (s *Service) Plugins(ctx context.Context, path string) ([]Listed, error) {
	found, failure, err := s.manifests(ctx, path)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return nil, fmt.Errorf("plugins: %s", failure.Message)
	}
	return listedAll(found, s.recentErrors(ctx), pages(checkout(path), s.cfg.Global)), nil
}

func listedAll(found []listedPlugin, errors map[string]int, all []Page) []Listed {
	out := []Listed{}
	for _, p := range found {
		out = append(out, listed(p, errors[errorsKey(p.target.Dir, p.Name)], all))
	}
	return out
}

// manifests is every plugin a folder can use; a project plugin hides an
// environment one of its name. A Worker's own failure comes back as it said it.
func (s *Service) manifests(ctx context.Context, path string) ([]listedPlugin, *outcome.Failure, error) {
	targets, err := s.targets(path)
	if err != nil {
		return nil, nil, err
	}
	var out []listedPlugin
	seen := map[string]bool{}
	for _, t := range targets {
		answer, err := s.ask(ctx, t, "list", t)
		if err != nil || answer.Error != nil {
			return nil, answer.Error, err
		}
		var manifest struct {
			Plugins []manifestPlugin `json:"plugins"`
		}
		if err := json.Unmarshal(answer.Result, &manifest); err != nil {
			return nil, nil, fmt.Errorf("plugins: manifest of %s: %w", t.Dir, err)
		}
		for _, p := range manifest.Plugins {
			if seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			out = append(out, listedPlugin{manifestPlugin: p, target: t})
		}
	}
	return out, nil, nil
}

func listed(p listedPlugin, errors int, all []Page) Listed {
	out := Listed{Name: p.Name, Dir: p.dir(), Errors: errors, Page: indexPage(p.Name, all), Description: p.Description, Methods: []ListedMethod{}}
	for _, m := range p.Methods {
		out.Methods = append(out.Methods, ListedMethod{Name: m.Name, Kind: m.Kind, Asks: m.Confirm != "", Input: Signature(m.Input), Description: m.Description})
	}
	return out
}

func detail(p listedPlugin, errors int, all []Page, crons []Cron) Detail {
	out := Detail{Name: p.Name, Dir: p.dir(), Path: filepath.Join(p.target.Dir, p.Name), Errors: errors, Description: p.Description, Methods: []DetailMethod{}, Storage: present(p.Storage), Crons: crons}
	for _, m := range p.Methods {
		out.Methods = append(out.Methods, DetailMethod{Name: m.Name, Kind: m.Kind, Description: m.Description, Confirm: m.Confirm, Input: schema(m.Input), Output: schema(m.Output)})
	}
	for _, page := range all {
		if ownMethod(p.Name, page.Method) {
			out.Pages = append(out.Pages, page)
		}
	}
	return out
}

// cronsOf is the lines of crons.json naming a plugin in its folder, and those
// naming it whose folder couldn't be found.
func (s *Service) cronsOf(p listedPlugin) []Cron {
	dir := realOrSame(p.target.Dir)
	var out []Cron
	for _, c := range s.Crons() {
		if c.Plugin == p.Name && (c.Dir == "" || c.Dir == dir) {
			out = append(out, c)
		}
	}
	return out
}

func ownMethod(plugin, method string) bool {
	return strings.HasPrefix(method, plugin+".")
}

// indexPage is the page to open a plugin by: the index of its own pages
// folder, else its own first page, else the first page showing a method of it.
func indexPage(plugin string, all []Page) string {
	own := "/plugins/" + plugin + "/pages/"
	var first, ownFirst string
	for _, page := range all {
		if !ownMethod(plugin, page.Method) {
			continue
		}
		path, _, _ := strings.Cut(page.Target, "?")
		if strings.Contains(path, own) && strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) == "index" {
			return path
		}
		if ownFirst == "" && strings.Contains(path, own) {
			ownFirst = path
		}
		if first == "" {
			first = path
		}
	}
	if ownFirst != "" {
		return ownFirst
	}
	return first
}

// Signature is a schema's top-level fields in order, optional ones ending in
// `?`; "" for an input with none, or one that isn't an object.
func Signature(schema json.RawMessage) string {
	var s struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return ""
	}
	d := json.NewDecoder(bytes.NewReader(s.Properties))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	var fields []string
	for d.More() {
		t, err := d.Token()
		name, ok := t.(string)
		if err != nil || !ok {
			return ""
		}
		var field json.RawMessage
		if err := d.Decode(&field); err != nil {
			return ""
		}
		if !slices.Contains(s.Required, name) {
			name += "?"
		}
		fields = append(fields, name)
	}
	return strings.Join(fields, ", ")
}

// schema is a JSON Schema without its `$schema` line, which every one repeats.
func schema(raw json.RawMessage) json.RawMessage {
	raw = present(raw)
	var fields map[string]json.RawMessage
	if raw == nil || json.Unmarshal(raw, &fields) != nil {
		return raw
	}
	delete(fields, "$schema")
	out, err := encode(fields)
	if err != nil {
		return raw
	}
	return out.Result
}

// present is raw JSON, or nil for a null, so omitempty leaves it out.
func present(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// encode is compact JSON, unescaped, so a page's target reads as the agent
// writes it: `?a=1&b=2`.
func encode(v any) (outcome.Answer, error) {
	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return outcome.Answer{}, err
	}
	return outcome.Answer{Result: bytes.TrimSpace(result.Bytes())}, nil
}
