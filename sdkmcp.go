package qoder

import (
	"context"
	"encoding/json"
	"fmt"
)

// ToolHandler executes one SDK MCP tool invocation. args carries the
// tool input; extra carries invocation metadata (nil-safe). The returned
// map may either be the full MCP CallToolResult shape
// ({"content": [...], "is_error": true}) or an arbitrary result object,
// which is wrapped as a single JSON text content block.
type ToolHandler func(ctx context.Context, args map[string]any, extra *ToolInvocationContext) (map[string]any, error)

// ToolInvocationContext is passed as the extra argument of a ToolHandler.
type ToolInvocationContext struct {
	// RequestID identifies the bridged MCP message.
	RequestID string
	// Meta holds optional request metadata from the CLI.
	Meta map[string]any
}

// ToolAnnotations mirrors MCP tool annotations.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Tool is one SDK MCP tool definition; create with NewTool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object for the tool arguments.
	InputSchema map[string]any
	Annotations *ToolAnnotations
	Handler     ToolHandler
}

// NewTool defines an SDK MCP tool. The full model-visible name is
// mcp__{serverName}__{name}.
func NewTool(name, description string, inputSchema map[string]any, handler ToolHandler, annotations *ToolAnnotations) *Tool {
	return &Tool{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
		Annotations: annotations,
		Handler:     handler,
	}
}

// SDKMCPServer is an in-process MCP server. Register it through
// Options.MCPServers; the CLI reaches it over the control protocol, so no
// extra transport is required.
type SDKMCPServer struct {
	name    string
	version string
	tools   map[string]*Tool
	order   []string
}

// NewSDKMCPServer creates an in-process MCP server with the given tools.
func NewSDKMCPServer(name, version string, tools ...*Tool) *SDKServerConfig {
	s := &SDKMCPServer{
		name:    name,
		version: version,
		tools:   map[string]*Tool{},
	}
	for _, t := range tools {
		if t == nil || t.Name == "" {
			continue
		}
		if _, dup := s.tools[t.Name]; !dup {
			s.order = append(s.order, t.Name)
		}
		s.tools[t.Name] = t
	}
	return &SDKServerConfig{Name: name, server: s}
}

// handleMessage routes one bridged JSON-RPC message and returns the full
// JSON-RPC response object ({"jsonrpc","id","result"|"error"}).
func (s *SDKMCPServer) handleMessage(ctx context.Context, message map[string]any) map[string]any {
	id := message["id"]
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)

	switch method {
	case "initialize":
		return jsonrpcResult(id, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    s.name,
				"version": s.versionOr("1.0.0"),
			},
		})
	case "tools/list":
		list := make([]any, 0, len(s.order))
		for _, name := range s.order {
			t := s.tools[name]
			entry := map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": schemaOrEmpty(t.InputSchema),
			}
			if t.Annotations != nil {
				entry["annotations"] = annotationsWire(t.Annotations)
			}
			list = append(list, entry)
		}
		return jsonrpcResult(id, map[string]any{"tools": list})
	case "tools/call":
		name, _ := params["name"].(string)
		tool := s.tools[name]
		if tool == nil {
			return jsonrpcError(id, -32602, fmt.Sprintf("Tool not found: %s", name))
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		extra := &ToolInvocationContext{Meta: params}
		if rid, ok := message["id"].(string); ok {
			extra.RequestID = rid
		}
		result, err := tool.Handler(ctx, args, extra)
		if err != nil {
			// Handler failures surface as tool errors (isError=true),
			// matching the Python SDK semantics.
			return jsonrpcResult(id, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			})
		}
		return jsonrpcResult(id, normalizeToolResult(result))
	default:
		return jsonrpcError(id, -32601, fmt.Sprintf("Method not found: %s", method))
	}
}

func (s *SDKMCPServer) versionOr(fallback string) string {
	if s.version != "" {
		return s.version
	}
	return fallback
}

func schemaOrEmpty(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}
	return schema
}

// annotationsWire renders annotations as a plain JSON map so consumers of
// the in-memory result see the same shape the CLI receives on the wire.
func annotationsWire(a *ToolAnnotations) map[string]any {
	b, err := json.Marshal(a)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// normalizeToolResult adapts a handler result to the MCP CallToolResult
// wire shape. A "content" key means the handler built the full result; any
// other map is serialized into a single text block.
func normalizeToolResult(result map[string]any) map[string]any {
	if result == nil {
		return map[string]any{"content": []any{}}
	}
	if _, ok := result["content"]; ok {
		out := map[string]any{"content": result["content"]}
		switch {
		case result["isError"] == true:
			out["isError"] = true
		case result["is_error"] == true:
			out["isError"] = true
		}
		return out
	}
	b, err := json.Marshal(result)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", result))
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(b)}},
	}
}

func jsonrpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func jsonrpcError(id any, code int64, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}
