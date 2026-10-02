package flatimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/repogo/host/bench/flatimport/fb"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

func encode(events []agent.Event) []byte {
	objects := make([]*fb.EventT, len(events))
	for i := range events {
		objects[i] = packEvent(&events[i])
	}
	builder := flatbuffers.NewBuilder(1024)
	root := (&fb.EventsT{Events: objects}).Pack(builder)
	builder.Finish(root)
	return builder.FinishedBytes()
}

func decode(data []byte) []agent.Event {
	root := fb.GetRootAsEvents(data, 0)
	events := make([]agent.Event, root.EventsLength())
	var item fb.Event
	for i := range events {
		root.Events(&item, i)
		events[i] = *unpackEvent(item.UnPack())
	}
	return events
}

func check(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fixture(t testing.TB, kind string, count, turns int) (*session.Store, []session.Meta) {
	t.Helper()
	home := t.TempDir()
	var provider session.Provider
	if kind == "claude" {
		provider = claude.NewSessions(home)
	} else {
		provider = codex.NewSessions(home)
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("thread-%04d", i)
		var buf bytes.Buffer
		write := func(v any) { data, err := json.Marshal(v); check(t, err); buf.Write(data); buf.WriteByte('\n') }
		cwd := fmt.Sprintf("/synthetic/project-%d", i%8)
		stamp := "2026-09-29T10:00:00Z"
		if kind == "codex" {
			write(map[string]any{"type": "session_meta", "timestamp": stamp, "payload": map[string]any{"session_id": id, "cwd": cwd}})
		}
		for j := 0; j < turns; j++ {
			prompt := fmt.Sprintf("Inspect module %d for thread %d and explain its behavior.", j, i)
			text := strings.Repeat(fmt.Sprintf("Module %d uses structured records; check paths, Unicode café 日本語, and errors.\n", j), 12)
			call := fmt.Sprintf("call-%d", j)
			if kind == "claude" {
				write(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "timestamp": stamp, "promptId": fmt.Sprint(j), "message": map[string]any{"role": "user", "content": prompt}})
				write(map[string]any{"type": "assistant", "timestamp": stamp, "message": map[string]any{"role": "assistant", "id": call, "content": []any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "tool_use", "id": call, "name": "Read", "input": map[string]any{"file_path": fmt.Sprintf("/synthetic/file-%d.go", j)}}}}})
				write(map[string]any{"type": "user", "timestamp": stamp, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": call, "content": strings.Repeat("synthetic source line\n", 30)}}}})
			} else {
				for _, payload := range []any{
					map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}},
					map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}},
					map[string]any{"type": "function_call", "call_id": call, "name": "exec_command", "arguments": fmt.Sprintf(`{"cmd":"cat file-%d.go"}`, j)},
					map[string]any{"type": "function_call_output", "call_id": call, "output": strings.Repeat("synthetic source line\n", 30)},
				} {
					write(map[string]any{"type": "response_item", "timestamp": stamp, "payload": payload})
				}
			}
		}
		dir := filepath.Join(home, "projects", "synthetic")
		if kind == "codex" {
			dir = filepath.Join(home, "sessions", "2026", "09", "29")
		}
		check(t, os.MkdirAll(dir, 0700))
		check(t, os.WriteFile(filepath.Join(dir, id+".jsonl"), buf.Bytes(), 0600))
	}
	sessions := session.NewStore(provider)
	metas, errs := sessions.List()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(metas) != count {
		t.Fatalf("discovered %d, want %d", len(metas), count)
	}
	return sessions, metas
}

func TestRoundTrip(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			sessions, metas := fixture(t, kind, 2, 3)
			for _, m := range metas {
				events, err := sessions.Events(m)
				check(t, err)
				if len(events) < 12 {
					t.Fatalf("only %d events", len(events))
				}
				if !reflect.DeepEqual(events, decode(encode(events))) {
					t.Fatal("FlatBuffers changed parsed events")
				}
			}
		})
	}
	full := []agent.Event{{Kind: agent.EventText, Seq: 99, At: 42, TurnID: "turn", Text: "text", SessionID: "session", Error: "error", Tool: &agent.ToolCall{CallID: "call", Name: "tool", Input: json.RawMessage(`{"a":1}`), Output: "output", IsError: true}, Usage: &agent.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: 0.5, DurationMS: 6, ContextUsed: 7, ContextSize: 8}, Approval: &agent.Approval{CallID: "call", Title: "title", Kind: "question", Input: json.RawMessage(`{}`), Options: []agent.ApprovalOption{{OptionID: "id", Name: "name", Kind: "allow"}}, Questions: []agent.Question{{ID: "id", CustomID: "custom", Header: "header", Text: "question", MultiSelect: true, Options: []agent.QuestionOption{{Label: "yes", Description: "desc"}}}}}}}
	if !reflect.DeepEqual(full, decode(encode(full))) {
		t.Fatal("full event round trip differs")
	}
}

func BenchmarkImport(b *testing.B) {
	for _, kind := range []string{"claude", "codex"} {
		b.Run(kind, func(b *testing.B) {
			const count, turns = 64, 40
			sessions, metas := fixture(b, kind, count, turns)
			cache := b.TempDir()
			totalEvents := 0
			var sourceBytes, cacheBytes int64
			for i, m := range metas {
				events, err := sessions.Events(m)
				check(b, err)
				totalEvents += len(events)
				sourceBytes += m.SizeBytes
				data := encode(events)
				cacheBytes += int64(len(data))
				check(b, os.WriteFile(filepath.Join(cache, fmt.Sprint(i)), data, 0600))
				normalized, err := json.Marshal(events)
				check(b, err)
				check(b, os.WriteFile(filepath.Join(cache, fmt.Sprint(i)+".json"), normalized, 0600))
				if !reflect.DeepEqual(events, decode(data)) {
					b.Fatal("cache parity failed")
				}
			}
			for _, mode := range []string{"jsonl_initial", "jsonl_initial_build_flatcache", "flatcache_rebuild", "normalized_jsoncache_rebuild", "sqlite_write_only"} {
				b.Run(mode, func(b *testing.B) {
					b.StopTimer()
					root := b.TempDir()
					entries := make([]store.Entry, len(metas))
					for i, m := range metas {
						events, err := sessions.Events(m)
						check(b, err)
						entries[i] = store.Entry{Meta: m, Events: events}
					}
					var readTime, writeTime time.Duration
					b.ReportAllocs()
					for n := 0; n < b.N; n++ {
						dir := filepath.Join(root, fmt.Sprintf("db-%d", n))
						check(b, os.Mkdir(dir, 0700))
						db, err := store.Open(dir)
						check(b, err)
						cacheOut := filepath.Join(root, fmt.Sprintf("cache-%d", n))
						check(b, os.Mkdir(cacheOut, 0700))
						b.StartTimer()
						start := time.Now()
						batch := make([]store.Entry, len(metas))
						for i, m := range metas {
							var events []agent.Event
							if mode == "flatcache_rebuild" {
								data, err := os.ReadFile(filepath.Join(cache, fmt.Sprint(i)))
								check(b, err)
								events = decode(data)
							} else if mode == "normalized_jsoncache_rebuild" {
								data, err := os.ReadFile(filepath.Join(cache, fmt.Sprint(i)+".json"))
								check(b, err)
								check(b, sonic.Unmarshal(data, &events))
							} else if mode == "sqlite_write_only" {
								events = entries[i].Events
							} else {
								var err error
								events, err = sessions.Events(m)
								check(b, err)
								if mode == "jsonl_initial_build_flatcache" {
									check(b, os.WriteFile(filepath.Join(cacheOut, fmt.Sprint(i)), encode(events), 0600))
								}
							}
							batch[i] = store.Entry{Meta: m, Events: events}
						}
						readTime += time.Since(start)
						start = time.Now()
						check(b, db.SyncBatch(batch))
						writeTime += time.Since(start)
						b.StopTimer()
						page, err := db.Chats(store.ChatQuery{Limit: 100})
						check(b, err)
						if len(page.Chats) != count {
							b.Fatalf("imported %d threads", len(page.Chats))
						}
						sum := 0
						for _, chat := range page.Chats {
							sum += chat.EventCount
						}
						if sum != totalEvents {
							b.Fatalf("stored %d events, want %d", sum, totalEvents)
						}
						check(b, db.Close())
						check(b, os.RemoveAll(cacheOut))
						for _, suffix := range []string{"", "-wal", "-shm"} {
							err := os.Remove(path + suffix)
							if err != nil && !os.IsNotExist(err) {
								b.Fatal(err)
							}
						}
					}
					b.ReportMetric(float64(readTime.Nanoseconds())/float64(b.N)/1e6, "read-ms/op")
					b.ReportMetric(float64(writeTime.Nanoseconds())/float64(b.N)/1e6, "sqlite-ms/op")
					b.ReportMetric(float64(totalEvents), "events/op")
					b.ReportMetric(float64(sourceBytes), "jsonl-bytes")
					b.ReportMetric(float64(cacheBytes), "flat-bytes")
				})
			}
		})
	}
}
