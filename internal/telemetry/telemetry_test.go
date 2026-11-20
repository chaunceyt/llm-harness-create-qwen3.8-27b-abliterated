package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestSpanLifecycle(t *testing.T) {
	var records []Record
	tr := NewTracer(
		NewLogger(LoggerOptions{Writer: io.Discard, JSON: false}),
		func(r Record) { records = append(records, r) },
	)

	ctx, root := tr.Start(context.Background(), "agent.turn")
	root.SetAttribute("model", "m1")
	root.Event("marker", "k", "v", 42, "n")

	ctx2, child := tr.Start(ctx, "llm.call")
	child.SetAttribute("endpoint", "http://x")
	child.End()

	ctx3, grand := tr.Start(ctx2, "inner")
	if _, ok := SpanFromContext(ctx3); !ok {
		t.Fatal("grandchild context lost its span")
	}
	grand.End()
	root.End()

	if len(records) != 3 {
		t.Fatalf("exporter saw %d records, want 3", len(records))
	}
	// End order: innermost finishes first.
	if records[0].Name != "llm.call" || records[1].Name != "inner" || records[2].Name != "agent.turn" {
		t.Fatalf("end order = %v", []string{records[0].Name, records[1].Name, records[2].Name})
	}
	// One correlation id across the whole tree.
	if records[0].TraceID != records[2].TraceID || records[1].TraceID != records[2].TraceID {
		t.Error("trace ids diverged within one tree")
	}
	if records[0].TraceID == (TraceID{}) {
		t.Error("root trace id is zero")
	}
	// Parent linkage.
	if records[0].ParentID != records[2].SpanID {
		t.Error("child parent linkage broken")
	}
	if records[1].ParentID != records[0].SpanID {
		t.Error("grandchild parent linkage broken")
	}
	if records[2].ParentID != (SpanID{}) {
		t.Error("root must have no parent")
	}
	// Attribute and event capture.
	if got := records[2].Attributes["model"]; got != "m1" {
		t.Errorf("root attribute = %v", got)
	}
	if got := records[0].Attributes["endpoint"]; got != "http://x" {
		t.Errorf("child attribute = %v", got)
	}
	if len(records[2].Events) != 1 || records[2].Events[0].Name != "marker" {
		t.Fatalf("events = %+v", records[2].Events)
	}
	if records[2].Events[0].Attrs["k"] != "v" || records[2].Events[0].Attrs["42"] != "n" {
		t.Errorf("event attrs = %v", records[2].Events[0].Attrs)
	}
	if !records[0].End.After(records[0].Start) {
		t.Error("end not after start")
	}
}

func TestSlogEmission(t *testing.T) {
	var buf bytes.Buffer
	tr := NewTracer(NewLogger(LoggerOptions{Writer: &buf, JSON: true, Component: "test"}))
	ctx, sp := tr.Start(context.Background(), "agent.turn")
	sp.SetAttribute("mode", "coder")
	sp.End()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("emitted %d log lines, want 1", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line not JSON: %v\n%s", err, lines[0])
	}
	if rec["msg"] != "span completed" || rec["component"] != "test" {
		t.Errorf("log shape = %v", rec)
	}
	if rec["span.name"] != "agent.turn" {
		t.Errorf("span.name = %v", rec["span.name"])
	}
	if tid, _ := rec["span.trace_id"].(string); len(tid) != 32 {
		t.Errorf("trace id = %q", tid)
	}
	if sid, _ := rec["span.span_id"].(string); len(sid) != 16 {
		t.Errorf("span id = %q", sid)
	}
	if rec["span.attr.mode"] != "coder" {
		t.Errorf("attribute not emitted: %v", rec)
	}
	_ = ctx
}

func TestStartSpanContextRouting(t *testing.T) {
	var records []Record
	tr := NewTracer(NewLogger(LoggerOptions{Writer: io.Discard}), func(r Record) {
		records = append(records, r)
	})

	// Without a tracer installed, spans complete silently and safely.
	if _, sp := StartSpan(context.Background(), "orphan"); sp == nil {
		t.Fatal("orphan span nil")
	} else {
		sp.End()
	}
	if len(records) != 0 {
		t.Errorf("orphan span reached the tracer")
	}

	// With a tracer installed, the same call routes to it.
	ctx := ContextWithTracer(context.Background(), tr)
	if _, sp := StartSpan(ctx, "routed"); sp == nil {
		t.Fatal("routed span nil")
	} else {
		sp.End()
	}
	if len(records) != 1 || records[0].Name != "routed" {
		t.Fatalf("routed span not captured: %+v", records)
	}
}

func TestSpanConcurrency(t *testing.T) {
	tr := NewTracer(NewLogger(LoggerOptions{Writer: io.Discard}))
	_, sp := tr.Start(context.Background(), "storm")
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				sp.SetAttribute("k"+itoa(g)+"_"+itoa(i), i)
				sp.Event("tick", "g", g)
			}
		}(g)
	}
	wg.Wait()
	sp.End()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	j := len(b)
	for i > 0 {
		j--
		b[j] = byte('0' + i%10)
		i /= 10
	}
	return string(b[j:])
}
