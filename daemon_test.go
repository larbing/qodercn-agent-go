package qoder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the fake `qoderclicn --sdk` daemon used by the daemon
// transport tests. The child process is the test binary itself: the fake
// branch runs before m.Run(), so the CLI args passed by the SDK (--sdk,
// --print, ...) are never flag-parsed.
func TestMain(m *testing.M) {
	if os.Getenv("QODER_FAKE_DAEMON") == "1" {
		runFakeDaemon()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeDaemon dispatches between the --sdk daemon protocol and the legacy
// worker protocol (direct transport), matching how the real CLI tells the
// two apart (QODER_AGENT_SDK_DAEMON=1 is set by newDaemonSession only).
func runFakeDaemon() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 4<<20)
	out := bufio.NewWriter(os.Stdout)
	if os.Getenv("QODER_AGENT_SDK_DAEMON") == "1" {
		runFakeDaemonProtocol(in, out)
		return
	}
	runFakeWorkerProtocol(in, out)
}

// fakeDaemonCaps varies the negotiated capability list per script.
func fakeDaemonCaps(script string) []any {
	if script == "silent-no-keepalive" {
		return []any{daemonCapMultiSession}
	}
	return []any{daemonCapMultiSession, daemonCapKeepAlive}
}

// runFakeDaemonProtocol speaks the v2 daemon protocol. The script env var
// selects failure modes; the happy path serves initialize, session open /
// close, session-level control requests, and one user round trip.
func runFakeDaemonProtocol(in *bufio.Scanner, out *bufio.Writer) {
	script := os.Getenv("QODER_FAKE_DAEMON_SCRIPT")
	if script == "exit-immediately" {
		os.Exit(3)
	}
	if script == "garbage" {
		out.WriteString("this is not json at all\n")
		out.Flush()
	}
	for in.Scan() {
		var f map[string]any
		if json.Unmarshal(in.Bytes(), &f) != nil {
			continue
		}
		switch f["type"] {
		case "control_request":
			fakeHandleDaemonRequest(script, f, out)
		case "user":
			sid, _ := f["session_id"].(string)
			fakeWrite(out, fakeSystemInit(sid))
			fakeWrite(out, fakeAssistant(sid, "pong"))
			fakeWrite(out, fakeResult(sid, "pong"))
		case "control_response":
			if script == "reverse-auth" {
				resp, _ := f["response"].(map[string]any)
				if rid, _ := resp["request_id"].(string); rid == "daemon_sdk_rev_1" {
					return // reverse answer received: shut down cleanly
				}
			}
			if script == "permission-request" {
				resp, _ := f["response"].(map[string]any)
				if rid, _ := resp["request_id"].(string); rid == "perm_1" {
					// The SDK answered the can_use_tool request; finish the
					// turn so the test can drain the queue. The answer must
					// carry the routing session_id, otherwise these frames
					// are unroutable on the SDK side.
					sid, _ := f["session_id"].(string)
					fakeWrite(out, fakeAssistant(sid, "ok"))
					fakeWrite(out, fakeResult(sid, "ok"))
					return
				}
			}
		}
	}
}

// fakeHandleDaemonRequest answers one daemon-level control_request.
func fakeHandleDaemonRequest(script string, f map[string]any, out *bufio.Writer) {
	req, _ := f["request"].(map[string]any)
	id, _ := f["request_id"].(string)
	rt, _ := req["type"].(string)
	switch rt {
	case "daemon.initialize":
		switch script {
		case "version-mismatch":
			fakeWrite(out, fakeResponse(id, nil, map[string]any{
				"daemon_protocol_version": 1, "capabilities": []any{},
			}))
		case "version-unsupported":
			fakeWrite(out, fakeError(id, "PROTOCOL_VERSION_UNSUPPORTED", "protocol 2 unsupported"))
		case "legacy-reject":
			// A v1 CLI answers with a legacy untyped envelope.
			fakeWrite(out, map[string]any{"response": map[string]any{"request_id": id}})
		default:
			// Reject malformed handshakes so tests implicitly cover the
			// request shape this SDK sends.
			if v, ok := req["daemon_protocol_version"].(float64); !ok || int(v) != daemonProtocolVersion {
				fakeWrite(out, fakeError(id, "BAD_HANDSHAKE", "daemon_protocol_version must be 2"))
				break
			}
			caps, ok := req["capabilities"].([]any)
			if !ok || len(caps) == 0 {
				fakeWrite(out, fakeError(id, "BAD_HANDSHAKE", "capabilities must be a non-empty list"))
				break
			}
			fakeWrite(out, fakeResponse(id, nil, map[string]any{
				"daemon_protocol_version": daemonProtocolVersion,
				"capabilities":            fakeDaemonCaps(script),
			}))
			if script == "reverse-auth" {
				fakeWrite(out, map[string]any{
					"type":       "control_request",
					"request_id": "daemon_sdk_rev_1",
					"request":    map[string]any{"type": "daemon.auth.fetch_job_token", "reason": "initial"},
				})
			}
		}
	case "daemon.open_session":
		outer, _ := f["session_id"].(string)
		switch script {
		case "no-session-echo":
			fakeWrite(out, fakeResponse(id, nil, map[string]any{}))
		case "open-id-mismatch":
			fakeWrite(out, fakeResponse(id, map[string]any{"session_id": "someone-else"}, map[string]any{}))
		default:
			fakeWrite(out, fakeResponse(id, map[string]any{"session_id": outer}, map[string]any{}))
			switch script {
			case "status-event":
				fakeWrite(out, map[string]any{
					"type":   "status_event",
					"status": "daemon.session_updated",
					"data": map[string]any{
						"local_session_id": outer, "state": "failed",
						"error": "boom via status_event",
					},
				})
			case "permission-request":
				// A session-level can_use_tool request the SDK must forward
				// to the session's callback (not answer at the conn layer).
				fakeWrite(out, map[string]any{
					"type":       "control_request",
					"request_id": "perm_1",
					"session_id": outer,
					"request": map[string]any{
						"type": "can_use_tool", "tool_name": "Bash",
						"input": map[string]any{"command": "rm -rf /"},
					},
				})
			case "unknown-session":
				fakeWrite(out, map[string]any{"type": "system", "subtype": "init", "session_id": "ghost"})
			case "session-exit":
				os.Exit(4)
			}
		}
	case "daemon.close_session":
		outer, _ := f["session_id"].(string)
		fakeWrite(out, fakeResponse(id, map[string]any{"session_id": outer}, map[string]any{}))
	default:
		// Session-level control requests (initialize etc.): the daemon
		// stamps responses with the routing session id so the SDK can
		// route them back to the session's pending table.
		outer, _ := f["session_id"].(string)
		fakeWrite(out, fakeResponse(id, map[string]any{"session_id": outer}, map[string]any{"capabilities": []any{}}))
	}
}

// runFakeWorkerProtocol speaks the legacy direct-transport protocol: the
// initialize control request gets a success response, every user frame is
// answered with system init + assistant + result.
func runFakeWorkerProtocol(in *bufio.Scanner, out *bufio.Writer) {
	for in.Scan() {
		var f map[string]any
		if json.Unmarshal(in.Bytes(), &f) != nil {
			continue
		}
		switch f["type"] {
		case "control_request":
			id, _ := f["request_id"].(string)
			fakeWrite(out, fakeResponse(id, nil, map[string]any{"capabilities": []any{}}))
		case "user":
			sid, _ := f["session_id"].(string)
			if sid == "" {
				sid = defaultSessionID
			}
			fakeWrite(out, fakeSystemInit(sid))
			fakeWrite(out, fakeAssistant(sid, "pong"))
			fakeWrite(out, fakeResult(sid, "pong"))
		}
	}
}

func fakeWrite(out *bufio.Writer, v any) {
	b, _ := json.Marshal(v)
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

// fakeResponse builds a success control_response; outer fields (notably
// session_id) are stamped on the envelope level.
func fakeResponse(requestID string, outer map[string]any, payload map[string]any) map[string]any {
	resp := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   payload,
		},
	}
	for k, v := range outer {
		resp[k] = v
	}
	return resp
}

func fakeError(requestID, code, msg string) map[string]any {
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": requestID,
			"code":       code,
			"error":      msg,
		},
	}
}

func fakeSystemInit(sid string) map[string]any {
	return map[string]any{
		"type": "system", "subtype": "init", "session_id": sid,
		"data": map[string]any{
			"capabilities": []any{}, "tools": []any{}, "model": "fake-model",
		},
	}
}

func fakeAssistant(sid, text string) map[string]any {
	return map[string]any{
		"type": "assistant", "session_id": sid,
		"message": map[string]any{
			"id": "msg_fake_1", "type": "message", "role": "assistant",
			"model":       "fake-model",
			"content":     []any{map[string]any{"type": "text", "text": text}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	}
}

func fakeResult(sid, text string) map[string]any {
	return map[string]any{
		"type": "result", "subtype": "success", "session_id": sid,
		"is_error": false, "result": text, "num_turns": 1,
		"duration_ms": 1, "duration_api_ms": 1,
		"usage": map[string]any{}, "permission_denials": []any{},
		"uuid": "res-fake-1",
	}
}

// fakeDaemonOptions returns options pointing the transport at this test
// binary running as the fake daemon.
func fakeDaemonOptions(t *testing.T, script string, mode TransportMode) *Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QODER_FAKE_DAEMON", "1")
	t.Setenv("QODER_FAKE_DAEMON_SCRIPT", script)
	return &Options{
		Auth:             AccessToken("test-token"),
		CLIPath:          exe,
		SkipVersionCheck: true,
		TransportMode:    mode,
	}
}

// startTestDaemonConn spawns the fake daemon directly for conn-layer tests.
func startTestDaemonConn(t *testing.T, script string) *daemonConn {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QODER_FAKE_DAEMON", "1")
	t.Setenv("QODER_FAKE_DAEMON_SCRIPT", script)
	ctx, cancel := context.WithCancel(context.Background())
	env := append(os.Environ(), "QODER_AGENT_SDK_DAEMON=1")
	conn, err := startDaemonConn(ctx, exe, env)
	if err != nil {
		cancel()
		t.Fatalf("start daemon conn: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		conn.stop()
	})
	return conn
}

// readPipeLine reads one NDJSON line from r with a watchdog.
func readPipeLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		br := bufio.NewReader(r)
		line, err := br.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.line == "" && res.err != nil {
			t.Fatalf("read pipe: %v", res.err)
		}
		return strings.TrimRight(res.line, "\r\n")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading from pipe")
		return ""
	}
}

// --- conn layer ---

func TestDaemonConnInitializeOK(t *testing.T) {
	conn := startTestDaemonConn(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.initialize(ctx, nil, ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	conn.mu.Lock()
	caps := conn.capabilities
	keepAlive := conn.keepAlive
	conn.mu.Unlock()
	var hasMulti, hasKeepAlive bool
	for _, c := range caps {
		switch c {
		case daemonCapMultiSession:
			hasMulti = true
		case daemonCapKeepAlive:
			hasKeepAlive = true
		}
	}
	if !hasMulti || !hasKeepAlive || !keepAlive {
		t.Fatalf("capabilities not negotiated: caps=%v keepAlive=%v", caps, keepAlive)
	}
}

func TestDaemonConnVersionMismatch(t *testing.T) {
	conn := startTestDaemonConn(t, "version-mismatch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := conn.initialize(ctx, nil, "")
	if !errors.Is(err, errDaemonProtocolVersion) {
		t.Fatalf("want protocol version mismatch, got %v", err)
	}
}

func TestDaemonConnVersionUnsupported(t *testing.T) {
	conn := startTestDaemonConn(t, "version-unsupported")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := conn.initialize(ctx, nil, "")
	if !errors.Is(err, errDaemonProtocolVersion) {
		t.Fatalf("want protocol version mismatch, got %v", err)
	}
}

func TestDaemonConnLegacyReject(t *testing.T) {
	// A v1 CLI answers the v2 initialize with an untyped legacy envelope;
	// the read loop must surface it as a version mismatch.
	conn := startTestDaemonConn(t, "legacy-reject")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := conn.initialize(ctx, nil, "")
	if !errors.Is(err, errDaemonProtocolVersion) {
		t.Fatalf("want protocol version mismatch, got %v", err)
	}
}

func TestDaemonConnExitBeforeHandshake(t *testing.T) {
	conn := startTestDaemonConn(t, "exit-immediately")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := conn.initialize(ctx, nil, "")
	if err == nil || !strings.Contains(err.Error(), "exited with code 3") {
		t.Fatalf("want process exit error, got %v", err)
	}
}

func TestDaemonConnGarbageLine(t *testing.T) {
	conn := startTestDaemonConn(t, "garbage")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := conn.initialize(ctx, nil, "")
	if err == nil || !strings.Contains(err.Error(), "invalid daemon frame") {
		t.Fatalf("want JSON decode failure, got %v", err)
	}
}

func TestDaemonOpenSessionMissingEcho(t *testing.T) {
	conn := startTestDaemonConn(t, "no-session-echo")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.initialize(ctx, nil, ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := conn.openSession(ctx, "sess-1", t.TempDir(), []string{"--print"})
	if err == nil || !strings.Contains(err.Error(), "requires a session_id") {
		t.Fatalf("want missing session_id error, got %v", err)
	}
}

func TestDaemonOpenSessionReturnsEchoedID(t *testing.T) {
	// The daemon may hand back its own routing id; it must win over the
	// requested one.
	conn := startTestDaemonConn(t, "open-id-mismatch")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.initialize(ctx, nil, ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	returned, err := conn.openSession(ctx, "sess-1", t.TempDir(), []string{"--print"})
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if returned != "someone-else" {
		t.Fatalf("want echoed session id, got %q", returned)
	}
}

func TestDaemonHeartbeatTimeout(t *testing.T) {
	conn := startTestDaemonConn(t, "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.initialize(ctx, nil, ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	conn.heartbeatTimeout = 300 * time.Millisecond
	deadline := time.Now().Add(5 * time.Second)
	for !conn.dead() {
		if time.Now().After(deadline) {
			t.Fatal("connection did not fail on heartbeat timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	err := conn.failureOr(nil)
	if err == nil || !strings.Contains(err.Error(), "heartbeat timed out") {
		t.Fatalf("want heartbeat timeout, got %v", err)
	}
}

func TestDaemonNoKeepAliveNoWatchdog(t *testing.T) {
	conn := startTestDaemonConn(t, "silent-no-keepalive")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.initialize(ctx, nil, ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	conn.mu.Lock()
	keepAlive := conn.keepAlive
	conn.mu.Unlock()
	if keepAlive {
		t.Fatal("keep_alive must not be negotiated without the capability")
	}
	time.Sleep(400 * time.Millisecond)
	if conn.dead() {
		t.Fatalf("connection failed without keep_alive: %v", conn.failureOr(nil))
	}
}

func TestDaemonReverseAuth(t *testing.T) {
	conn := startTestDaemonConn(t, "reverse-auth")
	called := make(chan string, 1)
	conn.authHandler = func(method, requestID string, _ map[string]any) (map[string]any, error) {
		select {
		case called <- method + "|" + requestID:
		default:
		}
		return map[string]any{"token": "job-token", "expires_at": 1999999999999}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.initialize(ctx, nil, ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	select {
	case got := <-called:
		if want := "daemon.auth.fetch_job_token|daemon_sdk_rev_1"; got != want {
			t.Fatalf("handler called with %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reverse auth handler was not invoked")
	}
}

func TestClassifyReverseRequest(t *testing.T) {
	cases := []struct {
		name  string
		frame map[string]any
		want  string
	}{
		{"auth-request", map[string]any{"type": "control_request", "request_id": "daemon_sdk_1", "request": map[string]any{"type": "daemon.auth.fetch_job_token"}}, "valid"},
		{"auth-with-session", map[string]any{"type": "control_request", "request_id": "daemon_sdk_1", "session_id": "s", "request": map[string]any{"type": "daemon.auth.fetch_job_token"}}, "contradictory"},
		{"reserved-id-wrong-type", map[string]any{"type": "control_request", "request_id": "daemon_sdk_1", "request": map[string]any{"type": "can_use_tool"}}, "contradictory"},
		{"reserved-type-wrong-id", map[string]any{"type": "control_request", "request_id": "req_1", "request": map[string]any{"type": "daemon.auth.fetch_job_token"}}, "contradictory"},
		{"session-can-use-tool", map[string]any{"type": "control_request", "request_id": "req_1", "session_id": "s", "request": map[string]any{"type": "can_use_tool"}}, "none"},
		{"no-request-id", map[string]any{"type": "control_request", "request": map[string]any{"type": "can_use_tool"}}, "none"},
	}
	for _, tc := range cases {
		if got := classifyReverseRequest(tc.frame); got != tc.want {
			t.Errorf("%s: classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDaemonPermissionCallback(t *testing.T) {
	// Regression: a session-level can_use_tool control_request must reach
	// the session's callback and be answered (with the routing session_id),
	// not fail the whole daemon connection and hang the consumer forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := fakeDaemonOptions(t, "permission-request", TransportDaemon)
	var gotTool string
	var gotInput map[string]any
	opts.CanUseTool = func(ctx context.Context, toolName string, input map[string]any, _ *ToolPermissionContext) (PermissionResult, error) {
		gotTool = toolName
		gotInput = input
		return &PermissionResultDeny{Message: "not today"}, nil
	}
	s, err := newSession(ctx, opts)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if _, err := s.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	m1, err := s.queue.pop(ctx)
	if err != nil {
		t.Fatalf("pop assistant: %v", err)
	}
	assistant, ok := m1.(*AssistantMessage)
	if !ok || textOf(assistant) != "ok" {
		t.Fatalf("want assistant ok, got %#v", m1)
	}
	m2, err := s.queue.pop(ctx)
	if err != nil {
		t.Fatalf("pop result: %v", err)
	}
	result, ok := m2.(*ResultMessage)
	if !ok || result.Result != "ok" {
		t.Fatalf("want result ok, got %#v", m2)
	}
	if gotTool != "Bash" || gotInput["command"] != "rm -rf /" {
		t.Fatalf("callback got tool=%q input=%v", gotTool, gotInput)
	}
}

// --- session layer ---

func TestDaemonSessionEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := newSession(ctx, fakeDaemonOptions(t, "ok", TransportDaemon))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if s.conn == nil || s.daemonSessionID == "" || s.inbound == nil {
		t.Fatalf("daemon session not wired: conn=%v id=%q", s.conn, s.daemonSessionID)
	}
	if _, err := s.initialize(ctx); err != nil {
		t.Fatalf("session initialize: %v", err)
	}
	if err := s.writeUserMessage(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": "hi"},
		"parent_tool_use_id": nil,
		"session_id":         defaultSessionID,
		"priority":           "next",
	}); err != nil {
		t.Fatalf("writeUserMessage: %v", err)
	}

	m1, err := s.queue.pop(ctx)
	if err != nil {
		t.Fatalf("pop system: %v", err)
	}
	sys, ok := m1.(*SystemMessage)
	if !ok || sys.Subtype != "init" {
		t.Fatalf("want system init, got %#v", m1)
	}
	m2, err := s.queue.pop(ctx)
	if err != nil {
		t.Fatalf("pop assistant: %v", err)
	}
	assistant, ok := m2.(*AssistantMessage)
	if !ok || textOf(assistant) != "pong" {
		t.Fatalf("want assistant pong, got %#v", m2)
	}
	m3, err := s.queue.pop(ctx)
	if err != nil {
		t.Fatalf("pop result: %v", err)
	}
	result, ok := m3.(*ResultMessage)
	if !ok || result.Result != "pong" {
		t.Fatalf("want result pong, got %#v", m3)
	}
	// Every frame is stamped with the routing session id, so the session
	// state must have converged on it.
	if result.SessionID != s.daemonSessionID || s.currentSessionID() != s.daemonSessionID {
		t.Fatalf("session id mismatch: result=%q state=%q route=%q",
			result.SessionID, s.currentSessionID(), s.daemonSessionID)
	}

	s.finish(nil)
	if _, err := s.queue.pop(ctx); err != io.EOF {
		t.Fatalf("want io.EOF after finish, got %v", err)
	}
	s.conn.mu.Lock()
	retired := s.conn.retired[s.daemonSessionID]
	s.conn.mu.Unlock()
	if !retired {
		t.Fatal("session was not retired after release")
	}
}

func TestDaemonSessionIDStamping(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pw.Close() })
	mockTr := &transport{stdin: pw, done: make(chan struct{}), cmd: &exec.Cmd{}}
	s := &session{opts: &Options{}, closed: make(chan struct{}), queue: newMsgQueue()}
	s.conn = &daemonConn{
		tr:               mockTr,
		pending:          map[string]chan daemonResult{},
		pendingMethod:    map[string]string{},
		sessions:         map[string]*session{},
		retired:          map[string]bool{},
		heartbeatTimeout: time.Hour,
	}
	s.daemonSessionID = "route-9"

	err = s.writeUserMessage(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "x"},
	})
	if err != nil {
		t.Fatalf("writeUserMessage: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(readPipeLine(t, pr)), &frame); err != nil {
		t.Fatalf("frame json: %v", err)
	}
	if frame["session_id"] != "route-9" {
		t.Fatalf("session_id not stamped: %+v", frame)
	}
	uuid, _ := frame["uuid"].(string)
	if uuid == "" {
		t.Fatalf("uuid not added to user frame: %+v", frame)
	}
}

func TestDaemonStatusEventFailsSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := newSession(ctx, fakeDaemonOptions(t, "status-event", TransportDaemon))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	_, err = s.queue.pop(ctx)
	if err == nil || !strings.Contains(err.Error(), "boom via status_event") {
		t.Fatalf("want status_event failure, got %v", err)
	}
}

func TestDaemonUnknownSessionFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := newSession(ctx, fakeDaemonOptions(t, "unknown-session", TransportDaemon))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	_, err = s.queue.pop(ctx)
	if err == nil || !strings.Contains(err.Error(), "unknown session_id") {
		t.Fatalf("want unknown session failure, got %v", err)
	}
}

func TestDaemonSessionExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := newSession(ctx, fakeDaemonOptions(t, "session-exit", TransportDaemon))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	_, err = s.queue.pop(ctx)
	if err == nil || !strings.Contains(err.Error(), "exited with code 4") {
		t.Fatalf("want session exit error, got %v", err)
	}
}

// --- Query layer ---

func TestQueryDaemonTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := Query(ctx, "hi", fakeDaemonOptions(t, "ok", TransportDaemon))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var sawInit, sawAssistant, sawResult bool
	for {
		msg, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch m := msg.(type) {
		case *SystemMessage:
			sawInit = m.Subtype == "init"
		case *AssistantMessage:
			sawAssistant = textOf(m) == "pong"
		case *ResultMessage:
			sawResult = m.Subtype == "success" && m.Result == "pong"
		}
	}
	if !sawInit || !sawAssistant || !sawResult {
		t.Fatalf("missing messages: init=%v assistant=%v result=%v", sawInit, sawAssistant, sawResult)
	}
}

func TestQueryAutoFallsBackToDirect(t *testing.T) {
	// The daemon script exits before the handshake; auto mode must fall
	// back to the direct transport and still complete the query.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := Query(ctx, "hi", fakeDaemonOptions(t, "exit-immediately", TransportAuto))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var sawAssistant, sawResult bool
	for {
		msg, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch m := msg.(type) {
		case *AssistantMessage:
			sawAssistant = textOf(m) == "pong"
		case *ResultMessage:
			sawResult = m.Result == "pong"
		}
	}
	if !sawAssistant || !sawResult {
		t.Fatalf("fallback stream incomplete: assistant=%v result=%v", sawAssistant, sawResult)
	}
}

// --- helpers ---

func TestFrameQueue(t *testing.T) {
	q := newFrameQueue()
	q.push([]byte("a"))
	q.push([]byte("b"))
	q.push([]byte("c"))
	for i, want := range []string{"a", "b", "c"} {
		line, ok := q.pop()
		if !ok || string(line) != want {
			t.Fatalf("item %d: got (%q, %v), want %q", i, line, ok, want)
		}
	}
	q.close()
	if _, ok := q.pop(); ok {
		t.Fatal("closed queue must report false after drain")
	}
	q.push([]byte("late"))
	if _, ok := q.pop(); ok {
		t.Fatal("push after close must be ignored")
	}
}

func TestNewUUID(t *testing.T) {
	a, b := newUUID(), newUUID()
	if a == b {
		t.Fatal("uuids must differ")
	}
	if len(a) != 36 {
		t.Fatalf("bad length: %q", a)
	}
	for _, i := range []int{8, 13, 18, 23} {
		if a[i] != '-' {
			t.Fatalf("missing dash at %d: %q", i, a)
		}
	}
	if a[14] != '4' {
		t.Fatalf("not a v4 uuid: %q", a)
	}
	if a[19] != '8' && a[19] != '9' && a[19] != 'a' && a[19] != 'b' {
		t.Fatalf("bad variant: %q", a)
	}
	for i, c := range a {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("bad hex at %d: %q", i, a)
		}
	}
}
