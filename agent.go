package qoder

// AgentDefinition registers a custom subagent via Options.Agents. JSON tags
// follow the protocol-style camelCase the CLI expects. To delegate to
// subagents the session must expose the built-in "Agent" tool (e.g. via
// Options.AllowedTools).
type AgentDefinition struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools,omitempty"`
	// DisallowedTools uses camelCase to match the wire protocol.
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	Model           string   `json:"model,omitempty"`
	Skills          []string `json:"skills,omitempty"`
	// MCPServers lists server names or inline MCP config objects.
	MCPServers     []any          `json:"mcpServers,omitempty"`
	InitialPrompt  string         `json:"initialPrompt,omitempty"`
	MaxTurns       int            `json:"maxTurns,omitempty"`
	Effort         string         `json:"effort,omitempty"` // low | medium | high | max
	PermissionMode PermissionMode `json:"permissionMode,omitempty"`
}

// buildAgentsWire renders Options.Agents for the initialize control
// request: {name: definition, ...}.
func buildAgentsWire(agents map[string]AgentDefinition) map[string]any {
	if len(agents) == 0 {
		return nil
	}
	wire := make(map[string]any, len(agents))
	for name, def := range agents {
		wire[name] = def
	}
	return wire
}
