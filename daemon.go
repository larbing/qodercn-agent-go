package qoder

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// daemonProtocolVersion is the --sdk daemon protocol this SDK speaks.
const daemonProtocolVersion = 2

// Daemon budgets, mirroring the Python SDK's DaemonConnection defaults.
const (
	daemonInitTimeout    = 30 * time.Second
	daemonOpenTimeout    = 120 * time.Second
	daemonCloseTimeout   = 30 * time.Second
	daemonHeartbeat      = 30 * time.Second
	daemonHeartbeatCheck = 5 * time.Second
	daemonReverseAuthTTL = 30 * time.Second
)

// daemonSDKCapabilities advertises what this SDK supports in the handshake.
var daemonSDKCapabilities = []string{
	"multi_session_v2",
	"session_delete_v1",
	"daemon_keep_alive_v1",
	"daemon_async_session_teardown_v1",
}

const (
	daemonCapMultiSession = "multi_session_v2"
	daemonCapKeepAlive    = "daemon_keep_alive_v1"
)

// errDaemonProtocolVersion marks a handshake rejected for a protocol
// mismatch; auto mode falls back to direct transport on it.
var errDaemonProtocolVersion = errors.New("daemon protocol version mismatch")

// daemonResult is one resolved daemon RPC.
type daemonResult struct {
	data map[string]any
	err  error
}

// daemonConn is the protocol connection to one `qoderclicn --sdk` child: a
// single stdout reader, serialized writes, session routing by session_id, a
// heartbeat watchdog, and reverse auth callbacks. It reuses the plain
// subprocess transport for pipes, stderr capture, and process lifetime.
type daemonConn struct {
	tr *transport

	mu             sync.Mutex
	seq            int
	pending        map[string]chan daemonResult
	pendingMethod  map[string]string
	pendingSession map[string]string // request_id -> requested session id
	sessions       map[string]*session
	retired        map[string]bool
	authHandler    func(method, requestID string, request map[string]any) (map[string]any, error)
	failure        error
	capabilities   []string
	keepAlive      bool

	stopping         atomic.Bool
	lastInbound      atomic.Int64
	heartbeatTimeout time.Duration // daemonHeartbeat; overridable in tests
}

// startDaemonConn spawns `qoderclicn --sdk` and starts its read loop.
func startDaemonConn(ctx context.Context, cliPath string, env []string) (*daemonConn, error) {
	tr, err := startTransport(ctx, cliPath, []string{"--sdk"}, env, "", "")
	if err != nil {
		return nil, err
	}
	c := &daemonConn{
		tr:               tr,
		pending:          map[string]chan daemonResult{},
		pendingMethod:    map[string]string{},
		pendingSession:   map[string]string{},
		sessions:         map[string]*session{},
		retired:          map[string]bool{},
		heartbeatTimeout: daemonHeartbeat,
	}
	c.lastInbound.Store(time.Now().UnixNano())
	go c.readLoop()
	return c, nil
}

// initialize performs the daemon handshake and negotiates capabilities.
func (c *daemonConn) initialize(ctx context.Context, authPayload map[string]any, proxy string) error {
	params := map[string]any{
		"daemon_protocol_version": daemonProtocolVersion,
		"sdk_version":             SDKVersion,
		"capabilities":            daemonSDKCapabilities,
	}
	if authPayload != nil {
		params["auth"] = authPayload
	}
	if proxy != "" {
		params["proxy"] = proxy
	}
	res, err := c.request(ctx, "initialize", params, "", daemonInitTimeout)
	if err != nil {
		return err
	}
	if version, _ := res["daemon_protocol_version"].(float64); int(version) != daemonProtocolVersion {
		return fmt.Errorf("%w: daemon reported %v", errDaemonProtocolVersion, res["daemon_protocol_version"])
	}
	rawCaps, ok := res["capabilities"].([]any)
	if !ok {
		return newError(ErrKindMessageParse, "daemon capabilities must be a list")
	}
	caps := make([]string, 0, len(rawCaps))
	for _, rc := range rawCaps {
		cs, ok := rc.(string)
		if !ok {
			return newError(ErrKindMessageParse, "daemon capabilities must be strings")
		}
		caps = append(caps, cs)
	}
	hasMulti := false
	hasKeepAlive := false
	for _, cs := range caps {
		switch cs {
		case daemonCapMultiSession:
			hasMulti = true
		case daemonCapKeepAlive:
			hasKeepAlive = true
		}
	}
	if !hasMulti {
		return newError(ErrKindUnsupportedCapability, "daemon lacks %s capability", daemonCapMultiSession)
	}
	c.mu.Lock()
	c.capabilities = caps
	c.keepAlive = hasKeepAlive
	c.mu.Unlock()
	if hasKeepAlive {
		go c.watchHeartbeat()
	}
	return nil
}

// request performs one daemon RPC (`control_request` with a
// daemon.<method> request type). An empty sessionID sends a connection-level
// request; otherwise the response must echo the outer session_id.
func (c *daemonConn) request(ctx context.Context, method string, params map[string]any, sessionID string, timeout time.Duration) (map[string]any, error) {
	c.mu.Lock()
	if c.failure != nil {
		err := c.failure
		c.mu.Unlock()
		return nil, err
	}
	c.seq++
	id := fmt.Sprintf("sdk_daemon_%d", c.seq)
	ch := make(chan daemonResult, 1)
	c.pending[id] = ch
	c.pendingMethod[id] = method
	c.pendingSession[id] = sessionID
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.pendingMethod, id)
		delete(c.pendingSession, id)
		c.mu.Unlock()
	}()

	req := map[string]any{"type": "daemon." + method}
	for k, v := range params {
		req[k] = v
	}
	envelope := map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    req,
	}
	if sessionID != "" {
		envelope["session_id"] = sessionID
	}
	b, err := json.Marshal(envelope)
	if err != nil {
		return nil, wrapError(ErrKindMessageParse, err, "marshal daemon %s request", method)
	}
	if err := c.tr.writeLine(b); err != nil {
		return nil, err
	}

	var timerC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}
	select {
	case res := <-ch:
		return res.data, res.err
	case <-timerC:
		return nil, newError(ErrKindControl, "daemon %s request timed out", method)
	case <-ctx.Done():
		return nil, wrapError(ErrKindCLIConnection, ctx.Err(), "daemon %s request cancelled", method)
	}
}

// resolveResponse completes one pending daemon RPC.
func (c *daemonConn) resolveResponse(frame map[string]any) error {
	response, _ := frame["response"].(map[string]any)
	if response == nil {
		return newError(ErrKindMessageParse, "malformed daemon control response")
	}
	id, _ := response["request_id"].(string)
	c.mu.Lock()
	ch := c.pending[id]
	method := c.pendingMethod[id]
	c.mu.Unlock()
	if ch == nil {
		return nil // unknown or expired request: drop
	}
	outerSID, _ := frame["session_id"].(string)
	if subtype, _ := response["subtype"].(string); subtype == "error" {
		code, _ := response["code"].(string)
		msg, _ := response["error"].(string)
		if msg == "" {
			msg = "daemon request failed"
		}
		if method == "initialize" && code == "PROTOCOL_VERSION_UNSUPPORTED" {
			ch <- daemonResult{err: errDaemonProtocolVersion}
			return nil
		}
		if code != "" {
			msg = fmt.Sprintf("%s [%s]", msg, code)
		}
		ch <- daemonResult{err: newError(ErrKindControl, "%s", msg)}
		return nil
	}
	result, _ := response["response"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	if method == "open_session" {
		if outerSID == "" {
			ch <- daemonResult{err: newError(ErrKindMessageParse, "open_session response requires a session_id")}
			return nil
		}
		result["session_id"] = outerSID
		c.rekeySession(id, outerSID)
	}
	ch <- daemonResult{data: result}
	return nil
}

// rekeySession moves an already-registered session from its requested
// routing id to the id echoed on the open_session response. It runs on the
// read loop, so every later frame sees the updated table.
func (c *daemonConn) rekeySession(requestID, returnedID string) {
	c.mu.Lock()
	requested := c.pendingSession[requestID]
	s := c.sessions[requested]
	if s != nil && requested != "" && requested != returnedID {
		delete(c.sessions, requested)
		s.daemonSessionID = returnedID
		c.sessions[returnedID] = s
	}
	c.mu.Unlock()
}

// routeControlResponse resolves daemon-level RPCs (initialize, session
// open/close) itself and forwards session-level control responses (unknown
// request ids carrying a session_id) to the owning session's pending table.
func (c *daemonConn) routeControlResponse(frame map[string]any, line []byte) error {
	response, _ := frame["response"].(map[string]any)
	id, _ := response["request_id"].(string)
	c.mu.Lock()
	_, daemonLevel := c.pending[id]
	c.mu.Unlock()
	if daemonLevel {
		return c.resolveResponse(frame)
	}
	sid, _ := frame["session_id"].(string)
	if sid == "" {
		return nil // unknown or expired daemon-level response: drop
	}
	c.mu.Lock()
	s := c.sessions[sid]
	retired := c.retired[sid]
	c.mu.Unlock()
	if s == nil {
		if retired {
			return nil
		}
		return newError(ErrKindMessageParse, "daemon output has unknown session_id: %s", sid)
	}
	s.resolveControlResponse(line)
	return nil
}

// reverseRequestTypes are the connection-level control requests a daemon
// may send to the SDK (reverse auth); everything else arriving as a
// control_request belongs to a session's own inbound dispatch.
var reverseRequestTypes = map[string]bool{
	"daemon.auth.fetch_job_token":             true,
	"daemon.auth.fetch_service_account_token": true,
}

// classifyReverseRequest mirrors the Python SDK's _classify_reverse_request:
// "valid" marks a connection-level reverse-auth request (reserved type +
// reserved daemon_sdk_ request id + no session_id); "contradictory" marks a
// frame that reserves the daemon namespace but violates its routing rules;
// everything else ("none") is a session-level request (can_use_tool,
// hook_callback, ...) that must be forwarded to its session.
func classifyReverseRequest(frame map[string]any) string {
	request, _ := frame["request"].(map[string]any)
	reqType, _ := request["type"].(string)
	requestID, _ := frame["request_id"].(string)
	_, hasSession := frame["session_id"]
	reservedType := reverseRequestTypes[reqType]
	reservedID := strings.HasPrefix(requestID, "daemon_sdk_")
	if reservedType && reservedID && !hasSession {
		return "valid"
	}
	if reservedType || reservedID {
		return "contradictory"
	}
	return "none"
}

// routeReverse answers the daemon's connection-level control requests.
// Only the two auth token callbacks exist today.
func (c *daemonConn) routeReverse(frame map[string]any) error {
	requestID, _ := frame["request_id"].(string)
	request, _ := frame["request"].(map[string]any)
	reqType, _ := request["type"].(string)
	if requestID == "" || request == nil {
		return newError(ErrKindMessageParse, "malformed daemon reverse request")
	}
	switch reqType {
	case "daemon.auth.fetch_job_token", "daemon.auth.fetch_service_account_token":
		c.mu.Lock()
		handler := c.authHandler
		c.mu.Unlock()
		if handler == nil {
			c.writeReverseResponse(requestID, nil, newError(ErrKindAuthNotConfigured, "no auth handler configured"))
			return nil
		}
		go func() {
			c.mu.Lock()
			failed := c.failure
			c.mu.Unlock()
			if failed != nil {
				return
			}
			data, err := handler(reqType, requestID, request)
			c.writeReverseResponse(requestID, data, err)
		}()
		return nil
	}
	return newError(ErrKindMessageParse, "unsupported daemon reverse request: %s", reqType)
}

func (c *daemonConn) writeReverseResponse(requestID string, data map[string]any, err error) {
	resp := map[string]any{"request_id": requestID}
	if err != nil {
		resp["subtype"] = "error"
		resp["error"] = err.Error()
	} else {
		resp["subtype"] = "success"
		resp["response"] = data
	}
	b, merr := json.Marshal(map[string]any{"type": "control_response", "response": resp})
	if merr != nil {
		return
	}
	_ = c.tr.writeLine(b)
}

// routeStatusEvent propagates daemon.session_updated failures to the session.
func (c *daemonConn) routeStatusEvent(frame map[string]any) error {
	status, _ := frame["status"].(string)
	if status != "daemon.session_updated" {
		return nil // unrelated status event: ignore
	}
	data, _ := frame["data"].(map[string]any)
	if data == nil {
		return newError(ErrKindMessageParse, "daemon status_event data must be an object")
	}
	sid, _ := data["local_session_id"].(string)
	c.mu.Lock()
	s := c.sessions[sid]
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	if state, _ := data["state"].(string); state == "failed" {
		msg, _ := data["error"].(string)
		if msg == "" {
			msg = "daemon session failed"
		}
		s.finish(newError(ErrKindCLIConnection, "%s", msg))
	}
	return nil
}

// route dispatches one inbound frame to its consumer.
func (c *daemonConn) route(frame map[string]any, line []byte) error {
	t, _ := frame["type"].(string)
	switch t {
	case "keep_alive":
		return nil
	case "control_response":
		return c.routeControlResponse(frame, line)
	case "control_request":
		switch classifyReverseRequest(frame) {
		case "valid":
			return c.routeReverse(frame)
		case "contradictory":
			return newError(ErrKindMessageParse, "contradictory daemon reverse request routing markers")
		}
		// Session-level control requests (can_use_tool, hook_callback,
		// mcp_message, ...) fall through to the session routing below,
		// mirroring the Python daemon connection.
	case "status_event":
		return c.routeStatusEvent(frame)
	}
	return c.routeToSession(frame, line)
}

// routeToSession hands a frame carrying a session_id to its session's
// inbound queue.
func (c *daemonConn) routeToSession(frame map[string]any, line []byte) error {
	sid, _ := frame["session_id"].(string)
	if sid == "" {
		return newError(ErrKindMessageParse, "daemon frame has no session_id and unrecognized type %q", frame["type"])
	}
	c.mu.Lock()
	s := c.sessions[sid]
	retired := c.retired[sid]
	c.mu.Unlock()
	if s == nil {
		if retired {
			return nil // frames for a retired session are dropped
		}
		return newError(ErrKindMessageParse, "daemon output has unknown session_id: %s", sid)
	}
	s.inbound.push(line)
	return nil
}

// readLoop is the single reader goroutine for the daemon process.
func (c *daemonConn) readLoop() {
	for {
		line, ok := c.tr.nextLine()
		if !ok {
			// Streaming EOF: wait briefly for the exit code so failures
			// carry the concrete cause, like the direct transport.
			c.fail(c.tr.exitErrorAfterGrace())
			return
		}
		c.lastInbound.Store(time.Now().UnixNano())
		var frame map[string]any
		if err := json.Unmarshal(line, &frame); err != nil {
			c.fail(wrapError(ErrKindJSONDecode, err, "invalid daemon frame: %s", truncate(string(line), 200)))
			return
		}
		if frame == nil {
			c.fail(wrapError(ErrKindJSONDecode, nil, "invalid daemon frame: not a JSON object"))
			return
		}
		if _, typed := frame["type"]; !typed && c.isLegacyInitializeRejection(frame) {
			// A v1 CLI rejects the v2 initialize with a legacy untyped
			// envelope; surface it as a protocol version mismatch.
			c.fail(errDaemonProtocolVersion)
			return
		}
		if err := c.route(frame, line); err != nil {
			c.fail(err)
			return
		}
	}
}

// isLegacyInitializeRejection reports whether the untyped frame looks like
// a v1 CLI's rejection of our v2 initialize request.
func (c *daemonConn) isLegacyInitializeRejection(frame map[string]any) bool {
	response, _ := frame["response"].(map[string]any)
	if response == nil {
		return false
	}
	id, _ := response["request_id"].(string)
	if id == "" {
		return false
	}
	c.mu.Lock()
	method := c.pendingMethod[id]
	c.mu.Unlock()
	return method == "initialize"
}

// watchHeartbeat fails the connection when no inbound frame arrives within
// the negotiated heartbeat window.
func (c *daemonConn) watchHeartbeat() {
	for {
		time.Sleep(daemonHeartbeatCheck)
		if c.dead() {
			return
		}
		elapsed := time.Since(time.Unix(0, c.lastInbound.Load()))
		if elapsed > c.heartbeatTimeout {
			c.fail(newError(ErrKindCLIConnection,
				"daemon heartbeat timed out after %dms (no inbound frame)", elapsed.Milliseconds()))
			return
		}
	}
}

// openSession opens one session inside the daemon and returns the session
// id echoed on the response envelope (the daemon may hand back its own
// routing id, e.g. after resume; it wins for subsequent frames).
func (c *daemonConn) openSession(ctx context.Context, sessionID, cwd string, workerArgs []string) (string, error) {
	params := map[string]any{"cwd": cwd, "worker_args": workerArgs}
	res, err := c.request(ctx, "open_session", params, sessionID, daemonOpenTimeout)
	if err != nil {
		return "", err
	}
	returned, _ := res["session_id"].(string)
	if returned == "" {
		return "", cliConnectionError("daemon returned an unexpected session ID")
	}
	return returned, nil
}

// closeSession closes one session and waits for its ACK.
func (c *daemonConn) closeSession(ctx context.Context, sessionID string) error {
	_, err := c.request(ctx, "close_session", map[string]any{}, sessionID, daemonCloseTimeout)
	return err
}

// registerSession wires a session into the routing table.
func (c *daemonConn) registerSession(s *session) {
	c.mu.Lock()
	c.sessions[s.daemonSessionID] = s
	c.mu.Unlock()
}

// retire drops the session from routing; late frames are dropped, not
// errors.
func (c *daemonConn) retire(sessionID string) {
	c.mu.Lock()
	delete(c.sessions, sessionID)
	c.retired[sessionID] = true
	c.mu.Unlock()
}

// dead reports whether the connection has failed or is being stopped.
func (c *daemonConn) dead() bool {
	c.mu.Lock()
	failed := c.failure != nil
	c.mu.Unlock()
	return failed || c.stopping.Load()
}

// fail records the terminal failure once and fails every pending RPC and
// session with it.
func (c *daemonConn) fail(cause error) {
	if cause == nil {
		if c.stopping.Load() {
			return // stopped by us: sessions are already finishing
		}
		cause = cliConnectionError("daemon connection closed")
	}
	c.mu.Lock()
	if c.failure != nil {
		c.mu.Unlock()
		return
	}
	c.failure = cause
	sessions := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	for id, ch := range c.pending {
		delete(c.pending, id)
		delete(c.pendingMethod, id)
		ch <- daemonResult{err: cause}
	}
	c.mu.Unlock()
	for _, s := range sessions {
		s.finish(cause)
	}
}

// stop kills the daemon process (idempotent, fast).
func (c *daemonConn) stop() {
	c.stopping.Store(true)
	c.tr.stop()
}

// closeAndWait gracefully shuts the daemon down, returning its exit error.
func (c *daemonConn) closeAndWait() error {
	c.stopping.Store(true)
	return c.tr.close()
}

// failureOr returns the recorded connection failure, or fallback.
func (c *daemonConn) failureOr(fallback error) error {
	c.mu.Lock()
	err := c.failure
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return fallback
}

// frameQueue is an unbounded FIFO of raw frames for one session. The
// daemon's shared reader pushes; the session's read loop pops. Pushes never
// block (a slow consumer must not backpressure the shared pipe), and a
// closed-and-drained queue reports false from pop.
type frameQueue struct {
	mu     sync.Mutex
	items  [][]byte
	notify chan struct{}
	closed bool
}

func newFrameQueue() *frameQueue {
	return &frameQueue{notify: make(chan struct{}, 1)}
}

func (q *frameQueue) push(line []byte) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, line)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *frameQueue) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *frameQueue) pop() ([]byte, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			line := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return line, true
		}
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		q.mu.Unlock()
		<-q.notify
	}
}

// newUUID returns a random RFC 4122 v4 UUID string (crypto/rand based).
func newUUID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// Fall back to a time-derived value; uniqueness matters more than
		// cryptographic quality for command ids.
		return fmt.Sprintf("cmd-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0xbf) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
