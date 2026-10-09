//go:build native

package native

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo LDFLAGS: ${SRCDIR}/lib/librepogo_import.a
#include <stdlib.h>
#include "import.h"
*/
import "C"

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"unsafe"

	"github.com/repogo/host/internal/agent"
)

// kinds maps the kind byte in ffi.rs to the event kind, in the same order.
var kinds = [...]agent.EventKind{
	agent.EventTurnStarted, agent.EventUserMessage, agent.EventText, agent.EventReasoning,
	agent.EventToolCall, agent.EventToolResult, agent.EventTurnFinished, agent.EventTurnFailed,
}

const none = ^uint32(0)

type reader struct {
	buf []byte
	pos int
	bad bool
}

func (r *reader) ok() bool { return !r.bad }

func (r *reader) need(n int) bool {
	if r.bad || r.pos+n > len(r.buf) {
		r.bad = true
		return false
	}
	return true
}

func (r *reader) u8() byte {
	if !r.need(1) {
		return 0
	}
	v := r.buf[r.pos]
	r.pos++
	return v
}

func (r *reader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v
}

func (r *reader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v
}

func (r *reader) optStr() (string, bool) {
	n := r.u32()
	if n == none {
		return "", false
	}
	if !r.need(int(n)) {
		return "", false
	}
	s := string(r.buf[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, true
}

func (r *reader) str() string {
	s, _ := r.optStr()
	return s
}

// Available reports whether this build carries the native parser.
const Available = true

var errUnreadable = errors.New("native: file unreadable")

// ParseFile normalizes one transcript segment. Events come back without Seq
// or SessionID; the caller stamps those the way session's readSegment does.
func ParseFile(parser Parser, path, sessionID string) ([]agent.Event, error) {
	var which C.uint32_t
	switch parser {
	case ParserClaude:
		which = C.REPOGO_AGENT_CLAUDE
	case ParserCodex:
		which = C.REPOGO_AGENT_CODEX
	default:
		return nil, errors.New("native: unsupported agent")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	csid := C.CString(sessionID)
	defer C.free(unsafe.Pointer(csid))

	var n C.size_t
	buf := C.repogo_parse_file(which, cpath, csid, &n)
	if buf == nil {
		return nil, errUnreadable
	}
	defer C.repogo_free(buf, n)

	// The layout is documented in ffi.rs: a count, then per event a kind
	// byte, i64 at, two flag bytes, and eight length-prefixed strings. Each
	// string is copied once into Go memory; nothing is parsed.
	raw := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n))
	r := reader{buf: raw}
	count := int(r.u32())
	events := make([]agent.Event, 0, count)
	for i := 0; i < count && r.ok(); i++ {
		var e agent.Event
		e.Kind = kinds[r.u8()]
		e.At = int64(r.u64())
		hasTool := r.u8() != 0
		isError := r.u8() != 0
		e.TurnID = r.str()
		e.Text = r.str()
		e.Error = r.str()
		callID, name := r.str(), r.str()
		input, hasInput := r.optStr()
		output := r.str()
		result, hasResult := r.optStr()
		if hasTool {
			t := &agent.ToolCall{CallID: callID, Name: name, Output: output, IsError: isError}
			if hasInput {
				t.Input = []byte(input)
			}
			if hasResult {
				t.Result = &agent.ToolResult{}
				if json.Unmarshal([]byte(result), t.Result) != nil {
					return nil, errors.New("native: malformed tool result")
				}
			}
			e.Tool = t
		}
		events = append(events, e)
	}
	if !r.ok() || len(events) != count {
		return nil, errors.New("native: malformed event buffer")
	}
	return events, nil
}

func (r *reader) i64() int64 { return int64(r.u64()) }

// ScanUsage reads a transcript's token usage per request. The layout is
// documented in ffi.rs beside encode_usage.
func ScanUsage(parser Parser, path string) ([]Usage, error) {
	var which C.uint32_t
	switch parser {
	case ParserClaude:
		which = C.REPOGO_AGENT_CLAUDE
	case ParserCodex:
		which = C.REPOGO_AGENT_CODEX
	default:
		return nil, errors.New("native: unsupported agent")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var n C.size_t
	buf := C.repogo_scan_usage(which, cpath, &n)
	if buf == nil {
		return nil, errUnreadable
	}
	defer C.repogo_free(buf, n)

	r := reader{buf: unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n))}
	count := int(r.u32())
	out := make([]Usage, 0, count)
	for i := 0; i < count && r.ok(); i++ {
		u := Usage{Key: r.str(), Session: r.str(), Model: r.str()}
		u.At, u.Uncached, u.Cached = r.i64(), r.i64(), r.i64()
		u.Creation, u.Creation1h, u.Output, u.Reasoning = r.i64(), r.i64(), r.i64(), r.i64()
		u.Fast = r.u8() != 0
		u.HasReported = r.u8() != 0
		u.ReportedUSD = math.Float64frombits(r.u64())
		out = append(out, u)
	}
	if !r.ok() || len(out) != count {
		return nil, errors.New("native: malformed usage buffer")
	}
	return out, nil
}
