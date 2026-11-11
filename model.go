package qoder

import (
	"context"
	"time"
)

// ModelPurpose identifies why the runtime performs an LLM call. The
// ResolveModel callback receives it on every invocation.
type ModelPurpose string

const (
	PurposeMain        ModelPurpose = "main"
	PurposePlan        ModelPurpose = "plan"
	PurposeTask        ModelPurpose = "task"
	PurposeCompact     ModelPurpose = "compact"
	PurposeTitle       ModelPurpose = "title"
	PurposeSuggestion  ModelPurpose = "suggestion"
	PurposeGenerate    ModelPurpose = "generate"
	PurposeHookPrompt  ModelPurpose = "hook_prompt"
	PurposeSubagent    ModelPurpose = "subagent"
	PurposeWebFetch    ModelPurpose = "web_fetch"
	PurposeImageGen    ModelPurpose = "image_gen"
	PurposeCompression ModelPurpose = "compression"
	PurposeUtility     ModelPurpose = "utility"
)

// ModelInfo describes one account model. Value is the identifier usable in
// Options.Model, SetModel and ModelPolicyResult.Model. Display-only fields
// beyond the core five stay in Raw for forward compatibility.
type ModelInfo struct {
	Value       string
	DisplayName string
	Description string
	IsEnabled   bool
	IsNew       bool
	IsFree      bool
	PriceFactor *float64
	Raw         map[string]any
}

// modelInfoFromWire decodes a ModelInfo entry from the CLI payload.
func modelInfoFromWire(raw map[string]any) ModelInfo {
	info := ModelInfo{
		Value:       str(raw, "value"),
		DisplayName: str(raw, "displayName"),
		Description: str(raw, "description"),
		IsEnabled:   boolVal(raw, "isEnabled"),
		IsNew:       boolVal(raw, "isNew"),
		IsFree:      boolVal(raw, "isFree"),
		Raw:         raw,
	}
	if f, ok := raw["priceFactor"].(float64); ok {
		info.PriceFactor = &f
	}
	return info
}

// ModelPolicyContext is passed to ResolveModel on each LLM call.
type ModelPolicyContext struct {
	Purpose         ModelPurpose
	SessionID       string
	TurnIndex       int
	AvailableModels []ModelInfo
	// AgentID / AgentType are set when the call belongs to a subagent.
	AgentID      string
	AgentType    string
	HasAgentID   bool
	HasAgentType bool
	// ResolvedModel is the CLI's locally resolved base model, when provided.
	HasResolvedModel bool
	ResolvedModel    string
}

// ModelPolicyParameters carries optional per-call inference parameters.
type ModelPolicyParameters struct {
	// ContextWindow overrides the context window token budget.
	ContextWindow int64 `json:"contextWindow,omitempty"`
	// ReasoningEffort overrides the reasoning effort (e.g. "low"/"medium"/"high").
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// ModelPolicyResult is the ResolveModel callback return value. An empty
// Model keeps the CLI-resolved model.
type ModelPolicyResult struct {
	Model      string
	Parameters *ModelPolicyParameters
}

// ModelPolicyProvider is Options.ResolveModel. Returning an empty Model
// defers to the CLI resolution chain; returning an error fails the query.
type ModelPolicyProvider func(ctx context.Context, pctx *ModelPolicyContext) (ModelPolicyResult, error)

// resolveModelWithTimeout invokes the provider under Options.ResolveModelTimeoutMS
// and maps an overrun to ErrKindModelPolicyTimeout.
func resolveModelWithTimeout(ctx context.Context, provider ModelPolicyProvider, timeoutMS int, pctx *ModelPolicyContext) (ModelPolicyResult, error) {
	ms := timeoutMS
	if ms <= 0 {
		ms = 500
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
	defer cancel()

	type outcome struct {
		res ModelPolicyResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := provider(cctx, pctx)
		ch <- outcome{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-cctx.Done():
		return ModelPolicyResult{}, newError(ErrKindModelPolicyTimeout,
			"ResolveModel callback timed out after %dms", ms)
	}
}

// modelPolicyResponseToWire renders the get_model_policy control response.
// An empty result keeps the CLI-resolved model (matches the Python SDK).
func modelPolicyResponseToWire(res ModelPolicyResult) map[string]any {
	data := map[string]any{"type": "get_model_policy"}
	if res.Model != "" {
		data["model"] = res.Model
	}
	if res.Parameters != nil {
		params := map[string]any{}
		if res.Parameters.ContextWindow != 0 {
			params["contextWindow"] = res.Parameters.ContextWindow
		}
		if res.Parameters.ReasoningEffort != "" {
			params["reasoningEffort"] = res.Parameters.ReasoningEffort
		}
		data["parameters"] = params
	}
	return data
}
