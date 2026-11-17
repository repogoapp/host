package codex

import (
	"testing"

	"github.com/repogoapp/host/internal/agent"
)

func TestVoiceSegmentsReadAsMessages(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-11-17T04:13:50.000Z","type":"realtime_item","payload":{"realtime_session_id":"a","type":"realtime_session_started"}}`,
		`{"timestamp":"2026-11-17T04:13:55.000Z","type":"realtime_item","payload":{"realtime_session_id":"a","type":"transcript_segment","role":"user","text":" Hey, can you hear me"}}`,
		`{"timestamp":"2026-11-17T04:13:57.000Z","type":"realtime_item","payload":{"realtime_session_id":"a","type":"transcript_segment","role":"assistant","text":" Yeah, I'm here."}}`,
		`{"timestamp":"2026-11-17T04:13:59.000Z","type":"realtime_item","payload":{"realtime_session_id":"a","type":"transcript_segment","role":"assistant","text":" What's on your mind?"}}`,
		`{"timestamp":"2026-11-17T04:14:00.000Z","type":"realtime_item","payload":{"realtime_session_id":"a","type":"realtime_session_closed"}}`,
		`{"timestamp":"2026-11-17T04:14:01.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-11-17T04:14:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<realtime_delegation>\n  <source>transcript_tail_flush</source>\n</realtime_delegation>"}]}}`,
		`{"timestamp":"2026-11-17T04:14:01.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
		`{"timestamp":"2026-11-17T04:14:10.000Z","type":"realtime_item","payload":{"realtime_session_id":"b","type":"transcript_segment","role":"assistant","text":"Back again."}}`,
	}
	c := &Sessions{}
	var events []agent.Event
	for _, line := range lines {
		got, _ := c.ParseLine([]byte(line))
		events = append(events, got...)
	}
	events = c.Normalize(events)

	type row struct {
		kind       agent.EventKind
		text, turn string
	}
	want := []row{
		{agent.EventUserMessage, "Hey, can you hear me", ""},
		{agent.EventText, "Yeah, I'm here. What's on your mind?", ""},
		{agent.EventTurnStarted, "", "t1"},
		{agent.EventTurnFinished, "", "t1"},
		{agent.EventText, "Back again.", ""},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		e := events[i]
		if e.Kind != w.kind || e.Text != w.text || e.TurnID != w.turn {
			t.Errorf("event %d = {%s %q %q}, want {%s %q %q}", i, e.Kind, e.Text, e.TurnID, w.kind, w.text, w.turn)
		}
	}
}
