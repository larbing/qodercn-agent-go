package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// QueryOptions tunes a single Client.Query message.
type QueryOptions struct {
	// Priority is "now" (interrupt and process immediately), "next"
	// (default) or "later" (after the current reply).
	Priority string
	// MessageUUID identifies the message for cancellation and replay
	// dedup; do not reuse within one session.
	MessageUUID string
	// ShouldQuery controls whether the message may start an assistant turn
	// while the session is idle. Nil (the zero value of a *bool) means true,
	// matching the Python SDK default; point it at a false value to enqueue
	// the message without triggering a reply (e.g. context injection).
	ShouldQuery *bool
	// CustomContext overrides Options.CustomContext for this message.
	CustomContext map[string]string
}

// Client drives one long-lived CLI subprocess for multi-turn conversation.
// Create with NewClient, connect with Connect, then alternate Query /
// ReceiveMessages until Disconnect.
type Client struct {
	opts *Options

	mu       sync.Mutex
	sess     *session
	startErr error
	started  bool
}

// NewClient validates options without spawning the CLI; failures surface
// from Connect.
func NewClient(opts *Options) (*Client, error) {
	norm, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	return &Client{opts: norm}, nil
}

// Connect spawns qoderclicn, performs the initialize handshake and
// optionally sends an initial prompt.
func (c *Client) Connect(ctx context.Context, prompt ...string) error {
	c.mu.Lock()
	if c.started {
		err := c.startErr
		c.mu.Unlock()
		if err != nil {
			return err
		}
		return cliConnectionError("client already connected")
	}
	c.started = true
	c.mu.Unlock()

	sess, err := newSession(ctx, c.opts)
	if err != nil {
		c.startErr = err
		return err
	}
	if _, err := sess.initialize(ctx); err != nil {
		sess.finish(err)
		c.mu.Lock()
		c.startErr = err
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.sess = sess
	c.mu.Unlock()
	if len(prompt) > 0 && prompt[0] != "" {
		if err := c.query(ctx, prompt[0], QueryOptions{Priority: "next"}); err != nil {
			return err
		}
	}
	return nil
}

// connectedSession returns the live session or an error.
func (c *Client) connectedSession() (*session, error) {
	if c.sess == nil {
		return nil, cliConnectionError("not connected: call Connect first")
	}
	return c.sess, nil
}

// Query sends a user message into the live session. It does not wait for
// the reply; consume ReceiveMessages or ReceiveResponse.
func (c *Client) Query(ctx context.Context, prompt string, qopts ...QueryOptions) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	opts := QueryOptions{Priority: "next"}
	if len(qopts) > 0 {
		if qopts[0].Priority != "" {
			opts.Priority = qopts[0].Priority
		}
		opts.MessageUUID = qopts[0].MessageUUID
		opts.ShouldQuery = qopts[0].ShouldQuery
		opts.CustomContext = qopts[0].CustomContext
	}
	return c.query(ctx, prompt, opts)
}

// query writes the wire user message (the client must be live).
func (c *Client) query(_ context.Context, prompt string, opts QueryOptions) error {
	c.mu.Lock()
	sess := c.sess
	optsDefaults := c.opts.CustomContext
	c.mu.Unlock()
	if sess == nil {
		return cliConnectionError("not connected: call Connect first")
	}
	out := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": prompt},
		"parent_tool_use_id": nil,
		"session_id":         defaultSessionID,
		"priority":           opts.Priority,
	}
	if opts.MessageUUID != "" {
		out["uuid"] = opts.MessageUUID
	}
	if opts.ShouldQuery != nil && !*opts.ShouldQuery {
		out["shouldQuery"] = false
	}
	ctxValue := opts.CustomContext
	if ctxValue == nil {
		ctxValue = optsDefaults
	}
	if ctxValue != nil {
		out["custom_context"] = ctxValue
	}
	return sess.writeUserMessage(out)
}

// ReceiveMessages streams every message until the session ends (io.EOF) or
// ctx is cancelled.
func (c *Client) ReceiveMessages(ctx context.Context) (*MessageStream, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	return &MessageStream{sess: c.sess}, nil
}

// ReceiveResponse consumes the stream until the next ResultMessage and
// returns it (io.EOF if the stream ended first).
func (c *Client) ReceiveResponse(ctx context.Context) (*ResultMessage, error) {
	stream, err := c.ReceiveMessages(ctx)
	if err != nil {
		return nil, err
	}
	for {
		msg, err := stream.Next(ctx)
		if err != nil {
			return nil, err
		}
		if res, ok := msg.(*ResultMessage); ok {
			return res, nil
		}
	}
}

// Interrupt stops the current generation or tool execution without
// disconnecting or dropping queued messages.
func (c *Client) Interrupt(ctx context.Context) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

// SetModel switches the model of the running session.
func (c *Client) SetModel(ctx context.Context, model string) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	// The CLI accepts any set_model value (even invalid ones) and only
	// fails later with an opaque per-query error. Resolve against the
	// catalog and send the canonical value so a typo never poisons the
	// session.
	canonical, ok := resolveModelName(model, c.SupportedModels())
	if !ok {
		return fmt.Errorf("unknown model %q (not in the CLI model catalog)", model)
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "set_model", "model": canonical})
	return err
}

// SetPermissionMode updates the permission mode. "yolo" normalizes to
// bypassPermissions like the Python SDK.
func (c *Client) SetPermissionMode(ctx context.Context, mode PermissionMode) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	wireMode := string(mode)
	if mode == PermissionYolo {
		wireMode = string(PermissionBypassPermissions)
	}
	request := map[string]any{"subtype": "set_permission_mode", "mode": wireMode}
	if mode == PermissionPlan {
		request["preservePlanMode"] = true
	}
	_, err := c.sess.sendControlRequestDefault(ctx, request)
	return err
}

// SetProxy sets or clears (empty string) the CLI proxy at runtime.
func (c *Client) SetProxy(ctx context.Context, proxy string) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "set_proxy", "proxy": proxy})
	return err
}

// ApplyFlagSettings injects settings into the active session as if passed
// via command-line flags.
func (c *Client) ApplyFlagSettings(ctx context.Context, settings Settings) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "apply_flag_settings", "settings": settings})
	return err
}

// ServerInfo returns the initialize snapshot (capabilities, skills, ...).
func (c *Client) ServerInfo() map[string]any {
	if c.sess == nil {
		return nil
	}
	c.sess.stMu.Lock()
	defer c.sess.stMu.Unlock()
	return c.sess.initResult
}

// SupportedAgents lists agents registered with this session (from the
// initialize response). Empty before Connect.
func (c *Client) SupportedAgents() []map[string]any {
	info := c.ServerInfo()
	agents, _ := info["agents"].([]any)
	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		if m, ok := a.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// SupportedModels lists the model catalog the CLI advertised in the
// initialize response (entries carry value/modelId/displayName/isEnabled/...).
// Empty before Connect or with CLIs that omit the catalog.
func (c *Client) SupportedModels() []map[string]any {
	info := c.ServerInfo()
	models, _ := info["models"].([]any)
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// resolveModelName maps a user-supplied model name (value, modelId or
// displayName, case-insensitive) to the canonical catalog value the CLI
// expects. ok=false when the catalog exists and nothing matches; with no
// catalog (older CLIs) the input passes through unchanged.
func resolveModelName(model string, catalog []map[string]any) (string, bool) {
	if len(catalog) == 0 {
		return model, true
	}
	for _, entry := range catalog {
		value, _ := entry["value"].(string)
		for _, key := range []string{"value", "modelId", "displayName"} {
			if v, _ := entry[key].(string); v != "" && strings.EqualFold(v, model) {
				if value == "" {
					return model, true
				}
				return value, true
			}
		}
	}
	return "", false
}

// MCPStatus returns the runtime MCP server status.
func (c *Client) MCPStatus(ctx context.Context) (map[string]any, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "mcp_status"})
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return map[string]any{"mcpServers": []any{}}, nil
	}
	servers, ok := raw["mcpServers"]
	if !ok {
		servers = raw["servers"]
	}
	if servers == nil {
		servers = []any{}
	}
	return map[string]any{"mcpServers": servers}, nil
}

// SetMCPServers replaces the MCP server configuration at runtime and
// re-registers in-process SDK servers locally.
func (c *Client) SetMCPServers(ctx context.Context, servers map[string]MCPServerConfig) (*MCPSetServersResult, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	wire := map[string]any{}
	for name, cfg := range servers {
		wire[name] = cfg.wireConfig()
	}
	// Mirror the Python SDK: refresh the local SDK server registry before
	// the CLI learns the new set.
	c.sess.sdkMCP.Range(func(name, _ any) bool {
		c.sess.sdkMCP.Delete(name)
		return true
	})
	for name, cfg := range servers {
		if sdk, ok := cfg.(*SDKServerConfig); ok && sdk.server != nil {
			c.sess.sdkMCP.Store(name, sdk)
		}
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "mcp_set_servers", "servers": wire})
	if err != nil {
		return nil, err
	}
	res := &MCPSetServersResult{}
	if raw != nil {
		res.Added = stringList(raw["added"])
		res.Removed = stringList(raw["removed"])
		if errs, ok := raw["errors"].(map[string]any); ok {
			res.Errors = errs
		}
	}
	return res, nil
}

// MCPSetServersResult reports the diff applied by SetMCPServers.
type MCPSetServersResult struct {
	Added   []string
	Removed []string
	Errors  map[string]any
}

// ReconnectMCPServer reconnects one disconnected or failed MCP server.
func (c *Client) ReconnectMCPServer(ctx context.Context, name string) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "mcp_reconnect", "serverName": name})
	return err
}

// ToggleMCPServer enables or disables one MCP server.
func (c *Client) ToggleMCPServer(ctx context.Context, name string, enabled bool) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{
		"subtype": "mcp_toggle", "serverName": name, "enabled": enabled,
	})
	return err
}

// AvailableModels returns the account's available model catalog.
func (c *Client) AvailableModels(ctx context.Context) ([]ModelInfo, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "get_models"})
	if err != nil {
		return nil, err
	}
	list, _ := raw["models"].([]any)
	out := make([]ModelInfo, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, modelInfoFromWire(m))
		}
	}
	return out, nil
}

// ContextUsage returns a live context snapshot. Useful keys:
// contextWindow.usedPercentage and skills.items[].percentageOfContext.
func (c *Client) ContextUsage(ctx context.Context) (map[string]any, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	return c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "get_context_usage"})
}

// UsageInfo is the account quota payload returned by UsageInfo.
type UsageInfo struct {
	// UserType identifies the plan (e.g. "free", "pro").
	UserType string
	// TotalUsagePercentage is the overall quota consumption.
	TotalUsagePercentage float64
	// UserQuota holds plan quota fields (remaining/total/unit).
	UserQuota map[string]any
	// Session holds session credit fields when provided.
	Session map[string]any
	// Raw preserves the untouched CLI payload.
	Raw map[string]any
}

// UsageInfo fetches live account usage; nil means the CLI did not provide
// it (not logged in or an older CLI), matching the Python degradation.
func (c *Client) UsageInfo(ctx context.Context) (*UsageInfo, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "get_usage_info"})
	if err != nil {
		return nil, nil //nolint:nilerr // compat: old CLIs reject unknown subtypes
	}
	info := usageInfoFromWire(raw)
	if info == nil {
		return nil, nil
	}
	return info, nil
}

func usageInfoFromWire(raw map[string]any) *UsageInfo {
	if raw == nil {
		return nil
	}
	info := &UsageInfo{Raw: raw}
	if usage, ok := raw["usage"].(map[string]any); ok {
		info.UserType = str(usage, "userType")
		info.TotalUsagePercentage = numberVal(usage["totalUsagePercentage"])
		info.UserQuota, _ = usage["userQuota"].(map[string]any)
	}
	info.Session, _ = raw["session"].(map[string]any)
	if info.UserType == "" && info.TotalUsagePercentage == 0 && info.UserQuota == nil && info.Session == nil {
		return nil
	}
	return info
}

// SideQuestion asks an off-path question without interrupting the main
// turn. history supplies prior Q/A pairs as local context. A nil result
// means the CLI produced no answer.
func (c *Client) SideQuestion(ctx context.Context, question string, history []SideQuestionHistoryEntry) (*SideQuestionResult, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	request := map[string]any{"subtype": "side_question", "question": question}
	if len(history) > 0 {
		entries := make([]any, 0, len(history))
		for _, h := range history {
			entry := map[string]any{"question": h.Question, "response": h.Response}
			if h.FallbackNotice != "" {
				entry["fallback_notice"] = h.FallbackNotice
			}
			entries = append(entries, entry)
		}
		request["history"] = entries
	}
	// timeout=0: side questions wait indefinitely, like the Python SDK.
	raw, err := c.sess.sendControlRequest(ctx, request, 0)
	if err != nil {
		return nil, err
	}
	return sideQuestionResultFromWire(raw), nil
}

// CancelAsyncMessage cancels one queued message by UUID.
func (c *Client) CancelAsyncMessage(ctx context.Context, messageUUID string) (bool, error) {
	if _, err := c.connectedSession(); err != nil {
		return false, err
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, map[string]any{
		"subtype":      "cancel_async_message",
		"message_uuid": messageUUID,
	})
	if err != nil {
		return false, err
	}
	cancelled, _ := raw["cancelled"].(bool)
	return cancelled, nil
}

// ListPlugins lists plugins loaded in the current session.
func (c *Client) ListPlugins(ctx context.Context) ([]map[string]any, error) {
	if _, err := c.connectedSession(); err != nil {
		return nil, err
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "list_plugins"})
	if err != nil {
		return nil, err
	}
	list, _ := raw["plugins"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// StopTask stops exactly one background task without aborting the main
// turn.
func (c *Client) StopTask(ctx context.Context, taskID string) error {
	if _, err := c.connectedSession(); err != nil {
		return err
	}
	_, err := c.sess.sendControlRequestDefault(ctx, map[string]any{"subtype": "stop_task", "task_id": taskID})
	return err
}

// BackgroundTasks moves one (toolUseID) or all eligible foreground
// executions into the background. Reports whether anything was moved.
func (c *Client) BackgroundTasks(ctx context.Context, toolUseID string) (bool, error) {
	if _, err := c.connectedSession(); err != nil {
		return false, err
	}
	request := map[string]any{"subtype": "background_tasks"}
	if toolUseID != "" {
		request["tool_use_id"] = toolUseID
	}
	raw, err := c.sess.sendControlRequestDefault(ctx, request)
	if err != nil {
		return false, err
	}
	if raw == nil {
		return true, nil
	}
	backgrounded, ok := raw["backgrounded"].(bool)
	if !ok {
		return true, nil
	}
	return backgrounded, nil
}

// Disconnect closes the CLI process and releases resources. It is safe to
// call multiple times.
func (c *Client) Disconnect(_ context.Context) error {
	if c.sess == nil {
		return nil
	}
	err := c.sess.shutdown()
	c.sess.finish(nil)
	c.sess = nil
	return err
}

// writeUserMessage copies and writes one outbound user message.
func (s *session) writeUserMessage(out map[string]any) error {
	b, err := json.Marshal(out)
	if err != nil {
		return wrapError(ErrKindMessageParse, err, "marshal user message")
	}
	return s.sendFrame(b)
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		} else {
			out = append(out, fmt.Sprintf("%v", item))
		}
	}
	return out
}
