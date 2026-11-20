// Command harness is the interactive entrypoint to the agent runtime:
// provider wiring, sandboxed execution, interactive human review, workflow
// automation, and session persistence with resume/rewind.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/sandbox"
	"harness/internal/store"
	"harness/internal/telemetry"
	"harness/internal/tools"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin))
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".harness", "sessions.db")
}

// run executes the CLI and returns the process exit code:
//
//	0  task completed (final answer) or workflow fully succeeded
//	1  runtime failure (provider, sandbox, persistence faults)
//	2  configuration error, workflow step failure, or turn budget exhaustion
//	3  session escalated to a human
//	130 canceled by signal
func run(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "model provider: openai, anthropic, or ollama")
	model := fs.String("model", "", "model name or identifier")
	endpoint := fs.String("endpoint", "", "provider base URL override")
	modeName := fs.String("mode", string(agent.ModeCoder), "agent persona: coder, reviewer, or automator")
	allowNet := fs.Bool("allow-net", false, "permit outbound network from sandboxed commands")
	workspace := fs.String("workspace", "", "workspace root (default: current directory)")
	storePath := fs.String("store", defaultStorePath(), "session database path (':memory:' for transient; empty disables persistence)")
	resume := fs.String("resume", "", "session ID to resume")
	rewind := fs.Int64("rewind", -1, "with --resume: truncate the session to this turn sequence first")
	maxTurns := fs.Int("max-turns", 0, "turn budget per task (default 50)")
	window := fs.Int("window", 0, "context window in estimated tokens (default 128000)")
	workflow := fs.String("workflow", "", "workflow JSON file (automator mode)")
	allowAll := fs.Bool("allow-all", false, "answer human-review prompts with allow")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	task := strings.Join(fs.Args(), " ")
	if task == "" {
		var err error
		var raw []byte
		if isTTY(stdin) {
			var line string
			line, err = readLine(stdin)
			raw = []byte(line)
		} else {
			raw, err = io.ReadAll(stdin)
		}
		if err != nil && len(raw) == 0 {
			err = nil // empty stdin: no task supplied
		}
		if err != nil {
			fmt.Fprintf(stderr, "harness: read task: %v\n", err)
			return 1
		}
		task = strings.TrimSpace(string(raw))
	}
	if task == "" && *workflow == "" {
		fs.Usage()
		return 2
	}

	var mode agent.Mode
	switch *modeName {
	case string(agent.ModeCoder):
		mode = agent.ModeCoder
	case string(agent.ModeReviewer):
		mode = agent.ModeReviewer
	case string(agent.ModeAutomator):
		mode = agent.ModeAutomator
	default:
		fmt.Fprintf(stderr, "harness: unknown mode %q (want coder, reviewer, or automator)\n", *modeName)
		return 2
	}

	ws := *workspace
	if ws == "" {
		var err error
		if ws, err = os.Getwd(); err != nil {
			fmt.Fprintf(stderr, "harness: resolve workspace: %v\n", err)
			return 1
		}
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		fmt.Fprintf(stderr, "harness: prepare workspace %s: %v\n", ws, err)
		return 1
	}

	prov, err := buildProvider(*providerName, *endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return 2
	}
	if *model == "" {
		fmt.Fprintln(stderr, "harness: --model is required")
		return 2
	}
	if keyName := envKeyNameFor(*providerName); keyName != "" && envKeyFor(*providerName) == "" {
		fmt.Fprintf(stderr, "harness: note: no %s in environment; assuming a local endpoint\n", keyName)
	}

	var st *store.Store
	if *storePath != "" {
		if *storePath != ":memory:" {
			if dir := filepath.Dir(*storePath); dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					fmt.Fprintf(stderr, "harness: prepare store directory: %v\n", err)
					return 1
				}
			}
		}
		if st, err = store.Open(*storePath); err != nil {
			fmt.Fprintf(stderr, "harness: open store: %v\n", err)
			return 1
		}
		defer func() { _ = st.Close() }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var seed []llm.Message
	if *resume != "" {
		if *workflow != "" {
			fmt.Fprintln(stderr, "harness: --resume applies to task runs, not workflows")
			return 2
		}
		if st == nil {
			fmt.Fprintln(stderr, "harness: --resume requires a persistent store (see --store)")
			return 2
		}
		if *rewind >= 0 {
			if n, err := st.Rewind(ctx, *resume, *rewind); err != nil {
				fmt.Fprintf(stderr, "harness: rewind session %s to seq %d: %v\n", *resume, *rewind, err)
				return 1
			} else if n > 0 {
				fmt.Fprintf(stderr, "[session] rewound %s: deleted %d turn(s)\n", *resume, n)
			}
		}
		turns, err := st.LoadTurns(ctx, *resume)
		if err != nil {
			fmt.Fprintf(stderr, "harness: resume session %s: %v\n", *resume, err)
			return 1
		}
		seed = agent.FoldTurns(turns)
		fmt.Fprintf(stderr, "[session] resumed %s (%d persisted element(s))\n", *resume, len(turns))
	}

	var wf *agent.Workflow
	if *workflow != "" {
		if mode != agent.ModeAutomator {
			fmt.Fprintln(stderr, "harness: --workflow requires --mode automator")
			return 2
		}
		raw, err := os.ReadFile(*workflow)
		if err != nil {
			fmt.Fprintf(stderr, "harness: read workflow: %v\n", err)
			return 2
		}
		var decoded agent.Workflow
		if err := json.Unmarshal(raw, &decoded); err != nil {
			fmt.Fprintf(stderr, "harness: decode workflow %s: %v\n", *workflow, err)
			return 2
		}
		wf = &decoded
	}

	sink := &consoleSink{out: stdout, diag: stderr}
	approver := newApprover(stdin, stderr, *allowAll, isTTY(stdin))
	logger := telemetry.NewLogger(telemetry.LoggerOptions{Writer: stderr})
	tracer := telemetry.NewTracer(logger)

	ag := &agent.Agent{
		Provider:      prov,
		Model:         *model,
		Mode:          mode,
		Env:           tools.Env{Workspace: ws, AllowWrite: mode != agent.ModeReviewer, AllowNet: *allowNet, Sandbox: sandbox.New()},
		Store:         st,
		Tracer:        tracer,
		Logger:        logger,
		Sink:          sink,
		Approver:      approver,
		MaxTurns:      *maxTurns,
		Window:        *window,
		Seed:          seed,
		ResumeSession: *resume,
	}

	if wf != nil {
		return runWorkflow(ctx, ag, wf, stderr)
	}
	return runTask(ctx, ag, task, stderr)
}

// runTask executes one autonomous task and reports the outcome.
func runTask(ctx context.Context, ag *agent.Agent, task string, stderr io.Writer) int {
	rep, err := ag.Run(ctx, task)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "\n[session] id=%s stopped=%s turns=%d tokens=%d\n",
		displayID(rep.SessionID), rep.StoppedBy, rep.Turns, rep.Usage.TotalTokens)
	switch rep.StoppedBy {
	case "final":
		return 0
	case "escalation":
		fmt.Fprintf(stderr, "[session] escalated to human: %s\n", rep.Escalation)
		return 3
	case "max_turns":
		fmt.Fprintln(stderr, "[session] turn budget exhausted")
		return 2
	case "canceled":
		fmt.Fprintln(stderr, "[session] canceled")
		return 130
	}
	return 1
}

// runWorkflow executes a DAG workflow and reports step outcomes.
func runWorkflow(ctx context.Context, ag *agent.Agent, wf *agent.Workflow, stderr io.Writer) int {
	rep, err := ag.RunWorkflow(ctx, *wf)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return 1
	}
	for _, s := range rep.Steps {
		if s.Status == "ok" {
			fmt.Fprintf(stderr, "[step] %s ok\n", s.ID)
			continue
		}
		fmt.Fprintf(stderr, "[step] %s failed: %s\n", s.ID, displayExcerpt(s.Err, 2000))
	}
	if rep.Failed != "" {
		fmt.Fprintf(stderr, "[workflow] %q aborted at step %q\n", wf.Name, rep.Failed)
		return 2
	}
	fmt.Fprintf(stderr, "[workflow] %q completed (%d steps)\n", wf.Name, len(rep.Steps))
	return 0
}

// buildProvider constructs the configured model client.
func buildProvider(kind, endpoint string) (llm.Provider, error) {
	switch kind {
	case "openai":
		return llm.NewOpenAI(endpoint, envKeyFor("openai"), "", llm.DefaultRetry)
	case "anthropic":
		return llm.NewAnthropic(endpoint, envKeyFor("anthropic"), "", llm.DefaultRetry)
	case "ollama":
		return llm.NewOllama(endpoint, "", llm.DefaultRetry)
	}
	return nil, fmt.Errorf("unknown provider %q (want openai, anthropic, or ollama)", kind)
}

func envKeyFor(kind string) string {
	switch kind {
	case "openai":
		return os.Getenv("OPENAI_API_KEY")
	case "anthropic":
		return os.Getenv("ANTHROPIC_API_KEY")
	}
	return ""
}

func envKeyNameFor(kind string) string {
	switch kind {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	}
	return ""
}

// consoleSink streams model text to stdout and all tool activity, events,
// and diagnostics to stderr.
type consoleSink struct {
	mu       sync.Mutex
	sawDelta bool
	out      io.Writer
	diag     io.Writer
}

func (c *consoleSink) OnTextDelta(d string) {
	c.mu.Lock()
	c.sawDelta = true
	_, _ = io.WriteString(c.out, d)
	c.mu.Unlock()
}

func (c *consoleSink) OnToolCall(name string, args json.RawMessage) {
	c.mu.Lock()
	fmt.Fprintf(c.diag, "--> %s %s\n", name, displayExcerpt(strings.TrimSpace(string(args)), 2000))
	c.mu.Unlock()
}

func (c *consoleSink) OnToolResult(name, result string, isError bool) {
	c.mu.Lock()
	mark := ""
	if isError {
		mark = " [error]"
	}
	fmt.Fprintf(c.diag, "<-- %s%s:\n%s\n", name, mark, displayExcerpt(result, 4000))
	c.mu.Unlock()
}

func (c *consoleSink) OnEvent(event string, detail map[string]any) {
	b, err := json.Marshal(detail)
	if err != nil {
		b = []byte("{}")
	}
	c.mu.Lock()
	fmt.Fprintf(c.diag, "[event] %s %s\n", event, b)
	c.mu.Unlock()
}

// newApprover implements interactive human review. Answers: "y" (allow),
// "y: memo" (allow with a note surfaced to the model), anything else
// (deny). Non-interactive stdin without --allow-all denies by default.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

func newApprover(in io.Reader, out io.Writer, autoAllow bool, interactive bool) agent.Approver {
	if !interactive && !autoAllow {
		in = eofReader{}
	}
	return func(req agent.ApprovalRequest) (agent.Decision, error) {
		if autoAllow {
			fmt.Fprintf(out, "[permission] %s auto-allowed\n", req.Tool)
			return agent.Decision{Allow: true}, nil
		}
		fmt.Fprintf(out, "\n[permission] %s %s\n", req.Tool, req.Args)
		if req.Reason != "" {
			fmt.Fprintf(out, "           reason: %s\n", req.Reason)
		}
		fmt.Fprint(out, "approve? (y / y: memo / n) ")
		line, rerr := readLine(in)
		line = strings.TrimSpace(line)
		if rerr != nil && line == "" {
			fmt.Fprintln(out, "(no answer available; denying)")
			return agent.Decision{Allow: false}, nil
		}
		verb, memo, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(verb) {
		case "y", "yes":
			return agent.Decision{Allow: true, Memo: strings.TrimSpace(memo)}, nil
		default:
			return agent.Decision{Allow: false}, nil
		}
	}
}

func readLine(in io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			return strings.TrimRight(sb.String(), "\r\n"), err
		}
	}
	return strings.TrimRight(sb.String(), "\r\n"), nil
}

func isTTY(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func displayID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

func displayExcerpt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n[+%d more bytes]", len(s)-max)
}
