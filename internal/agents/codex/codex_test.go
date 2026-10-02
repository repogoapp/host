package codex

import (
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

func TestCodexDurableMessages(t *testing.T) {
	tests := []struct {
		name, line string
		kind       agent.EventKind
		text       string
	}{
		{"user", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix this"}]}}`, agent.EventUserMessage, "Fix this"},
		{"assistant", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The fix"},{"type":"output_text","text":" is ready."}]}}`, agent.EventText, "The fix is ready."},
		{"inbox directive", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Score: 79/100.\n\n::inbox-item{title=\"Gate ready\" summary=\"Pre-sell two deposits\"}"}]}}`, agent.EventText, "Score: 79/100."},
		{"inline colons kept", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Call std::vec{} here\n::Not{a directive}"}]}}`, agent.EventText, "Call std::vec{} here\n::Not{a directive}"},
		{"mixed media", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"test"},{"type":"input_text","text":"Describe this"}]}}`, agent.EventUserMessage, "Describe this"},
	}
	p := &Sessions{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := session.Parse(p, []byte(tt.line))
			if len(got) != 1 || got[0].Kind != tt.kind || got[0].Text != tt.text {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestCodexIgnoresInstructionsAndMirroredProse(t *testing.T) {
	p := &Sessions{}
	for _, line := range []string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Fix this"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"The fix is ready."}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"Private instructions"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"system","content":[{"type":"input_text","text":"System instructions"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"::inbox-item{title=\"Done\"}"}]}}`,
	} {
		if got := session.Parse(p, []byte(line)); len(got) != 0 {
			t.Fatalf("unexpected events: %+v", got)
		}
	}
}
