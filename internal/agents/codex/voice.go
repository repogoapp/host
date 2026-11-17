package codex

import (
	"strings"

	"github.com/repogoapp/host/internal/agent"
)

// A Codex voice session writes what each side said as `realtime_item` lines,
// outside any turn; the turns it starts carry only a `<realtime_delegation>`
// handoff, which is Codex's and hidden. So the segments are the conversation.

// voiceTurn tags a segment's TurnID with its voice session until
// joinVoiceSegments has merged and cleared it, since ParseLine is stateless.
const voiceTurn = "\x00voice:"

// parseRealtimeItem is one spoken segment: the user's words or the voice
// model's reply. Session starts and ends are bookkeeping.
func parseRealtimeItem(l codexLine) []agent.Event {
	if l.Payload.Type != "transcript_segment" {
		return nil
	}
	text := strings.TrimSpace(l.Payload.Text)
	if text == "" {
		return nil
	}
	turn := voiceTurn + l.Payload.RealtimeSessionID
	switch l.Payload.Role {
	case "user":
		return []agent.Event{{Kind: agent.EventUserMessage, Text: text, TurnID: turn}}
	case "assistant":
		return []agent.Event{{Kind: agent.EventText, Text: text, TurnID: turn}}
	}
	return nil
}

// joinVoiceSegments merges back-to-back segments from the same speaker in the
// same voice session into one message, then clears the tag so each takes the
// turn open around it, or none.
func joinVoiceSegments(events []agent.Event) []agent.Event {
	out := events[:0]
	for _, e := range events {
		if !strings.HasPrefix(e.TurnID, voiceTurn) {
			out = append(out, e)
			continue
		}
		if n := len(out) - 1; n >= 0 && out[n].TurnID == e.TurnID && out[n].Kind == e.Kind {
			out[n].Text += " " + e.Text
			continue
		}
		out = append(out, e)
	}
	for i := range out {
		if strings.HasPrefix(out[i].TurnID, voiceTurn) {
			out[i].TurnID = ""
		}
	}
	return out
}
