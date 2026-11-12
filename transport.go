package qoder

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// maxLineBytes bounds one NDJSON message (tool results can be large).
const maxLineBytes = 16 << 20 // 16 MiB

// transport owns the qoderclicn subprocess: stdin/stdout pipes, stderr
// capture, and process lifetime.
type transport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  *limitedBuffer

	wmu    sync.Mutex
	closed atomic.Bool

	done    chan struct{}
	waitErr error
}

// limitedBuffer captures stderr up to a cap for error reporting.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const cap = 16 << 10
	if b.buf.Len() < cap {
		room := cap - b.buf.Len()
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startTransport spawns the CLI. authPayloadPath must already exist; it is
// exported via the environment and removed by the caller on close.
func startTransport(ctx context.Context, cliPath string, args, env []string, cwd, authPayloadPath string) (*transport, error) {
	cmd := exec.Command(cliPath, args...)
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stderr = &limitedBuffer{}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, wrapError(ErrKindCLIConnection, err, "stdin pipe")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, wrapError(ErrKindCLIConnection, err, "stdout pipe")
	}

	// On ctx cancellation the caller invokes stop(); WaitDelay guarantees the
	// process never outlives Wait by more than a moment (also covers Windows,
	// where process groups are unavailable).
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return nil, wrapError(ErrKindCLINotFound, err, "start %s", cliPath)
	}

	t := &transport{
		cmd:     cmd,
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
		stderr:  cmd.Stderr.(*limitedBuffer),
		done:    make(chan struct{}),
	}
	t.scanner.Buffer(make([]byte, 64*1024), maxLineBytes)

	go func() {
		t.waitErr = cmd.Wait()
		close(t.done)
	}()

	// Abort early when the context is already cancelled.
	if ctx.Err() != nil {
		t.stop()
		return nil, wrapError(ErrKindCLIConnection, ctx.Err(), "start aborted")
	}
	return t, nil
}

// writeLine writes one NDJSON line, serialized across goroutines.
func (t *transport) writeLine(b []byte) error {
	debugWire("send", b)
	if t.closed.Load() {
		return cliConnectionError("transport closed")
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if _, err := t.stdin.Write(append(b, '\n')); err != nil {
		return wrapError(ErrKindCLIConnection, err, "write to CLI stdin")
	}
	return nil
}

// nextLine blocks until the next full line or the stream ends.
func (t *transport) nextLine() ([]byte, bool) {
	if t.closed.Load() {
		return nil, false
	}
	if t.scanner.Scan() {
		line := t.scanner.Bytes()
		debugWire("recv", line)
		return line, true
	}
	return nil, false
}

// debugWireEnabled is set by QODER_SDK_DEBUG=1; it dumps every raw wire
// line to stderr for protocol debugging.
var debugWireEnabled = sync.OnceValue(func() bool { return os.Getenv("QODER_SDK_DEBUG") != "" })

func debugWire(dir string, line []byte) {
	if debugWireEnabled() {
		fmt.Fprintf(os.Stderr, "qoder: wire %s %s\n", dir, line)
	}
}

// stop kills the subprocess (idempotent).
func (t *transport) stop() {
	if t.closed.CompareAndSwap(false, true) {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.stdin.Close()
	}
}

// close gracefully ends stdin then waits briefly for exit.
func (t *transport) close() error {
	t.closed.Store(true)
	_ = t.stdin.Close()
	select {
	case <-t.done:
	case <-time.After(3 * time.Second):
		t.stop()
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
		}
	}
	return t.exitError()
}

// exitError converts a non-zero exit into a typed error.
func (t *transport) exitError() error {
	if t.waitErr == nil || errors.Is(t.waitErr, context.Canceled) || isWaitDelayKill(t.waitErr) {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(t.waitErr, &ee) {
		return processError(ee.ExitCode(), t.stderr.String())
	}
	return wrapError(ErrKindProcess, t.waitErr, "CLI wait failed")
}

// exitErrorAfterGrace waits briefly for cmd.Wait to observe the process
// exit before reporting. Stdout EOF usually precedes Wait, so without this
// grace a non-zero exit's code and stderr would surface as a generic
// stream error.
func (t *transport) exitErrorAfterGrace() error {
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
	return t.exitError()
}

func isWaitDelayKill(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == -1 || ee.String() == "signal: killed"
	}
	return false
}

// findCLI locates the qoderclicn executable: explicit path, QODERCLI_PATH,
// then PATH. The Go SDK does not bundle a CLI binary.
func findCLI(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv("QODERCLI_PATH"); v != "" {
		return v, nil
	}
	name := "qoderclicn"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{
		home + "/.npm-global/bin",
		"/usr/local/bin",
		"/usr/bin",
	} {
		if dir == "" {
			continue
		}
		cand := dir + "/" + name
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	return "", clinotFoundError(
		"qoderclicn not found: install it (npm install -g @qodercn-ai/qodercn-cli), " +
			"set QODERCLI_PATH, or set Options.CLIPath")
}

// minimumRuntimeVersion mirrors the Python SDK floor.
const minimumRuntimeVersion = "0.2.0"

// checkVersion probes `cli -v` and warns (never fails) on an older runtime.
func checkVersion(cliPath string) {
	if os.Getenv("QODER_SKIP_VERSION_CHECK") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cliPath, "-v").Output()
	if err != nil {
		return // best effort only
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(string(bytes.TrimSpace(out)), "%d.%d.%d", &major, &minor, &patch); err != nil {
		return
	}
	var m1, m2, m3 int
	_, _ = fmt.Sscanf(minimumRuntimeVersion, "%d.%d.%d", &m1, &m2, &m3)
	if major < m1 || (major == m1 && minor < m2) || (major == m1 && minor == m2 && patch < m3) {
		// Match Python behavior: log a warning only.
		fmt.Fprintf(os.Stderr, "qoder: warning: runtime version %d.%d.%d < minimum %s; some features may be unavailable\n",
			major, minor, patch, minimumRuntimeVersion)
	}
}
