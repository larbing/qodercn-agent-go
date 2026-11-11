package qoder

import "fmt"

// MCPServerConfig is implemented by the four MCP server config kinds:
// *StdioServerConfig, *SSEServerConfig, *HTTPServerConfig and
// *SDKServerConfig (from NewSDKMCPServer).
type MCPServerConfig interface {
	mcpType() string
	// wireConfig renders the --mcp-config entry. Tool policies are stripped
	// here (matching mcp_serialize.py) and merged into --settings instead.
	wireConfig() map[string]any
	// toolPolicies returns per-tool permission policies, if any.
	toolPolicies() []McpToolPolicy
}

// MCP tool permission policies.
const (
	ToolPolicyAlwaysAllow = "always_allow"
	ToolPolicyAlwaysAsk   = "always_ask"
	ToolPolicyAlwaysDeny  = "always_deny"
)

// McpToolPolicy pins the permission behavior of one MCP tool.
type McpToolPolicy struct {
	Name             string
	PermissionPolicy string // always_allow | always_ask | always_deny
}

// StdioServerConfig launches an MCP server as a subprocess.
type StdioServerConfig struct {
	Command string
	Args    []string
	Env     map[string]string
	Tools   []McpToolPolicy
}

func (*StdioServerConfig) mcpType() string { return "stdio" }

func (c *StdioServerConfig) wireConfig() map[string]any {
	wire := map[string]any{"type": "stdio", "command": c.Command}
	if c.Args != nil {
		wire["args"] = c.Args
	}
	if c.Env != nil {
		wire["env"] = c.Env
	}
	return wire
}

func (c *StdioServerConfig) toolPolicies() []McpToolPolicy { return c.Tools }

// SSEServerConfig connects to an MCP server over Server-Sent Events.
type SSEServerConfig struct {
	URL     string
	Headers map[string]string
	Tools   []McpToolPolicy
}

func (*SSEServerConfig) mcpType() string { return "sse" }

func (c *SSEServerConfig) wireConfig() map[string]any {
	wire := map[string]any{"type": "sse", "url": c.URL}
	if c.Headers != nil {
		wire["headers"] = c.Headers
	}
	return wire
}

func (c *SSEServerConfig) toolPolicies() []McpToolPolicy { return c.Tools }

// HTTPServerConfig connects to a streamable-HTTP MCP server.
type HTTPServerConfig struct {
	URL     string
	Headers map[string]string
	Tools   []McpToolPolicy
}

func (*HTTPServerConfig) mcpType() string { return "http" }

func (c *HTTPServerConfig) wireConfig() map[string]any {
	wire := map[string]any{"type": "http", "url": c.URL}
	if c.Headers != nil {
		wire["headers"] = c.Headers
	}
	return wire
}

func (c *HTTPServerConfig) toolPolicies() []McpToolPolicy { return c.Tools }

// SDKServerConfig references an in-process SDK MCP server created by
// NewSDKMCPServer. Only type and name cross the wire; tool calls are routed
// back into this process over the control protocol.
type SDKServerConfig struct {
	Name  string
	Tools []McpToolPolicy

	server *SDKMCPServer
}

func (*SDKServerConfig) mcpType() string { return "sdk" }

func (c *SDKServerConfig) wireConfig() map[string]any {
	// Strip the in-process instance and tool list, mirroring
	// mcp_serialize.py: the CLI only needs type+name for routing.
	return map[string]any{"type": "sdk", "name": c.Name}
}

func (c *SDKServerConfig) toolPolicies() []McpToolPolicy { return c.Tools }

// mcpPolicySettings converts every configured tool policy into settings
// permission rules named mcp__{server}__{tool}, mirroring the TypeScript
// SDK behavior (always_allow -> permissions.allow, etc.).
func (o *Options) mcpPolicySettings() map[string]any {
	allow, ask, deny := []string{}, []string{}, []string{}
	for serverName, cfg := range o.MCPServers {
		for _, p := range cfg.toolPolicies() {
			rule := fmt.Sprintf("mcp__%s__%s", serverName, p.Name)
			switch p.PermissionPolicy {
			case ToolPolicyAlwaysAllow:
				allow = append(allow, rule)
			case ToolPolicyAlwaysAsk:
				ask = append(ask, rule)
			case ToolPolicyAlwaysDeny:
				deny = append(deny, rule)
			}
		}
	}
	if len(allow) == 0 && len(ask) == 0 && len(deny) == 0 {
		return nil
	}
	permissions := map[string]any{}
	if len(allow) > 0 {
		permissions["allow"] = allow
	}
	if len(ask) > 0 {
		permissions["ask"] = ask
	}
	if len(deny) > 0 {
		permissions["deny"] = deny
	}
	return map[string]any{"permissions": permissions}
}

// mergeSettings overlays the user settings on top of the policy settings
// (user values win), merging the "permissions" object list-wise.
func mergeSettings(base, user Settings) Settings {
	if len(base) == 0 {
		return user
	}
	if len(user) == 0 {
		return base
	}
	out := Settings{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range user {
		if k != "permissions" {
			out[k] = v
			continue
		}
		if basePerms, ok := base["permissions"].(map[string]any); ok {
			userPerms, _ := v.(map[string]any)
			merged := map[string]any{}
			for pk, pv := range basePerms {
				merged[pk] = pv
			}
			for pk, pv := range userPerms {
				merged[pk] = pv
			}
			out["permissions"] = merged
			continue
		}
		out[k] = v
	}
	return out
}
