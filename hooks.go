package qoder

import (
	"context"
)

// HookEvent enumerates the lifecycle events that can carry SDK hooks.
type HookEvent string

const (
	HookPreToolUse         HookEvent = "PreToolUse"
	HookPostToolUse        HookEvent = "PostToolUse"
	HookPostToolUseFailure HookEvent = "PostToolUseFailure"
	HookUserPromptSubmit   HookEvent = "UserPromptSubmit"
	HookSessionStart       HookEvent = "SessionStart"
	HookSessionEnd         HookEvent = "SessionEnd"
	HookStop               HookEvent = "Stop"
	HookSubagentStart      HookEvent = "SubagentStart"
	HookSubagentStop       HookEvent = "SubagentStop"
	HookPreCompact         HookEvent = "PreCompact"
	HookPostCompact        HookEvent = "PostCompact"
	HookCwdChanged         HookEvent = "CwdChanged"
	HookInstructionsLoaded HookEvent = "InstructionsLoaded"
	HookFileChanged        HookEvent = "FileChanged"
	HookPermissionRequest  HookEvent = "PermissionRequest"
)

// HookCallback runs when a registered matcher fires. ctx is cancelled when
// the CLI abandons the callback via control_cancel_request. toolUseID is
// empty for events that do not concern a specific tool invocation.
type HookCallback func(ctx context.Context, input *HookInput, toolUseID string) (*HookJSONOutput, error)

// HookMatcher selects which invocations trigger the hooks. Matcher is an
// optional regex applied to tool_name (PreToolUse/PostToolUse family).
// Timeout is in seconds; 0 uses the CLI default (60).
type HookMatcher struct {
	Matcher string
	Timeout int
	Hooks   []HookCallback
}

// HookInput aggregates the fields of every hook event input type. Only the
// fields relevant to the event (see the Python SDK Hooks Reference) are
// populated; Raw always carries the untouched CLI payload.
type HookInput struct {
	// Base fields.
	HookEventName  HookEvent
	SessionID      string
	TranscriptPath string
	Cwd            string

	// PermissionMode is set for PreToolUse.
	PermissionMode string

	// Tool invocation events (PreToolUse/PostToolUse/PostToolUseFailure/
	// PermissionRequest).
	ToolName              string
	ToolInput             any
	ToolResponse          any    // PostToolUse
	Error                 string // PostToolUseFailure
	IsInterrupt           *bool  // PostToolUseFailure
	PermissionSuggestions []any  // PermissionRequest

	// UserPromptSubmit.
	Prompt string

	// SessionStart ("startup"/"resume"/"clear"/"compact").
	Source string

	// SessionEnd ("clear"/"resume"/"logout"/"prompt_input_exit"/"other"/...).
	Reason string

	// Stop / SubagentStop.
	StopHookActive bool

	// SubagentStart.
	AgentID   string
	AgentType string

	// PreCompact ("manual"/"auto").
	Trigger            string
	CustomInstructions string

	// PostCompact.
	CompactSummary string

	// CwdChanged.
	OldCwd string
	NewCwd string

	// InstructionsLoaded ("nested_traversal"/"path_glob_match").
	LoadReason string

	// FileChanged ("change"/"add"/"unlink").
	FilePath string
	Event    string

	// Raw is the full request payload from the CLI.
	Raw map[string]any
}

// HookJSONOutput is the value returned by a HookCallback. Field names map
// onto the CLI wire keys during serialization (continue_ -> "continue",
// hookSpecificOutput etc.).
type HookJSONOutput struct {
	// Continue=false terminates the session (supported events only).
	Continue *bool
	// StopReason is the human-readable reason paired with Continue=false.
	StopReason string
	// SuppressOutput hides stdout from transcript mode.
	SuppressOutput *bool
	// Decision is "approve" or "block"; "block" wins across multiple hooks.
	Decision string
	// Reason explains the decision; for Stop+"block" it is injected as the
	// continuation prompt.
	Reason string
	// SystemMessage adds a warning for the user.
	SystemMessage string
	// HookSpecificOutput carries event-specific fields.
	HookSpecificOutput *HookSpecificOutput
}

// HookSpecificOutput is the event-specific portion of HookJSONOutput.
type HookSpecificOutput struct {
	HookEventName HookEvent
	// PermissionDecision applies to PreToolUse: "allow" / "deny" / "ask" /
	// "defer".
	PermissionDecision string
	// PermissionDecisionReason explains the decision.
	PermissionDecisionReason string
	// UpdatedInput (PreToolUse) replaces the tool arguments.
	UpdatedInput map[string]any
	// UpdatedPermissions (PreToolUse) applies dynamic permission updates.
	UpdatedPermissions []PermissionUpdate
	// UpdatedToolOutput (PostToolUse) replaces what the model sees of the
	// tool response.
	UpdatedToolOutput any
	// AdditionalContext (UserPromptSubmit/SessionStart/PostToolUse/
	// PreToolUse) injects extra context for the next model round.
	AdditionalContext string
}

// normalizeHookInput converts the raw CLI callback payload into the typed
// HookInput.
func normalizeHookInputFromCLI(raw map[string]any) *HookInput {
	in := &HookInput{Raw: raw}
	in.HookEventName = HookEvent(str(raw, "hook_event_name"))
	in.SessionID = str(raw, "session_id")
	in.TranscriptPath = str(raw, "transcript_path")
	in.Cwd = str(raw, "cwd")
	in.PermissionMode = str(raw, "permission_mode")
	in.ToolName = str(raw, "tool_name")
	in.ToolInput = raw["tool_input"]
	in.ToolResponse = raw["tool_response"]
	in.Error = str(raw, "error")
	in.IsInterrupt = optBool(raw, "is_interrupt")
	in.PermissionSuggestions, _ = raw["permission_suggestions"].([]any)
	in.Prompt = str(raw, "prompt")
	in.Source = str(raw, "source")
	in.Reason = str(raw, "reason")
	in.StopHookActive = boolVal(raw, "stop_hook_active")
	in.AgentID = str(raw, "agent_id")
	in.AgentType = str(raw, "agent_type")
	in.Trigger = str(raw, "trigger")
	in.CustomInstructions = str(raw, "custom_instructions")
	in.CompactSummary = str(raw, "compact_summary")
	in.OldCwd = str(raw, "old_cwd")
	in.NewCwd = str(raw, "new_cwd")
	in.LoadReason = str(raw, "load_reason")
	in.FilePath = str(raw, "file_path")
	in.Event = str(raw, "event")
	return in
}

// hookOutputToWire converts a callback result to the CLI-expected JSON,
// renaming Go-friendly fields to their wire keys.
func hookOutputToWire(out *HookJSONOutput) map[string]any {
	if out == nil {
		return map[string]any{}
	}
	wire := map[string]any{}
	if out.Continue != nil {
		wire["continue"] = *out.Continue
	}
	if out.StopReason != "" {
		wire["stopReason"] = out.StopReason
	}
	if out.SuppressOutput != nil {
		wire["suppressOutput"] = *out.SuppressOutput
	}
	if out.Decision != "" {
		wire["decision"] = out.Decision
	}
	if out.Reason != "" {
		wire["reason"] = out.Reason
	}
	if out.SystemMessage != "" {
		wire["systemMessage"] = out.SystemMessage
	}
	if hs := out.HookSpecificOutput; hs != nil {
		specific := map[string]any{"hookEventName": string(hs.HookEventName)}
		if hs.PermissionDecision != "" {
			specific["permissionDecision"] = hs.PermissionDecision
		}
		if hs.PermissionDecisionReason != "" {
			specific["permissionDecisionReason"] = hs.PermissionDecisionReason
		}
		if hs.UpdatedInput != nil {
			specific["updatedInput"] = hs.UpdatedInput
		}
		if hs.UpdatedPermissions != nil {
			perms := make([]any, 0, len(hs.UpdatedPermissions))
			for _, p := range hs.UpdatedPermissions {
				perms = append(perms, p.wire())
			}
			specific["updatedPermissions"] = perms
		}
		if hs.UpdatedToolOutput != nil {
			specific["updatedToolOutput"] = hs.UpdatedToolOutput
		}
		if hs.AdditionalContext != "" {
			specific["additionalContext"] = hs.AdditionalContext
		}
		wire["hookSpecificOutput"] = specific
	}
	return wire
}

// Small extraction helpers over the raw wire map.

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func boolVal(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func optBool(m map[string]any, key string) *bool {
	if v, ok := m[key].(bool); ok {
		return &v
	}
	return nil
}
