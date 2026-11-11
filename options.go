package qoder

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SDKVersion is reported to the CLI via the QODER_SDK_VERSION env var.
const SDKVersion = "1.0.0"

// PermissionMode controls how tool calls are approved.
type PermissionMode string

const (
	PermissionDefault           PermissionMode = "default"
	PermissionAcceptEdits       PermissionMode = "acceptEdits"
	PermissionPlan              PermissionMode = "plan"
	PermissionBypassPermissions PermissionMode = "bypassPermissions"
	PermissionYolo              PermissionMode = "yolo"
	PermissionDontAsk           PermissionMode = "dontAsk"
	PermissionAuto              PermissionMode = "auto"
)

// SettingSource selects which filesystem settings layers the CLI loads.
type SettingSource string

const (
	SettingSourceUser    SettingSource = "user"    // ~/.qoder-cn/settings.json
	SettingSourceProject SettingSource = "project" // .qoder-cn/settings.json
	SettingSourceLocal   SettingSource = "local"   // .qoder-cn/settings.local.json
)

// SessionStoreFlushMode controls when session data is written to an external
// SessionStore.
type SessionStoreFlushMode string

const (
	FlushBatched SessionStoreFlushMode = "batched"
	FlushEager   SessionStoreFlushMode = "eager"
)

// TransportMode selects the CLI transport. The zero value (TransportAuto)
// prefers the --sdk daemon protocol and falls back to the direct subprocess
// protocol when the daemon handshake fails; the explicit values lock one
// transport in.
type TransportMode string

const (
	TransportAuto   TransportMode = "auto"
	TransportDaemon TransportMode = "daemon"
	TransportDirect TransportMode = "direct"
)

// SkillsAll enables every discovered skill (Python: skills="all").
const SkillsAll = "__all__"

// Settings is an inline settings object; keys use camelCase and are passed
// to the CLI verbatim.
type Settings = map[string]any

// Options configures query() and Client. Zero values are valid; defaults are
// applied by Normalize. Field names mirror QoderAgentOptions.
type Options struct {
	// Auth selects the credential strategy (required).
	Auth Auth
	// CustomContext is the default custom context for inference requests.
	CustomContext map[string]string
	// OnAuthExpired fires at most once per session when auth failure is
	// observed.
	OnAuthExpired func()

	// Tool scoping.
	Tools           []string // empty slice + ToolsPresetDefault => --tools ""
	ToolsAllDefault bool     // tools="default" preset
	AllowedTools    []string
	DisallowedTools []string

	// Permissions.
	CanUseTool                      CanUseTool
	PermissionMode                  PermissionMode
	AllowDangerouslySkipPermissions bool
	PermissionPromptToolName        string

	// Model selection.
	Model                 string
	ResolveModel          ModelPolicyProvider
	ResolveModelTimeoutMS int // default 500

	// Runtime behavior.
	SystemPrompt       string // custom system prompt (overrides CLI default)
	SystemPromptFile   string // load system prompt from file
	AppendSystemPrompt string // preset mode: append to qodercli prompt
	Cwd                string
	Env                map[string]string
	Proxy              string
	CLIPath            string

	// Session management.
	SessionID            string
	Resume               string
	ResumeSessionAt      string
	ResumeDropsTurn      *bool
	ContinueConversation bool
	ForkSession          bool
	SessionStore         SessionStore
	SessionStoreFlush    SessionStoreFlushMode // default "batched"
	LoadTimeoutMS        int                   // default 60000

	// Streaming.
	MaxTurns               int
	IncludePartialMessages bool

	// MCP.
	MCPServers            map[string]MCPServerConfig
	AllowedMCPServerNames []string
	StrictMCPConfig       bool

	// Callbacks and agents.
	Hooks     map[HookEvent][]HookMatcher
	Agents    map[string]AgentDefinition
	Agent     string
	Skills    []string // nil = unset; empty non-nil = disable all; SkillsAll = all
	SkillsAll bool

	// Settings.
	Settings       Settings // inline settings object
	SettingsPath   string   // settings file path (mutually exclusive)
	SettingSources []SettingSource

	// Misc.
	AddDirs                 []string
	ExtraArgs               map[string]*string // nil value = boolean flag
	Plugins                 []SDKPluginConfig
	EnableFileCheckpointing *bool

	// Control protocol tuning.
	ControlRequestTimeoutMS int  // default 60000; 0 disables timeout
	SkipVersionCheck        bool // default false; run `cli -v` probe
	InitializeTimeoutSec    int  // default 60

	// TransportMode picks the CLI transport: "auto" (default; daemon first
	// with direct fallback), "daemon", or "direct".
	TransportMode TransportMode

	// authPayloadPath is created at transport start and cleaned up on close.
	authPayloadPath string
}

// SDKPluginConfig loads a local plugin directory.
type SDKPluginConfig struct {
	// Type is currently always "local".
	Type string // "local"
	Path string
}

// LocalPlugin builds a local plugin config.
func LocalPlugin(path string) SDKPluginConfig {
	return SDKPluginConfig{Type: "local", Path: path}
}

// Normalize fills defaults and validates mutually exclusive fields. It
// returns a copy; the receiver is not modified.
func (o *Options) Normalize() (*Options, error) {
	c := *o
	if c.ResolveModelTimeoutMS == 0 {
		c.ResolveModelTimeoutMS = 500
	}
	if c.LoadTimeoutMS == 0 {
		c.LoadTimeoutMS = 60000
	}
	if c.SessionStoreFlush == "" {
		c.SessionStoreFlush = FlushBatched
	}
	if c.ControlRequestTimeoutMS == 0 {
		c.ControlRequestTimeoutMS = 60000
	}
	if c.InitializeTimeoutSec == 0 {
		c.InitializeTimeoutSec = 60
	}
	switch c.TransportMode {
	case "":
		c.TransportMode = TransportAuto
	case TransportAuto, TransportDaemon, TransportDirect:
	default:
		return nil, newError(ErrKindAuthNotConfigured,
			"invalid TransportMode %q: use auto, daemon, or direct", string(c.TransportMode))
	}
	if c.CanUseTool != nil && c.PermissionPromptToolName != "" && c.PermissionPromptToolName != "stdio" {
		return nil, newError(ErrKindAuthNotConfigured,
			"CanUseTool cannot be combined with PermissionPromptToolName; use one or the other")
	}
	if (c.PermissionMode == PermissionBypassPermissions || c.PermissionMode == PermissionYolo) &&
		!c.AllowDangerouslySkipPermissions && !c.skipDangerousFlag() {
		return nil, newError(ErrKindAuthNotConfigured,
			"permission mode %q requires AllowDangerouslySkipPermissions=true", c.PermissionMode)
	}
	return &c, nil
}

// skipDangerousFlag mirrors Python: --yolo mode does not require the
// explicit opt-in flag.
func (o *Options) skipDangerousFlag() bool { return o.PermissionMode == PermissionYolo }

// validateDaemonOptions rejects options the daemon session protocol does
// not support, mirroring validate_daemon_session_options.
func (o *Options) validateDaemonOptions() error {
	switch o.PermissionMode {
	case "", PermissionDefault, PermissionAcceptEdits, PermissionAuto,
		PermissionBypassPermissions, PermissionYolo:
	default:
		return newError(ErrKindUnsupportedCapability,
			"permission mode %q is not supported by the daemon transport; use default, acceptEdits, auto, bypassPermissions, or yolo",
			string(o.PermissionMode))
	}
	if o.ResumeSessionAt != "" || o.ResumeDropsTurn != nil {
		return newError(ErrKindUnsupportedCapability,
			"daemon transport does not support resume truncation (ResumeSessionAt/ResumeDropsTurn)")
	}
	if o.ContinueConversation && o.SessionStore == nil {
		return newError(ErrKindUnsupportedCapability,
			"daemon transport does not support ContinueConversation without a SessionStore; resume a session id instead")
	}
	return nil
}

// usesStdioPermissionPrompt reports whether the SDK serves permission
// prompts itself over the control protocol.
func (o *Options) usesStdioPermissionPrompt() bool {
	return o.CanUseTool != nil
}

// buildCLIArgs maps Options onto qoderclicn flags, matching the Python SDK
// argument builder exactly.
func (o *Options) buildCLIArgs() ([]string, error) {
	cmd := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
	}

	if o.SessionStore != nil {
		cmd = append(cmd, "--session-mirror")
	}

	switch {
	case o.SystemPromptFile != "":
		cmd = append(cmd, "--system-prompt-file", o.SystemPromptFile)
	case o.AppendSystemPrompt != "":
		cmd = append(cmd, "--append-system-prompt", o.AppendSystemPrompt)
	case o.SystemPrompt == "":
		// Match Python: no custom prompt => blank, so the CLI default prompt
		// is not injected in SDK mode.
		cmd = append(cmd, "--system-prompt", "")
	default:
		cmd = append(cmd, "--system-prompt", o.SystemPrompt)
	}

	if o.ToolsAllDefault {
		cmd = append(cmd, "--tools", "default")
	} else if o.Tools != nil {
		if len(o.Tools) == 0 {
			cmd = append(cmd, "--tools", "")
		} else {
			cmd = append(cmd, "--tools", strings.Join(o.Tools, ","))
		}
	}

	// allowed-tools = explicit allowlist + implicit Skill(...) entries from
	// the skills policy (matches the TypeScript SDK).
	allowed := append([]string(nil), o.AllowedTools...)
	seen := map[string]bool{}
	for _, t := range allowed {
		seen[t] = true
	}
	var skillTools []string
	switch {
	case o.SkillsAll:
		skillTools = []string{"Skill"}
	case o.Skills != nil:
		for _, name := range o.Skills {
			skillTools = append(skillTools, fmt.Sprintf("Skill(%s)", name))
		}
	}
	for _, t := range skillTools {
		if !seen[t] {
			allowed = append(allowed, t)
			seen[t] = true
		}
	}
	for _, t := range allowed {
		cmd = append(cmd, "--allowed-tools", t)
	}

	if o.MaxTurns > 0 {
		cmd = append(cmd, "--max-turns", fmt.Sprintf("%d", o.MaxTurns))
	}
	for _, t := range o.DisallowedTools {
		cmd = append(cmd, "--disallowed-tools", t)
	}
	if o.Model != "" {
		cmd = append(cmd, "--model", o.Model)
	}
	if o.Proxy != "" {
		cmd = append(cmd, "--proxy", o.Proxy)
	}

	if o.usesStdioPermissionPrompt() {
		cmd = append(cmd, "--permission-prompt-tool", "stdio")
	} else if o.PermissionPromptToolName != "" {
		cmd = append(cmd, "--permission-prompt-tool", o.PermissionPromptToolName)
	}

	switch {
	case o.PermissionMode == PermissionYolo:
		cmd = append(cmd, "--yolo")
	case o.PermissionMode != "":
		cmd = append(cmd, "--permission-mode", string(o.PermissionMode))
	}
	if o.AllowDangerouslySkipPermissions && o.PermissionMode != PermissionYolo {
		cmd = append(cmd, "--dangerously-skip-permissions")
	}

	if o.ContinueConversation {
		cmd = append(cmd, "--continue")
	}
	if o.Resume != "" {
		cmd = append(cmd, "--resume", o.Resume)
	}
	if o.ResumeSessionAt != "" {
		cmd = append(cmd, "--resume-session-at="+o.ResumeSessionAt)
	}
	if o.ResumeDropsTurn != nil {
		cmd = append(cmd, fmt.Sprintf("--resume-drops-turn=%t", *o.ResumeDropsTurn))
	}
	if o.SessionID != "" {
		cmd = append(cmd, "--session-id", o.SessionID)
	}

	settingsValue, err := o.buildSettingsValue()
	if err != nil {
		return nil, err
	}
	if settingsValue != "" {
		cmd = append(cmd, "--settings", settingsValue)
	}

	for _, dir := range o.AddDirs {
		cmd = append(cmd, "--add-dir", dir)
	}

	if len(o.MCPServers) > 0 {
		wire := map[string]any{}
		for name, cfg := range o.MCPServers {
			wire[name] = cfg.wireConfig()
		}
		b, err := json.Marshal(map[string]any{"mcpServers": wire})
		if err != nil {
			return nil, wrapError(ErrKindMessageParse, err, "marshal mcp config")
		}
		cmd = append(cmd, "--mcp-config", string(b))
	}
	for _, name := range o.AllowedMCPServerNames {
		cmd = append(cmd, "--allowed-mcp-server-names", name)
	}
	if o.StrictMCPConfig {
		cmd = append(cmd, "--strict-mcp-config")
	}
	if o.IncludePartialMessages {
		cmd = append(cmd, "--include-partial-messages")
	}
	if o.ForkSession {
		cmd = append(cmd, "--fork-session")
	}
	// Agents are always delivered via the initialize control request.
	if o.Agent != "" {
		cmd = append(cmd, "--agent", o.Agent)
	}
	if o.SettingSources != nil {
		parts := make([]string, len(o.SettingSources))
		for i, s := range o.SettingSources {
			parts[i] = string(s)
		}
		cmd = append(cmd, "--setting-sources", strings.Join(parts, ","))
	}
	// SDK mode never loads the CLI's bundled skills.
	cmd = append(cmd, "--disable-builtin-skills")

	for _, p := range o.Plugins {
		if p.Type != "local" {
			return nil, newError(ErrKindAuthNotConfigured, "unsupported plugin type %q", p.Type)
		}
		cmd = append(cmd, "--plugin-dir", p.Path)
	}

	for flag, value := range o.ExtraArgs {
		if value == nil {
			cmd = append(cmd, "--"+flag)
		} else {
			cmd = append(cmd, "--"+flag, *value)
		}
	}
	return cmd, nil
}

// buildSettingsValue renders --settings as a path or inline JSON string.
// MCP tool policies are folded into the inline settings as permission
// rules; a settings file path takes precedence over inline merging.
func (o *Options) buildSettingsValue() (string, error) {
	switch {
	case o.SettingsPath != "" && o.Settings != nil:
		return "", newError(ErrKindAuthNotConfigured, "Settings and SettingsPath are mutually exclusive")
	case o.SettingsPath != "":
		return o.SettingsPath, nil
	}
	policySettings := Settings(o.mcpPolicySettings())
	switch {
	case o.Settings != nil && policySettings != nil:
		merged := mergeSettings(policySettings, o.Settings)
		b, err := json.Marshal(merged)
		if err != nil {
			return "", wrapError(ErrKindMessageParse, err, "marshal settings")
		}
		return string(b), nil
	case o.Settings != nil:
		b, err := json.Marshal(o.Settings)
		if err != nil {
			return "", wrapError(ErrKindMessageParse, err, "marshal settings")
		}
		return string(b), nil
	case policySettings != nil:
		b, err := json.Marshal(policySettings)
		if err != nil {
			return "", wrapError(ErrKindMessageParse, err, "marshal mcp policy settings")
		}
		return string(b), nil
	}
	return "", nil
}

// buildEnv assembles the subprocess environment.
func (o *Options) buildEnv(authFile string) ([]string, error) {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		// Drop inherited proxy vars for the CLI itself, like the Python SDK
		// (they still flow through to tools and grandchildren processes via
		// option env when explicitly set).
		if strings.HasPrefix(kv, "QODERCLI_") {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	_ = env // markers below are appended after option env overrides

	out := make([]string, 0, len(env)+8)
	// Apply caller env overrides (empty string deletes).
	for k, v := range o.Env {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// SDK markers must win over inherited values.
	out = append(out,
		"QODER_ENTRYPOINT=sdk-go",
		"QODER_AGENT_SDK_ENTRYPOINT=sdk-go",
		"QODER_SDK_VERSION="+SDKVersion,
		"QODER_AGENT_SDK_VERSION="+SDKVersion,
	)
	if authFile != "" {
		out = append(out, authPayloadFileEnvVar+"="+authFile)
	}
	return out, nil
}

// cleanupAuthPayload removes the temporary auth payload file if present.
func (o *Options) cleanupAuthPayload() {
	if o.authPayloadPath != "" {
		_ = os.Remove(o.authPayloadPath)
		o.authPayloadPath = ""
	}
}
