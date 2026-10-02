package claude

import "github.com/repogo/host/internal/agent"

// claudeTurns gives each unnamed row the turn of the prompt before it: the CLI
// stamps a prompt and its tool results with the prompt's id, not the replies.
func claudeTurns(events []agent.Event) {
	current := ""
	for i := range events {
		if events[i].TurnID != "" {
			current = events[i].TurnID
			continue
		}
		events[i].TurnID = current
	}
}

// Normalize folds late results onto their calls, then names each row's turn;
// a row the file does not settle keeps an empty TurnID rather than a guess.
func (c *Sessions) Normalize(events []agent.Event) []agent.Event {
	events = foldLateResults(events)
	claudeTurns(events)
	return events
}
