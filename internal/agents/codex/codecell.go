package codex

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// cellCall is one `tools.<name>(…)` call in a Codex code cell. Input is the
// argument as written: an object for most tools, the patch text for
// apply_patch, nil when it is an expression rather than a literal.
type cellCall struct {
	Name  string
	Input any
}

// codeCellCalls reads the calls an `exec` cell's JavaScript makes, in order.
// It skips strings and comments, notes `const x = <literal>` so
// `tools.apply_patch(patch)` finds its patch, and reads literal arguments.
func codeCellCalls(script string) []cellCall {
	p := &cellParser{src: script, bindings: map[string]any{}}
	return p.run()
}

type cellParser struct {
	src      string
	i        int
	bindings map[string]any
	calls    []cellCall
}

func (p *cellParser) run() []cellCall {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			p.string()
		case c == '/' && p.skipComment():
		case isIdentStart(c):
			switch p.identifier() {
			case "tools":
				p.toolCall()
			case "const", "let", "var":
				p.binding()
			}
		default:
			p.i++
		}
	}
	return p.calls
}

func (p *cellParser) toolCall() {
	p.skipSpace()
	if p.peek() != '.' {
		return
	}
	p.i++
	p.skipSpace()
	if !isIdentStart(p.peek()) {
		return
	}
	name := p.identifier()
	p.skipSpace()
	if p.peek() != '(' {
		return
	}
	p.i++
	start := p.i
	input, ok := p.value()
	if !ok {
		p.i = start
	}
	p.calls = append(p.calls, cellCall{Name: name, Input: input})
}

func (p *cellParser) binding() {
	p.skipSpace()
	if !isIdentStart(p.peek()) {
		return
	}
	name := p.identifier()
	p.skipSpace()
	if p.peek() != '=' || p.at(p.i+1) == '=' {
		return
	}
	p.i++
	start := p.i
	if v, ok := p.value(); ok {
		p.bindings[name] = v
	} else {
		p.i = start
	}
}

// value reads a literal, or a name bound to one; null is (nil, true).
func (p *cellParser) value() (any, bool) {
	p.skipSpace()
	c := p.peek()
	switch {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"' || c == '\'' || c == '`':
		return p.string()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case isIdentStart(c):
		switch name := p.identifier(); name {
		case "true":
			return true, true
		case "false":
			return false, true
		case "null", "undefined":
			return nil, true
		default:
			v, ok := p.bindings[name]
			return v, ok
		}
	}
	return nil, false
}

func (p *cellParser) object() (any, bool) {
	p.i++
	obj := map[string]any{}
	for {
		p.skipSpace()
		c := p.peek()
		if c == 0 {
			return nil, false
		}
		if c == '}' {
			p.i++
			return obj, true
		}
		var key string
		switch {
		case c == '"' || c == '\'':
			s, ok := p.string()
			if !ok {
				return nil, false
			}
			key = s.(string)
		case isIdentStart(c):
			key = p.identifier()
		default:
			return nil, false
		}
		p.skipSpace()
		if p.peek() == ':' {
			p.i++
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			obj[key] = v
		} else if bound, ok := p.bindings[key]; ok {
			obj[key] = bound
		} else {
			return nil, false
		}
		p.skipSpace()
		if p.peek() == ',' {
			p.i++
		} else if p.peek() != '}' {
			return nil, false
		}
	}
}

func (p *cellParser) array() (any, bool) {
	p.i++
	items := []any{}
	for {
		p.skipSpace()
		c := p.peek()
		if c == 0 {
			return nil, false
		}
		if c == ']' {
			p.i++
			return items, true
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		items = append(items, v)
		p.skipSpace()
		if p.peek() == ',' {
			p.i++
		} else if p.peek() != ']' {
			return nil, false
		}
	}
}

// string reads a quoted string with its escapes. A template's `${…}` is kept
// as written: only the cell's own run knows its value.
func (p *cellParser) string() (any, bool) {
	quote := p.src[p.i]
	p.i++
	var out strings.Builder
	depth := 0
	for p.i < len(p.src) {
		c := p.src[p.i]
		if depth > 0 {
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
			}
			out.WriteByte(c)
			p.i++
			continue
		}
		switch {
		case c == quote:
			p.i++
			return out.String(), true
		case c == '\\':
			p.i++
			if p.i >= len(p.src) {
				return nil, false
			}
			e := p.src[p.i]
			p.i++
			switch e {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case 'u':
				out.WriteString(p.unicodeEscape())
			case '\n':
			default:
				out.WriteByte(e)
			}
		case quote == '`' && c == '$' && p.at(p.i+1) == '{':
			depth = 1
			out.WriteString("${")
			p.i += 2
		default:
			out.WriteByte(c)
			p.i++
		}
	}
	return nil, false
}

// unicodeEscape reads `\uXXXX` (a surrogate pair's halves together) or
// `\u{…}` after the `\u`.
func (p *cellParser) unicodeEscape() string {
	var r rune
	if p.peek() == '{' {
		end := strings.IndexByte(p.src[p.i:], '}')
		if end < 0 {
			return ""
		}
		v, err := strconv.ParseUint(p.src[p.i+1:p.i+end], 16, 32)
		if err != nil {
			return ""
		}
		r = rune(v)
		p.i += end + 1
	} else {
		high, ok := p.hex4()
		if !ok {
			return ""
		}
		r = high
		if high >= 0xD800 && high <= 0xDBFF && p.at(p.i) == '\\' && p.at(p.i+1) == 'u' {
			save := p.i
			p.i += 2
			if low, ok := p.hex4(); ok && low >= 0xDC00 && low <= 0xDFFF {
				r = 0x10000 + (high-0xD800)<<10 + (low - 0xDC00)
			} else {
				p.i = save
			}
		}
	}
	if !utf8.ValidRune(r) {
		return ""
	}
	return string(r)
}

func (p *cellParser) hex4() (rune, bool) {
	if p.i+4 > len(p.src) {
		return 0, false
	}
	v, err := strconv.ParseUint(p.src[p.i:p.i+4], 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return rune(v), true
}

// number reads a numeric literal as a float64, as encoding/json would.
func (p *cellParser) number() (any, bool) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	for c := p.peek(); (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '_'; c = p.peek() {
		p.i++
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(p.src[start:p.i], "_", ""), 64)
	if err != nil {
		return nil, false
	}
	return f, true
}

// peek is the byte at i, 0 at the end.
func (p *cellParser) peek() byte { return p.at(p.i) }

func (p *cellParser) at(i int) byte {
	if i < len(p.src) {
		return p.src[i]
	}
	return 0
}

func (p *cellParser) identifier() string {
	start := p.i
	for c := p.peek(); isIdentStart(c) || (c >= '0' && c <= '9'); c = p.peek() {
		p.i++
	}
	return p.src[start:p.i]
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$' || c >= 0x80
}

func (p *cellParser) skipSpace() {
	for p.i < len(p.src) {
		switch c := p.src[p.i]; {
		case c == ' ' || c == '\n' || c == '\t' || c == '\r':
			p.i++
		case c == '/' && p.skipComment():
		default:
			return
		}
	}
}

// skipComment skips a `//` or `/* */` comment at i; false when the slash
// starts neither.
func (p *cellParser) skipComment() bool {
	switch p.at(p.i + 1) {
	case '/':
		for p.i < len(p.src) && p.src[p.i] != '\n' {
			p.i++
		}
		return true
	case '*':
		p.i += 2
		for p.i < len(p.src) && !(p.src[p.i] == '*' && p.at(p.i+1) == '/') {
			p.i++
		}
		p.i = min(p.i+2, len(p.src))
		return true
	}
	return false
}
