package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestMsgQueuePushPop(t *testing.T) {
	q := newMsgQueue()
	q.push(&UserMessage{}) // any Message value works for the queue mechanics
	if _, err := q.pop(context.Background()); err != nil {
		t.Fatalf("pop: %v", err)
	}
	// The queue is empty now: pop must block instead of returning zero
	// values; prove it by popping with a short deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := q.pop(ctx); err == nil {
		t.Fatal("pop on empty queue must block until ctx deadline")
	}
}

func TestMsgQueueCloseSemantics(t *testing.T) {
	q := newMsgQueue()
	q.push(&UserMessage{})
	q.close(nil)
	if _, err := q.pop(context.Background()); err != nil {
		t.Fatalf("drain after close: %v", err)
	}
	if _, err := q.pop(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF, got %v", err)
	}
	// Terminal error wins after close.
	q2 := newMsgQueue()
	q2.close(cliConnectionError("boom"))
	_, err := q2.pop(context.Background())
	if !errorHasKind(err, ErrKindCLIConnection) {
		t.Fatalf("terminal error: %v", err)
	}
}

func TestMsgQueueBoundedOverflow(t *testing.T) {
	q := newMsgQueue()
	q.limit = 1
	if !q.push(&UserMessage{}) {
		t.Fatal("first push must fit")
	}
	if q.push(&UserMessage{}) {
		t.Fatal("overflow must be reported")
	}
}

func TestMsgQueuePopContextCancel(t *testing.T) {
	q := newMsgQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := q.pop(ctx); err == nil {
		t.Fatal("cancelled pop must error")
	}
}

func TestResolveControlResponseSuccessAndError(t *testing.T) {
	s := &session{}
	ch := make(chan controlResult, 1)
	s.pending.Store("req_1", ch)

	s.resolveControlResponse([]byte(`{"type":"control_response","response":{
		"subtype":"success","request_id":"req_1","response":{"ok":true}}}`))
	res := <-ch
	if res.err != nil || res.data["ok"] != true {
		t.Fatalf("success: %+v", res)
	}

	ch2 := make(chan controlResult, 1)
	s.pending.Store("req_2", ch2)
	s.resolveControlResponse([]byte(`{"type":"control_response","response":{
		"subtype":"error","request_id":"req_2","error":"bad thing","code":"kaput"}}`))
	res2 := <-ch2
	if res2.err == nil || !errorHasKind(res2.err, ErrKindControl) {
		t.Fatalf("error: %+v", res2.err)
	}
	if !contains(res2.err.Error(), "bad thing [kaput]") {
		t.Fatalf("error message: %v", res2.err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestRegisterHooksBuildsInitializeWire(t *testing.T) {
	s := &session{hooks: map[string]HookCallback{}}
	cb := func(ctx context.Context, in *HookInput, toolUseID string) (*HookJSONOutput, error) {
		return nil, nil
	}
	s.registerHooks(map[HookEvent][]HookMatcher{
		HookPreToolUse: {
			{Matcher: "Bash.*", Timeout: 30, Hooks: []HookCallback{cb, cb}},
			{Hooks: []HookCallback{cb}},
		},
		HookStop: {{Hooks: []HookCallback{cb}}},
	})
	if s.nextHook != 4 {
		t.Fatalf("registered callbacks: %d", s.nextHook)
	}
	s.hookMu.Lock()
	wire := s.hooksWire
	s.hookMu.Unlock()

	pre := wire[HookPreToolUse]
	if len(pre) != 2 {
		t.Fatalf("PreToolUse matchers: %+v", pre)
	}
	ids := pre[0]["hookCallbackIds"].([]string)
	if len(ids) != 2 || ids[0] != "hook_0" || ids[1] != "hook_1" {
		t.Fatalf("ids: %+v", ids)
	}
	if pre[0]["matcher"] != "Bash.*" || pre[0]["timeout"] != 30 {
		t.Fatalf("matcher config: %+v", pre[0])
	}
	stop := wire[HookStop]
	if len(stop) != 1 || stop[0]["hookCallbackIds"].([]string)[0] != "hook_3" {
		t.Fatalf("stop config: %+v", stop)
	}
	// Callback lookup round-trips.
	s.hookMu.Lock()
	cbGot := s.hooks["hook_2"]
	s.hookMu.Unlock()
	if cbGot == nil {
		t.Fatal("callback registry lookup failed")
	}
}

func TestInitializeRequestShape(t *testing.T) {
	provider := func(ctx context.Context, pctx *ModelPolicyContext) (ModelPolicyResult, error) {
		return ModelPolicyResult{Model: "glm-4.7"}, nil
	}
	ckpt := true
	s := &session{
		hooks: map[string]HookCallback{},
		opts: &Options{
			ResolveModel:            provider,
			InitializeTimeoutSec:    60,
			ControlRequestTimeoutMS: 60000,
			Skills:                  []string{"review"},
			EnableFileCheckpointing: &ckpt,
			Agents: map[string]AgentDefinition{
				"reviewer": {Description: "d", Prompt: "p", DisallowedTools: []string{"Bash"}},
			},
		},
	}
	s.sdkMCP.Store("calc", &SDKServerConfig{Name: "calc"})

	request := s.initializeRequest()
	b, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"type":"initialize"`, `"subtype":"initialize"`,
		`"modelPolicyProvider":true`, `"initializeTimeoutMs":60000`,
		`"supportsCatalogReadyInitialize":true`, `"skills":["review"]`,
		`"enableFileCheckpointing":true`, `"sdkMcpServers":["calc"]`,
		`"agents"`, `"disallowedTools":["Bash"]`,
	} {
		if !contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestBuildAuthPayloadVariants(t *testing.T) {
	if _, err := buildAuthPayload(nil, nil); !AsAuthNotConfigured(err) {
		t.Fatal("nil auth must be AuthNotConfigured")
	}
	payload, err := buildAuthPayload(AccessToken("tok"), nil)
	if err != nil || payload["type"] != "accessToken" || payload["accessToken"] != "tok" {
		t.Fatalf("access token: %+v %v", payload, err)
	}
	payload, _ = buildAuthPayload(QoderCLIAuth(), nil)
	if payload["type"] != "qodercli" {
		t.Fatalf("qodercli: %+v", payload)
	}
	payload, _ = buildAuthPayload(ServiceAccountKey("k"), nil)
	if payload["serviceAccountKey"] != "k" {
		t.Fatalf("sa key: %+v", payload)
	}
	payload, _ = buildAuthPayload(ServiceAccountToken(func() (ServiceAccountTokenResult, error) {
		return ServiceAccountTokenResult{}, nil
	}), nil)
	if payload["serviceAccountTokenProvider"] != "host" {
		t.Fatalf("sa host provider: %+v", payload)
	}
	payload, _ = buildAuthPayload(JobToken(func(req JobTokenRequest, ctx *JobTokenContext) (JobTokenResult, error) {
		return JobTokenResult{}, nil
	}), nil)
	if payload["jobTokenProvider"] != "host" {
		t.Fatalf("job host provider: %+v", payload)
	}
	// Env resolution: option env wins over the process environment.
	payload, err = buildAuthPayload(AccessTokenFromEnv("MY_TEST_PAT"), map[string]string{"MY_TEST_PAT": "env-tok"})
	if err != nil || payload["accessToken"] != "env-tok" {
		t.Fatalf("env token: %+v %v", payload, err)
	}
	if _, err := buildAuthPayload(AccessTokenFromEnv("MY_TEST_PAT_UNSET_XYZ"), nil); err == nil {
		t.Fatal("unset env var must error")
	}
}

func TestErrorKindsUnwrap(t *testing.T) {
	base := processError(41, "stderr text")
	if !IsProcessExitWithCode(base, 41) || IsProcessExitWithCode(base, 1) {
		t.Fatalf("exit code check: %v", base)
	}
	wrapped := wrapError(ErrKindCLIConnection, base, "during startup")
	if !IsProcessExitWithCode(wrapped, 41) {
		t.Fatal("wrapped chain must preserve exit code")
	}
	if !AsCLINotFound(clinotFoundError("x")) {
		t.Fatal("AsCLINotFound")
	}
	if !AsModelPolicyTimeout(newError(ErrKindModelPolicyTimeout, "t")) {
		t.Fatal("AsModelPolicyTimeout")
	}
}

func TestConcurrentPendingResolution(t *testing.T) {
	s := &session{}
	var wg sync.WaitGroup
	// The nanosecond timestamp alone can collide on coarse Windows timers;
	// the loop index keeps each id unique regardless.
	for i := 0; i < 50; i++ {
		id := "req_c_" + strconv.Itoa(i) + "_" + time.Now().Format("150405.000000000")
		ch := make(chan controlResult, 1)
		s.pending.Store(id, ch)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s.resolveControlResponse([]byte(`{"type":"control_response","response":{"subtype":"success","request_id":"` + id + `","response":{}}}`))
			res := <-ch
			if res.err != nil {
				t.Errorf("unexpected error: %v", res.err)
			}
		}(id)
	}
	wg.Wait()
}

// Regression: a one-shot Query must end at the terminal ResultMessage. The
// CLI keeps the stream open afterwards (waiting for further prompts), so
// without the session finish the third Next blocked forever.
func TestQueryStreamEndsAfterResult(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	s := &session{
		opts:   &Options{},
		closed: make(chan struct{}),
		queue:  newMsgQueue(),
		tr:     &transport{stdin: pw, done: make(chan struct{}), cmd: &exec.Cmd{}},
	}
	stream := &MessageStream{sess: s, ownsSession: true}
	s.queue.push(&UserMessage{})
	s.queue.push(&ResultMessage{Subtype: "success"})

	// Buffered message drains first.
	msg, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, ok := msg.(*UserMessage); !ok {
		t.Fatalf("first Next returned %T, want *UserMessage", msg)
	}

	// The terminal result is delivered and finishes the session.
	msg, err = stream.Next(context.Background())
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	res, ok := msg.(*ResultMessage)
	if !ok || res.Subtype != "success" {
		t.Fatalf("second Next returned %T %+v, want success result", msg, res)
	}

	// Queue drained then closed: EOF.
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("third Next: got %v, want io.EOF", err)
	}

	// Close stays safe after EOF.
	if err := stream.Close(); err != nil {
		t.Fatalf("Close after EOF: %v", err)
	}
}
