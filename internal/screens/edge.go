package screens

import (
	"context"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/screens/desktop"
)

// After a scroll, the Mac reads the scroll under its cursor once the app has
// moved: soon after the first of a run of scrolls, again each time one comes
// later, and once more when the run has settled, for an app whose scrolling
// eases out after the last.
const (
	edgeSoon    = 100 * time.Millisecond
	edgeSettled = 300 * time.Millisecond
)

// edgeWatch tells the phone when its scrolls bring a Mac's scroll to its
// top or bottom, or away from one: only when that changes.
type edgeWatch struct {
	ctx   context.Context
	reply func(any) error
	// read is the scroll's edge after scrolls heading up (positive) or down
	// since the last read, forgetting first what it kept when another input
	// came; empty when it can't tell.
	read func(heading int, forget bool) ScrollEdge

	mu sync.Mutex
	// last is what the phone was told; empty when it was never told, or
	// another input made both sides forget.
	last    ScrollEdge
	heading int
	forget  bool
	soon    *time.Timer
	settled *time.Timer
	// reading holds one read at a time: the two timers can fire together.
	reading sync.Mutex
	// checks counts the reads done, for a test to wait on.
	checks int
}

// saw is each input the phone sends, in order.
func (w *edgeWatch) saw(in StreamInput) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case in.Scroll != nil && in.Scroll.DY != 0:
		w.heading = in.Scroll.DY
		if w.soon == nil {
			w.soon = time.AfterFunc(edgeSoon, func() {
				w.mu.Lock()
				w.soon = nil
				w.mu.Unlock()
				w.check()
			})
		}
		if w.settled == nil {
			w.settled = time.AfterFunc(edgeSettled, w.check)
		} else {
			w.settled.Reset(edgeSettled)
		}
	case in.Pointer != nil, in.Touch != nil, in.Key != nil, in.Text != nil, in.Button != nil:
		w.last = ""
		w.forget = true
	}
}

// check reads the scroll's edge and tells the phone if it changed.
func (w *edgeWatch) check() {
	if w.ctx.Err() != nil {
		return
	}
	w.reading.Lock()
	defer w.reading.Unlock()
	defer func() {
		w.mu.Lock()
		w.checks++
		w.mu.Unlock()
	}()
	w.mu.Lock()
	heading, forget := w.heading, w.forget
	w.heading, w.forget = 0, false
	w.mu.Unlock()
	edge := w.read(heading, forget)
	if edge == "" {
		return
	}
	w.mu.Lock()
	changed := edge != w.last
	w.last = edge
	w.mu.Unlock()
	if changed {
		w.reply(StreamMessage{Type: "edge", Edge: &StreamEdge{Vertical: edge}})
	}
}

// macScrollEdge reads a Mac's scroll for one stream.
func macScrollEdge() func(heading int, forget bool) ScrollEdge {
	var reader desktop.ScrollReader
	return func(heading int, forget bool) ScrollEdge {
		if forget {
			reader.Forget()
		}
		return scrollEdgeOf(reader.Edge(heading))
	}
}

func scrollEdgeOf(edge desktop.Edge) ScrollEdge {
	switch edge {
	case desktop.EdgeNone:
		return ScrollEdgeNone
	case desktop.EdgeTop:
		return ScrollEdgeTop
	case desktop.EdgeBottom:
		return ScrollEdgeBottom
	}
	return ""
}
