package plugins

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
	// Error is why the plugins failed to load, for a device; the agent's tool
	// answers it as its failure instead.
	Error string `json:"error,omitempty"`
}

// Listed is one plugin in the default answer. Page is the page to open it by,
// when one shows a method of it.
type Listed struct {
	Name string `json:"name"`
	// Title is the name a device shows, the plugin's own; Name stays its id.
	Title  string `json:"title,omitempty"`
	Icon   *Icon  `json:"icon,omitempty"`
	Errors int    `json:"errors,omitempty"`
	Page   string `json:"page,omitempty"`
	// UI is its own pages/index page, which a device opens when the plugin is
	// tapped; Page may fall back to another plugin's page showing its methods.
	UI          string         `json:"ui,omitempty"`
	Description string         `json:"description,omitempty"`
	Methods     []ListedMethod `json:"methods"`
}

// ListedMethod is one method in a line: its effect (read, write or
// destructive), Stream for a read that sends items, Asks when it has an
// approve question, and Input as its top-level fields: `id, text?, done?`.
type ListedMethod struct {
	Name        string `json:"name"`
	Effect      string `json:"effect"`
	Stream      bool   `json:"stream,omitempty"`
	Asks        bool   `json:"asks,omitempty"`
	Input       string `json:"input,omitempty"`
	Description string `json:"description,omitempty"`
	// HTTP is the addresses it answers plain HTTP at, as its URLs are built.
	HTTP []Address `json:"http,omitempty"`
}

// Detail is one plugin in full: its folder, each method's schemas and approve,
// its storage, the pages that show its methods and the cron lines naming it.
type Detail struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Icon        *Icon           `json:"icon,omitempty"`
	Path        string          `json:"path"`
	Errors      int             `json:"errors,omitempty"`
	Description string          `json:"description,omitempty"`
	Methods     []DetailMethod  `json:"methods"`
	Storage     json.RawMessage `json:"storage,omitempty"`
	// Key is whether its HTTP key is made, for a plugin with a method taking
	// one; never the key, which only plugins.keys answers.
	Key *KeyStatus `json:"key,omitempty"`
	// Screens are the page files in its own pages folder, to open it by.
	Screens []Screen `json:"screens,omitempty"`
	Pages   []Page   `json:"pages,omitempty"`
	Crons   []Cron   `json:"crons,omitempty"`
}

// KeyStatus is whether a plugin's HTTP key is made, and when (milliseconds).
type KeyStatus struct {
	Set       bool  `json:"set"`
	CreatedAt int64 `json:"created_at,omitempty"`
}

// DetailMethod is one method with its input's JSON Schema, and its output's
// when it declares one.
type DetailMethod struct {
	Name        string          `json:"name"`
	Effect      string          `json:"effect"`
	Stream      bool            `json:"stream,omitempty"`
	Description string          `json:"description,omitempty"`
	Approve     string          `json:"approve,omitempty"`
	ApproveUpTo string          `json:"approveUpTo,omitempty"`
	Input       json.RawMessage `json:"input"`
	Output      json.RawMessage `json:"output,omitempty"`
	// URLs are where it answers plain HTTP; none when it doesn't.
	URLs []MethodURL `json:"urls,omitempty"`
}

// manifestPlugin is one plugin as its Worker lists it.
type manifestPlugin struct {
	Name        string           `json:"name"`
	Title       string           `json:"title"`
	Icon        json.RawMessage  `json:"icon"`
	Description string           `json:"description"`
	Methods     []manifestMethod `json:"methods"`
	Storage     json.RawMessage  `json:"storage"`
}

type manifestMethod struct {
	Name        string             `json:"name"`
	Effect      string             `json:"effect"`
	Stream      bool               `json:"stream"`
	Description string             `json:"description"`
	Approve     string             `json:"approve"`
	ApproveUpTo string             `json:"approveUpTo"`
	HTTP        map[Address]string `json:"http"`
	Input       json.RawMessage    `json:"input"`
	Output      json.RawMessage    `json:"output"`
}

// listedPlugin is a plugin with the folder it came from.
type listedPlugin struct {
	manifestPlugin
	target Target
}

// List answers the `plugins` tool for a folder: every plugin in a line per
// method, or with plugin set, that one in full. crons.json is read again
// first, so an edit shows.
func (s *Service) List(ctx context.Context, path, plugin string) (outcome.Answer, error) {
	if plugin == "" {
		l, failure, err := s.listAll(ctx, path)
		if err != nil || failure != nil {
			return outcome.Answer{Error: failure}, err
		}
		return encode(l)
	}
	d, failure, err := s.describeOne(ctx, path, plugin)
	if err != nil || failure != nil {
		return outcome.Answer{Error: failure}, err
	}
	return encode(d)
}

// Listing is every plugin a folder can use, for plugins.list; an empty path
// is the environment alone. Plugins that
// failed to load answer why in Error; an environment without plugins has none.
func (s *Service) Listing(ctx context.Context, path string) (Listing, error) {
	l, failure, err := s.listAll(ctx, path)
	switch {
	case errors.Is(err, ErrNoPlugins):
		return Listing{Plugins: []Listed{}}, nil
	case err != nil:
		return Listing{}, err
	case failure != nil:
		return Listing{Plugins: []Listed{}, Crons: s.Crons(), Error: failure.Message}, nil
	}
	return l, nil
}

// Detail is one plugin in full, for plugins.get.
func (s *Service) Detail(ctx context.Context, path, plugin string) (Detail, error) {
	d, failure, err := s.describeOne(ctx, path, plugin)
	if err == nil && failure != nil {
		err = fmt.Errorf("plugins: %s", failure.Message)
	}
	return d, err
}

func (s *Service) listAll(ctx context.Context, path string) (Listing, *outcome.Failure, error) {
	path = cmp.Or(path, s.cfg.Global)
	if s.cfg.Crons != "" {
		s.reconcile(ctx)
	}
	found, failure, err := s.manifests(ctx, path)
	if err != nil || failure != nil {
		return Listing{}, failure, err
	}
	return Listing{Plugins: listedAll(found, s.recentErrors(ctx), pages(checkout(path), s.cfg.Global), s.urlBases()), Crons: s.Crons()}, nil, nil
}

func (s *Service) describeOne(ctx context.Context, path, plugin string) (Detail, *outcome.Failure, error) {
	path = cmp.Or(path, s.cfg.Global)
	if s.cfg.Crons != "" {
		s.reconcile(ctx)
	}
	found, failure, err := s.manifests(ctx, path)
	if err != nil || failure != nil {
		return Detail{}, failure, err
	}
	i := slices.IndexFunc(found, func(p listedPlugin) bool { return p.Name == plugin })
	if i < 0 {
		names := make([]string, len(found))
		for i, p := range found {
			names[i] = p.Name
		}
		return Detail{}, nil, errkind.ErrNotFound.Errorf("plugins: there's no plugin %q here; there are %s", plugin, strings.Join(names, ", "))
	}
	p := found[i]
	d := describe(p, s.recentErrors(ctx)[errorsKey(p.target.Dir, p.Name)], pages(checkout(path), s.cfg.Global), s.cronsOf(p), s.urlBases())
	if slices.ContainsFunc(d.Methods, func(m DetailMethod) bool {
		return slices.ContainsFunc(m.URLs, func(u MethodURL) bool { return u.Key })
	}) {
		key, createdAt, err := s.cfg.Keys.PluginKey(p.Name)
		if err != nil {
			return Detail{}, nil, err
		}
		d.Key = &KeyStatus{Set: key != "", CreatedAt: createdAt}
	}
	return d, nil, nil
}

// Asking is how a method asks before it runs: Effect, and its approve
// question and the least strict mode that still asks (ApproveUpTo).
type Asking struct {
	Effect      string
	Approve     string
	ApproveUpTo string
}

// Method is how a folder's method asks, by its `<plugin>.<method>` name;
// false when no plugin has it.
func (s *Service) Method(ctx context.Context, path, name string) (Asking, bool, error) {
	found, failure, err := s.manifests(ctx, path)
	if err != nil {
		return Asking{}, false, err
	}
	if failure != nil {
		return Asking{}, false, fmt.Errorf("plugins: %s", failure.Message)
	}
	for _, p := range found {
		for _, m := range p.Methods {
			if m.Name == name {
				return Asking{Effect: m.Effect, Approve: m.Approve, ApproveUpTo: m.ApproveUpTo}, true, nil
			}
		}
	}
	return Asking{}, false, nil
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
	return listedAll(found, s.recentErrors(ctx), pages(checkout(path), s.cfg.Global), s.urlBases()), nil
}

func listedAll(found []listedPlugin, errorCounts map[string]int, all []Page, bases map[Address]string) []Listed {
	out := []Listed{}
	for _, p := range found {
		out = append(out, listed(p, errorCounts[errorsKey(p.target.Dir, p.Name)], all, bases))
	}
	return out
}

// manifests is every plugin a folder's project can use: the environment's,
// run for that project.
func (s *Service) manifests(ctx context.Context, path string) ([]listedPlugin, *outcome.Failure, error) {
	t, err := s.target(path)
	if err != nil {
		return nil, nil, err
	}
	return s.manifest(ctx, t)
}

// manifest is the plugins as their Worker lists them; a Worker's own failure
// comes back as it said it.
func (s *Service) manifest(ctx context.Context, t Target) ([]listedPlugin, *outcome.Failure, error) {
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
	out := make([]listedPlugin, 0, len(manifest.Plugins))
	for _, p := range manifest.Plugins {
		out = append(out, listedPlugin{manifestPlugin: p, target: t})
	}
	return out, nil, nil
}

func listed(p listedPlugin, errorCount int, all []Page, bases map[Address]string) Listed {
	out := Listed{Name: p.Name, Title: p.Title, Icon: pluginIcon(p.Icon, filepath.Join(p.target.Dir, p.Name)), Errors: errorCount, Page: indexPage(p.Name, all), UI: ownIndex(p.Name, filepath.Join(p.target.Dir, p.Name)), Description: p.Description, Methods: []ListedMethod{}}
	for _, m := range p.Methods {
		var at []Address
		for _, u := range methodURLs(bases, p.Name, m) {
			at = append(at, u.Address)
		}
		out.Methods = append(out.Methods, ListedMethod{Name: m.Name, Effect: m.Effect, Stream: m.Stream, Asks: m.Approve != "", Input: Signature(m.Input), Description: m.Description, HTTP: at})
	}
	return out
}

func describe(p listedPlugin, errorCount int, all []Page, crons []Cron, bases map[Address]string) Detail {
	dir := filepath.Join(p.target.Dir, p.Name)
	out := Detail{Name: p.Name, Title: p.Title, Icon: pluginIcon(p.Icon, dir), Path: dir, Errors: errorCount, Description: p.Description, Methods: []DetailMethod{}, Storage: present(p.Storage), Screens: screens(p.Name, dir), Crons: crons}
	for _, m := range p.Methods {
		out.Methods = append(out.Methods, DetailMethod{Name: m.Name, Effect: m.Effect, Stream: m.Stream, Description: m.Description, Approve: m.Approve, ApproveUpTo: m.ApproveUpTo, Input: schema(m.Input), Output: schema(m.Output), URLs: methodURLs(bases, p.Name, m)})
	}
	for _, page := range all {
		if ownMethod(p.Name, page.Method) {
			out.Pages = append(out.Pages, page)
		}
	}
	return out
}

// cronsOf is the lines of crons.json naming a plugin.
func (s *Service) cronsOf(p listedPlugin) []Cron {
	var out []Cron
	for _, c := range s.Crons() {
		if c.Plugin == p.Name {
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
