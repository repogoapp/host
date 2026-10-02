// Command payload measures what chats.messages pages and chats.streaming pushes
// cost on the wire today against smaller proposed shapes. It reads
// local sessions and prints sizes only; -out writes pages for the Swift decode bench.
package main

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

// wireBytes is what one message costs through the relay: securechan's codec
// (deflate at BestSpeed from 1 KB, kept at 90% or less), then the record byte
// and the AES-GCM tag.
func wireBytes(plain []byte) int {
	body := 1 + len(plain)
	if len(plain) >= 1<<10 {
		if n := 1 + deflated(plain); n*100 <= len(plain)*90+100 {
			body = n
		}
	}
	return 1 + body + 16
}

func deflated(b []byte) int {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(b)
	w.Close()
	return buf.Len()
}

// objectMessage is a store.Message whose tool call is a JSON object rather
// than a string holding one, so the phone decodes it once.
type objectMessage struct {
	Idx  int             `json:"idx"`
	Kind string          `json:"kind"`
	Turn string          `json:"turn,omitempty"`
	Text string          `json:"text,omitempty"`
	Tool json.RawMessage `json:"tool,omitempty"`
	At   int64           `json:"at,omitempty"`
}

// deferredTool is a tool call with its output cut to a preview; the rest
// would be fetched when the user expands the call.
type deferredTool struct {
	CallID      string          `json:"call_id,omitempty"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input,omitempty"`
	Output      string          `json:"output,omitempty"`
	OutputBytes int             `json:"output_bytes,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
}

const outputPreview = 512

// labelTool is the chat lane's tool: no input or output, only what the row
// draws. The labels are stand-ins of a typical length, built from the input.
type labelTool struct {
	CallID  string   `json:"call_id,omitempty"`
	Name    string   `json:"name"`
	Icon    string   `json:"icon"`
	Labels  []string `json:"labels"`
	IsError bool     `json:"is_error,omitempty"`
}

func labelEvents(msgs []store.Message) []objectMessage {
	out := objectEvents(msgs, false)
	for i := range out {
		var call agent.ToolCall
		if out[i].Tool == nil || json.Unmarshal(out[i].Tool, &call) != nil {
			continue
		}
		subject := string(call.Input)
		if len(subject) > 40 {
			subject = subject[:40]
		}
		b, _ := json.Marshal(labelTool{CallID: call.CallID, Name: call.Name, Icon: "terminal",
			Labels: []string{"Running " + subject, "Ran " + subject, "Command failed"}, IsError: call.IsError})
		out[i].Tool = b
	}
	return out
}

type objectPage struct {
	store.Page
	Events []objectMessage `json:"events"`
}

func objectEvents(msgs []store.Message, deferOutput bool) []objectMessage {
	out := make([]objectMessage, len(msgs))
	for i, m := range msgs {
		out[i] = objectMessage{Idx: m.Idx, Kind: m.Kind, Turn: m.Turn, Text: m.Text, At: m.At}
		if m.Tool == "" {
			continue
		}
		out[i].Tool = json.RawMessage(m.Tool)
		if !deferOutput {
			continue
		}
		var call agent.ToolCall
		if json.Unmarshal([]byte(m.Tool), &call) != nil || len(call.Output) <= outputPreview {
			continue
		}
		b, _ := json.Marshal(deferredTool{CallID: call.CallID, Name: call.Name, Input: call.Input,
			Output: call.Output[:outputPreview], OutputBytes: len(call.Output), IsError: call.IsError})
		out[i].Tool = b
	}
	return out
}

// valuesOnly is a lower bound for any schema'd binary format: every string
// and number in the message, length-prefixed, with no keys or punctuation.
func valuesOnly(doc []byte) []byte {
	var v any
	d := json.NewDecoder(bytes.NewReader(doc))
	d.UseNumber()
	d.Decode(&v)
	var buf bytes.Buffer
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			buf.Write(binary.AppendUvarint(nil, uint64(len(x))))
			buf.WriteString(x)
		case json.Number:
			if i, err := x.Int64(); err == nil {
				buf.Write(binary.AppendVarint(nil, i))
			} else {
				f, _ := x.Float64()
				buf.Write(binary.LittleEndian.AppendUint64(nil, math.Float64bits(f)))
			}
		case bool:
			buf.WriteByte(0)
		case []any:
			buf.Write(binary.AppendUvarint(nil, uint64(len(x))))
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k])
			}
		}
	}
	walk(v)
	return buf.Bytes()
}

func envelope(result any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	return b
}

type series struct {
	name  string
	raw   int64
	wire  int64
	sizes []int
}

func (s *series) add(plain []byte) {
	n := wireBytes(plain)
	s.raw += int64(len(plain))
	s.wire += int64(n)
	s.sizes = append(s.sizes, n)
}

func (s *series) pct(p float64) int {
	c := append([]int(nil), s.sizes...)
	sort.Ints(c)
	return c[int(float64(len(c)-1)*p)]
}

func kb(n int64) string { return fmt.Sprintf("%.1f KB", float64(n)/1000) }

// streamText is a turn's prose as chatlive streams it: text events joined,
// with a paragraph break where a tool call or thinking came between them.
func streamTexts(msgs []store.Message) map[string]string {
	texts := map[string]*strings.Builder{}
	breakPending := map[string]bool{}
	for _, m := range msgs {
		if m.Turn == "" {
			continue
		}
		switch agent.EventKind(m.Kind) {
		case agent.EventText:
			b := texts[m.Turn]
			if b == nil {
				b = &strings.Builder{}
				texts[m.Turn] = b
			}
			if breakPending[m.Turn] && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n\n")
			}
			breakPending[m.Turn] = false
			b.WriteString(m.Text)
		case agent.EventReasoning, agent.EventToolCall:
			breakPending[m.Turn] = true
		}
	}
	out := map[string]string{}
	for turn, b := range texts {
		out[turn] = b.String()
	}
	return out
}

// streamPush mirrors chatlive.Streaming as it goes out: the snapshot sends the
// turn's text so far; a delta sends only what is new, from offset.
type streamPush struct {
	ChatID       string `json:"chat_id"`
	TurnID       string `json:"turn_id"`
	Text         string `json:"text"`
	Offset       *int   `json:"offset,omitempty"`
	Done         bool   `json:"done"`
	StartedAt    int64  `json:"started_at,omitempty"`
	FirstFrameAt int64  `json:"first_frame_at,omitempty"`
	Epoch        string `json:"epoch"`
	Revision     int64  `json:"revision"`
}

func push(p streamPush) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "chats.streaming", "params": p})
	return b
}

func main() {
	var (
		limit   = flag.Int("sessions", 400, "sessions to read, newest first")
		perTick = flag.Int("chars-per-tick", 60, "streamed characters per 150 ms push (60 is about 400 chars/s)")
		outDir  = flag.String("out", "", "write open pages and streamed turns here for the Swift decode and FlatBuffers benches")
	)
	flag.Parse()

	registry, err := agents.New(agent.Dependencies{})
	if err != nil {
		panic(err)
	}
	defer registry.Close()
	st := session.NewStore(registry.Sessions...)
	metas, _ := st.List()
	if len(metas) > *limit {
		metas = metas[:*limit]
	}

	today := &series{name: "today: tool as a JSON string"}
	object := &series{name: "tool as an object"}
	deferred := &series{name: "object + output over 512 B deferred"}
	floor := &series{name: "floor: values only, no keys (any binary)"}
	labelsOnly := &series{name: "chat lane: labels only, no input/output"}
	var textBytes, inputBytes, outputBytes, pageBytes int64
	var marshalString, marshalObject time.Duration

	snapshot := &series{name: "snapshots (today)"}
	delta := &series{name: "deltas"}
	deltaFloor := &series{name: "deltas, values only"}
	var turns, streamedChars int64

	var stringPages, objectPages, labelPages, turnTexts []any
	used := 0
	for _, meta := range metas {
		_, events, err := st.Read(meta.ID)
		if err != nil || len(events) == 0 {
			continue
		}
		used++
		msgs := store.MessagesOf(events)
		lo := max(0, len(msgs)-40)
		page := store.Page{ChatID: store.ChatID(meta.ID), Events: msgs[lo:], HostID: "h", Cwd: meta.Cwd,
			Agent: string(meta.Agent), Generation: 1, EventCount: len(msgs), NextIdx: len(msgs), FirstIdx: lo, HasBefore: lo > 0}

		t := time.Now()
		a := envelope(page)
		marshalString += time.Since(t)
		op := objectPage{Page: page, Events: objectEvents(page.Events, false)}
		t = time.Now()
		b := envelope(op)
		marshalObject += time.Since(t)
		today.add(a)
		object.add(b)
		deferred.add(envelope(objectPage{Page: page, Events: objectEvents(page.Events, true)}))
		floor.add(valuesOnly(b))
		lp := objectPage{Page: page, Events: labelEvents(page.Events)}
		labelsOnly.add(envelope(lp))

		pageBytes += int64(len(a))
		for _, m := range page.Events {
			textBytes += int64(len(m.Text))
			var call agent.ToolCall
			if m.Tool != "" && json.Unmarshal([]byte(m.Tool), &call) == nil {
				inputBytes += int64(len(call.Input))
				outputBytes += int64(len(call.Output))
			}
		}
		if *outDir != "" {
			stringPages = append(stringPages, page)
			objectPages = append(objectPages, op)
			labelPages = append(labelPages, lp)
		}

		for turn, text := range streamTexts(msgs) {
			if text == "" {
				continue
			}
			turns++
			streamedChars += int64(len(text))
			base := streamPush{ChatID: string(page.ChatID), TurnID: turn, StartedAt: 1, FirstFrameAt: 1, Epoch: "e"}
			if *outDir != "" {
				turnTexts = append(turnTexts, streamPush{ChatID: base.ChatID, TurnID: turn, Text: text})
			}
			for end, rev := 0, int64(1); end < len(text); rev++ {
				start := end
				end = min(len(text), end+*perTick)
				s := base
				s.Revision, s.Text, s.Done = rev, text[:end], end == len(text)
				snapshot.add(push(s))
				d := base
				d.Revision, d.Text, d.Done, d.Offset = rev, text[start:end], end == len(text), &start
				delta.add(push(d))
				deltaFloor.add(valuesOnly(push(d)))
			}
		}
	}

	fmt.Printf("sessions read: %d of %d; opening a chat = the tail page of 40 events\n\n", used, len(metas))
	fmt.Printf("what an open page's JSON holds (before compression, %s in all):\n", kb(pageBytes))
	share := func(name string, n int64) {
		fmt.Printf("  %-26s %10s  %5.1f%%\n", name, kb(n), 100*float64(n)/float64(pageBytes))
	}
	share("message text", textBytes)
	share("tool input", inputBytes)
	share("tool output", outputBytes)
	share("keys, escapes, the rest", pageBytes-textBytes-inputBytes-outputBytes)

	fmt.Printf("\nopening a chat, bytes on the wire (compressed and sealed):\n")
	fmt.Printf("  %-42s %10s %10s %9s %9s %9s\n", "shape", "raw", "wire", "vs today", "p50", "p90")
	for _, s := range []*series{today, object, deferred, labelsOnly, floor} {
		fmt.Printf("  %-42s %10s %10s %8.0f%% %9s %9s\n", s.name, kb(s.raw), kb(s.wire),
			100*float64(s.wire)/float64(today.wire), kb(int64(s.pct(.5))), kb(int64(s.pct(.9))))
	}
	fmt.Printf("  host json.Marshal per page: string %v, object %v\n",
		(marshalString / time.Duration(used)).Round(time.Microsecond), (marshalObject / time.Duration(used)).Round(time.Microsecond))

	fmt.Printf("\nstreaming %d turns (%s of prose) at %d chars per 150 ms push:\n", turns, kb(streamedChars), *perTick)
	for _, s := range []*series{snapshot, delta, deltaFloor} {
		fmt.Printf("  %-20s %6d pushes %11s raw %11s wire  p90 push %9s\n", s.name, len(s.sizes), kb(s.raw), kb(s.wire), kb(int64(s.pct(.9))))
	}
	fmt.Printf("  deltas send %.1f%% of today's bytes\n", 100*float64(delta.wire)/float64(snapshot.wire))

	if *outDir != "" {
		for name, pages := range map[string][]any{"pages-string.json": stringPages, "pages-object.json": objectPages, "pages-labels.json": labelPages, "turns.json": turnTexts} {
			b, _ := json.Marshal(pages)
			if err := os.WriteFile(filepath.Join(*outDir, name), b, 0o600); err != nil {
				panic(err)
			}
		}
		fmt.Printf("\nwrote %d pages to %s\n", len(stringPages), *outDir)
	}
}
