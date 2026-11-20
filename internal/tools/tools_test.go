package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"harness/internal/sandbox"
)

// testEnv builds a fresh temp workspace with the real sandbox attached.
func testEnv(t *testing.T, allowWrite bool) Env {
	t.Helper()
	ws, err := os.MkdirTemp("", "tools-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	return Env{
		Workspace:  ws,
		AllowWrite: allowWrite,
		Sandbox:    sandbox.New(),
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustCall(t *testing.T, tool Tool, env Env, params string) *Result {
	t.Helper()
	res, err := tool.Call(context.Background(), env, json.RawMessage(params))
	if err != nil {
		t.Fatalf("%s(%s): unexpected fault: %v", tool.Name(), params, err)
	}
	return res
}

func TestConfinement(t *testing.T) {
	env := testEnv(t, false)
	outside := filepath.Join(filepath.Dir(env.Workspace), "tools-test-outside")
	writeFile(t, filepath.Join(outside, "target.txt"), "out\n")
	t.Cleanup(func() { os.RemoveAll(outside) })

	if _, err := env.confine("../target.txt"); err == nil {
		t.Error("confine(../target.txt): expected escape rejection")
	}
	if _, err := env.confine(".."); err == nil {
		t.Error("confine(..): expected escape rejection")
	}
	if _, err := env.confine(filepath.Join(outside, "target.txt")); err == nil {
		t.Error("confine(absolute outside): expected escape rejection")
	}

	inside := filepath.Join(env.Workspace, "a.txt")
	writeFile(t, inside, "in\n")
	if got, err := env.confine("a.txt"); err != nil || got != inside {
		t.Errorf("confine(a.txt) = %q, %v; want %q", got, err, inside)
	}
	if got, err := env.confine(inside); err != nil || got != inside {
		t.Errorf("confine(absolute inside) = %q, %v", got, err)
	}
	if got, err := env.confine("sub/../a.txt"); err != nil || got != inside {
		t.Errorf("confine(sub/../a.txt) = %q, %v; dot segments must clean inside", got, err)
	}

	// Symlink-verified containment: a link that points outside must be
	// rejected even though the lexical path is inside.
	if err := os.Symlink(outside, filepath.Join(env.Workspace, "dlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(env.Workspace, "flink")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.confine("flink"); err == nil {
		t.Error("confine(flink): symlink escape not detected")
	}
	if _, err := env.confine("dlink/target.txt"); err == nil {
		t.Error("confine(dlink/target.txt): symlinked-dir escape not detected")
	}

	real := filepath.Join(env.Workspace, "real.txt")
	writeFile(t, real, "r\n")
	if err := os.Symlink(real, filepath.Join(env.Workspace, "slink")); err != nil {
		t.Fatal(err)
	}
	if got, err := env.confine("slink"); err != nil || got != filepath.Join(env.Workspace, "slink") {
		t.Errorf("confine(slink) = %q, %v; inside symlink must pass lexically", got, err)
	}
}

func TestFSRead(t *testing.T) {
	env := testEnv(t, false)
	tool := newFSRead(env)

	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("L%d", i))
	}
	writeFile(t, filepath.Join(env.Workspace, "lines.txt"), strings.Join(lines, "\n")+"\n")

	res := mustCall(t, tool, env, `{"path":"lines.txt","offset":3,"limit":2}`)
	if res.IsError {
		t.Fatalf("window read: %s", res.Content)
	}
	if !strings.Contains(res.Content, "3\tL3") || !strings.Contains(res.Content, "4\tL4") {
		t.Errorf("window missing expected numbered lines:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "L5") {
		t.Errorf("window exceeded limit:\n%s", res.Content)
	}

	res = mustCall(t, tool, env, `{"path":"lines.txt","offset":100}`)
	if res.IsError || !strings.Contains(res.Content, "(no lines in window; file has 10 lines)") {
		t.Errorf("empty window: %v / %s", res.IsError, res.Content)
	}

	res = mustCall(t, tool, env, `{"path":"lines.txt","offset":0}`)
	if !res.IsError {
		t.Error("offset=0: expected soft error")
	}
	res = mustCall(t, tool, env, `{}`)
	if !res.IsError {
		t.Error("missing path: expected soft error")
	}
	res = mustCall(t, tool, env, `{"path":5}`)
	if !res.IsError || !strings.Contains(res.Content, "decode") {
		t.Errorf("bad param type: want decode soft error, got %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"path":"../nope"}`)
	if !res.IsError || !strings.Contains(res.Content, "escapes") {
		t.Errorf("escaping path: want escape soft error, got %v / %s", res.IsError, res.Content)
	}

	// Single-line byte cap: a 300KiB line must be cut with a marker.
	writeFile(t, filepath.Join(env.Workspace, "long.txt"), strings.Repeat("x", 300<<10)+"\n")
	res = mustCall(t, tool, env, `{"path":"long.txt"}`)
	if res.IsError {
		t.Fatalf("long line read: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[line truncated]") {
		t.Error("long line: missing truncation marker")
	}
	if len(res.Content) > 300<<10 {
		t.Errorf("long line: content %d bytes not bounded", len(res.Content))
	}
}

func TestFSEdit(t *testing.T) {
	env := testEnv(t, true)
	ro := Env{Workspace: env.Workspace, AllowWrite: false, Sandbox: env.Sandbox}
	tool := newFSEdit(env)
	path := filepath.Join(env.Workspace, "edit.txt")
	writeFile(t, path, "alpha\nbeta\ngamma\nbeta\n")

	res := mustCall(t, tool, env, `{"path":"edit.txt","old_string":"alpha","new_string":"ALPHA"}`)
	if res.IsError {
		t.Fatalf("unique edit: %s", res.Content)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "ALPHA\nbeta\ngamma\nbeta\n") {
		t.Errorf("unique edit not applied:\n%s", b)
	}

	writeFile(t, path, "x\nbeta\ny\nbeta\n")
	res = mustCall(t, tool, env, `{"path":"edit.txt","old_string":"beta","new_string":"B"}`)
	if !res.IsError || !strings.Contains(res.Content, "2 times") {
		t.Errorf("multi-match: want uniqueness rejection with count, got %v / %s", res.IsError, res.Content)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "B\n") {
		t.Error("multi-match: file was modified")
	}

	res = mustCall(t, tool, env, `{"path":"edit.txt","old_string":"beta\n","new_string":"","replace_all":true}`)
	if res.IsError || !strings.Contains(res.Content, "replaced 2 occurrence(s)") {
		t.Errorf("replace_all: %v / %s", res.IsError, res.Content)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "x\ny\n" {
		t.Errorf("replace_all result:\n%q", b)
	}

	writeFile(t, path, "same\n")
	res = mustCall(t, tool, env, `{"path":"edit.txt","old_string":"same","new_string":"same"}`)
	if !res.IsError {
		t.Error("identical old/new: expected soft error")
	}
	res = mustCall(t, tool, env, `{"path":"edit.txt","old_string":"absent","new_string":"x"}`)
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Errorf("not-found: %v / %s", res.IsError, res.Content)
	}

	// Reviewer posture: mutation denied, file untouched.
	roTool := newFSEdit(ro)
	writeFile(t, path, "ro\n")
	res = mustCall(t, roTool, ro, `{"path":"edit.txt","old_string":"ro","new_string":"RW"}`)
	if !res.IsError || !strings.Contains(res.Content, "read-only") {
		t.Errorf("reviewer posture: %v / %s", res.IsError, res.Content)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "ro\n" {
		t.Error("reviewer posture: file was modified")
	}

	res = mustCall(t, tool, env, `{"path":"../outside.txt","old_string":"a","new_string":"b"}`)
	if !res.IsError || !strings.Contains(res.Content, "escapes") {
		t.Errorf("escaping edit path: %v / %s", res.IsError, res.Content)
	}
}

func TestFSSearch(t *testing.T) {
	env := testEnv(t, false)
	tool := newFSSearch(env)
	ws := env.Workspace

	writeFile(t, filepath.Join(ws, ".gitignore"), "*.log\n!keep.log\n/anchor.txt\nbuild/\n**/vendor\n*.tmp\n")
	writeFile(t, filepath.Join(ws, "keep.log"), "KEEPME\n")
	writeFile(t, filepath.Join(ws, "drop.log"), "DROPME\n")
	writeFile(t, filepath.Join(ws, "anchor.txt"), "ANCHORROOT\n")
	writeFile(t, filepath.Join(ws, "sub", "anchor.txt"), "ANCHORSUB\n")
	writeFile(t, filepath.Join(ws, "build", "inner.txt"), "NEVER\n")
	writeFile(t, filepath.Join(ws, "sub2", "build"), "BUILDNOTDIR\n")
	writeFile(t, filepath.Join(ws, "sub", ".gitignore"), "!sub.tmp\n")
	writeFile(t, filepath.Join(ws, "sub", "sub.tmp"), "SUBTMP\n")
	writeFile(t, filepath.Join(ws, "sub", "other.tmp"), "OTHERTMP\n")
	writeFile(t, filepath.Join(ws, "vendor", "v.txt"), "VEND\n")
	writeFile(t, filepath.Join(ws, "deep", "vendor", "d.txt"), "VEND\n")
	writeFile(t, filepath.Join(ws, "bin.dat"), "\x00NUL\n")
	writeFile(t, filepath.Join(ws, "g1.go"), "GLOBME\n")
	writeFile(t, filepath.Join(ws, "g2.txt"), "GLOBME\n")
	writeFile(t, filepath.Join(ws, "cap.txt"), "HIT\nHIT\nHIT\nHIT\nHIT\n")

	// Sparse file past the native size bound.
	huge, err := os.OpenFile(filepath.Join(ws, "huge.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := huge.Truncate(17 << 20); err != nil {
		t.Fatal(err)
	}
	huge.Close()

	call := func(params string) *Result { return mustCall(t, tool, env, params) }

	// Basic match rendering.
	res := call(`{"pattern":"KEEPME"}`)
	if res.IsError || !strings.Contains(res.Content, "keep.log:1:KEEPME") {
		t.Fatalf("basic match: %v / %s", res.IsError, res.Content)
	}

	// Gitignore semantics, one differential per pattern.
	if res := call(`{"pattern":"DROPME"}`); !res.IsError && res.Content != "(no matches)" {
		t.Errorf("unanchored *.log should ignore drop.log: %s", res.Content)
	}
	if res := call(`{"pattern":"KEEPME"}`); !strings.Contains(res.Content, "keep.log") {
		t.Errorf("negation !keep.log should restore keep.log: %s", res.Content)
	}
	res = call(`{"pattern":"ANCHOR"}`)
	if !strings.Contains(res.Content, "sub/anchor.txt:1:ANCHORSUB") || strings.Contains(res.Content, "anchor.txt:1:ANCHORROOT") {
		t.Errorf("anchored /anchor.txt should ignore root only:\n%s", res.Content)
	}
	if res := call(`{"pattern":"NEVER"}`); res.Content != "(no matches)" {
		t.Errorf("dir-only build/ should prune directory:\n%s", res.Content)
	}
	if res := call(`{"pattern":"BUILDNOTDIR"}`); !strings.Contains(res.Content, "sub2/build:1") {
		t.Errorf("dir-only pattern must not ignore a file named build:\n%s", res.Content)
	}
	if res := call(`{"pattern":"VEND"}`); res.Content != "(no matches)" {
		t.Errorf("spanning **/vendor should prune at any depth:\n%s", res.Content)
	}
	if res := call(`{"pattern":"SUBTMP"}`); !strings.Contains(res.Content, "sub/sub.tmp") {
		t.Errorf("nested negation (deepest last-match) should restore sub/sub.tmp:\n%s", res.Content)
	}
	if res := call(`{"pattern":"OTHERTMP"}`); res.Content != "(no matches)" {
		t.Errorf("root *.tmp should still ignore sub/other.tmp:\n%s", res.Content)
	}

	// Binary and size guards: only huge.txt would match \x00, and it must be
	// skipped by the size bound (bin.dat is skipped as binary).
	res = call(`{"pattern":"\u0000"}`)
	if res.IsError || res.Content != "(no matches)" {
		t.Errorf("size/binary guards: want no matches, got %v / %s", res.IsError, res.Content)
	}

	// Glob filtering by file name.
	res = call(`{"pattern":"GLOBME","glob":"*.go"}`)
	if !strings.Contains(res.Content, "g1.go") || strings.Contains(res.Content, "g2.txt") {
		t.Errorf("glob *.go:\n%s", res.Content)
	}
	res = call(`{"pattern":"GLOBME","glob":"["}`)
	if !res.IsError || !strings.Contains(res.Content, "bad glob") {
		t.Errorf("invalid glob: %v / %s", res.IsError, res.Content)
	}

	// Result cap.
	res = call(`{"pattern":"HIT","max_results":2}`)
	if strings.Count(res.Content, "HIT\n") != 2 || !strings.Contains(res.Content, "results truncated") {
		t.Errorf("cap:\n%s", res.Content)
	}

	// Path restriction.
	res = call(`{"pattern":"KEEPME","path":"sub"}`)
	if res.Content != "(no matches)" {
		t.Errorf("path restriction leaked:\n%s", res.Content)
	}
	res = call(`{"pattern":"HIT","path":"../"}`)
	if !res.IsError || !strings.Contains(res.Content, "escapes") {
		t.Errorf("escaping search path: %v / %s", res.IsError, res.Content)
	}

	res = call(`{"pattern":"("}`)
	if !res.IsError || !strings.Contains(res.Content, "compile pattern") {
		t.Errorf("bad regexp: %v / %s", res.IsError, res.Content)
	}

	// Sandboxed ripgrep lane, exercised when rg is installed.
	if rg, err := exec.LookPath("rg"); err == nil {
		rgEnv := env
		rgEnv.Ripgrep = rg
		rgTool := newFSSearch(rgEnv)
		res = mustCall(t, rgTool, rgEnv, `{"pattern":"KEEPME"}`)
		if res.IsError || !strings.Contains(res.Content, "keep.log") {
			t.Errorf("ripgrep lane: %v / %s", res.IsError, res.Content)
		}
		if res = mustCall(t, rgTool, rgEnv, `{"pattern":"DROPME"}`); res.Content != "(no matches)" {
			t.Errorf("ripgrep lane gitignore divergence:\n%s", res.Content)
		}
	}
}

func TestShellExec(t *testing.T) {
	env := testEnv(t, true)
	tool := newShellExec(env)

	res := mustCall(t, tool, env, `{"command":"echo hi"}`)
	if res.IsError || !strings.Contains(res.Content, "exit: 0") || !strings.Contains(res.Content, "hi") {
		t.Fatalf("basic exec: %v / %s", res.IsError, res.Content)
	}

	// Workspace writes are visible to the host.
	res = mustCall(t, tool, env, `{"command":"echo wrote > w.txt"}`)
	if res.IsError {
		t.Fatalf("workspace write: %s", res.Content)
	}
	if b, err := os.ReadFile(filepath.Join(env.Workspace, "w.txt")); err != nil || string(b) != "wrote\n" {
		t.Errorf("workspace write not visible to host: %q, %v", b, err)
	}

	// Nonzero exits are data, not faults.
	res = mustCall(t, tool, env, `{"command":"exit 3"}`)
	if res.IsError || !strings.Contains(res.Content, "exit: 3") {
		t.Errorf("nonzero exit: %v / %s", res.IsError, res.Content)
	}

	res = mustCall(t, tool, env, `{"command":"echo oops >&2"}`)
	if !strings.Contains(res.Content, "stderr:") || !strings.Contains(res.Content, "oops") {
		t.Errorf("stderr surfacing:\n%s", res.Content)
	}

	// A missing binary inside the shell is data (exit 127), not a transport
	// fault: /bin/sh itself executed.
	res = mustCall(t, tool, env, `{"command":"/nonexistent-xyz-123"}`)
	if res.IsError || !strings.Contains(res.Content, "exit: 127") {
		t.Errorf("missing binary: %v / %s", res.IsError, res.Content)
	}

	res = mustCall(t, tool, env, `{}`)
	if !res.IsError {
		t.Error("missing command: expected soft error")
	}

	// Deadline enforcement: a 5s sleep under a 700ms budget must be killed
	// promptly in either sandbox lane.
	res = mustCall(t, tool, env, `{"command":"sleep 5","timeout_ms":700}`)
	if res.IsError {
		t.Fatalf("timeout exec faulted: %s", res.Content)
	}
	if !strings.Contains(res.Content, "(timed out)") {
		t.Errorf("timeout: missing marker:\n%s", res.Content)
	}
	m := regexp.MustCompile(`duration: (\S+)`).FindStringSubmatch(res.Content)
	if m == nil {
		t.Fatalf("timeout: no duration rendered:\n%s", res.Content)
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatalf("timeout: unparsable duration %q", m[1])
	}
	if d >= 4*time.Second {
		t.Errorf("timeout: duration %v shows the deadline was not enforced", d)
	}

	// Output truncation at the sandbox byte cap.
	res = mustCall(t, tool, env, `{"command":"yes x | head -c 300000"}`)
	if res.IsError {
		t.Fatalf("truncation exec: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[stdout truncated]") {
		t.Errorf("truncation: missing marker")
	}
	if len(res.Content) >= 300000 {
		t.Errorf("truncation: %d bytes retained", len(res.Content))
	}
}

func gitFixture(t *testing.T, dir string) {
	t.Helper()
	genv := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+dir,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = genv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\n")
	run("add", "a.txt")
	run("commit", "-m", "c1")
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\n")
	run("add", "a.txt")
	run("commit", "-m", "c2")
	writeFile(t, filepath.Join(dir, "a.txt"), "alpha\nbeta\ngamma\n") // unstaged
}

func TestGitOps(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	env := testEnv(t, false)
	gitFixture(t, env.Workspace)
	tool := newGitOps(env)

	res := mustCall(t, tool, env, `{"op":"push"}`)
	if !res.IsError || !strings.Contains(res.Content, "unknown op") {
		t.Errorf("unknown op: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"log","ref":"HEAD~1"}`)
	if !res.IsError || !strings.Contains(res.Content, "bad ref") {
		t.Errorf("tilde ref must be rejected lexically: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"log","ref":"a b"}`)
	if !res.IsError || !strings.Contains(res.Content, "bad ref") {
		t.Errorf("space ref must be rejected: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"log","count":0}`)
	if !res.IsError || !strings.Contains(res.Content, "count") {
		t.Errorf("count=0: %v / %s", res.IsError, res.Content)
	}

	res = mustCall(t, tool, env, `{"op":"status"}`)
	if res.IsError || !strings.Contains(res.Content, "a.txt") {
		t.Fatalf("status: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"log"}`)
	if res.IsError || !strings.Contains(res.Content, "c2") || !strings.Contains(res.Content, "c1") {
		t.Fatalf("log: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"log","count":1}`)
	if res.IsError || !strings.Contains(res.Content, "c2") || strings.Contains(res.Content, "c1") {
		t.Errorf("log count=1: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"show"}`)
	if res.IsError || !strings.Contains(res.Content, "c2") {
		t.Errorf("show: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"diff"}`)
	if res.IsError || !strings.Contains(res.Content, "gamma") {
		t.Errorf("diff must surface the unstaged change: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"diff","path":"a.txt"}`)
	if res.IsError || !strings.Contains(res.Content, "gamma") {
		t.Errorf("diff with path: %v / %s", res.IsError, res.Content)
	}
	res = mustCall(t, tool, env, `{"op":"diff","path":"../outside"}`)
	if !res.IsError || !strings.Contains(res.Content, "escapes") {
		t.Errorf("escaping diff path: %v / %s", res.IsError, res.Content)
	}
}

func TestRegistry(t *testing.T) {
	ws, err := os.MkdirTemp("", "tools-test-reg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })

	if _, err := New(Env{}); err == nil {
		t.Error("empty workspace: expected error")
	}

	reg, err := New(Env{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Names(); strings.Join(got, ",") != strings.Join(builtinOrder, ",") {
		t.Errorf("default selection = %v, want %v", got, builtinOrder)
	}
	if _, ok := reg.Get("fs_read"); !ok {
		t.Error("Get(fs_read) missing")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get(nope) should miss")
	}

	reg, err = New(Env{Workspace: ws}, "shell_exec", "fs_read")
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Names(); len(got) != 2 || got[0] != "shell_exec" || got[1] != "fs_read" {
		t.Errorf("subset selection = %v", got)
	}
	if _, err := New(Env{Workspace: ws}, "bogus"); err == nil {
		t.Error("unknown tool: expected error")
	}
	if _, err := New(Env{Workspace: ws}, "fs_read", "fs_read"); err == nil {
		t.Error("duplicate tool: expected error")
	}
}
