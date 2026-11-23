package agent

const (
	KindClaude Kind = "claude"
	KindCodex  Kind = "codex"
	// KindAntigravity is Google's Antigravity CLI (agy), run over ACP.
	KindAntigravity Kind = "antigravity"

	// KindProject is the environment's agents (~/.repogo/plugins/<name>/agent.json),
	// run in a project over the Agent Client Protocol;
	// TurnRequest.ProjectAgent names which one a new chat starts.
	KindProject Kind = "project"
)
