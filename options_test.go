package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeDefaults(t *testing.T) {
	o, err := (&Options{Auth: AccessToken("t")}).Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if o.ResolveModelTimeoutMS != 500 || o.LoadTimeoutMS != 60000 ||
		o.ControlRequestTimeoutMS != 60000 || o.InitializeTimeoutSec != 60 ||
		o.SessionStoreFlush != FlushBatched {
		t.Fatalf("defaults: %+v", o)
	}
	// Receiver must stay untouched.
	orig := &Options{Auth: AccessToken("t")}
	if _, err := orig.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if orig.ResolveModelTimeoutMS != 0 {
		t.Fatal("Normalize must not mutate the receiver")
	}
}

func TestNormalizeBypassRequiresOptIn(t *testing.T) {
	if _, err := (&Options{Auth: AccessToken("t"), PermissionMode: PermissionBypassPermissions}).Normalize(); err == nil {
		t.Fatal("bypassPermissions without opt-in must fail")
	}
	if _, err := (&Options{Auth: AccessToken("t"), PermissionMode: PermissionBypassPermissions, AllowDangerouslySkipPermissions: true}).Normalize(); err != nil {
		t.Fatalf("opt-in bypass: %v", err)
	}
	// yolo is the legacy alias: no explicit opt-in required.
	if _, err := (&Options{Auth: AccessToken("t"), PermissionMode: PermissionYolo}).Normalize(); err != nil {
		t.Fatalf("yolo: %v", err)
	}
}

func TestNormalizeCanUseToolExclusive(t *testing.T) {
	_, err := (&Options{Auth: AccessToken("t"), CanUseTool: func(ctx context.Context, name string, input map[string]any, pctx *ToolPermissionContext) (PermissionResult, error) {
		return nil, nil
	}, PermissionPromptToolName: "mytool"}).Normalize()
	if err == nil {
		t.Fatal("CanUseTool + PermissionPromptToolName must be mutually exclusive")
	}
}

func TestBuildCLIArgsBase(t *testing.T) {
	drop := true
	o := &Options{
		SystemPrompt:           "be brief",
		Tools:                  []string{"Read", "Grep"},
		AllowedTools:           []string{"Bash"},
		DisallowedTools:        []string{"WebFetch"},
		Model:                  "glm-4.7",
		MaxTurns:               3,
		PermissionMode:         PermissionAcceptEdits,
		ContinueConversation:   true,
		SessionID:              "abc",
		AddDirs:                []string{"/tmp"},
		IncludePartialMessages: true,
		ForkSession:            true,
		ResumeDropsTurn:        &drop,
		SettingSources:         []SettingSource{SettingSourceProject},
		Plugins:                []SDKPluginConfig{LocalPlugin("/p")},
	}
	args, err := o.buildCLIArgs()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	assertArgSequence(t, args, [][]string{
		{"--print"}, {"--output-format", "stream-json"}, {"--input-format", "stream-json"},
		{"--system-prompt", "be brief"},
		{"--tools", "Read,Grep"},
		{"--allowed-tools", "Bash"},
		{"--max-turns", "3"},
		{"--disallowed-tools", "WebFetch"},
		{"--model", "glm-4.7"},
		{"--permission-mode", "acceptEdits"},
		{"--continue"}, {"--resume-drops-turn=true"}, {"--session-id", "abc"},
		{"--add-dir", "/tmp"},
		{"--include-partial-messages"}, {"--fork-session"},
		{"--setting-sources", "project"},
		{"--disable-builtin-skills"},
		{"--plugin-dir", "/p"},
	})
}

func assertArgSequence(t *testing.T, args []string, want [][]string) {
	t.Helper()
	pos := 0
	for _, seq := range want {
		found := -1
		for i := pos; i <= len(args)-len(seq); i++ {
			ok := true
			for j, w := range seq {
				if args[i+j] != w {
					ok = false
					break
				}
			}
			if ok {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("missing %v in args %v", seq, args)
		}
		pos = found + len(seq)
	}
}

func TestBuildCLIArgsDefaultPromptBlank(t *testing.T) {
	args, err := (&Options{}).buildCLIArgs()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	assertArgSequence(t, args, [][]string{{"--system-prompt", ""}})
}

func TestBuildCLIArgsToolsPresets(t *testing.T) {
	args, _ := (&Options{ToolsAllDefault: true}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{{"--tools", "default"}})

	args, _ = (&Options{Tools: []string{}}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{{"--tools", ""}})
}

func TestBuildCLIArgsSkills(t *testing.T) {
	args, _ := (&Options{Skills: []string{"review", "docs"}}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{
		{"--allowed-tools", "Skill(review)"},
		{"--allowed-tools", "Skill(docs)"},
		{"--disable-builtin-skills"},
	})
	args, _ = (&Options{SkillsAll: true}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{{"--allowed-tools", "Skill"}})
}

func TestBuildCLIArgsYolo(t *testing.T) {
	args, _ := (&Options{PermissionMode: PermissionYolo}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{{"--yolo"}})
	args, _ = (&Options{PermissionMode: PermissionBypassPermissions, AllowDangerouslySkipPermissions: true}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{{"--permission-mode", "bypassPermissions"}, {"--dangerously-skip-permissions"}})
}

func TestBuildCLIArgsPermissionPrompt(t *testing.T) {
	args, _ := (&Options{CanUseTool: func(_ context.Context, _ string, _ map[string]any, _ *ToolPermissionContext) (PermissionResult, error) {
		return nil, nil
	}}).buildCLIArgs()
	assertArgSequence(t, args, [][]string{{"--permission-prompt-tool", "stdio"}})
}

func TestBuildCLIArgsMCPConfig(t *testing.T) {
	sdk := NewSDKMCPServer("calc", "2.0.0", NewTool("add", "Add", map[string]any{"type": "object"}, nil, nil))
	o := &Options{MCPServers: map[string]MCPServerConfig{
		"calc": sdk,
		"remote": &HTTPServerConfig{URL: "https://mcp.example", Headers: map[string]string{"k": "v"},
			Tools: []McpToolPolicy{{Name: "fetch", PermissionPolicy: ToolPolicyAlwaysDeny}}},
	}}
	args, err := o.buildCLIArgs()
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	var cfgArg string
	for i, a := range args {
		if a == "--mcp-config" {
			cfgArg = args[i+1]
		}
	}
	if cfgArg == "" {
		t.Fatal("--mcp-config missing")
	}
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfgArg), &cfg); err != nil {
		t.Fatalf("config json: %v (%s)", err, cfgArg)
	}
	calc := cfg.MCPServers["calc"]
	if calc["type"] != "sdk" || calc["name"] != "calc" {
		t.Fatalf("sdk wire: %+v", calc)
	}
	if _, has := calc["tools"]; has {
		t.Fatal("sdk wire must strip tools")
	}
	remote := cfg.MCPServers["remote"]
	if remote["type"] != "http" || remote["url"] != "https://mcp.example" {
		t.Fatalf("http wire: %+v", remote)
	}
}

func TestBuildSettingsValuePolicies(t *testing.T) {
	o := &Options{MCPServers: map[string]MCPServerConfig{
		"orders": &StdioServerConfig{
			Command: "uvx", Args: []string{"orders-mcp"},
			Tools: []McpToolPolicy{
				{Name: "lookup", PermissionPolicy: ToolPolicyAlwaysAllow},
				{Name: "refund", PermissionPolicy: ToolPolicyAlwaysDeny},
			},
		},
	}}
	v, err := o.buildSettingsValue()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(v), &settings); err != nil {
		t.Fatalf("settings json: %v", err)
	}
	perms := settings["permissions"].(map[string]any)
	if len(perms["allow"].([]any)) != 1 || len(perms["deny"].([]any)) != 1 {
		t.Fatalf("permissions: %+v", perms)
	}
	if perms["allow"].([]any)[0] != "mcp__orders__lookup" {
		t.Fatalf("allow rule: %+v", perms)
	}

	// User settings merge on top of policies.
	o.Settings = Settings{"skillOverrides": map[string]any{"x": "off"}}
	v, err = o.buildSettingsValue()
	if err != nil {
		t.Fatalf("settings merge: %v", err)
	}
	if !strings.Contains(v, "skillOverrides") || !strings.Contains(v, "mcp__orders__lookup") {
		t.Fatalf("merged settings: %s", v)
	}
}

func TestBuildEnvMarkers(t *testing.T) {
	o := &Options{Env: map[string]string{"FOO": "bar"}}
	env, err := o.buildEnv("/tmp/auth.json")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"QODER_ENTRYPOINT=sdk-go",
		"QODER_AGENT_SDK_ENTRYPOINT=sdk-go",
		"QODER_SDK_VERSION=" + SDKVersion,
		"QODER_AGENT_SDK_VERSION=" + SDKVersion,
		"QODER_SDK_AUTH_PAYLOAD_FILE=/tmp/auth.json",
		"FOO=bar",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %s in %v", want, env)
		}
	}
}

func TestBuildSettingsExclusive(t *testing.T) {
	o := &Options{Settings: Settings{"a": 1}, SettingsPath: "/x.json"}
	if _, err := o.buildSettingsValue(); err == nil {
		t.Fatal("Settings + SettingsPath must be mutually exclusive")
	}
}

func TestNormalizeTransportMode(t *testing.T) {
	o, err := (&Options{Auth: AccessToken("t")}).Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if o.TransportMode != TransportAuto {
		t.Fatalf("zero value must normalize to auto, got %q", o.TransportMode)
	}
	for _, mode := range []TransportMode{TransportAuto, TransportDaemon, TransportDirect} {
		o, err := (&Options{Auth: AccessToken("t"), TransportMode: mode}).Normalize()
		if err != nil {
			t.Fatalf("normalize %q: %v", mode, err)
		}
		if o.TransportMode != mode {
			t.Fatalf("mode %q not preserved: %q", mode, o.TransportMode)
		}
	}
	if _, err := (&Options{Auth: AccessToken("t"), TransportMode: "bogus"}).Normalize(); err == nil {
		t.Fatal("unknown TransportMode must fail")
	}
}

// memStore is a minimal SessionStore stub for daemon validation tests.
type memStore struct{}

func (memStore) Append(context.Context, SessionKey, []SessionStoreEntry) error { return nil }
func (memStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

func TestValidateDaemonOptions(t *testing.T) {
	for _, mode := range []PermissionMode{"", PermissionDefault, PermissionAcceptEdits,
		PermissionAuto, PermissionBypassPermissions, PermissionYolo} {
		if err := (&Options{PermissionMode: mode}).validateDaemonOptions(); err != nil {
			t.Fatalf("mode %q must be accepted: %v", mode, err)
		}
	}
	for _, mode := range []PermissionMode{PermissionDontAsk, PermissionPlan} {
		err := (&Options{PermissionMode: mode}).validateDaemonOptions()
		if err == nil {
			t.Fatalf("mode %q must be rejected", mode)
		}
		var qerr *Error
		if !errors.As(err, &qerr) || qerr.Kind != ErrKindUnsupportedCapability {
			t.Fatalf("mode %q: want unsupported capability error, got %v", mode, err)
		}
	}
	drop := true
	for _, o := range []*Options{
		{ResumeSessionAt: "abc"},
		{ResumeDropsTurn: &drop},
		{ContinueConversation: true},
	} {
		if err := o.validateDaemonOptions(); err == nil {
			t.Fatalf("option %+v must be rejected", o)
		}
	}
	if err := (&Options{ContinueConversation: true, SessionStore: memStore{}}).validateDaemonOptions(); err != nil {
		t.Fatalf("continue with store must be accepted: %v", err)
	}
}
