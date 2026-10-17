package acp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/repogoapp/host/internal/stdiorpc"
)

// failureLimit caps the agent's own words in a failure the user reads.
const failureLimit = 300

// failure is the agent or its process failing a step of a turn. What the
// user reads names the agent, the step and the agent's own message; the
// transport's detail stays in err, for the host's log.
type failure struct {
	name string
	// step is what didn't happen, as "couldn't start".
	step string
	err  error
}

func (f failure) Error() string {
	var answered *stdiorpc.Error
	if errors.As(f.err, &answered) {
		if message := strings.TrimSpace(answered.Message); message != "" {
			if len(message) > failureLimit {
				message = message[:failureLimit] + "…"
			}
			return fmt.Sprintf("%s %s: %s", f.name, f.step, message)
		}
	}
	return fmt.Sprintf("%s %s.", f.name, f.step)
}

func (f failure) Unwrap() error { return f.err }

// detail is everything known about a failure, for the log: the transport's
// error and the last line the agent logged.
func detail(err error, c *stdiorpc.Client) string {
	text := err.Error()
	var f failure
	if errors.As(err, &f) {
		text = f.err.Error()
	}
	if c != nil {
		if l := lastLine(c.Stderr()); l != "" {
			text += " (" + l + ")"
		}
	}
	return text
}
