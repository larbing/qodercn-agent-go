// Package qoder is a Go SDK for the Qoder Agent runtime (qoderclicn).
//
// It mirrors the public API surface of the official Python SDK
// (qodercn-agent-sdk): one-shot Query, multi-turn Client, authentication,
// permission callbacks, hooks, MCP integration (including in-process SDK
// MCP servers), agent definitions, model policy, and external session
// storage.
//
// The SDK spawns a qoderclicn subprocess and communicates over NDJSON on
// stdin/stdout, plus a control protocol for bidirectional requests.
package qoder

import (
	"errors"
	"fmt"
)

// ErrorKind classifies SDK errors so callers can branch with errors.Is.
type ErrorKind string

const (
	// ErrKindCLIConnection indicates the CLI process could not be reached,
	// died unexpectedly, or the stream broke mid-session.
	ErrKindCLIConnection ErrorKind = "cli_connection"
	// ErrKindCLINotFound indicates no qoderclicn executable was located.
	ErrKindCLINotFound ErrorKind = "cli_not_found"
	// ErrKindProcess indicates the CLI exited with a non-zero status.
	ErrKindProcess ErrorKind = "process"
	// ErrKindJSONDecode indicates a malformed message from the CLI.
	ErrKindJSONDecode ErrorKind = "json_decode"
	// ErrKindAuthNotConfigured indicates Options.Auth was not provided.
	ErrKindAuthNotConfigured ErrorKind = "auth_not_configured"
	// ErrKindAuthAccessTokenEnvVar indicates the PAT env var is unset/empty.
	ErrKindAuthAccessTokenEnvVar ErrorKind = "auth_access_token_env_var"
	// ErrKindAuthServiceAccountEnvVar indicates the SA key env var is unset.
	ErrKindAuthServiceAccountEnvVar ErrorKind = "auth_service_account_env_var"
	// ErrKindModelPolicyTimeout indicates the ResolveModel callback overran
	// its deadline; the query fails without degradation.
	ErrKindModelPolicyTimeout ErrorKind = "model_policy_timeout"
	// ErrKindUnsupportedCapability indicates the paired CLI lacks a feature
	// the SDK tried to use (e.g. plan mode).
	ErrKindUnsupportedCapability ErrorKind = "unsupported_capability"
	// ErrKindMessageParse indicates an internal message-shape mismatch.
	ErrKindMessageParse ErrorKind = "message_parse"
	// ErrKindControl indicates a control-protocol failure (timeout/error
	// response from the CLI).
	ErrKindControl ErrorKind = "control"
)

// Exit codes defined by qoderclicn.
const cliExitCodeAuthError = 41

// Error is the base error type returned by this SDK. Use errors.Is with the
// sentinel kinds or helpers such as AsCLINotFound to branch on failure class.
type Error struct {
	Kind    ErrorKind
	Message string
	Cause   error
	// ExitCode is set for ErrKindProcess when the CLI reported an exit code.
	ExitCode int
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("qoder: %s: %s: %v", e.Kind, e.Message, e.Cause)
	}
	return fmt.Sprintf("qoder: %s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Kind == e.Kind
	}
	return false
}

func newError(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func wrapError(kind ErrorKind, cause error, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// Convenience constructors.

func cliConnectionError(format string, args ...any) *Error {
	return newError(ErrKindCLIConnection, format, args...)
}

func clinotFoundError(format string, args ...any) *Error {
	return newError(ErrKindCLINotFound, format, args...)
}

func jsonDecodeError(cause error, line string) *Error {
	return &Error{
		Kind:    ErrKindJSONDecode,
		Message: fmt.Sprintf("failed to decode CLI message: %s", truncate(line, 200)),
		Cause:   cause,
	}
}

func processError(exitCode int, stderr string) *Error {
	if exitCode == cliExitCodeAuthError {
		return &Error{
			Kind:     ErrKindProcess,
			Message:  fmt.Sprintf("CLI exited with code %d (authentication error). Check Options.Auth or refresh credentials.", exitCode),
			ExitCode: exitCode,
		}
	}
	msg := fmt.Sprintf("CLI exited with code %d", exitCode)
	if stderr != "" {
		msg += ": " + truncate(stderr, 500)
	}
	return &Error{Kind: ErrKindProcess, Message: msg, ExitCode: exitCode}
}

// IsProcessExitWithCode reports whether err is (or wraps) a Process error
// carrying the given CLI exit code (e.g. 41 for authentication failures).
func IsProcessExitWithCode(err error, code int) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	for cur := e; cur != nil; cur, _ = cur.Cause.(*Error) {
		if cur.Kind == ErrKindProcess && cur.ExitCode == code {
			return true
		}
	}
	return false
}

// Helper predicates.

// AsCLINotFound reports whether err indicates the CLI binary was not found.
func AsCLINotFound(err error) bool {
	return errorHasKind(err, ErrKindCLINotFound)
}

// AsAuthNotConfigured reports whether err indicates a missing auth config.
func AsAuthNotConfigured(err error) bool {
	return errorHasKind(err, ErrKindAuthNotConfigured)
}

// AsModelPolicyTimeout reports whether err is a ResolveModel timeout.
func AsModelPolicyTimeout(err error) bool {
	return errorHasKind(err, ErrKindModelPolicyTimeout)
}

func errorHasKind(err error, kind ErrorKind) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == kind
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
