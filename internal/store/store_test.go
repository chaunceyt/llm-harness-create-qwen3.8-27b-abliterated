package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}

	sess, err := st.CreateSession(ctx, "coder", "openai", "model-x", "/ws", json.RawMessage(`{"pin":true}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "coder" || got.Model != "model-x" || got.Workspace != "/ws" {
		t.Errorf("session round-trip = %+v", got)
	}
	var md map[string]any
	if err := json.Unmarshal(got.Metadata, &md); err != nil || md["pin"] != true {
		t.Errorf("metadata round-trip = %s, %v", got.Metadata, err)
	}

	seq1, err := st.AppendTurn(ctx, sess.ID, TurnMessage, "user", "do the thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := st.AppendTurn(ctx, sess.ID, TurnToolCall, "", "", map[string]any{
		"name":   "fs_read",
		"params": json.RawMessage(`{"path":"x.txt"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	seq3, err := st.AppendTurn(ctx, sess.ID, TurnToolResult, "", "file contents", map[string]any{
		"name":     "fs_read",
		"is_error": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 || seq2 != 2 || seq3 != 3 {
		t.Fatalf("sequences = %d %d %d, want 1 2 3", seq1, seq2, seq3)
	}

	turns, err := st.LoadTurns(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("loaded %d turns", len(turns))
	}
	var tc struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(turns[1].Data, &tc); err != nil || tc.Name != "fs_read" {
		t.Errorf("tool_call data round-trip = %s, %v", turns[1].Data, err)
	}
	if turns[2].Content != "file contents" || turns[0].Role != "user" {
		t.Errorf("turn fields = %+v / %+v", turns[0], turns[2])
	}

	if err := st.Audit(ctx, sess.ID, "permission.ask", "tool=fs_edit path=x.txt"); err != nil {
		t.Fatal(err)
	}

	// Rewind to seq 2 deletes exactly the third turn.
	deleted, err := st.Rewind(ctx, sess.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("rewind deleted %d, want 1", deleted)
	}
	turns, err = st.LoadTurns(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("post-rewind turns = %d", len(turns))
	}

	// Append after rewind resumes the sequence.
	seq4, err := st.AppendTurn(ctx, sess.ID, TurnMessage, "assistant", "resumed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if seq4 != 3 {
		t.Fatalf("post-rewind seq = %d, want 3", seq4)
	}

	// Audit history survives rewind and reads back.
	log, err := st.AuditLog(ctx, sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Kind != "permission.ask" {
		t.Fatalf("audit log = %+v", log)
	}

	if _, err := st.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing session err = %v", err)
	}
	if _, err := st.Rewind(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("rewind missing session err = %v", err)
	}
	if _, err := st.AppendTurn(ctx, "nope", TurnMessage, "", "", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("append missing session err = %v", err)
	}
	if n, err := st.Rewind(ctx, sess.ID, -1); err == nil {
		t.Errorf("negative rewind seq accepted (%d)", n)
	}

	// Persistence across reopen.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "persist.db")
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := st2.CreateSession(ctx, "reviewer", "anthropic", "claude", "/ws2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st2.AppendTurn(ctx, s2.ID, TurnMessage, "user", "persist me", nil); err != nil {
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	st3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	turns, err = st3.LoadTurns(ctx, s2.ID)
	if err != nil || len(turns) != 1 || turns[0].Content != "persist me" {
		t.Fatalf("persistence: %d turns, %v", len(turns), err)
	}
	list, err := st3.ListSessions(ctx)
	if err != nil || len(list) != 1 || list[0].Mode != "reviewer" {
		t.Fatalf("list sessions: %v, %v", list, err)
	}
}

func TestStoreMemory(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.CreateSession(ctx, "automator", "ollama", "qwen", "/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTurn(ctx, s.ID, TurnEvent, "", "", map[string]any{"event": "compaction"}); err != nil {
		t.Fatal(err)
	}
	turns, err := st.LoadTurns(ctx, s.ID)
	if err != nil || len(turns) != 1 || turns[0].Kind != TurnEvent {
		t.Fatalf("memory store: %d turns, %v", len(turns), err)
	}
}

func TestStoreConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "conc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.CreateSession(ctx, "coder", "openai", "m", "/ws", nil)
	if err != nil {
		t.Fatal(err)
	}

	const workers, per = 8, 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fails int
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := st.AppendTurn(ctx, s.ID, TurnMessage, "user", "w", map[string]any{"w": w, "i": i}); err != nil {
					mu.Lock()
					fails++
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	if fails != 0 {
		t.Fatalf("%d concurrent appends failed", fails)
	}
	turns, err := st.LoadTurns(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != workers*per {
		t.Fatalf("loaded %d turns, want %d", len(turns), workers*per)
	}
	seen := make(map[int64]bool, len(turns))
	for i, tn := range turns {
		if tn.Seq != int64(i+1) {
			t.Fatalf("turn %d has seq %d, want %d (contiguity/ordering broken)", i, tn.Seq, i+1)
		}
		seen[tn.Seq] = true
	}
}
