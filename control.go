package qoder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// defaultSessionID is the inbound wire session id used by one-shot Query.
const defaultSessionID = "default"

// mirrorFlushInterval paces batched SessionStore writes; mirrorBatchLimit
// forces an early flush for bursty streams.
const (
	mirrorFlushInterval = 300 * time.Millisecond
	mirrorBatchLimit    = 64
)

// controlResult carries one outbound control RPC outcome.
type controlResult struct {
	data map[string]any
	err  error
}

// session owns one CLI subprocess and all control protocol state shared by
// Query (single turn) and Client (multi-turn).
type session struct {
	opts    *Options
	tr      *transport
	cliPath string

	// Daemon transport (nil on the direct transport): the shared --sdk
	// connection, this session's routing id, and its inbound frame queue.
	conn            *daemonConn
	daemonSessionID string
	inbound         *frameQueue

	// Outbound control RPC.
	reqSeq  atomic.Uint64
	pending sync.Map // request_id -> chan controlResult

	// Inbound request cancellation (CLI -> SDK).
	inflight sync.Map // request_id -> context.CancelFunc

	// Hook registry: callback id -> callback, plus the prebuilt initialize
	// hooks config in registration order.
	hookMu    sync.Mutex
	hooks     map[string]HookCallback
	nextHook  int
	hooksWire map[HookEvent][]map[string]any

	// In-process SDK MCP servers by config name.
	sdkMCP sync.Map // name -> *SDKServerConfig

	queue *msgQueue

	closeOnce sync.Once
	closed    chan struct{}

	stMu             sync.Mutex
	sessionID        string
	initResult       map[string]any
	initialized      bool
	authExpiredFired bool

	// Session mirror buffering.
	mirrorMu   sync.Mutex
	mirrorBuf  []SessionStoreEntry
	mirrorTick *time.Ticker
}

// newSession spawns the CLI and starts the read loop. Auto mode prefers
// the daemon transport and falls back to the direct one when the daemon
// handshake fails.
func newSession(ctx context.Context, opts *Options) (*session, error) {
	if opts == nil {
		opts = &Options{}
	}
	norm, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	switch norm.TransportMode {
	case TransportDirect:
		return newDirectSession(ctx, norm)
	case TransportDaemon:
		return newDaemonSession(ctx, norm)
	default:
		s, err := newDaemonSession(ctx, norm)
		if err == nil {
			return s, nil
		}
		fmt.Fprintf(os.Stderr, "qoder: daemon transport unavailable (%v); falling back to direct mode\n", err)
		return newDirectSession(ctx, norm)
	}
}

// newDirectSession spawns a plain CLI subprocess (legacy worker protocol).
func newDirectSession(ctx context.Context, norm *Options) (*session, error) {
	cliPath, err := findCLI(norm.CLIPath)
	if err != nil {
		return nil, err
	}
	if !norm.SkipVersionCheck {
		checkVersion(cliPath)
	}
	args, err := norm.buildCLIArgs()
	if err != nil {
		return nil, err
	}
	payload, err := buildAuthPayload(norm.Auth, norm.Env)
	if err != nil {
		return nil, err
	}
	authFile, err := writeAuthPayloadFile(payload)
	if err != nil {
		return nil, err
	}
	norm.authPayloadPath = authFile
	env, err := norm.buildEnv(authFile)
	if err != nil {
		norm.cleanupAuthPayload()
		return nil, err
	}
	tr, err := startTransport(ctx, cliPath, args, env, norm.Cwd, authFile)
	if err != nil {
		norm.cleanupAuthPayload()
		return nil, err
	}

	s := newSessionShell(norm, cliPath, tr)
	go s.readLoop()
	return s, nil
}

// newDaemonSession spawns `qoderclicn --sdk`, performs the daemon
// handshake, and opens one session inside it.
func newDaemonSession(ctx context.Context, norm *Options) (*session, error) {
	if err := norm.validateDaemonOptions(); err != nil {
		return nil, err
	}
	cliPath, err := findCLI(norm.CLIPath)
	if err != nil {
		return nil, err
	}
	if !norm.SkipVersionCheck {
		checkVersion(cliPath)
	}
	args, err := norm.buildCLIArgs()
	if err != nil {
		return nil, err
	}
	payload, err := buildAuthPayload(norm.Auth, norm.Env)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		authFile, ferr := writeAuthPayloadFile(payload)
		if ferr != nil {
			return nil, ferr
		}
		norm.authPayloadPath = authFile
	}
	env, err := norm.buildEnv(norm.authPayloadPath)
	if err != nil {
		norm.cleanupAuthPayload()
		return nil, err
	}
	env = append(env, "QODER_AGENT_SDK_DAEMON=1")

	conn, err := startDaemonConn(ctx, cliPath, env)
	if err != nil {
		norm.cleanupAuthPayload()
		return nil, err
	}
	if err := conn.initialize(ctx, payload, ""); err != nil {
		conn.stop()
		norm.cleanupAuthPayload()
		return nil, err
	}

	s := newSessionShell(norm, cliPath, conn.tr)
	s.conn = conn
	s.daemonSessionID = daemonSessionID(norm)
	s.inbound = newFrameQueue()
	conn.authHandler = s.handleDaemonAuth
	// Register under the requested id before opening: frames may arrive
	// right after the open ACK, and rekeySession runs on the read loop.
	conn.registerSession(s)

	cwd, err := daemonCWD(norm.Cwd)
	if err != nil {
		s.finish(err)
		return nil, err
	}
	returned, err := conn.openSession(ctx, s.daemonSessionID, cwd, args)
	if err != nil {
		s.finish(err)
		return nil, err
	}
	// The daemon may hand back its own routing id; the ACK handler already
	// rekeyed the routing table, this just mirrors it on the session.
	s.daemonSessionID = returned
	go s.readLoop()
	return s, nil
}

// newSessionShell builds the session struct shared by both transports.
func newSessionShell(norm *Options, cliPath string, tr *transport) *session {
	s := &session{
		opts:      norm,
		tr:        tr,
		cliPath:   cliPath,
		hooks:     map[string]HookCallback{},
		queue:     newMsgQueue(),
		closed:    make(chan struct{}),
		sessionID: defaultSessionID,
	}
	for name, cfg := range norm.MCPServers {
		if sdk, ok := cfg.(*SDKServerConfig); ok && sdk.server != nil {
			s.sdkMCP.Store(name, sdk)
		}
	}
	s.registerHooks(norm.Hooks)
	return s
}

// daemonSessionID resolves the routing id: an explicit SessionID, a plain
// resume target, or a fresh UUID.
func daemonSessionID(norm *Options) string {
	if norm.SessionID != "" {
		return norm.SessionID
	}
	if norm.Resume != "" && !norm.ForkSession {
		return norm.Resume
	}
	return newUUID()
}

// daemonCWD physicalizes the session working directory (realpath of the
// absolute path), matching how the CLI derives transcript project keys.
func daemonCWD(cwd string) (string, error) {
	dir := cwd
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", wrapError(ErrKindCLIConnection, err, "resolve working directory")
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", wrapError(ErrKindCLIConnection, err, "resolve working directory")
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

// handleDaemonAuth answers the daemon's connection-level auth callbacks.
func (s *session) handleDaemonAuth(method, requestID string, request map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonReverseAuthTTL)
	defer cancel()
	switch method {
	case "daemon.auth.fetch_service_account_token":
		return s.handleFetchServiceAccountToken()
	case "daemon.auth.fetch_job_token":
		return s.handleFetchJobToken(ctx, requestID, request)
	}
	return nil, fmt.Errorf("unsupported daemon auth request: %s", method)
}

// sendFrame writes one outbound protocol line. Daemon sessions stamp the
// routing session_id and a uuid on user frames; direct sessions write
// verbatim.
func (s *session) sendFrame(b []byte) error {
	if s.conn == nil {
		return s.tr.writeLine(b)
	}
	var frame map[string]any
	if err := json.Unmarshal(b, &frame); err != nil {
		return wrapError(ErrKindMessageParse, err, "decode outbound frame")
	}
	frame["session_id"] = s.daemonSessionID
	if t, _ := frame["type"].(string); t == "user" {
		if u, _ := frame["uuid"].(string); u == "" {
			frame["uuid"] = newUUID()
		}
	}
	out, err := json.Marshal(frame)
	if err != nil {
		return wrapError(ErrKindMessageParse, err, "encode daemon frame")
	}
	return s.conn.tr.writeLine(out)
}

// recvFrame blocks for the next inbound protocol line.
func (s *session) recvFrame() ([]byte, bool) {
	if s.inbound != nil {
		return s.inbound.pop()
	}
	return s.tr.nextLine()
}

// releaseDaemon best-effort closes the daemon session (close_session ACK
// keeps routes open so SessionEnd hooks still arrive) and retires it.
func (s *session) releaseDaemon(skipRPC bool) {
	if s.conn == nil {
		return
	}
	if !skipRPC && !s.conn.dead() {
		ctx, cancel := context.WithTimeout(context.Background(), daemonCloseTimeout)
		_ = s.conn.closeSession(ctx, s.daemonSessionID)
		cancel()
	}
	s.conn.retire(s.daemonSessionID)
}

// shutdown gracefully ends the transport (Client.Disconnect path).
func (s *session) shutdown() error {
	if s.conn != nil {
		s.releaseDaemon(s.conn.dead())
		return s.conn.closeAndWait()
	}
	return s.tr.close()
}

// writeAuthPayloadFile persists the auth payload to a 0600 temp file.
func writeAuthPayloadFile(payload map[string]any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", wrapError(ErrKindMessageParse, err, "marshal auth payload")
	}
	f, err := os.CreateTemp("", "qoder-sdk-auth-*.json")
	if err != nil {
		return "", wrapError(ErrKindCLIConnection, err, "create auth payload file")
	}
	path := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(path)
		return "", wrapError(ErrKindCLIConnection, err, "write auth payload")
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", wrapError(ErrKindCLIConnection, err, "close auth payload")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		os.Remove(path)
		return "", wrapError(ErrKindCLIConnection, err, "chmod auth payload")
	}
	_ = filepath.Clean(path)
	return path, nil
}

// registerHooks assigns stable callback ids and pre-builds the initialize
// hooks config ({event: [{matcher, hookCallbackIds, timeout?}]}) so the two
// stay in sync.
func (s *session) registerHooks(hooks map[HookEvent][]HookMatcher) {
	if len(hooks) == 0 {
		return
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	// Iterate events in sorted order so callback id assignment (and the
	// serialized initialize payload) is deterministic.
	events := make([]HookEvent, 0, len(hooks))
	for event := range hooks {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i] < events[j] })
	wire := map[HookEvent][]map[string]any{}
	for _, event := range events {
		for _, m := range hooks[event] {
			ids := []string{}
			for _, cb := range m.Hooks {
				if cb == nil {
					continue
				}
				id := fmt.Sprintf("hook_%d", s.nextHook)
				s.nextHook++
				s.hooks[id] = cb
				ids = append(ids, id)
			}
			if len(ids) == 0 {
				continue
			}
			entry := map[string]any{"hookCallbackIds": ids}
			if m.Matcher != "" {
				entry["matcher"] = m.Matcher
			}
			if m.Timeout > 0 {
				entry["timeout"] = m.Timeout
			}
			wire[event] = append(wire[event], entry)
		}
	}
	s.hooksWire = wire
}

// sessionID returns the current session id (updated from stream messages).
func (s *session) currentSessionID() string {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.sessionID
}

// capabilities returns the initialize response capability list.
func (s *session) capabilities() []string {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	raw, _ := s.initResult["capabilities"].([]any)
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if cs, ok := c.(string); ok {
			out = append(out, cs)
		}
	}
	return out
}

// hasCapability reports whether the CLI advertised the named capability.
func (s *session) hasCapability(capability string) bool {
	for _, c := range s.capabilities() {
		if c == capability {
			return true
		}
	}
	return false
}

// initializeRequest builds the initialize control request payload.
func (s *session) initializeRequest() map[string]any {
	timeoutMS := s.opts.InitializeTimeoutSec * 1000
	request := map[string]any{
		"type":                "initialize",
		"subtype":             "initialize",
		"modelPolicyProvider": s.opts.ResolveModel != nil,
		"initializeTimeoutMs": timeoutMS,
	}
	if timeoutMS >= 60_000 {
		request["supportsCatalogReadyInitialize"] = true
	}
	s.hookMu.Lock()
	hooksWire := s.hooksWire
	s.hookMu.Unlock()
	if len(hooksWire) > 0 {
		request["hooks"] = hooksWire
	}
	if agents := buildAgentsWire(s.opts.Agents); agents != nil {
		request["agents"] = agents
	}
	if s.opts.Skills != nil {
		request["skills"] = s.opts.Skills
	}
	if s.opts.EnableFileCheckpointing != nil {
		request["enableFileCheckpointing"] = *s.opts.EnableFileCheckpointing
	}
	var sdkNames []string
	s.sdkMCP.Range(func(name, _ any) bool {
		sdkNames = append(sdkNames, name.(string))
		return true
	})
	if len(sdkNames) > 0 {
		request["sdkMcpServers"] = sdkNames
	}
	return request
}

// initialize performs the initialize handshake (idempotent).
func (s *session) initialize(ctx context.Context) (map[string]any, error) {
	s.stMu.Lock()
	if s.initialized {
		res := s.initResult
		s.stMu.Unlock()
		return res, nil
	}
	s.stMu.Unlock()

	timeoutMS := s.opts.InitializeTimeoutSec * 1000
	request := s.initializeRequest()
	res, err := s.sendControlRequest(ctx, request, timeoutMS)
	if err != nil {
		return nil, err
	}
	s.stMu.Lock()
	s.initialized = true
	s.initResult = res
	s.stMu.Unlock()
	return res, nil
}

// sendControlRequest performs one outbound control RPC with the default
// timeout (0 disables the timeout entirely, like the Python SDK).
func (s *session) sendControlRequest(ctx context.Context, request map[string]any, timeoutMS int) (map[string]any, error) {
	seq := s.reqSeq.Add(1)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	requestID := fmt.Sprintf("req_%d_%s", seq, hex.EncodeToString(rnd[:]))

	ch := make(chan controlResult, 1)
	s.pending.Store(requestID, ch)
	defer s.pending.Delete(requestID)

	envelope := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	}
	b, err := json.Marshal(envelope)
	if err != nil {
		return nil, wrapError(ErrKindMessageParse, err, "marshal control request")
	}
	if err := s.sendFrame(b); err != nil {
		return nil, err
	}

	var timerC <-chan time.Time
	if timeoutMS > 0 {
		timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
		defer timer.Stop()
		timerC = timer.C
	}

	select {
	case res := <-ch:
		return res.data, res.err
	case <-timerC:
		s.cancelControlRequest(requestID)
		return nil, newError(ErrKindControl, "control request %s timed out after %dms", request["subtype"], timeoutMS)
	case <-ctx.Done():
		s.cancelControlRequest(requestID)
		return nil, wrapError(ErrKindCLIConnection, ctx.Err(), "control request cancelled")
	case <-s.closed:
		// finish() closes the signal before failPending delivers the concrete
		// cause; give that delivery a brief window so callers see the real
		// error (e.g. CLI stderr) instead of a generic message.
		select {
		case res := <-ch:
			return res.data, res.err
		case <-time.After(100 * time.Millisecond):
		}
		return nil, cliConnectionError("session closed while awaiting control response")
	}
}

// sendControlRequestDefault uses Options.ControlRequestTimeoutMS.
func (s *session) sendControlRequestDefault(ctx context.Context, request map[string]any) (map[string]any, error) {
	return s.sendControlRequest(ctx, request, s.opts.ControlRequestTimeoutMS)
}

func (s *session) cancelControlRequest(requestID string) {
	b, err := json.Marshal(map[string]any{
		"type":       "control_cancel_request",
		"request_id": requestID,
	})
	if err == nil {
		_ = s.sendFrame(b)
	}
}

// readLoop is the single reader goroutine: it classifies control frames,
// dispatches inbound requests, and fans messages out to the queue.
func (s *session) readLoop() {
	var probe struct {
		Type    string `json:"type"`
		Error   string `json:"error"`
		Message struct {
			Error string `json:"error"`
		} `json:"message"`
	}
	for {
		line, ok := s.recvFrame()
		if !ok {
			if s.conn != nil {
				// The connection layer fails all sessions with the concrete
				// cause; reaching here means the queue closed without a
				// recorded failure (e.g. a clean stop).
				s.finish(s.conn.failureOr(nil))
			} else {
				s.finish(s.tr.exitErrorAfterGrace())
			}
			return
		}

		probe.Type = ""
		if err := json.Unmarshal(line, &probe); err != nil {
			s.finish(jsonDecodeError(err, string(line)))
			return
		}

		switch probe.Type {
		case "control_response":
			s.resolveControlResponse(line)
		case "control_request":
			var env struct {
				RequestID string          `json:"request_id"`
				Request   json.RawMessage `json:"request"`
			}
			if err := json.Unmarshal(line, &env); err != nil {
				s.finish(jsonDecodeError(err, string(line)))
				return
			}
			var request map[string]any
			if err := json.Unmarshal(env.Request, &request); err != nil {
				s.finish(jsonDecodeError(err, string(env.Request)))
				return
			}
			go s.handleInbound(env.RequestID, request)
		case "control_cancel_request":
			var env struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(line, &env); err == nil {
				if cancel, loaded := s.inflight.LoadAndDelete(env.RequestID); loaded {
					cancel.(context.CancelFunc)()
				}
			}
		default:
			// Auth expiry detection mirrors the Python SDK: auth_status
			// carrying an error, or an assistant message flagged
			// authentication_failed.
			if (probe.Type == "auth_status" && probe.Error != "") ||
				(probe.Type == "assistant" && probe.Message.Error == "authentication_failed") {
				s.fireAuthExpired()
			}
			msg, err := parseMessage(line)
			if err != nil {
				s.finish(err)
				return
			}
			if msg != nil {
				s.trackMessage(msg)
				s.mirrorMessage(msg)
				if !s.queue.push(msg) {
					s.finish(cliConnectionError(
						"message stream stalled: consumer is not reading fast enough"))
					return
				}
			}
		}
	}
}

// resolveControlResponse completes one pending outbound RPC.
func (s *session) resolveControlResponse(line []byte) {
	var env struct {
		Response struct {
			Subtype   string         `json:"subtype"`
			RequestID string         `json:"request_id"`
			Response  map[string]any `json:"response"`
			Error     string         `json:"error"`
			Code      string         `json:"code"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return // malformed control frame: ignore like the Python SDK
	}
	chAny, ok := s.pending.LoadAndDelete(env.Response.RequestID)
	if !ok {
		return
	}
	ch := chAny.(chan controlResult)
	if env.Response.Subtype == "success" {
		ch <- controlResult{data: env.Response.Response}
		return
	}
	msg := env.Response.Error
	if msg == "" {
		msg = "control request failed"
	}
	if env.Response.Code != "" {
		msg = fmt.Sprintf("%s [%s]", msg, env.Response.Code)
	}
	ch <- controlResult{err: newError(ErrKindControl, "%s", msg)}
}

// handleInbound processes one CLI-initiated control request and writes the
// control_response envelope (success or error).
func (s *session) handleInbound(requestID string, request map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	s.inflight.Store(requestID, cancel)
	defer func() {
		s.inflight.Delete(requestID)
		cancel()
	}()

	subtype, _ := request["subtype"].(string)
	if subtype == "" {
		subtype, _ = request["type"].(string)
	}

	responseData, err := s.dispatchInbound(ctx, subtype, requestID, request)
	if err != nil {
		var qerr *Error
		resp := map[string]any{
			"subtype":    "error",
			"request_id": requestID,
			"error":      err.Error(),
		}
		if errors.As(err, &qerr) && qerr.ExitCode != 0 {
			resp["code"] = qerr.Kind
		}
		s.writeControlResponse(map[string]any{"type": "control_response", "response": resp})
		return
	}
	success := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   responseData,
		},
	}
	s.writeControlResponse(success)
}

func (s *session) writeControlResponse(envelope map[string]any) {
	b, err := json.Marshal(envelope)
	if err != nil {
		return
	}
	_ = s.sendFrame(b)
}

// dispatchInbound routes one inbound request by subtype, mirroring the
// Python _handle_control_request branches.
func (s *session) dispatchInbound(ctx context.Context, subtype, requestID string, request map[string]any) (map[string]any, error) {
	switch subtype {
	case "can_use_tool":
		return s.handleCanUseTool(ctx, request)
	case "hook_callback":
		return s.handleHookCallback(ctx, request)
	case "mcp_message":
		return s.handleMCPMessage(ctx, request)
	case "elicitation", "elicitation_response":
		// No host elicitation callback: cancel like the TS/Python default.
		return map[string]any{"action": "cancel"}, nil
	case "get_model_policy":
		return s.handleGetModelPolicy(ctx, request)
	case "fetch_service_account_token":
		return s.handleFetchServiceAccountToken()
	case "fetch_job_token":
		return s.handleFetchJobToken(ctx, requestID, request)
	default:
		return nil, fmt.Errorf("unsupported control request subtype: %s", subtype)
	}
}

// handleCanUseTool runs the Options.CanUseTool callback.
func (s *session) handleCanUseTool(ctx context.Context, request map[string]any) (map[string]any, error) {
	callback := s.opts.CanUseTool
	if callback == nil {
		return nil, fmt.Errorf("can_use_tool received but no CanUseTool callback is configured")
	}
	toolName := str(request, "tool_name")
	input, _ := request["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	suggestions, _ := request["permission_suggestions"].([]any)

	pctx := &ToolPermissionContext{Suggestions: suggestions}
	pctx.BlockedPath = str(request, "blocked_path")
	pctx.DecisionReason = str(request, "decision_reason")
	pctx.DecisionReasonType = str(request, "decision_reason_type")
	if v, ok := request["classifier_approvable"].(bool); ok {
		pctx.ClassifierApprovable = &v
	}
	pctx.Title = str(request, "title")
	pctx.DisplayName = str(request, "display_name")
	pctx.Description = str(request, "description")
	pctx.ToolUseID = str(request, "tool_use_id")
	pctx.AgentID = str(request, "agent_id")
	if ep, ok := request["exit_plan_mode"].(map[string]any); ok {
		pctx.ExitPlanMode = &ExitPlanModeApprovalDetails{
			PlanPath:    str(ep, "plan_path"),
			PlanContent: str(ep, "plan_content"),
		}
	}

	res, err := callback(ctx, toolName, input, pctx)
	if err != nil {
		return nil, err
	}
	return permissionResultToWire(res, input, pctx.ToolUseID)
}

// handleHookCallback runs one registered hook callback.
func (s *session) handleHookCallback(ctx context.Context, request map[string]any) (map[string]any, error) {
	callbackID := str(request, "callback_id")
	s.hookMu.Lock()
	callback := s.hooks[callbackID]
	s.hookMu.Unlock()
	if callback == nil {
		return nil, fmt.Errorf("no hook callback found for ID: %s", callbackID)
	}
	rawInput, _ := request["input"].(map[string]any)
	out, err := callback(ctx, normalizeHookInputFromCLI(rawInput), str(request, "tool_use_id"))
	if err != nil {
		return nil, err
	}
	return hookOutputToWire(out), nil
}

// handleMCPMessage bridges one JSON-RPC message into the in-process server.
func (s *session) handleMCPMessage(ctx context.Context, request map[string]any) (map[string]any, error) {
	serverName := str(request, "server_name")
	message, _ := request["message"].(map[string]any)
	if serverName == "" || message == nil {
		return nil, fmt.Errorf("missing server_name or message for MCP request")
	}
	cfgAny, ok := s.sdkMCP.Load(serverName)
	if !ok {
		return map[string]any{"mcp_response": jsonrpcError(message["id"], -32601, "Server not found: "+serverName)}, nil
	}
	cfg := cfgAny.(*SDKServerConfig)
	return map[string]any{"mcp_response": cfg.server.handleMessage(ctx, message)}, nil
}

// handleGetModelPolicy runs the ResolveModel provider with its timeout.
func (s *session) handleGetModelPolicy(ctx context.Context, request map[string]any) (map[string]any, error) {
	provider := s.opts.ResolveModel
	if provider == nil {
		// Keep the CLI-resolved model (matches the TypeScript SDK).
		return map[string]any{"type": "get_model_policy"}, nil
	}
	pctx := &ModelPolicyContext{
		Purpose:         ModelPurpose(str(request, "purpose")),
		SessionID:       str(request, "sessionId"),
		TurnIndex:       int(numberVal(request["turnIndex"])),
		AvailableModels: modelsFromWire(request["models"]),
	}
	if v, ok := request["agentId"]; ok && v != nil {
		pctx.AgentID = fmt.Sprintf("%v", v)
		pctx.HasAgentID = true
	}
	if v, ok := request["agentType"]; ok && v != nil {
		pctx.AgentType = fmt.Sprintf("%v", v)
		pctx.HasAgentType = true
	}
	if v, ok := request["resolvedModel"]; ok && v != nil {
		pctx.ResolvedModel = fmt.Sprintf("%v", v)
		pctx.HasResolvedModel = true
	}
	if pctx.Purpose == "" {
		pctx.Purpose = PurposeMain
	}
	res, err := resolveModelWithTimeout(ctx, provider, s.opts.ResolveModelTimeoutMS, pctx)
	if err != nil {
		return nil, err
	}
	return modelPolicyResponseToWire(res), nil
}

// handleFetchServiceAccountToken serves a short-lived SAT from the host.
func (s *session) handleFetchServiceAccountToken() (map[string]any, error) {
	sa, ok := s.opts.Auth.(ServiceAccountAuth)
	if !ok || sa.FetchToken == nil {
		return nil, fmt.Errorf("service_account_token_unavailable: no fetch callback configured")
	}
	res, err := sa.FetchToken()
	if err != nil {
		return nil, err
	}
	return tokenResponse(res.Token, res.ExpiresAt, "service_account_token_unavailable")
}

// handleFetchJobToken serves a first-party Job Token from the host.
func (s *session) handleFetchJobToken(ctx context.Context, requestID string, request map[string]any) (map[string]any, error) {
	jt, ok := s.opts.Auth.(JobTokenAuth)
	if !ok || jt.Fetch == nil {
		return nil, fmt.Errorf("job_token_unavailable: no fetch callback configured")
	}
	reason := str(request, "reason")
	if reason != "initial" && reason != "expiring" && reason != "unauthorized" {
		reason = "initial"
	}
	req := JobTokenRequest{
		RequestID: requestID,
		Reason:    reason,
		SessionID: str(request, "session_id"),
	}
	res, err := jt.Fetch(req, &JobTokenContext{Done: ctx.Done()})
	if err != nil {
		return nil, err
	}
	return tokenResponse(res.Token, res.ExpiresAt, "job_token_unavailable")
}

// tokenResponse validates the host-provided token and renders the payload.
func tokenResponse(token string, expiresAt int64, errPrefix string) (map[string]any, error) {
	if token == "" {
		return nil, fmt.Errorf("%s: host did not provide a token", errPrefix)
	}
	out := map[string]any{"token": token}
	if expiresAt != 0 {
		if expiresAt < 1_000_000_000_000 {
			return nil, fmt.Errorf("%s: expires_at must be a Unix timestamp in milliseconds", errPrefix)
		}
		out["expires_at"] = expiresAt
	}
	return out, nil
}

// trackMessage updates session state from the stream (session id, init).
func (s *session) trackMessage(msg Message) {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	switch m := msg.(type) {
	case *SystemMessage:
		if m.Subtype == "init" && m.SessionID != "" {
			s.sessionID = m.SessionID
		}
	case *ResultMessage:
		if m.SessionID != "" {
			s.sessionID = m.SessionID
		}
	default:
		if bm := baseOf(msg); bm != nil && bm.SessionID != "" {
			s.sessionID = bm.SessionID
		}
	}
}

func baseOf(msg Message) *baseMessage {
	switch m := msg.(type) {
	case *UserMessage:
		return &m.baseMessage
	case *AssistantMessage:
		return &m.baseMessage
	case *StreamEvent:
		return &m.baseMessage
	}
	return nil
}

func (s *session) fireAuthExpired() {
	s.stMu.Lock()
	fired := s.authExpiredFired
	s.authExpiredFired = true
	cb := s.opts.OnAuthExpired
	s.stMu.Unlock()
	if !fired && cb != nil {
		go cb()
	}
}

// mirrorMessage appends conversation messages to the external SessionStore.
func (s *session) mirrorMessage(msg Message) {
	store := s.opts.SessionStore
	if store == nil {
		return
	}
	entry, ok := mirrorEntryOf(msg)
	if !ok {
		return
	}
	s.mirrorMu.Lock()
	s.mirrorBuf = append(s.mirrorBuf, entry)
	flush := s.opts.SessionStoreFlush == FlushEager || len(s.mirrorBuf) >= mirrorBatchLimit
	if !flush && s.opts.SessionStoreFlush == FlushBatched && s.mirrorTick == nil {
		s.mirrorTick = time.NewTicker(mirrorFlushInterval)
		go s.mirrorPump(s.mirrorTick)
	}
	s.mirrorMu.Unlock()
	if flush {
		s.flushMirror()
	}
}

// mirrorPump drains batched writes until the ticker is stopped in finish.
func (s *session) mirrorPump(ticker *time.Ticker) {
	for range ticker.C {
		s.flushMirror()
	}
}

// flushMirror writes buffered entries to the store, emitting a mirror error
// system message on failure.
func (s *session) flushMirror() {
	s.mirrorMu.Lock()
	buf := s.mirrorBuf
	s.mirrorBuf = nil
	s.mirrorMu.Unlock()
	if len(buf) == 0 || s.opts.SessionStore == nil {
		return
	}
	key := SessionKey{ProjectKey: DefaultProjectKey(s.opts.Cwd), SessionID: s.currentSessionID()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.opts.LoadTimeoutMS)*time.Millisecond)
	defer cancel()
	if err := s.opts.SessionStore.Append(ctx, key, buf); err != nil {
		s.queue.push(&SystemMessage{
			Subtype: "sdk_mirror_error",
			Data:    map[string]any{"error": err.Error(), "session_id": key.SessionID},
		})
	}
}

func mirrorEntryOf(msg Message) (SessionStoreEntry, bool) {
	switch m := msg.(type) {
	case *UserMessage:
		return map[string]any{
			"type": "user", "uuid": m.UUID, "session_id": m.SessionID,
			"message": map[string]any{"role": "user", "content": userContentWire(m)},
		}, true
	case *AssistantMessage:
		return map[string]any{
			"type": "assistant", "uuid": m.UUID, "session_id": m.SessionID,
			"message": map[string]any{"role": "assistant", "model": m.Model},
		}, true
	}
	return nil, false
}

func userContentWire(m *UserMessage) any {
	if m.hasContentList {
		return m.ContentBlocks
	}
	return m.Content
}

// finish tears the session down exactly once: stop the process, fail
// pending RPCs, flush the mirror and close the queue.
func (s *session) finish(err error) {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.releaseDaemon(err != nil)
		s.tr.stop()
		s.flushMirror()
		s.mirrorMu.Lock()
		if s.mirrorTick != nil {
			s.mirrorTick.Stop()
			s.mirrorTick = nil
		}
		s.mirrorMu.Unlock()
		s.opts.cleanupAuthPayload()
		s.failPending(err)
		s.queue.close(err)
	})
}

// failPending resolves every outstanding outbound RPC with cause (or a
// generic connection error).
func (s *session) failPending(cause error) {
	if cause == nil {
		cause = cliConnectionError("CLI stream ended")
	}
	s.pending.Range(func(id, chAny any) bool {
		if ch, loaded := s.pending.LoadAndDelete(id); loaded {
			ch.(chan controlResult) <- controlResult{err: cause}
		}
		return true
	})
}

func numberVal(v any) float64 {
	f, _ := v.(float64)
	return f
}

func modelsFromWire(v any) []ModelInfo {
	list, _ := v.([]any)
	out := make([]ModelInfo, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, modelInfoFromWire(m))
		}
	}
	return out
}

// msgQueue is a FIFO of stream messages. Producers are the read loop and
// mirror error emission; consumers iterate Query/Client streams. A positive
// limit makes push report overflow instead of buffering without bound.
// pop returns io.EOF after close(nil) or the terminal error otherwise.
type msgQueue struct {
	mu     sync.Mutex
	items  []Message
	limit  int
	notify chan struct{}
	closed bool
	err    error
}

func newMsgQueue() *msgQueue {
	return &msgQueue{notify: make(chan struct{}, 1)}
}

// push appends one message. It returns false when the bounded queue is
// full (the caller must fail the stream); messages dropped after close are
// ignored.
func (q *msgQueue) push(m Message) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return true
	}
	if q.limit > 0 && len(q.items) >= q.limit {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, m)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

func (q *msgQueue) close(err error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.err = err
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// pop blocks until a message, the terminal error, or ctx cancellation.
func (q *msgQueue) pop(ctx context.Context) (Message, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			m := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return m, nil
		}
		if q.closed {
			err := q.err
			q.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		q.mu.Unlock()

		select {
		case <-q.notify:
		case <-ctx.Done():
			return nil, wrapError(ErrKindCLIConnection, ctx.Err(), "message stream cancelled")
		}
	}
}
