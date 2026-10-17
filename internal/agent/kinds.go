package agent

const (
	KindClaude Kind = "claude"
	KindCodex  Kind = "codex"

	// KindACP runs a project's own agents, any program speaking the Agent Client
	// Protocol; TurnRequest.ProjectAgent names which one a new chat starts.
	KindACP Kind = "acp"
)
