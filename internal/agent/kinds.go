package agent

const (
	KindClaude Kind = "claude"
	KindCodex  Kind = "codex"

	// KindProject is a project's own agents (.repogo/agents), run over the Agent
	// Client Protocol; TurnRequest.ProjectAgent names which one a new chat starts.
	KindProject Kind = "project"
)
