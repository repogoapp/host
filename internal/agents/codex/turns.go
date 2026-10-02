package codex

import "github.com/repogo/host/internal/agent"

// codexTurns names each item by the turn open around it; where turns overlap,
// a prompt joins the newest and a tool result its call's.
func codexTurns(events []agent.Event) {
	type span struct {
		id         string
		start, end int
	}
	var spans []*span
	byID := map[string]*span{}
	for i, e := range events {
		if e.TurnID == "" {
			continue
		}
		switch e.Kind {
		case agent.EventTurnStarted:
			if byID[e.TurnID] == nil {
				s := &span{id: e.TurnID, start: i, end: -1}
				byID[e.TurnID] = s
				spans = append(spans, s)
			}
		case agent.EventTurnFinished, agent.EventTurnFailed:
			if s := byID[e.TurnID]; s != nil {
				s.end = i
			}
		}
	}
	for n, s := range spans {
		if s.end < 0 {
			s.end = len(events)
			if n+1 < len(spans) {
				s.end = spans[n+1].start
			}
		}
	}

	callTurn := map[string]string{}
	var open []*span
	next := 0
	for i := range events {
		e := &events[i]
		// Spans open strictly after their start row and close at their end row.
		kept := open[:0]
		for _, s := range open {
			if s.end > i {
				kept = append(kept, s)
			}
		}
		open = kept
		for next < len(spans) && spans[next].start < i {
			if spans[next].end > i {
				open = append(open, spans[next])
			}
			next++
		}

		if e.TurnID == "" {
			switch {
			case len(open) == 1:
				e.TurnID = open[0].id
			case len(open) > 1 && e.Kind == agent.EventUserMessage:
				e.TurnID = open[len(open)-1].id
			case len(open) > 1 && e.Kind == agent.EventToolResult && e.Tool != nil:
				e.TurnID = callTurn[e.Tool.CallID]
			}
		}
		if e.Kind == agent.EventToolCall && e.Tool != nil && e.Tool.CallID != "" && e.TurnID != "" {
			callTurn[e.Tool.CallID] = e.TurnID
		}
	}
}

// Normalize names the turn each row belongs to; a row the file does not
// settle keeps an empty TurnID rather than a guess.
func (c *Sessions) Normalize(events []agent.Event) []agent.Event {
	codexTurns(events)
	return events
}
