package qoder

import (
	"context"
	"fmt"
)

// CanUseTool is invoked whenever the CLI asks the SDK to approve a tool
// call. It requires --permission-prompt-tool stdio, which Options adds
// automatically. The context is cancelled when the CLI abandons the
// request via control_cancel_request.
//
// toolName is the fully qualified name, e.g. "Bash", "Read" or
// "mcp__orders__lookup_order". The returned value must be a
// *PermissionResultAllow or a *PermissionResultDeny.
type CanUseTool func(ctx context.Context, toolName string, input map[string]any, permCtx *ToolPermissionContext) (PermissionResult, error)

// PermissionResult is the outcome of a CanUseTool callback.
// Concrete types: *PermissionResultAllow, *PermissionResultDeny.
type PermissionResult interface {
	permissionResult()
}

// PermissionResultAllow approves the call. UpdatedInput replaces the
// arguments the tool actually receives (nil keeps the original input).
type PermissionResultAllow struct {
	UpdatedInput           map[string]any
	UpdatedPermissions     []PermissionUpdate
	DecisionClassification string
}

func (*PermissionResultAllow) permissionResult() {}

// PermissionResultDeny rejects the call. Message is required. Interrupt
// additionally aborts the current agent turn.
type PermissionResultDeny struct {
	Message                string
	Interrupt              bool
	DecisionClassification string
}

func (*PermissionResultDeny) permissionResult() {}

// ExitPlanModeApprovalDetails carries plan context when the tool being
// approved is ExitPlanMode.
type ExitPlanModeApprovalDetails struct {
	PlanPath    string
	PlanContent string
}

// ToolPermissionContext carries the CLI-provided context for a permission
// request. Cancellation is delivered through the ctx argument of CanUseTool.
type ToolPermissionContext struct {
	// Suggestions are permission updates proposed by the CLI (opaque wire
	// objects; pass them back inside UpdatedPermissions to accept).
	Suggestions []any
	// BlockedPath is the file path that triggered the request, when
	// applicable.
	BlockedPath string
	// DecisionReason is the human-readable reason supplied by the CLI.
	DecisionReason string
	// DecisionReasonType classifies the reason (e.g. "rule", "classifier").
	DecisionReasonType string
	// ClassifierApprovable reports whether auto-mode may approve this
	// automatically. Nil when unknown.
	ClassifierApprovable *bool
	// Title / DisplayName / Description are runtime-generated prompt copy.
	Title        string
	DisplayName  string
	Description  string
	ToolUseID    string
	AgentID      string
	ExitPlanMode *ExitPlanModeApprovalDetails
}

// PermissionUpdateType enumerates the supported dynamic permission updates.
type PermissionUpdateType string

const (
	PermUpdateAddRules       PermissionUpdateType = "addRules"
	PermUpdateReplaceRules   PermissionUpdateType = "replaceRules"
	PermUpdateRemoveRules    PermissionUpdateType = "removeRules"
	PermUpdateSetMode        PermissionUpdateType = "setMode"
	PermUpdateAddDirectories PermissionUpdateType = "addDirectories"
	PermUpdateRemoveDirs     PermissionUpdateType = "removeDirectories"
)

// PermissionBehavior is the rule behavior used by the rules-based updates.
type PermissionBehavior string

const (
	PermissionAllow PermissionBehavior = "allow"
	PermissionDeny  PermissionBehavior = "deny"
	PermissionAsk   PermissionBehavior = "ask"
)

// PermissionUpdateDestination selects which settings layer a permission
// update is persisted to.
type PermissionUpdateDestination string

const (
	DestinationUserSettings    PermissionUpdateDestination = "userSettings"
	DestinationProjectSettings PermissionUpdateDestination = "projectSettings"
	DestinationLocalSettings   PermissionUpdateDestination = "localSettings"
	DestinationSession         PermissionUpdateDestination = "session"
)

// PermissionRuleValue is one permission rule entry.
type PermissionRuleValue struct {
	ToolName    string
	RuleContent string // optional tool-specific rule body
}

// PermissionUpdate is a dynamic permission change returned from
// PermissionResultAllow.UpdatedPermissions.
type PermissionUpdate struct {
	Type        PermissionUpdateType
	Rules       []PermissionRuleValue       // rules-based types
	Behavior    PermissionBehavior          // rules-based types
	Mode        PermissionMode              // setMode
	Directories []string                    // add/removeDirectories
	Destination PermissionUpdateDestination // optional for all types
}

// wire converts the update to the TypeScript control-protocol dict shape.
func (u PermissionUpdate) wire() map[string]any {
	out := map[string]any{"type": string(u.Type)}
	if u.Destination != "" {
		out["destination"] = string(u.Destination)
	}
	switch u.Type {
	case PermUpdateAddRules, PermUpdateReplaceRules, PermUpdateRemoveRules:
		if u.Rules != nil {
			rules := make([]any, 0, len(u.Rules))
			for _, r := range u.Rules {
				rule := map[string]any{"toolName": r.ToolName}
				if r.RuleContent != "" {
					rule["ruleContent"] = r.RuleContent
				}
				rules = append(rules, rule)
			}
			out["rules"] = rules
		}
		if u.Behavior != "" {
			out["behavior"] = string(u.Behavior)
		}
	case PermUpdateSetMode:
		if u.Mode != "" {
			out["mode"] = string(u.Mode)
		}
	case PermUpdateAddDirectories, PermUpdateRemoveDirs:
		if u.Directories != nil {
			out["directories"] = u.Directories
		}
	}
	return out
}

// permissionResultToWire converts a callback result into the can_use_tool
// control response payload, mirroring the Python SDK conversion.
func permissionResultToWire(res PermissionResult, originalInput map[string]any, toolUseID string) (map[string]any, error) {
	switch r := res.(type) {
	case *PermissionResultAllow:
		updated := r.UpdatedInput
		if updated == nil {
			updated = originalInput
		}
		data := map[string]any{"behavior": "allow", "updatedInput": updated}
		if toolUseID != "" {
			data["toolUseID"] = toolUseID
		}
		if r.UpdatedPermissions != nil {
			perms := make([]any, 0, len(r.UpdatedPermissions))
			for _, p := range r.UpdatedPermissions {
				perms = append(perms, p.wire())
			}
			data["updatedPermissions"] = perms
		}
		if r.DecisionClassification != "" {
			data["decisionClassification"] = r.DecisionClassification
		}
		return data, nil
	case *PermissionResultDeny:
		if r.Message == "" {
			return nil, fmt.Errorf("permission deny result must include a message")
		}
		data := map[string]any{"behavior": "deny", "message": r.Message}
		if toolUseID != "" {
			data["toolUseID"] = toolUseID
		}
		if r.Interrupt {
			data["interrupt"] = true
		}
		if r.DecisionClassification != "" {
			data["decisionClassification"] = r.DecisionClassification
		}
		return data, nil
	default:
		return nil, fmt.Errorf("tool permission callback must return *PermissionResultAllow or *PermissionResultDeny, got %T", res)
	}
}
