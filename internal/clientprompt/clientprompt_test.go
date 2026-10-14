package clientprompt_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/clientprompt"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/testhost"
)

var update = flag.Bool("update", false, "rewrite cards.md from the phone's CardInstructions")

const cardInstructions = "../../../swift/Packages/RepoGoCards/Sources/RepoGoCards/CardInstructions.swift"

func TestForPicksByMarkdownFeature(t *testing.T) {
	prompt := func(features map[string]int) string {
		return clientprompt.For(device.Client{Platform: "ios", Features: features})
	}
	full := prompt(map[string]int{"markdown": 1})
	if !strings.Contains(full, "```mermaid") || !strings.Contains(full, "```ui") {
		t.Fatalf("markdown 1 does not describe Mermaid and cards:\n%s", full)
	}
	if got := prompt(map[string]int{"markdown": 9}); got != full {
		t.Error("a newer markdown version does not get the newest prompt")
	}
	for _, features := range []map[string]int{nil, {"blocks": 3}, {"markdown": 0}} {
		if got := prompt(features); got != "" {
			t.Errorf("For(%v) = %q, want nothing", features, got)
		}
	}
}

// The card prompt is the phone's: the phone renders the cards, so its
// CardInstructions is the source and cards.md a copy.
func TestCardsMatchThePhone(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../swift")
	src, err := os.ReadFile(cardInstructions)
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(src), "#\"\"\"\n")
	body, _, closed := strings.Cut(body, "\n    \"\"\"#")
	if !ok || !closed {
		t.Fatalf("no #\"\"\" … \"\"\"# string in %s", cardInstructions)
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "    ")
		if strings.TrimSpace(lines[i]) == "" {
			lines[i] = ""
		}
	}
	testhost.Golden(t, "cards.md", []byte(strings.Join(lines, "\n")+"\n"), *update,
		"run: go test ./internal/clientprompt -update")
}
