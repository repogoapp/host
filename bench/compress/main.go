// Scratch: how much would per-message deflate shrink what the host sends
// through the relay? Builds real chats.messages pages from local sessions and
// prints sizes only.
package main

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

type stat struct {
	n              int
	raw, fast, def int64
	fastDur        time.Duration
	inflateDur     time.Duration
	sizes          []int
	fastSizes      []int
}

func (s *stat) add(msg []byte) {
	s.n++
	s.raw += int64(len(msg))
	s.sizes = append(s.sizes, len(msg))
	t := time.Now()
	f := deflate(msg, flate.BestSpeed)
	s.fastDur += time.Since(t)
	s.fast += int64(f)
	s.fastSizes = append(s.fastSizes, f)
	s.def += int64(deflate(msg, flate.DefaultCompression))
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	w.Write(msg)
	w.Close()
	t = time.Now()
	io.Copy(io.Discard, flate.NewReader(bytes.NewReader(buf.Bytes())))
	s.inflateDur += time.Since(t)
}

func deflate(b []byte, level int) int {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, level)
	w.Write(b)
	w.Close()
	return buf.Len()
}

func pct(xs []int, p float64) int {
	if len(xs) == 0 {
		return 0
	}
	c := append([]int(nil), xs...)
	sort.Ints(c)
	return c[int(float64(len(c)-1)*p)]
}

func kb(n int64) string { return fmt.Sprintf("%.1f KB", float64(n)/1024) }

func rpcResult(v any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v})
	return b
}

func page(meta session.Meta, msgs []store.Message, lo, hi int) store.Page {
	return store.Page{ChatID: store.ChatID(meta.ID), Events: msgs[lo:hi], HostID: "h", Cwd: meta.Cwd,
		Agent: string(meta.Agent), EventCount: len(msgs), NextIdx: hi, FirstIdx: lo, HasBefore: lo > 0}
}

func main() {
	registry, err := agents.New(agent.Dependencies{})
	if err != nil {
		panic(err)
	}
	defer registry.Close()
	st := session.NewStore(registry.Sessions...)
	metas, _ := st.List()
	open, older, live := &stat{}, &stat{}, &stat{}
	buckets := map[string]*stat{}
	bucket := func(n int) string {
		switch {
		case n < 1024:
			return "a <1 KB"
		case n < 16<<10:
			return "b 1-16 KB"
		case n < 128<<10:
			return "c 16-128 KB"
		default:
			return "d >128 KB"
		}
	}
	used := 0
	if len(metas) > 400 {
		metas = metas[:400]
	}
	for _, m := range metas {
		_, events, err := st.Read(m.ID)
		if err != nil || len(events) == 0 {
			continue
		}
		used++
		msgs := store.MessagesOf(events)
		// Opening a chat: tail page of 40.
		lo := max(0, len(msgs)-40)
		open.add(rpcResult(page(m, msgs, lo, len(msgs))))
		// Scrolling back: pages of 120.
		for hi := lo; hi > 0; hi -= 120 {
			older.add(rpcResult(page(m, msgs, max(0, hi-120), hi)))
		}
		// Live: each event pushed on its own as chats.appended.
		for i := range msgs {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "chats.appended", "params": page(m, msgs, i, i+1)})
			live.add(b)
			k := bucket(len(b))
			if buckets[k] == nil {
				buckets[k] = &stat{}
			}
			buckets[k].add(b)
		}
	}
	fmt.Printf("sessions read: %d of %d\n\n", used, len(metas))
	fmt.Printf("%-28s %8s %11s %11s %7s %7s %11s %11s %10s\n", "traffic", "msgs", "raw", "deflate(1)", "ratio", "ratio6", "p50 raw", "p90 raw", "cpu/msg")
	row := func(name string, s *stat) {
		if s.n == 0 {
			return
		}
		fmt.Printf("%-28s %8d %11s %11s %6.1fx %6.1fx %11s %11s %10s\n", name, s.n, kb(s.raw), kb(s.fast),
			float64(s.raw)/float64(s.fast), float64(s.raw)/float64(s.def),
			kb(int64(pct(s.sizes, .5))), kb(int64(pct(s.sizes, .9))), (s.fastDur / time.Duration(s.n)).Round(time.Microsecond))
	}
	for _, x := range []struct {
		n string
		s *stat
	}{{"open", open}, {"scroll", older}} {
		fmt.Printf("%s: avg %s raw, compress %v, decompress %v per page\n", x.n, kb(x.s.raw/int64(x.s.n)),
			(x.s.fastDur / time.Duration(x.s.n)).Round(time.Microsecond), (x.s.inflateDur / time.Duration(x.s.n)).Round(time.Microsecond))
	}
	row("open chat (tail 40)", open)
	row("scroll back (120/page)", older)
	row("live push, per event", live)
	fmt.Println()
	keys := []string{}
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		row("  live events "+k[2:], buckets[k])
	}
	fmt.Println()
	for _, q := range []float64{.5, .9, .99} {
		r, f := pct(open.sizes, q), pct(open.fastSizes, q)
		fmt.Printf("open chat p%.0f: %s -> %s   at 5 Mbps: %.0f ms -> %.0f ms\n", q*100, kb(int64(r)), kb(int64(f)),
			float64(r)*8/5e6*1000, float64(f)*8/5e6*1000)
	}
	_ = os.Stdout
}
