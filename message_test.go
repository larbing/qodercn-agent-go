package qoder

import (
	"encoding/json"
	"testing"
)

func TestParseUserMessageText(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"u1","session_id":"s1","parent_tool_use_id":null,
		"message":{"role":"user","content":"hello"},"priority":"next","should_query":true}`)
	msg, err := parseMessage(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := msg.(*UserMessage)
	if !ok {
		t.Fatalf("want *UserMessage, got %T", msg)
	}
	if m.Content != "hello" || m.UUID != "u1" || m.SessionID != "s1" {
		t.Fatalf("fields: %+v", m)
	}
	if m.Priority != "next" {
		t.Fatalf("priority: %q", m.Priority)
	}
	if m.IsContentList() {
		t.Fatal("plain string content must not be a list")
	}
}

func TestParseUserMessageBlocks(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tu1","content":"ok","is_error":false}]}}`)
	msg, err := parseMessage(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := msg.(*UserMessage)
	if !ok {
		t.Fatalf("want *UserMessage, got %T", msg)
	}
	if !m.IsContentList() || len(m.ContentBlocks) != 1 {
		t.Fatalf("blocks: %+v", m.ContentBlocks)
	}
	tr, ok := m.ContentBlocks[0].(*ToolResultBlock)
	if !ok || tr.ToolUseID != "tu1" {
		t.Fatalf("tool result block: %+v", m.ContentBlocks[0])
	}
}

func TestParseAssistantMessage(t *testing.T) {
	line := []byte(`{"type":"assistant","session_id":"s2","message":{
		"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7",
		"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"tu9","name":"Bash","input":{"cmd":"ls"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}}`)
	msg, err := parseMessage(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := msg.(*AssistantMessage)
	if !ok {
		t.Fatalf("want *AssistantMessage, got %T", msg)
	}
	if m.ID != "msg_1" || m.Model != "glm-4.7" || m.StopReason != "tool_use" {
		t.Fatalf("fields: %+v", m)
	}
	if len(m.Content) != 2 {
		t.Fatalf("content blocks: %d", len(m.Content))
	}
	tu, ok := m.Content[1].(*ToolUseBlock)
	if !ok || tu.Name != "Bash" || tu.Input["cmd"] != "ls" {
		t.Fatalf("tool use: %+v", m.Content[1])
	}
	if m.Usage == nil || m.Usage.InputTokens != 10 {
		t.Fatalf("usage: %+v", m.Usage)
	}
}

func TestParseSystemInit(t *testing.T) {
	// Real CLI shape: init fields live at the top level of the wire object.
	line := []byte(`{"type":"system","subtype":"init","session_id":"s3","cwd":"/tmp","model":"glm-4.7"}`)
	msg, err := parseMessage(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := msg.(*SystemMessage)
	if !ok || m.Subtype != "init" {
		t.Fatalf("want init SystemMessage, got %T", msg)
	}
	if m.Data["session_id"] != "s3" || m.Data["cwd"] != "/tmp" || m.Data["model"] != "glm-4.7" {
		t.Fatalf("data: %+v", m.Data)
	}
}

func TestParseResultMessage(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","duration_ms":100,"duration_api_ms":80,
		"num_turns":2,"result":"done","session_id":"s4","total_cost_usd":0.01,
		"is_error":false,"permission_denials":[]}`)
	msg, err := parseMessage(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := msg.(*ResultMessage)
	if !ok {
		t.Fatalf("want *ResultMessage, got %T", msg)
	}
	if m.Subtype != "success" || m.Result != "done" || m.NumTurns != 2 || m.IsError {
		t.Fatalf("fields: %+v", m)
	}
	if m.TotalCostUSD == nil || *m.TotalCostUSD != 0.01 {
		t.Fatalf("cost: %+v", m.TotalCostUSD)
	}
}

func TestParseStreamEvent(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"a"}}}`)
	msg, err := parseMessage(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := msg.(*StreamEvent)
	if !ok {
		t.Fatalf("want *StreamEvent, got %T", msg)
	}
	if m.Event["type"] != "content_block_delta" {
		t.Fatalf("event: %+v", m.Event)
	}
}

func TestParseUnknownTypeSkipped(t *testing.T) {
	msg, err := parseMessage([]byte(`{"type":"future_thing","x":1}`))
	if err != nil {
		t.Fatalf("unknown type must not error: %v", err)
	}
	if msg != nil {
		t.Fatalf("unknown type must yield nil, got %T", msg)
	}
}

func TestParseMalformed(t *testing.T) {
	if _, err := parseMessage([]byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
	if _, err := parseMessage([]byte(`{"no_type":true}`)); err == nil {
		t.Fatal("missing type must error")
	}
}

func TestPermissionResultToWire(t *testing.T) {
	allow, err := permissionResultToWire(&PermissionResultAllow{
		UpdatedPermissions: []PermissionUpdate{{
			Type:        PermUpdateAddRules,
			Rules:       []PermissionRuleValue{{ToolName: "Bash", RuleContent: "ls"}},
			Behavior:    PermissionAllow,
			Destination: DestinationSession,
		}},
	}, map[string]any{"cmd": "orig"}, "tu1")
	if err != nil {
		t.Fatalf("allow wire: %v", err)
	}
	if allow["behavior"] != "allow" || allow["updatedInput"].(map[string]any)["cmd"] != "orig" {
		t.Fatalf("allow wire: %+v", allow)
	}
	if allow["toolUseID"] != "tu1" {
		t.Fatalf("toolUseID missing: %+v", allow)
	}
	perms := allow["updatedPermissions"].([]any)[0].(map[string]any)
	if perms["type"] != "addRules" || perms["destination"] != "session" {
		t.Fatalf("permission update: %+v", perms)
	}
	rule := perms["rules"].([]any)[0].(map[string]any)
	if rule["toolName"] != "Bash" || rule["ruleContent"] != "ls" {
		t.Fatalf("rule: %+v", rule)
	}

	if _, err := permissionResultToWire(&PermissionResultDeny{}, nil, ""); err == nil {
		t.Fatal("deny without message must error")
	}
	deny, err := permissionResultToWire(&PermissionResultDeny{Message: "no", Interrupt: true}, nil, "tu2")
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	if deny["behavior"] != "deny" || deny["message"] != "no" || deny["interrupt"] != true {
		t.Fatalf("deny wire: %+v", deny)
	}
}

func TestHookOutputToWireRenames(t *testing.T) {
	cont := false
	wire := hookOutputToWire(&HookJSONOutput{
		Continue:   &cont,
		StopReason: "user abort",
		Decision:   "block",
		Reason:     "keep going",
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:      HookPreToolUse,
			PermissionDecision: "deny",
			AdditionalContext:  "ctx",
		},
	})
	b, _ := json.Marshal(wire)
	got := string(b)
	for _, key := range []string{`"continue":false`, `"stopReason":"user abort"`, `"hookSpecificOutput"`, `"permissionDecision":"deny"`, `"additionalContext":"ctx"`} {
		if !jsonContains(got, key) {
			t.Fatalf("missing %s in %s", key, got)
		}
	}
}

func jsonContains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestNormalizeHookInput(t *testing.T) {
	in := normalizeHookInputFromCLI(map[string]any{
		"hook_event_name":  "PreToolUse",
		"session_id":       "s",
		"transcript_path":  "/t",
		"cwd":              "/w",
		"permission_mode":  "default",
		"tool_name":        "Bash",
		"tool_input":       map[string]any{"command": "ls"},
		"stop_hook_active": true,
	})
	if in.HookEventName != HookPreToolUse || in.ToolName != "Bash" || !in.StopHookActive {
		t.Fatalf("hook input: %+v", in)
	}
	ti, _ := in.ToolInput.(map[string]any)
	if ti["command"] != "ls" {
		t.Fatalf("tool input: %+v", in.ToolInput)
	}
}
