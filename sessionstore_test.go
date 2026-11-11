package qoder

import (
	"context"
	"testing"
)

func TestInMemoryAppendLoad(t *testing.T) {
	store := NewInMemorySessionStore()
	key := SessionKey{ProjectKey: "/proj", SessionID: "s1"}
	ctx := context.Background()

	if entries, _ := store.Load(ctx, key); entries != nil {
		t.Fatal("missing session must load nil")
	}
	if err := store.Append(ctx, key, []SessionStoreEntry{{"type": "user", "uuid": "u1"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := store.Load(ctx, key)
	if err != nil || len(entries) != 1 || entries[0]["uuid"] != "u1" {
		t.Fatalf("load: %v %+v", err, entries)
	}
	// Load must return a copy.
	entries[0]["uuid"] = "mutated"
	again, _ := store.Load(ctx, key)
	if again[0]["uuid"] != "u1" {
		t.Fatal("Load must copy entries")
	}
	if store.Size() != 1 {
		t.Fatalf("size: %d", store.Size())
	}
}

func TestInMemoryListSessionsAndSubkeys(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()
	root := SessionKey{ProjectKey: "/proj", SessionID: "s1"}
	sub := SessionKey{ProjectKey: "/proj", SessionID: "s1", Subpath: "agent-1"}
	other := SessionKey{ProjectKey: "/other", SessionID: "s2"}

	_ = store.Append(ctx, root, []SessionStoreEntry{
		{"type": "summary", "summary": "working on it"},
		{"type": "user", "uuid": "u1", "message": map[string]any{"content": "fix the bug"}},
	})
	_ = store.Append(ctx, sub, []SessionStoreEntry{{"type": "user", "uuid": "u2"}})
	_ = store.Append(ctx, other, []SessionStoreEntry{{"type": "user", "uuid": "u3"}})

	rows, err := store.ListSessions(ctx, "/proj")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %+v", err, rows)
	}
	if rows[0].SessionID != "s1" || rows[0].Summary != "working on it" {
		t.Fatalf("row: %+v", rows[0])
	}
	if rows[0].FirstPrompt != "fix the bug" {
		t.Fatalf("first prompt: %+v", rows[0])
	}

	subs, err := store.ListSubkeys(ctx, root)
	if err != nil || len(subs) != 1 || subs[0] != "agent-1" {
		t.Fatalf("subkeys: %v %+v", err, subs)
	}

	if err := store.Delete(ctx, root); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if entries, _ := store.Load(ctx, root); entries != nil {
		t.Fatal("session must be gone")
	}
	if subs, _ := store.ListSubkeys(ctx, root); len(subs) != 1 {
		t.Fatal("subkey must survive parent delete (delete helper handles cascade)")
	}
	store.Clear()
	if store.Size() != 0 {
		t.Fatal("clear")
	}
}

func TestSessionMessagesPagination(t *testing.T) {
	entries := []SessionStoreEntry{
		{"type": "meta", "customTitle": "x"},
		{"type": "user", "uuid": "u1", "session_id": "s", "message": map[string]any{}},
		{"type": "summary", "summary": "ignored"},
		{"type": "assistant", "uuid": "a1", "session_id": "s"},
		{"type": "user", "uuid": "u2", "session_id": "s"},
	}
	msgs := sessionMessages(entries, "s")
	if len(msgs) != 3 || msgs[0].UUID != "u1" || msgs[1].Type != "assistant" {
		t.Fatalf("messages: %+v", msgs)
	}
	page := paginateMessages(msgs, StoreQueryOptions{Offset: 1, Limit: 1})
	if len(page) != 1 || page[0].UUID != "a1" {
		t.Fatalf("page: %+v", page)
	}
	if got := paginateMessages(msgs, StoreQueryOptions{Offset: 99}); got != nil {
		t.Fatalf("offset overflow: %+v", got)
	}
}

func TestTruncateAtMessage(t *testing.T) {
	entries := []SessionStoreEntry{
		{"type": "user", "uuid": "u1"},
		{"type": "assistant", "uuid": "a1"},
		{"type": "user", "uuid": "u2"},
	}
	got := truncateAtMessage(entries, "a1")
	if len(got) != 2 {
		t.Fatalf("truncate: %d", len(got))
	}
	if same := truncateAtMessage(entries, "missing"); len(same) != 3 {
		t.Fatalf("missing id keeps all: %d", len(same))
	}
}

func TestGetMessagesAndInfoFromStore(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()
	key := SessionKey{ProjectKey: DefaultProjectKey(""), SessionID: "s9"}
	_ = store.Append(ctx, key, []SessionStoreEntry{
		{"type": "summary", "summary": "sum"},
		{"type": "meta", "customTitle": "title", "firstPrompt": "hi"},
		{"type": "user", "uuid": "u1", "session_id": "s9", "message": map[string]any{}},
	})
	opts := StoreQueryOptions{Directory: ""}
	info, err := GetSessionInfoFromStore(ctx, store, "s9", opts)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Summary != "sum" || info.CustomTitle != "title" || info.FirstPrompt != "hi" {
		t.Fatalf("info: %+v", info)
	}
	msgs, err := GetSessionMessagesFromStore(ctx, store, "s9", opts)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages: %v %+v", err, msgs)
	}
	if missing, _ := GetSessionInfoFromStore(ctx, store, "nope", opts); missing != nil {
		t.Fatal("missing session info must be nil")
	}
}

func TestForkAndRenameTag(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()
	opts := StoreQueryOptions{Directory: ""}
	key := SessionKey{ProjectKey: DefaultProjectKey(""), SessionID: "src"}
	_ = store.Append(ctx, key, []SessionStoreEntry{
		{"type": "user", "uuid": "u1"},
		{"type": "assistant", "uuid": "a1"},
		{"type": "user", "uuid": "u2"},
	})

	fork, err := ForkSessionViaStore(ctx, store, "src", StoreQueryOptions{Directory: "", UpToMessageID: "a1", Title: "forked"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if fork.SessionID == "" || fork.SessionID == "src" {
		t.Fatalf("fork id: %+v", fork)
	}
	entries := store.Entries(SessionKey{ProjectKey: DefaultProjectKey(""), SessionID: fork.SessionID})
	if len(entries) != 3 { // 2 truncated messages + meta title
		t.Fatalf("fork entries: %+v", entries)
	}
	if entries[2]["customTitle"] != "forked" {
		t.Fatalf("fork title: %+v", entries[2])
	}

	if err := RenameSessionViaStore(ctx, store, "src", "renamed", opts); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := TagSessionViaStore(ctx, store, "src", "release", opts); err != nil {
		t.Fatalf("tag: %v", err)
	}
	src := store.Entries(key)
	if src[len(src)-2]["customTitle"] != "renamed" || src[len(src)-1]["tag"] != "release" {
		t.Fatalf("meta entries: %+v", src)
	}
}

func TestListSessionsCapabilityBoundary(t *testing.T) {
	ctx := context.Background()
	// InMemorySessionStore implements the listing capability; an empty
	// store lists zero rows.
	rows, err := ListSessionsFromStore(ctx, NewInMemorySessionStore(), StoreQueryOptions{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("InMemory store must support listing: %v %+v", err, rows)
	}
	// A bare store without ListSessions degrades into an error.
	if _, err := ListSessionsFromStore(ctx, bareStore{}, StoreQueryOptions{}); err == nil {
		t.Fatal("bare store listing must error")
	}
}

// bareStore satisfies only the required SessionStore interface.
type bareStore struct{}

func (bareStore) Append(context.Context, SessionKey, []SessionStoreEntry) error { return nil }

func (bareStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, nil
}

func TestDefaultProjectKey(t *testing.T) {
	if DefaultProjectKey("/tmp/../tmp") == "/tmp/../tmp" {
		t.Fatal("project key must be cleaned")
	}
	if DefaultProjectKey("relative") == "relative" && !isAbs(DefaultProjectKey("relative")) {
		t.Fatal("relative dir must resolve to absolute")
	}
}

func isAbs(path string) bool {
	return len(path) > 0 && (path[0] == '/' || path[1] == ':')
}

func TestSideQuestionFromWire(t *testing.T) {
	if res := sideQuestionResultFromWire(nil); res != nil {
		t.Fatal("nil payload -> nil result")
	}
	res := sideQuestionResultFromWire(map[string]any{
		"response": "cfg.yaml", "synthetic": true,
		"refusal_fallback": map[string]any{
			"original_model": "a", "fallback_model": "b", "content": "c",
		},
	})
	if res.Response != "cfg.yaml" || !res.Synthetic {
		t.Fatalf("res: %+v", res)
	}
	if res.RefusalFallback == nil || res.RefusalFallback.FallbackModel != "b" {
		t.Fatalf("fallback: %+v", res.RefusalFallback)
	}
}
