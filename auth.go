package qoder

import (
	"fmt"
	"os"
)

// DefaultAuthEnvVars matches the Python SDK defaults.
const (
	DefaultAccessTokenEnvVar       = "QODERCN_PERSONAL_ACCESS_TOKEN"
	DefaultServiceAccountEnvVar    = "QODERCN_SERVICE_ACCOUNT_KEY"
	authPayloadFileEnvVar          = "QODER_SDK_AUTH_PAYLOAD_FILE"
	serviceAccountTokenProviderKey = "serviceAccountTokenProvider"
	jobTokenProviderKey            = "jobTokenProvider"
	hostProviderValue              = "host"
)

// Auth is the credential configuration passed to Options.Auth. Implementations
// are provided by the package constructors below; the payload produced is
// written to a temporary file and handed to qoderclicn via the
// QODER_SDK_AUTH_PAYLOAD_FILE environment variable. It is never logged.
type Auth interface {
	// buildPayload resolves the credential against resolver (which reads
	// environment variables) and returns the wire payload for the CLI.
	buildPayload(resolver func(string) (string, bool)) (map[string]any, error)
}

// AccessTokenAuth authenticates with a personal access token (PAT).
type AccessTokenAuth struct{ Token string }

func (a AccessTokenAuth) buildPayload(func(string) (string, bool)) (map[string]any, error) {
	return map[string]any{"type": "accessToken", "accessToken": a.Token}, nil
}

// AccessToken authenticates with a caller-provided personal access token.
func AccessToken(token string) Auth { return AccessTokenAuth{Token: token} }

// AccessTokenFromEnvAuth reads a PAT from an environment variable.
type AccessTokenFromEnvAuth struct{ EnvVar string }

func (a AccessTokenFromEnvAuth) buildPayload(resolver func(string) (string, bool)) (map[string]any, error) {
	token, ok := resolver(a.EnvVar)
	if !ok || token == "" {
		return nil, &Error{
			Kind:    ErrKindAuthAccessTokenEnvVar,
			Message: fmt.Sprintf("access token environment variable %q is not set or empty", a.EnvVar),
		}
	}
	return map[string]any{"type": "accessToken", "accessToken": token}, nil
}

// AccessTokenFromEnv reads a PAT from the given environment variable
// (default QODERCN_PERSONAL_ACCESS_TOKEN).
func AccessTokenFromEnv(envVar ...string) Auth {
	v := DefaultAccessTokenEnvVar
	if len(envVar) > 0 && envVar[0] != "" {
		v = envVar[0]
	}
	return AccessTokenFromEnvAuth{EnvVar: v}
}

// QoderCLIAuthConfig reuses the local qoderclicn login state.
type QoderCLIAuthConfig struct{}

func (QoderCLIAuthConfig) buildPayload(func(string) (string, bool)) (map[string]any, error) {
	return map[string]any{"type": "qodercli"}, nil
}

// QoderCLIAuth reuses the local `qoderclicn login` state; no credential is
// handled by the host application.
func QoderCLIAuth() Auth { return QoderCLIAuthConfig{} }

// ServiceAccountTokenResult is the host callback result carrying a
// short-lived Service Account Token (SAT).
type ServiceAccountTokenResult struct {
	Token string
	// ExpiresAt is the absolute expiration as a Unix millisecond timestamp.
	// Optional; zero means unknown.
	ExpiresAt int64
}

// ServiceAccountAuth authenticates as a Service Account. Exactly one of
// Key or FetchToken must be provided.
type ServiceAccountAuth struct {
	// Key is a static Service Account key; the CLI obtains and refreshes
	// the short-lived SAT by itself.
	Key string
	// EnvVar reads the SA key from an environment variable (mutually
	// exclusive with Key/FetchToken).
	EnvVar string
	// FetchToken is a host callback providing short-lived SATs on demand.
	FetchToken func() (ServiceAccountTokenResult, error)
}

func (a ServiceAccountAuth) buildPayload(resolver func(string) (string, bool)) (map[string]any, error) {
	if a.FetchToken != nil {
		if a.Key != "" || a.EnvVar != "" {
			return nil, newError(ErrKindAuthNotConfigured,
				"ServiceAccountAuth requires exactly one credential source")
		}
		// Host-provider mode: the CLI asks back over the control protocol
		// whenever it needs a fresh token.
		return map[string]any{"type": "serviceAccount", serviceAccountTokenProviderKey: hostProviderValue}, nil
	}
	if a.Key != "" {
		return map[string]any{"type": "serviceAccount", "serviceAccountKey": a.Key}, nil
	}
	if a.EnvVar != "" {
		key, ok := resolver(a.EnvVar)
		if !ok || key == "" {
			return nil, &Error{
				Kind:    ErrKindAuthServiceAccountEnvVar,
				Message: fmt.Sprintf("service account key environment variable %q is not set or empty", a.EnvVar),
			}
		}
		return map[string]any{"type": "serviceAccount", "serviceAccountKey": key}, nil
	}
	return nil, newError(ErrKindAuthNotConfigured,
		"ServiceAccountAuth requires one of Key, EnvVar or FetchToken")
}

// ServiceAccountKey authenticates with a static Service Account key.
func ServiceAccountKey(key string) Auth { return ServiceAccountAuth{Key: key} }

// ServiceAccountKeyFromEnv reads the SA key from an environment variable.
func ServiceAccountKeyFromEnv(envVar ...string) Auth {
	v := DefaultServiceAccountEnvVar
	if len(envVar) > 0 && envVar[0] != "" {
		v = envVar[0]
	}
	return ServiceAccountAuth{EnvVar: v}
}

// ServiceAccountToken authenticates via a host-provided token callback; the
// CLI requests fresh SATs from the SDK over the control protocol.
func ServiceAccountToken(fetch func() (ServiceAccountTokenResult, error)) Auth {
	return ServiceAccountAuth{FetchToken: fetch}
}

// JobTokenAuth authenticates by serving first-party Job Tokens on demand.
type JobTokenAuth struct {
	Fetch func(req JobTokenRequest, ctx *JobTokenContext) (JobTokenResult, error)
}

func (a JobTokenAuth) buildPayload(func(string) (string, bool)) (map[string]any, error) {
	if a.Fetch == nil {
		return nil, newError(ErrKindAuthNotConfigured, "JobTokenAuth requires a Fetch callback")
	}
	return map[string]any{"type": "jobToken", jobTokenProviderKey: hostProviderValue}, nil
}

// JobToken authenticates by serving first-party Job Tokens to qoderclicn
// on demand.
func JobToken(fetch func(JobTokenRequest, *JobTokenContext) (JobTokenResult, error)) Auth {
	return JobTokenAuth{Fetch: fetch}
}

// JobTokenRequest is the host callback request for a first-party Job Token.
type JobTokenRequest struct {
	RequestID string
	// Reason is one of "initial", "expiring", "unauthorized".
	Reason    string
	SessionID string
}

// JobTokenContext carries cancellation for a Job Token callback.
type JobTokenContext struct {
	Done <-chan struct{}
}

// JobTokenResult is the host callback result for a first-party Job Token.
type JobTokenResult struct {
	Token     string
	ExpiresAt int64 // Unix ms, optional
}

// envResolver returns a resolver that checks Options.Env first, then the
// process environment, matching the Python SDK resolution order.
func envResolver(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if v, ok := env[name]; ok {
			return v, v != ""
		}
		v, ok := os.LookupEnv(name)
		return v, ok && v != ""
	}
}

// buildAuthPayload resolves Options.Auth into the wire payload. It errors
// with ErrKindAuthNotConfigured when no auth is configured.
func buildAuthPayload(auth Auth, env map[string]string) (map[string]any, error) {
	if auth == nil {
		return nil, newError(ErrKindAuthNotConfigured,
			"no auth configured: set Options.Auth (AccessToken / AccessTokenFromEnv / QoderCLIAuth / ServiceAccount* / JobToken)")
	}
	return auth.buildPayload(envResolver(env))
}
