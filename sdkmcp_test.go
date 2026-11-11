package qoder

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSDKMCPInitialize(t *testing.T) {
	cfg := NewSDKMCPServer("calc", "2.0.0")
	resp := cfg.server.handleMessage(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if resp["jsonrpc"] != "2.0" || resp["id"] != 1 {
		t.Fatalf("envelope: %+v", resp)
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion: %+v", result)
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "calc" || info["version"] != "2.0.0" {
		t.Fatalf("serverInfo: %+v", info)
	}
}

func TestSDKMCPToolsList(t *testing.T) {
	ro := true
	cfg := NewSDKMCPServer("calc", "1.0.0",
		NewTool("add", "Add numbers", map[string]any{"type": "object", "properties": map[string]any{}},
			func(ctx context.Context, args map[string]any, extra *ToolInvocationContext) (map[string]any, error) {
				return nil, nil
			}, nil),
		NewTool("meta", "Meta", nil, nil, &ToolAnnotations{Title: "Meta", ReadOnlyHint: &ro}),
	)
	resp := cfg.server.handleMessage(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools: %+v", tools)
	}
	first := tools[0].(map[string]any)
	if first["name"] != "add" || first["description"] != "Add numbers" {
		t.Fatalf("tool entry: %+v", first)
	}
	if _, has := first["annotations"]; has {
		t.Fatal("nil annotations must be omitted")
	}
	second := tools[1].(map[string]any)
	if second["annotations"].(map[string]any)["title"] != "Meta" {
		t.Fatalf("annotations: %+v", second)
	}
}

func TestSDKMCPToolsCallWrappedResult(t *testing.T) {
	cfg := NewSDKMCPServer("calc", "1.0.0",
		NewTool("add", "Add", map[string]any{"type": "object"},
			func(ctx context.Context, args map[string]any, extra *ToolInvocationContext) (map[string]any, error) {
				return map[string]any{"sum": 3.0}, nil
			}, nil),
	)
	resp := cfg.server.handleMessage(context.Background(), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "add", "arguments": map[string]any{"a": 1.0, "b": 2.0}},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" {
		t.Fatalf("content: %+v", content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["sum"] != 3.0 {
		t.Fatalf("payload: %+v", payload)
	}
}

func TestSDKMCPToolsCallContentShape(t *testing.T) {
	cfg := NewSDKMCPServer("calc", "1.0.0",
		NewTool("hello", "Hello", nil,
			func(ctx context.Context, args map[string]any, extra *ToolInvocationContext) (map[string]any, error) {
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": "hi"}}, "is_error": false}, nil
			}, nil),
	)
	resp := cfg.server.handleMessage(context.Background(), map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "hello", "arguments": map[string]any{}},
	})
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	if content["text"] != "hi" {
		t.Fatalf("content: %+v", content)
	}
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("isError must default falsy: %+v", result)
	}
}

func TestSDKMCPHandlerErrorIsToolError(t *testing.T) {
	cfg := NewSDKMCPServer("calc", "1.0.0",
		NewTool("boom", "Boom", nil,
			func(ctx context.Context, args map[string]any, extra *ToolInvocationContext) (map[string]any, error) {
				return nil, context.Canceled
			}, nil),
	)
	resp := cfg.server.handleMessage(context.Background(), map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "boom"},
	})
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("handler error must set isError: %+v", result)
	}
	if _, hasErr := resp["error"]; hasErr {
		t.Fatal("handler error must not be a JSON-RPC error")
	}
}

func TestSDKMCPUnknownToolAndMethod(t *testing.T) {
	cfg := NewSDKMCPServer("calc", "1.0.0")
	resp := cfg.server.handleMessage(context.Background(), map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{"name": "nope"},
	})
	errObj := resp["error"].(map[string]any)
	if code, _ := errObj["code"].(int64); code != -32602 {
		t.Fatalf("unknown tool code: %+v", errObj)
	}
	resp = cfg.server.handleMessage(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": "resources/list"})
	errObj = resp["error"].(map[string]any)
	if code, _ := errObj["code"].(int64); code != -32601 {
		t.Fatalf("unknown method code: %+v", errObj)
	}
}

func TestSDKServerWireConfigStripsInstance(t *testing.T) {
	cfg := NewSDKMCPServer("calc", "1.0.0", NewTool("add", "Add", nil, nil, nil))
	wire := cfg.wireConfig()
	if len(wire) != 2 || wire["type"] != "sdk" || wire["name"] != "calc" {
		t.Fatalf("wire must carry only type+name: %+v", wire)
	}
	if cfg.server == nil || cfg.server.tools["add"] == nil {
		t.Fatal("instance must stay local for control-protocol routing")
	}
	if cfg.mcpType() != "sdk" {
		t.Fatal("mcpType")
	}
}

func TestNormalizeToolResultNil(t *testing.T) {
	out := normalizeToolResult(nil)
	if list, ok := out["content"].([]any); !ok || len(list) != 0 {
		t.Fatalf("nil result: %+v", out)
	}
}

func TestTokenResponseValidation(t *testing.T) {
	if _, err := tokenResponse("", 0, "x"); err == nil {
		t.Fatal("empty token must error")
	}
	if _, err := tokenResponse("t", 42, "x"); err == nil {
		t.Fatal("sub-millisecond-epoch expiresAt must error")
	}
	out, err := tokenResponse("t", 1_700_000_000_000, "x")
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if out["expires_at"] != int64(1_700_000_000_000) {
		t.Fatalf("expires_at: %+v", out)
	}
}

func TestModelPolicyWire(t *testing.T) {
	wire := modelPolicyResponseToWire(ModelPolicyResult{})
	if _, has := wire["model"]; has {
		t.Fatal("empty model must keep CLI resolution")
	}
	wire = modelPolicyResponseToWire(ModelPolicyResult{
		Model:      "glm-4.7",
		Parameters: &ModelPolicyParameters{ContextWindow: 200000, ReasoningEffort: "high"},
	})
	if wire["model"] != "glm-4.7" {
		t.Fatalf("model: %+v", wire)
	}
	params := wire["parameters"].(map[string]any)
	if params["contextWindow"] != int64(200000) || params["reasoningEffort"] != "high" {
		t.Fatalf("parameters: %+v", params)
	}
}

func TestModelInfoFromWire(t *testing.T) {
	info := modelInfoFromWire(map[string]any{
		"value": "glm-4.7", "displayName": "GLM", "isEnabled": true, "priceFactor": 1.5,
	})
	if info.Value != "glm-4.7" || !info.IsEnabled || info.PriceFactor == nil || *info.PriceFactor != 1.5 {
		t.Fatalf("info: %+v", info)
	}
}
