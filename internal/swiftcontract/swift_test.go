package swiftcontract

import (
	"reflect"
	"strings"
	"testing"

	fsrpc "github.com/repogo/host/internal/rpc/fs"
	mcprpc "github.com/repogo/host/internal/rpc/mcp"
)

type customWire string

func (customWire) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

type customText string

func (customText) MarshalText() ([]byte, error) { return []byte("custom"), nil }

func TestUnsupportedSwiftMappingsFail(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[uint64](), reflect.TypeFor[map[int]string](),
		reflect.TypeFor[customText](), reflect.TypeFor[customWire](), reflect.TypeFor[chan string](),
	} {
		t.Run(typ.String(), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("unsupported mapping was accepted")
				}
			}()
			(&swiftGen{}).typeRef(typ, "Unsupported")
		})
	}
}

func TestSwiftPresenceAndPublicInit(t *testing.T) {
	type fields struct {
		Required *string  `json:"required"`
		Omitted  string   `json:"omitted,omitempty"`
		Both     *string  `json:"both,omitempty"`
		Items    []string `json:"items" wire:"array"`
	}
	typ := reflect.TypeFor[fields]()
	g := &swiftGen{dirs: map[reflect.Type]int{typ: encodes | decodes}}
	got := g.structBody(typ, "Fields")
	for _, want := range []string{
		"public init(required: String? = nil, omitted: String? = nil, both: String? = nil, items: [String])",
		"required = try c.decodeIfPresent(String.self, forKey: .required)",
		"both = try c.decodeIfPresent(String.self, forKey: .both)",
		"try c.encodeIfPresent(required, forKey: .required)",
		"try c.encodeIfPresent(both, forKey: .both)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestParameterPresence(t *testing.T) {
	type params struct {
		Label    string  `json:"label,omitempty"`
		Token    *string `json:"token,omitempty"`
		Required *string `json:"required"`
	}
	typ := reflect.TypeFor[params]()
	g := &swiftGen{dirs: map[reflect.Type]int{typ: encodes}}
	got := g.structBody(typ, "MCP.TestParams")
	for _, want := range []string{
		"label: String? = nil, token: String? = nil, required: String?",
		"try c.encodeIfPresent(token, forKey: .token)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFamilyNames(t *testing.T) {
	for family, want := range map[string]string{"mcp": "MCP", "fs": "FS", "tunnels": "Tunnels", "hosts": "Hosts"} {
		if got := swiftFamily(family); got != want {
			t.Errorf("%s: %s, want %s", family, got, want)
		}
	}
}

func TestAddingFamilyKeepsExistingNames(t *testing.T) {
	mcpType := reflect.TypeFor[mcprpc.ListParams]()
	fsType := reflect.TypeFor[fsrpc.ListParams]()
	g := &swiftGen{names: map[reflect.Type]string{}, seen: []reflect.Type{mcpType}}
	g.assignNames()
	before := g.names[mcpType]
	g.seen = append(g.seen, fsType)
	g.assignNames()
	if g.names[mcpType] != before || before != "MCP.ListParams" || g.names[fsType] != "FS.ListParams" {
		t.Fatalf("unstable family names: %v", g.names)
	}
}

func TestNoDoubleOptionals(t *testing.T) {
	type fields struct {
		Pointer       **string          `json:"pointer,omitempty"`
		Items         []string          `json:"items,omitempty"`
		RequiredItems []string          `json:"required_items"`
		Mapping       map[string]string `json:"mapping,omitempty"`
	}
	typ := reflect.TypeFor[fields]()
	for _, dir := range []int{encodes, decodes, encodes | decodes} {
		g := &swiftGen{dirs: map[reflect.Type]int{typ: dir}}
		got := g.structBody(typ, "Fields")
		if strings.Contains(got, "??") {
			t.Fatalf("double optional in direction %d:\n%s", dir, got)
		}
		want := "public var requiredItems: [String]\n"
		if dir&decodes != 0 {
			want = "public var requiredItems: [String]?\n"
		}
		if !strings.Contains(got, want) {
			t.Fatalf("wrong collection optionality:\n%s", got)
		}
	}
}

func TestSwiftSelfFieldDoesNotShadowReceiver(t *testing.T) {
	type fields struct {
		Self string `json:"self"`
	}
	typ := reflect.TypeFor[fields]()
	g := &swiftGen{dirs: map[reflect.Type]int{typ: encodes | decodes}}
	got := g.structBody(typ, "Fields")
	for _, want := range []string{
		"public var selfValue: String", "self.selfValue = selfValue",
		`case selfValue = "self"`, "selfValue = try c.decode(String.self, forKey: .selfValue)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in generated self field:\n%s", want, got)
		}
	}
}
