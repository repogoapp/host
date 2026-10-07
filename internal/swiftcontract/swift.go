// Package swiftcontract generates the Foundation-only Swift wire contract.
package swiftcontract

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	hostemit "github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/wiredoc"
)

var methodExports = map[string]bool{
	"cloud.projects.list": true,
	"vercel.logs":         true,
	"vercel.deployments":  true,
	"chats.messages":      true, "chats.subscribe": true, "chats.unsubscribe": true,
	"chats.tools_subscribe": true, "chats.tools_unsubscribe": true,
	"chats.subagent": true, "chats.neighbors": true,
	"limits.read": true, "limits.reset": true,
	// Usage, devices and project picking.
	"usage.history": true, "usage.daily": true, "devices.register_push": true, "devices.list": true,
	"github.repos": true, "github.clone": true, "fs.add_project": true, "fs.new_project": true,
	// Editor, git and chat list rows.
	"fs.write": true, "fs.replace": true, "fs.mkdir": true, "fs.rename": true,
	"fs.delete": true, "fs.search": true, "fs.watch": true, "fs.stop": true,
	"git.status": true, "git.diff": true, "git.changes": true, "git.branches": true,
	"git.pull": true, "git.reset_hard": true, "git.switch_branch": true,
	"git.create_branch": true, "git.patch": true, "git.commit_push": true,
	"github.pr_create": true, "github.publish": true,
	"chats.list": true, "chats.info": true, "chats.resolve": true, "chats.update": true,
	"chats.delete": true, "chats.handoff": true, "sync.pull": true,
	// A chat's turns, in the order the user acts on them.
	"chats.send": true, "chats.start": true, "turns.list": true, "turns.get": true, "turns.stop": true, "chats.stop": true,
	"turns.respond": true, "turns.edit_queued": true, "turns.remove_queued": true,
	"turns.send_queued": true, "chats.queue": true,
	// Projects and actions.
	"project.list": true, "project.detect_icon": true, "project.rename": true, "project.pin": true,
	"actions.list": true, "actions.run": true, "actions.start": true, "actions.stop": true,
	// Terminal and forwarding.
	"terminal.create": true, "terminal.list": true, "terminal.subscribe": true,
	"terminal.unsubscribe": true, "terminal.attach": true, "terminal.detach": true,
	"terminal.input": true, "terminal.resize": true, "terminal.close": true,
	"ports.list": true, "ports.kill": true, "forward.fetch": true,
	"forward.pipe_open": true, "forward.pipe_send": true, "forward.pipe_close": true,
	"fs.read": true,
	// Hosts and setup.
	"host.status": true, "host.setup": true, "host.update": true, "host.set_stops_at": true,
	"github.avatar": true,
	"tools.install": true, "tools.update": true, "tools.login_start": true,
	"tools.login_complete": true, "tools.login_cancel": true, "tools.logins": true,
	"tools.logout": true, "tools.uninstall": true,
	// Env sources and folder browsing.
	"env.sources.list": true, "env.sources.set": true, "env.sources.remove": true,
	"env.read": true, "env.pending": true, "env.provide": true,
	"fs.list": true,
	// Browser requests.
	"browser.pending": true, "browser.respond": true,
	// MCP.
	"mcp.list": true, "mcp.probe": true, "mcp.connect": true,
	"mcp.oauth_start": true, "mcp.oauth_finish": true, "mcp.rename": true,
	"mcp.set_enabled": true, "mcp.remove": true,
	// Tunnels.
	"tunnels.list": true, "tunnels.open": true, "tunnels.close": true, "hosts.claim": true,
	// A cloud card's environment variables.
	"cloud.env.list": true, "cloud.env.value": true, "cloud.env.set": true, "cloud.env.remove": true,
}

// methodsGen generates the client's method types: the bodies of types the event
// file does not own, and each family's method constants.
func methodsGen(specs []rpc.Spec) (*swiftGen, *swiftGen, map[string]string) {
	g := &swiftGen{names: map[reflect.Type]string{}, dirs: map[reflect.Type]int{}}
	var exports []rpc.Spec
	for _, s := range specs {
		if methodExports[s.Name] {
			exports = append(exports, s)
			g.collect(s.Params, map[reflect.Type]bool{})
			g.collect(s.Result, map[reflect.Type]bool{})
			g.markDir(s.Params, encodes)
			g.markDir(s.Result, decodes)
		}
	}
	g.assignNames()
	events := eventGenerator(hostemit.Catalog())
	for typ, name := range events.names {
		g.names[typ] = name
	}
	found := map[string]bool{}
	members := map[string]string{}
	for _, s := range exports {
		found[s.Name] = true
		split := strings.LastIndexByte(s.Name, '.')
		family, method := s.Name[:split], s.Name[split+1:]
		namespace := swiftFamily(family)
		base := namespace + "." + pascal(method)
		if strings.EqualFold(s.Result.Name(), pascal(method)+"Result") {
			owner, _, _ := strings.Cut(g.names[s.Result], ".")
			g.names[s.Result] = owner + "." + pascal(method) + "Result"
		}
		p := base + "Params"
		g.queue = append(g.queue, pending{s.Params, p})
		r := g.typeRef(s.Result, base+"Result")
		members[namespace] += fmt.Sprintf("  public static let %s = HostAPIMethod<%s, %s>(%q)\n", swiftIdent(method), "HostAPI."+p, r, s.Name)
	}
	for name := range methodExports {
		if !found[name] {
			panic("unknown Swift export: " + name)
		}
	}
	g.drain()
	for _, name := range events.names {
		delete(g.bodies, name)
	}
	return g, events, members
}

func Methods(specs []rpc.Spec) string {
	g, events, members := methodsGen(specs)
	names := make([]string, 0, len(g.bodies))
	for name := range g.bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := g.bodies[name]
		family, _, _ := strings.Cut(name, ".")
		members[family] += "\n" + body
	}
	if strings.Contains(fmt.Sprint(members), "Shared.JSONValue") {
		members["Shared"] += "\n" + swiftJSONValue
	}
	for _, name := range events.names {
		family, _, _ := strings.Cut(name, ".")
		if _, ok := members[family]; !ok {
			members[family] = ""
		}
	}
	families := make([]string, 0, len(members))
	for family := range members {
		families = append(families, family)
	}
	sort.Strings(families)
	var b strings.Builder
	b.WriteString("// Generated by `go test ./internal/rpc/conformance -update` in apps/host. Do not edit.\n\nimport Foundation\n\n")
	b.WriteString(swiftPrelude)
	b.WriteString("\npublic nonisolated enum HostAPI {\n")
	for _, family := range families {
		fmt.Fprintf(&b, "  public nonisolated enum %s {}\n", family)
	}
	b.WriteString("}\n")
	for _, family := range families {
		fmt.Fprintf(&b, "\nextension HostAPI.%s {\n%s}\n", family, members[family])
	}
	return b.String()
}

// Initialisms use their conventional uppercase spelling in family namespaces.
func swiftFamily(family string) string {
	switch family {
	case "mcp", "fs", "rpc":
		return strings.ToUpper(family)
	default:
		return pascal(family)
	}
}

const swiftPrelude = `public protocol HostEvent: Decodable {
  static var method: String { get }
}

/// A family's ordered events, decoded by their full wire names.
public protocol HostEventFamily: Sendable {
  static var family: String { get }
  static func decode(method: String, data: Data) throws -> Self?
}

public nonisolated struct HostAPIMethod<Params: Encodable & Sendable, Result: Decodable & Sendable>: Sendable {
  public let name: String
  public init(_ name: String) { self.name = name }
}
`

const swiftJSONValue = `  /// Any JSON the host sends untyped.
  public nonisolated enum JSONValue: Codable, Hashable, Sendable {
    case null, bool(Bool), number(Double), string(String), array([JSONValue]), object([String: JSONValue])

    public init(from decoder: Decoder) throws {
      let c = try decoder.singleValueContainer()
      if c.decodeNil() { self = .null }
      else if let v = try? c.decode(Bool.self) { self = .bool(v) }
      else if let v = try? c.decode(Double.self) { self = .number(v) }
      else if let v = try? c.decode(String.self) { self = .string(v) }
      else if let v = try? c.decode([JSONValue].self) { self = .array(v) }
      else { self = .object(try c.decode([String: JSONValue].self)) }
    }

    public func encode(to encoder: Encoder) throws {
      var c = encoder.singleValueContainer()
      switch self {
      case .null: try c.encodeNil()
      case .bool(let v): try c.encode(v)
      case .number(let v): try c.encode(v)
      case .string(let v): try c.encode(v)
      case .array(let v): try c.encode(v)
      case .object(let v): try c.encode(v)
      }
    }
  }
`

type swiftGen struct {
	names        map[reflect.Type]string // Go struct → Swift name
	seen         []reflect.Type
	bodies       map[string]string
	queue        []pending
	dirs         map[reflect.Type]int // encodes, decodes, or both
	props        map[string][]prop    // Swift name → its stored properties, in init order
	conformances map[string]string    // Swift name → the conformance its struct declares
}

// prop is one stored property of a generated struct.
type prop struct {
	swift, json, ty string
	optional        bool
	// omitted is Go's omitempty: absent when empty, where a nullable field without it is sent as null.
	omitted bool
}

const (
	encodes = 1 << iota
	decodes
)

func (g *swiftGen) markDir(t reflect.Type, d int) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		if t == wiredoc.RawMessage {
			return
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == timeT || g.dirs[t]&d != 0 {
		return
	}
	g.dirs[t] |= d
	for _, f := range wiredoc.Fields(t) {
		g.markDir(f.Type, d)
	}
}

// conformance gives a type only the direction it crosses the wire: Codable
// everywhere costs the App Clip about a fifth more.
func (g *swiftGen) conformance(t reflect.Type) string {
	if t.PkgPath() == "github.com/repogo/host/internal/agentusage" || (t.PkgPath() == "github.com/repogo/host/internal/clitool" && t.Name() == "Account") {
		return "Codable"
	}
	// Daily reports are persisted until the leaderboard accepts them.
	if t.PkgPath() == "github.com/repogo/host/internal/shipping" && (t.Name() == "Report" || t.Name() == "Bucket") {
		return "Codable"
	}
	if (t.PkgPath() == "github.com/repogo/host/internal/git" && (t.Name() == "Status" || t.Name() == "ChangeSet" || t.Name() == "FileChange")) ||
		(t.PkgPath() == "github.com/repogo/host/internal/files" && t.Name() == "Entry") ||
		(t.PkgPath() == "github.com/repogo/host/internal/store" && t.Name() == "VoiceHandle") {
		return "Codable"
	}
	if (t.PkgPath() == "github.com/repogo/host/internal/store" && t.Name() == "Project") ||
		(t.PkgPath() == "github.com/repogo/host/internal/rpc/project" && t.Name() == "ListResult") ||
		t.PkgPath() == "github.com/repogo/host/internal/agentcatalog" ||
		(t.PkgPath() == "github.com/repogo/host/internal/actions" && t.Name() == "Action") ||
		(t.PkgPath() == "github.com/repogo/host/internal/rpc/actions" && t.Name() == "ListResult") {
		return "Codable"
	}
	// The agent picker paints the last host.setup it saved before asking again.
	if (t.PkgPath() == "github.com/repogo/host/internal/hostsetup" && (t.Name() == "Setup" || t.Name() == "Agent")) ||
		(t.PkgPath() == "github.com/repogo/host/internal/clitool" && t.Name() == "Install") ||
		(t.PkgPath() == "github.com/repogo/host/internal/clilogin" && t.Name() == "Code") ||
		(t.PkgPath() == "github.com/repogo/host/internal/hostinfo" && t.Name() == "Release") ||
		(t.PkgPath() == "github.com/repogo/host/internal/github" && t.Name() == "CloneFolder") {
		return "Codable"
	}
	// Battery is persisted in widget snapshots; status is encoded for diagnostics.
	if (t.PkgPath() == "github.com/repogo/host/internal/power" && t.Name() == "Battery") ||
		(t.PkgPath() == "github.com/repogo/host/internal/hostinfo" && t.Name() == "Status") ||
		(t.PkgPath() == "github.com/repogo/host/internal/hostinfo" && (t.Name() == "Network" || t.Name() == "Cloud")) {
		return "Codable"
	}
	switch g.dirs[t] {
	case encodes:
		return "Encodable"
	case decodes:
		return "Decodable"
	}
	return "Codable"
}

type pending struct {
	t    reflect.Type
	name string
}

var (
	timeT          = reflect.TypeOf(time.Time{})
	marshalerT     = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerT = reflect.TypeFor[encoding.TextMarshaler]()
)

func (g *swiftGen) collect(t reflect.Type, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		if t == wiredoc.RawMessage || (t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8) {
			return
		}
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] || t == timeT {
		return
	}
	seen[t] = true
	if t.Name() != "" {
		g.seen = append(g.seen, t)
	}
	for _, f := range wiredoc.Fields(t) {
		g.collect(f.Type, seen)
	}
}

// Ownership is explicit so exporting another family never renames existing types.
var typeFamilies = map[string]string{
	"github.com/repogo/host/internal/fly":           "cloud.projects",
	"github.com/repogo/host/internal/cloudprojects": "cloud.projects",
	"github.com/repogo/host/internal/cloudenv":      "cloud.env",
	"github.com/repogo/host/internal/cloudscan":     "cloud.projects",
	"github.com/repogo/host/internal/rpc/cloud":     "cloud.projects",
	"github.com/repogo/host/internal/vercel":        "vercel",
	"github.com/repogo/host/internal/rpc/vercel":    "vercel",
	"github.com/repogo/host/internal/chatwire":      "chats",
	"github.com/repogo/host/internal/toollabel":     "chats",
	"github.com/repogo/host/internal/agentusage":    "limits",
	"github.com/repogo/host/internal/rpc/shipping":  "usage",
	"github.com/repogo/host/internal/shipping":      "usage",
	"github.com/repogo/host/internal/rpc/devices":   "devices",
	"github.com/repogo/host/internal/device":        "devices",
	"github.com/repogo/host/internal/rpc/git":       "git",
	"github.com/repogo/host/internal/git":           "git",
	"github.com/repogo/host/internal/projectwatch":  "git",
	"github.com/repogo/host/internal/rpc/chats":     "chats",
	"github.com/repogo/host/internal/chat":          "chats",
	"github.com/repogo/host/internal/chatlive":      "chats",
	"github.com/repogo/host/internal/rpc/sync":      "sync",
	"github.com/repogo/host/internal/rpc/project":   "project",
	"github.com/repogo/host/internal/project":       "project",
	"github.com/repogo/host/internal/projectsync":   "project",
	"github.com/repogo/host/internal/store":         "project",
	"github.com/repogo/host/internal/rpc/actions":   "actions",
	"github.com/repogo/host/internal/actions":       "actions",
	"github.com/repogo/host/internal/agentcatalog":  "agents",
	"github.com/repogo/host/internal/rpc/terminal":  "terminal",
	"github.com/repogo/host/internal/terminal":      "terminal",
	"github.com/repogo/host/internal/rpc/ports":     "ports",
	"github.com/repogo/host/internal/ports":         "ports",
	"github.com/repogo/host/internal/rpc/forward":   "forward",
	"github.com/repogo/host/internal/forward":       "forward",

	"github.com/repogo/host/internal/rpc/host":    "host",
	"github.com/repogo/host/internal/hostinfo":    "host",
	"github.com/repogo/host/internal/hostsetup":   "host",
	"github.com/repogo/host/internal/hostupdate":  "host",
	"github.com/repogo/host/internal/power":       "host",
	"github.com/repogo/host/internal/rpc/github":  "github",
	"github.com/repogo/host/internal/github":      "github",
	"github.com/repogo/host/internal/rpc/turns":   "agents",
	"github.com/repogo/host/internal/agent":       "agents",
	"github.com/repogo/host/internal/clilogin":    "tools",
	"github.com/repogo/host/internal/clitool":     "tools",
	"github.com/repogo/host/internal/rpc/tools":   "tools",
	"github.com/repogo/host/internal/rpc/browser": "browser",
	"github.com/repogo/host/internal/repogomcp":   "browser",
	"github.com/repogo/host/internal/rpc/env":     "env",
	"github.com/repogo/host/internal/envsource":   "env",
	"github.com/repogo/host/internal/environment": "environment",
	"github.com/repogo/host/internal/files":       "fs",
	"github.com/repogo/host/internal/rpc":         "shared",
	"github.com/repogo/host/internal/rpc/mcp":     "mcp",
	"github.com/repogo/host/internal/mcp":         "mcp",
	"github.com/repogo/host/internal/rpc/tunnels": "tunnels",
	"github.com/repogo/host/internal/tunnel":      "tunnels",
	"github.com/repogo/host/internal/rpc/hosts":   "hosts",
	"github.com/repogo/host/internal/account":     "hosts",
	"github.com/repogo/host/internal/rpc/fs":      "fs",
}

func (g *swiftGen) assignNames() {
	for _, t := range g.seen {
		family, ok := typeFamilies[t.PkgPath()]
		if !ok {
			panic("declare Swift wire family for " + t.PkgPath())
		}
		if t.PkgPath() == "github.com/repogo/host/internal/store" {
			switch t.Name() {
			case "Chat", "VoiceHandle", "Queue", "QueuedTurn":
				family = "chats"
			case "PullRequest", "PullReply":
				family = "sync"
			}
		}
		if t.PkgPath() == "github.com/repogo/host/internal/projectwatch" && (t.Name() == "Change" || t.Name() == "FileChange") {
			family = "fs"
		}
		name := cleanName(t.Name())
		if swiftTypeNames[name] {
			name += "Value"
		}
		g.names[t] = swiftFamily(family) + "." + name
	}
}

// typeRef is the Swift spelling of t; hint names an anonymous struct.
func (g *swiftGen) typeRef(t reflect.Type, hint string) string {
	switch {
	case t.Kind() == reflect.Pointer:
		// A pointer is its element, optional; *time.Time must not read as a marshaler.
		return strings.TrimSuffix(g.typeRef(t.Elem(), hint), "?") + "?"
	case t == wiredoc.RawMessage:
		return "HostAPI.Shared.JSONValue"
	case t == reflect.TypeFor[json.Number]():
		panic("unsupported JSON number: " + t.String())
	case t == timeT:
		return "String"
	case t.Implements(marshalerT) || reflect.PointerTo(t).Implements(marshalerT) ||
		t.Implements(textMarshalerT) || reflect.PointerTo(t).Implements(textMarshalerT):
		panic("unsupported custom marshaler: " + t.String())
	}
	switch t.Kind() {
	case reflect.Pointer:
		return strings.TrimSuffix(g.typeRef(t.Elem(), hint), "?") + "?"
	case reflect.String:
		return "String"
	case reflect.Bool:
		return "Bool"
	case reflect.Int64:
		return "Int64"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return "Int"
	case reflect.Float32, reflect.Float64:
		return "Double"
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return "Data"
		}
		return "[" + g.typeRef(t.Elem(), hint+"Item") + "]"
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			panic("unsupported map key: " + t.String())
		}
		return "[String: " + g.typeRef(t.Elem(), hint+"Value") + "]"
	case reflect.Interface:
		if t.NumMethod() != 0 {
			panic("unsupported interface: " + t.String())
		}
		return "HostAPI.Shared.JSONValue"
	case reflect.Struct:
		name, ok := g.names[t]
		if !ok {
			name = hint
			g.names[t] = name
		}
		g.queue = append(g.queue, pending{t, name})
		if strings.Contains(name, ".") {
			return "HostAPI." + name
		}
		return name
	}
	panic("unsupported Swift mapping: " + t.String())
}

func (g *swiftGen) drain() {
	g.bodies = map[string]string{}
	owners := map[string]reflect.Type{}
	for len(g.queue) > 0 {
		p := g.queue[0]
		g.queue = g.queue[1:]
		if owner, ok := owners[p.name]; ok && owner != p.t {
			panic("Swift name has two owners: " + p.name)
		}
		owners[p.name] = p.t
		if _, done := g.bodies[p.name]; done {
			continue
		}
		g.bodies[p.name] = "" // reserve against recursion
		g.bodies[p.name] = g.structBody(p.t, p.name)
	}
}

func (g *swiftGen) structBody(t reflect.Type, name string) string {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || !f.IsExported() {
			continue
		}
		if strings.Contains(tag, ",string") || strings.Contains(tag, "omitzero") || (f.Anonymous && f.Type.Kind() == reflect.Pointer) {
			panic("unsupported JSON field: " + t.String() + "." + f.Name)
		}
	}
	fields := wiredoc.Fields(t)
	var b strings.Builder
	fmt.Fprintf(&b, "  public nonisolated struct %s: %s, Hashable, Sendable {\n", name[strings.LastIndexByte(name, '.')+1:], g.conformance(t))
	var props []prop
	used := map[string]bool{}
	for _, f := range fields {
		sw := swiftIdent(f.Name)
		for used[sw] {
			sw += "_"
		}
		used[sw] = true
		ty := g.typeRef(f.Type, name+pascal(f.Name))
		optional := f.Omitted || f.Type.Kind() == reflect.Pointer
		if g.dirs[t]&decodes != 0 {
			optional = optional || f.Nullable
		}
		if optional && !strings.HasSuffix(ty, "?") {
			ty += "?"
		}
		fmt.Fprintf(&b, "    public var %s: %s\n", sw, ty)
		props = append(props, prop{sw, f.Name, ty, optional, f.Omitted})
	}
	if g.props == nil {
		g.props, g.conformances = map[string][]prop{}, map[string]string{}
	}
	g.props[name] = props
	g.conformances[name] = g.conformance(t)
	b.WriteString("\n    public init(")
	for i, p := range props {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %s", p.swift, p.ty)
		if p.optional {
			b.WriteString(" = nil")
		}
	}
	b.WriteString(") {\n")
	for _, p := range props {
		fmt.Fprintf(&b, "      self.%s = %s\n", p.swift, p.swift)
	}
	b.WriteString("    }\n")
	if len(props) > 0 {
		b.WriteString("\n    enum CodingKeys: String, CodingKey {\n")
		for _, p := range props {
			fmt.Fprintf(&b, "      case %s = %q\n", p.swift, p.json)
		}
		b.WriteString("    }\n")
		if g.dirs[t]&decodes != 0 {
			b.WriteString("\n    public init(from decoder: Decoder) throws {\n      let c = try decoder.container(keyedBy: CodingKeys.self)\n")
			for _, p := range props {
				op := "decode"
				ty := p.ty
				if p.optional {
					ty = strings.TrimSuffix(ty, "?")
					op = "decodeIfPresent"
				}
				fmt.Fprintf(&b, "      %s = try c.%s(%s.self, forKey: .%s)\n", p.swift, op, ty, p.swift)
			}
			b.WriteString("    }\n")
		}
		if g.dirs[t]&encodes != 0 || g.conformance(t) == "Codable" {
			b.WriteString("\n    public func encode(to encoder: Encoder) throws {\n      var c = encoder.container(keyedBy: CodingKeys.self)\n")
			for _, p := range props {
				if p.optional {
					fmt.Fprintf(&b, "      try c.encodeIfPresent(%s, forKey: .%s)\n", p.swift, p.swift)
				} else {
					fmt.Fprintf(&b, "      try c.encode(%s, forKey: .%s)\n", p.swift, p.swift)
				}
			}
			b.WriteString("    }\n")
		}

	}

	b.WriteString("  }\n")
	return b.String()
}

// Names a nested type must not take: keywords, and types the generated code
// itself spells.
var swiftTypeNames = map[string]bool{"Self": true, "Type": true, "Protocol": true, "Any": true,
	"String": true, "Bool": true, "Int": true, "Int64": true, "Double": true, "Data": true,
	"Date": true, "URL": true, "Error": true, "Optional": true, "JSONValue": true}

var swiftKeywords = map[string]bool{"default": true, "case": true, "class": true, "enum": true, "extension": true,
	"func": true, "import": true, "init": true, "let": true, "var": true, "protocol": true, "struct": true,
	"self": true, "Self": true, "static": true, "subscript": true, "switch": true, "where": true, "while": true,
	"return": true, "repeat": true, "in": true, "is": true, "as": true, "operator": true, "private": true,
	"public": true, "internal": true, "for": true, "if": true, "else": true, "do": true, "try": true,
	"throw": true, "throws": true, "catch": true, "break": true, "continue": true, "true": true, "false": true,
	"nil": true, "super": true, "Type": true, "defer": true, "guard": true, "inout": true, "fallthrough": true}

func swiftIdent(jsonName string) string {
	s := lowerFirst(pascal(jsonName))
	if s == "self" {
		return "selfValue"
	}
	if s == "" {
		s = "value"
	}
	if unicode.IsDigit(rune(s[0])) {
		s = "_" + s
	}
	if swiftKeywords[s] {
		return "`" + s + "`"
	}
	return s
}

func pascal(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '_' || r == '-' || r == '.' || r == ' ' || r == '/' {
			up = true
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		if up {
			b.WriteRune(unicode.ToUpper(r))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// cleanName drops generic brackets: Page[pkg.Item] → PageItem.
func cleanName(n string) string {
	if i := strings.IndexByte(n, '['); i >= 0 {
		inner := n[i+1 : len(n)-1]
		inner = inner[strings.LastIndexByte(inner, '.')+1:]
		return pascal(n[:i]) + pascal(inner)
	}
	return pascal(n)
}

// EventExports names only features using typed subscriptions.
var EventExports = map[string]string{
	"chats.appended": "Chats.Appended", "chats.streaming": "Chats.Streaming",
	"chats.approval": "Chats.Approval", "chats.tool": "Chats.ToolChanged",
	"fs.change":           "FS.Change",
	"git.changed":         "Git.Changed",
	"chats.changed":       "Chats.Changed",
	"chats.removed":       "Chats.Removed",
	"chats.attention":     "Chats.Attention",
	"terminal.changed":    "Terminal.Changed",
	"terminal.output":     "Terminal.Output",
	"terminal.exit":       "Terminal.Exit",
	"forward.pipe_data":   "Forward.PipeData",
	"forward.pipe_closed": "Forward.PipeClosed",
	"host.power":          "Host.Power",
	"host.setup_changed":  "Host.SetupChanged",
	"browser.request":     "Browser.Request",
	"env.request":         "Env.Request",
	"environment.status":  "Environment.Status",
	"mcp.changed":         "MCP.Changed",
	"tunnels.changed":     "Tunnels.Changed",
	"devices.changed":     "Devices.Changed",
	"project.changed":     "Project.Changed",
}

func eventGenerator(catalog []hostemit.Event) *swiftGen {
	g := &swiftGen{names: map[reflect.Type]string{}, dirs: map[reflect.Type]int{}}
	found := map[string]bool{}
	for _, ev := range catalog {
		if _, ok := EventExports[ev.Method()]; !ok {
			continue
		}
		typ := reflect.TypeOf(ev)
		g.collect(typ, map[reflect.Type]bool{})
		g.markDir(typ, decodes)
		found[ev.Method()] = true
	}
	for method := range EventExports {
		if !found[method] {
			panic("unknown Swift event export: " + method)
		}
	}
	g.assignNames()
	for _, ev := range catalog {
		if name, ok := EventExports[ev.Method()]; ok {
			g.names[reflect.TypeOf(ev)] = name
			// A row-only event shares its generated shape with method snapshots.
			typ := reflect.TypeOf(ev)
			if typ.NumField() == 1 && typ.Field(0).Anonymous {
				g.names[typ.Field(0).Type] = name
			}
			g.typeRef(reflect.TypeOf(ev), name)
		}
	}
	g.drain()
	return g
}

// EventFamilies preserve wire order when a consumer needs several event kinds.
var EventFamilies = map[string]bool{"terminal": true, "forward": true, "chats": true}

// Events owns shared snapshot types so a method and its push use one shape.
func Events(catalog []hostemit.Event) string {
	g := eventGenerator(catalog)
	var b strings.Builder
	b.WriteString("// Generated by `go test ./internal/emit -update` in apps/host. Do not edit.\n\nimport Foundation\n")
	names := make([]string, 0, len(g.bodies))
	for name := range g.bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		family, _, _ := strings.Cut(name, ".")
		fmt.Fprintf(&b, "\nextension HostAPI.%s {\n%s}\n", family, g.bodies[name])
	}
	for _, ev := range catalog {
		if name, ok := EventExports[ev.Method()]; ok {
			fmt.Fprintf(&b, "\nextension HostAPI.%s: HostEvent {\n  public static let method = %q\n}\n", name, ev.Method())
		}
	}
	for _, family := range []string{"terminal", "forward", "chats"} {
		namespace := swiftFamily(family)
		fmt.Fprintf(&b, "\nextension HostAPI.%s {\n  public nonisolated enum Event: HostEventFamily {\n", namespace)
		for _, ev := range catalog {
			if strings.HasPrefix(ev.Method(), family+".") && EventExports[ev.Method()] != "" {
				name := EventExports[ev.Method()]
				_, bare, _ := strings.Cut(ev.Method(), ".")
				fmt.Fprintf(&b, "    case %s(HostAPI.%s)\n", swiftIdent(bare), name)
			}
		}
		fmt.Fprintf(&b, "    public static let family = %q\n", family)
		b.WriteString("    public static func decode(method: String, data: Foundation.Data) throws -> Self? {\n      switch method {\n")
		for _, ev := range catalog {
			if strings.HasPrefix(ev.Method(), family+".") && EventExports[ev.Method()] != "" {
				name := EventExports[ev.Method()]
				_, bare, _ := strings.Cut(ev.Method(), ".")
				fmt.Fprintf(&b, "      case HostAPI.%s.method: return .%s(try JSONDecoder().decode(HostAPI.%s.self, from: data))\n", name, swiftIdent(bare), name)
			}
		}
		b.WriteString("      default: return nil\n      }\n    }\n  }\n}\n")
	}
	return b.String()
}
