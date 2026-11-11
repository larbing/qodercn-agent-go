package qoder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SessionKey identifies one stored session. ProjectKey groups sessions by
// project (typically the working directory), Subpath addresses subagent
// transcripts (use the subagent id as subpath).
type SessionKey struct {
	ProjectKey string
	SessionID  string
	Subpath    string
}

// String renders the canonical key form used by in-process maps. The
// subpath segment is omitted when empty so root sessions do not carry a
// trailing separator (which would break prefix filtering).
func (k SessionKey) String() string {
	if k.Subpath == "" {
		return k.ProjectKey + "\x00" + k.SessionID
	}
	return k.ProjectKey + "\x00" + k.SessionID + "\x00" + k.Subpath
}

// SessionStoreEntry is one opaque JSON-safe record in a session transcript.
// The CLI mirrors user/assistant messages; host tools may append metadata
// records (e.g. {"type":"summary","summary":"..."}).
type SessionStoreEntry = map[string]any

// SessionStore is the external session storage adapter (Options.SessionStore).
// Append and Load are required. Implement optional capabilities by also
// implementing SessionLister, SessionDeleter and/or SubkeyLister; missing
// capabilities degrade gracefully like the Python SDK.
type SessionStore interface {
	Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error
	// Load returns nil (not an error) for a missing session.
	Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error)
}

// SessionLister is the optional listing capability.
type SessionLister interface {
	ListSessions(ctx context.Context, projectKey string) ([]SessionStoreListEntry, error)
}

// SessionDeleter is the optional delete capability.
type SessionDeleter interface {
	Delete(ctx context.Context, key SessionKey) error
}

// SubkeyLister is the optional subagent listing capability.
type SubkeyLister interface {
	ListSubkeys(ctx context.Context, key SessionKey) ([]string, error)
}

// SessionStoreListEntry is one row of ListSessions.
type SessionStoreListEntry struct {
	SessionID    string
	LastModified int64 // Unix ms
	Summary      string
	FileSize     int64
	CustomTitle  string
	FirstPrompt  string
}

// SDKSessionInfo is the session metadata returned by the store helpers.
type SDKSessionInfo struct {
	SessionID    string
	Summary      string
	LastModified int64 // Unix ms
	FileSize     *int64
	CustomTitle  string
	FirstPrompt  string
	GitBranch    string
	Cwd          string
	Tag          string
	CreatedAt    *int64
}

// SessionMessage is a historical user/assistant message.
type SessionMessage struct {
	Type            string // "user" | "assistant"
	UUID            string
	SessionID       string
	Message         map[string]any
	ParentToolUseID string
}

// ForkSessionResult reports the newly created fork.
type ForkSessionResult struct {
	SessionID string
}

// StoreQueryOptions tunes the store helper functions.
type StoreQueryOptions struct {
	// Directory identifies the project; empty uses the current directory.
	Directory string
	Limit     int
	Offset    int
	// UpToMessageID truncates a fork at (and including) this message uuid.
	UpToMessageID string
	// Title renames the forked session.
	Title string
}

// DefaultProjectKey resolves the default project identity: the absolute
// working directory.
func DefaultProjectKey(directory string) string {
	if directory == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return filepath.Clean(wd)
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return directory
	}
	return filepath.Clean(abs)
}

// ListSessionsFromStore lists sessions ordered by last modification.
func ListSessionsFromStore(ctx context.Context, store SessionStore, opts StoreQueryOptions) ([]SDKSessionInfo, error) {
	lister, ok := store.(SessionLister)
	if !ok {
		return nil, newError(ErrKindUnsupportedCapability,
			"session store does not implement ListSessions")
	}
	rows, err := lister.ListSessions(ctx, DefaultProjectKey(opts.Directory))
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastModified > rows[j].LastModified })
	if opts.Offset > 0 {
		if opts.Offset >= len(rows) {
			rows = nil
		} else {
			rows = rows[opts.Offset:]
		}
	}
	if opts.Limit > 0 && opts.Limit < len(rows) {
		rows = rows[:opts.Limit]
	}
	out := make([]SDKSessionInfo, 0, len(rows))
	for _, r := range rows {
		size := r.FileSize
		info := SDKSessionInfo{
			SessionID:    r.SessionID,
			Summary:      r.Summary,
			LastModified: r.LastModified,
			CustomTitle:  r.CustomTitle,
			FirstPrompt:  r.FirstPrompt,
		}
		if r.FileSize > 0 {
			info.FileSize = &size
		}
		out = append(out, info)
	}
	return out, nil
}

// GetSessionInfoFromStore reads one session's metadata derived from its
// stored entries.
func GetSessionInfoFromStore(ctx context.Context, store SessionStore, sessionID string, opts StoreQueryOptions) (*SDKSessionInfo, error) {
	entries, err := store.Load(ctx, SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, nil
	}
	info := &SDKSessionInfo{SessionID: sessionID, LastModified: time.Now().UnixMilli()}
	for _, e := range entries {
		switch e["type"] {
		case "summary":
			info.Summary = str(e, "summary")
		case "meta":
			info.CustomTitle = str(e, "customTitle")
			info.FirstPrompt = str(e, "firstPrompt")
			info.GitBranch = str(e, "gitBranch")
			info.Cwd = str(e, "cwd")
			info.Tag = str(e, "tag")
		}
	}
	return info, nil
}

// GetSessionMessagesFromStore returns the valid conversation chain.
func GetSessionMessagesFromStore(ctx context.Context, store SessionStore, sessionID string, opts StoreQueryOptions) ([]SessionMessage, error) {
	entries, err := store.Load(ctx, SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	messages := sessionMessages(entries, sessionID)
	return paginateMessages(messages, opts), nil
}

// ListSubagentsFromStore lists subagent ids stored under a session.
func ListSubagentsFromStore(ctx context.Context, store SessionStore, sessionID string, opts StoreQueryOptions) ([]string, error) {
	lister, ok := store.(SubkeyLister)
	if !ok {
		return nil, newError(ErrKindUnsupportedCapability,
			"session store does not implement ListSubkeys")
	}
	return lister.ListSubkeys(ctx, SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID})
}

// GetSubagentMessagesFromStore reads one subagent transcript.
func GetSubagentMessagesFromStore(ctx context.Context, store SessionStore, sessionID, agentID string, opts StoreQueryOptions) ([]SessionMessage, error) {
	entries, err := store.Load(ctx, SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID, Subpath: agentID})
	if err != nil {
		return nil, err
	}
	messages := sessionMessages(entries, sessionID)
	return paginateMessages(messages, opts), nil
}

// RenameSessionViaStore stores a custom title.
func RenameSessionViaStore(ctx context.Context, store SessionStore, sessionID, title string, opts StoreQueryOptions) error {
	return store.Append(ctx, SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID},
		[]SessionStoreEntry{{"type": "meta", "customTitle": title}})
}

// TagSessionViaStore stores a session tag.
func TagSessionViaStore(ctx context.Context, store SessionStore, sessionID, tag string, opts StoreQueryOptions) error {
	return store.Append(ctx, SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID},
		[]SessionStoreEntry{{"type": "meta", "tag": tag}})
}

// DeleteSessionViaStore removes a session and its subagent data.
func DeleteSessionViaStore(ctx context.Context, store SessionStore, sessionID string, opts StoreQueryOptions) error {
	deleter, ok := store.(SessionDeleter)
	if !ok {
		return nil // capability missing: nothing deleted, like the Python SDK
	}
	root := SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID}
	if lister, ok := store.(SubkeyLister); ok {
		if subs, err := lister.ListSubkeys(ctx, root); err == nil {
			for _, sub := range subs {
				subKey := root
				subKey.Subpath = sub
				_ = deleter.Delete(ctx, subKey)
			}
		}
	}
	return deleter.Delete(ctx, root)
}

// ForkSessionViaStore creates an independent session from existing history,
// optionally truncated at a message and renamed.
func ForkSessionViaStore(ctx context.Context, store SessionStore, sessionID string, opts StoreQueryOptions) (*ForkSessionResult, error) {
	source := SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID}
	entries, err := store.Load(ctx, source)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, newError(ErrKindCLIConnection, "session %q not found in store", sessionID)
	}
	if opts.UpToMessageID != "" {
		entries = truncateAtMessage(entries, opts.UpToMessageID)
	}
	newID, err := newSessionUUID()
	if err != nil {
		return nil, wrapError(ErrKindCLIConnection, err, "generate fork id")
	}
	fork := SessionKey{ProjectKey: source.ProjectKey, SessionID: newID}
	if err := store.Append(ctx, fork, entries); err != nil {
		return nil, err
	}
	if opts.Title != "" {
		if err := store.Append(ctx, fork, []SessionStoreEntry{{"type": "meta", "customTitle": opts.Title}}); err != nil {
			return nil, err
		}
	}
	return &ForkSessionResult{SessionID: newID}, nil
}

// ImportSessionOptions tunes ImportSessionToStore.
type ImportSessionOptions struct {
	StoreQueryOptions
	// TranscriptPath points at the local CLI transcript to import. Empty
	// probes the default CLI storage layout for sessionID.
	TranscriptPath string
	// IncludeSubagents also imports agent/<id> transcripts when found.
	IncludeSubagents bool
	// BatchSize controls append chunking (default 500).
	BatchSize int
}

// ImportSessionToStore copies a local CLI session into the external store.
func ImportSessionToStore(ctx context.Context, sessionID string, store SessionStore, opts ImportSessionOptions) error {
	path := opts.TranscriptPath
	if path == "" {
		found, err := findLocalTranscript(sessionID, opts.Directory)
		if err != nil {
			return err
		}
		path = found
	}
	entries, err := readJSONLFile(path)
	if err != nil {
		return wrapError(ErrKindCLIConnection, err, "read transcript %s", path)
	}
	key := SessionKey{ProjectKey: DefaultProjectKey(opts.Directory), SessionID: sessionID}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = 500
	}
	for i := 0; i < len(entries); i += batch {
		end := i + batch
		if end > len(entries) {
			end = len(entries)
		}
		if err := store.Append(ctx, key, entries[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// sessionMessages converts stored entries into SessionMessage values.
func sessionMessages(entries []SessionStoreEntry, sessionID string) []SessionMessage {
	out := []SessionMessage{}
	for _, e := range entries {
		t, _ := e["type"].(string)
		if t != "user" && t != "assistant" {
			continue
		}
		msg, _ := e["message"].(map[string]any)
		out = append(out, SessionMessage{
			Type:            t,
			UUID:            str(e, "uuid"),
			SessionID:       str(e, "session_id"),
			Message:         msg,
			ParentToolUseID: str(e, "parent_tool_use_id"),
		})
	}
	return out
}

func paginateMessages(messages []SessionMessage, opts StoreQueryOptions) []SessionMessage {
	if opts.Offset > 0 {
		if opts.Offset >= len(messages) {
			return nil
		}
		messages = messages[opts.Offset:]
	}
	if opts.Limit > 0 && opts.Limit < len(messages) {
		messages = messages[:opts.Limit]
	}
	return messages
}

func truncateAtMessage(entries []SessionStoreEntry, messageID string) []SessionStoreEntry {
	for i, e := range entries {
		if str(e, "uuid") == messageID {
			return entries[:i+1]
		}
	}
	return entries
}

func newSessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// readJSONLFile parses a newline-delimited JSON transcript into entries.
func readJSONLFile(path string) ([]SessionStoreEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []SessionStoreEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry SessionStoreEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, wrapError(ErrKindMessageParse, err, "decode transcript line in %s", path)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// findLocalTranscript probes the default CLI storage layout for a session
// transcript (~/.qoder-cn/projects/<project>/**/<session_id>.jsonl).
func findLocalTranscript(sessionID, directory string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".qoder-cn", "projects")
	var found string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if strings.TrimSuffix(filepath.Base(path), ".jsonl") == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", newError(ErrKindCLINotFound,
			"local transcript for session %q not found under %s; pass ImportSessionOptions.TranscriptPath", sessionID, root)
	}
	return found, nil
}

// InMemorySessionStore is the built-in development store. It implements
// every optional capability plus Entries/Size/Clear inspection helpers.
// Data is lost when the process exits.
type InMemorySessionStore struct {
	mu   sync.Mutex
	data map[string][]SessionStoreEntry
}

// NewInMemorySessionStore creates an empty in-memory store.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{data: map[string][]SessionStoreEntry{}}
}

func (s *InMemorySessionStore) Append(_ context.Context, key SessionKey, entries []SessionStoreEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key.String()] = append(s.data[key.String()], copyEntries(entries)...)
	return nil
}

func (s *InMemorySessionStore) Load(_ context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyEntries(s.data[key.String()]), nil
}

func (s *InMemorySessionStore) ListSessions(_ context.Context, projectKey string) ([]SessionStoreListEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := projectKey + "\x00"
	var rows []SessionStoreListEntry
	for key, entries := range s.data {
		if !strings.HasPrefix(key, prefix) || strings.Contains(strings.TrimPrefix(key, prefix), "\x00") {
			continue // different project or subkey transcript
		}
		row := SessionStoreListEntry{SessionID: strings.TrimPrefix(key, prefix), LastModified: time.Now().UnixMilli()}
		for _, e := range entries {
			switch e["type"] {
			case "summary":
				row.Summary = str(e, "summary")
			case "meta":
				row.CustomTitle = str(e, "customTitle")
				row.FirstPrompt = str(e, "firstPrompt")
			case "user":
				if row.FirstPrompt == "" {
					row.FirstPrompt = firstPromptOf(e)
				}
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SessionID < rows[j].SessionID })
	return rows, nil
}

func (s *InMemorySessionStore) Delete(_ context.Context, key SessionKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key.String())
	return nil
}

func (s *InMemorySessionStore) ListSubkeys(_ context.Context, key SessionKey) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := key.String() + "\x00"
	var subs []string
	for k := range s.data {
		if strings.HasPrefix(k, prefix) {
			subs = append(subs, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(subs)
	return subs, nil
}

// Entries exposes the raw entries of one key (test/debug helper).
func (s *InMemorySessionStore) Entries(key SessionKey) []SessionStoreEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyEntries(s.data[key.String()])
}

// Size reports the number of distinct keys stored.
func (s *InMemorySessionStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}

// Clear removes every stored entry.
func (s *InMemorySessionStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = map[string][]SessionStoreEntry{}
}

func firstPromptOf(entry SessionStoreEntry) string {
	msg, _ := entry["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	content, _ := msg["content"].(string)
	return content
}

// copyEntries deep-copies one entry list (entry maps are copied too) so
// callers can never mutate stored data through returned references.
func copyEntries(entries []SessionStoreEntry) []SessionStoreEntry {
	if entries == nil {
		return nil
	}
	out := make([]SessionStoreEntry, len(entries))
	for i, e := range entries {
		m := make(SessionStoreEntry, len(e))
		for k, v := range e {
			m[k] = v
		}
		out[i] = m
	}
	return out
}
