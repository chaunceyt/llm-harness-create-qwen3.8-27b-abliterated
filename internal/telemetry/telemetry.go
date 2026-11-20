// Package telemetry provides the harness's structured observability core:
// log/slog component logging and lightweight OpenTelemetry-compatible span
// hooks. Spans are in-memory operation records shaped like OTel spans
// (128-bit trace / 64-bit span identifiers, parent linkage, attributes,
// events); completed spans are emitted as structured slog records and offered
// to registered exporters (e.g. audit persistence). Every Tracer is an
// instance; the package holds no mutable global state.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// TraceID is a 128-bit operation correlation identifier.
type TraceID [16]byte

// SpanID is a 64-bit span identifier.
type SpanID [8]byte

func (t TraceID) String() string { return hex.EncodeToString(t[:]) }
func (s SpanID) String() string  { return hex.EncodeToString(s[:]) }

func newIDs() (TraceID, SpanID) {
	var tr TraceID
	var sp SpanID
	if _, err := rand.Read(tr[:]); err != nil {
		panic("telemetry: trace id: " + err.Error())
	}
	if _, err := rand.Read(sp[:]); err != nil {
		panic("telemetry: span id: " + err.Error())
	}
	return tr, sp
}

// Event is one named attribute-bearing marker recorded on a live span.
type Event struct {
	Time  time.Time
	Name  string
	Attrs map[string]any
}

// Record is one completed span: the unit exporters consume.
type Record struct {
	Name       string
	TraceID    TraceID
	SpanID     SpanID
	ParentID   SpanID // zero for roots
	Start      time.Time
	End        time.Time
	Attributes map[string]any
	Events     []Event
}

// Span is one in-flight operation record. Methods are concurrency-safe.
type Span interface {
	Name() string
	ID() SpanID
	TraceID() TraceID
	SetAttribute(key string, value any)
	// Event appends a named marker; attrs are alternating key/value pairs.
	Event(name string, attrs ...any)
	End()
}

type span struct {
	tracer *Tracer
	name   string
	id     SpanID
	trace  TraceID
	parent SpanID
	start  time.Time

	mu     sync.Mutex
	attrs  map[string]any
	events []Event
}

func (s *span) Name() string     { return s.name }
func (s *span) ID() SpanID       { return s.id }
func (s *span) TraceID() TraceID { return s.trace }

func (s *span) SetAttribute(key string, value any) {
	s.mu.Lock()
	s.attrs[key] = value
	s.mu.Unlock()
}

func (s *span) Event(name string, attrs ...any) {
	m := make(map[string]any, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		k, ok := attrs[i].(string)
		if !ok {
			k = slog.AnyValue(attrs[i]).String()
		}
		m[k] = attrs[i+1]
	}
	s.mu.Lock()
	s.events = append(s.events, Event{Time: time.Now(), Name: name, Attrs: m})
	s.mu.Unlock()
}

func (s *span) End() {
	s.mu.Lock()
	rec := Record{
		Name:       s.name,
		TraceID:    s.trace,
		SpanID:     s.id,
		ParentID:   s.parent,
		Start:      s.start,
		End:        time.Now(),
		Attributes: s.attrs,
		Events:     s.events,
	}
	s.mu.Unlock()
	s.tracer.complete(rec)
}

// Tracer starts and completes spans, emitting finished records to a
// component logger and registered exporters.
type Tracer struct {
	logger    *slog.Logger
	exporters []func(Record)
}

// NewTracer builds a tracer. Exporters receive each completed record, in
// registration order, synchronously at End.
func NewTracer(logger *slog.Logger, exporters ...func(Record)) *Tracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracer{logger: logger, exporters: exporters}
}

type spanCtxKey struct{}

// Start begins a span named name, linking it to any span already in ctx.
// The returned context carries the new span for descendant linking.
func (tr *Tracer) Start(ctx context.Context, name string) (context.Context, Span) {
	var (
		trace  TraceID
		parent SpanID
	)
	if prev, ok := SpanFromContext(ctx); ok {
		trace, parent = prev.TraceID(), prev.ID()
	}
	if trace == (TraceID{}) {
		trace, _ = newIDs()
	}
	_, id := newIDs()
	sp := &span{
		tracer: tr,
		name:   name,
		id:     id,
		trace:  trace,
		parent: parent,
		start:  time.Now(),
		attrs:  make(map[string]any),
	}
	return context.WithValue(ctx, spanCtxKey{}, sp), sp
}

// SpanFromContext returns the innermost span carried by ctx, if any.
func SpanFromContext(ctx context.Context) (Span, bool) {
	sp, ok := ctx.Value(spanCtxKey{}).(*span)
	return sp, ok
}

// complete emits one finished record to the component logger and exporters.
func (tr *Tracer) complete(rec Record) {
	args := []any{
		"span.name", rec.Name,
		"span.trace_id", rec.TraceID.String(),
		"span.span_id", rec.SpanID.String(),
		"span.parent_id", rec.ParentID.String(),
		"span.start", rec.Start,
		"span.end", rec.End,
		"span.duration", rec.End.Sub(rec.Start),
	}
	keys := make([]string, 0, len(rec.Attributes))
	for k := range rec.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "span.attr."+k, rec.Attributes[k])
	}
	tr.logger.Info("span completed", args...)
	for _, ex := range tr.exporters {
		ex(rec)
	}
}

// nopTracer serves spans started before any tracer is installed. It is an
// immutable value writing to nowhere.
var nopTracer = &Tracer{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}

// StartSpan begins a span using the tracer carried by ctx (nop when none).
func StartSpan(ctx context.Context, name string) (context.Context, Span) {
	if tr, ok := ctx.Value(tracerCtxKey{}).(*Tracer); ok {
		return tr.Start(ctx, name)
	}
	return nopTracer.Start(ctx, name)
}

type tracerCtxKey struct{}

// ContextWithTracer installs tr for spans started below ctx.
func ContextWithTracer(ctx context.Context, tr *Tracer) context.Context {
	return context.WithValue(ctx, tracerCtxKey{}, tr)
}

// LoggerOptions configures a component logger.
type LoggerOptions struct {
	Writer    io.Writer  // default: stderr
	Level     slog.Level // default: InfoLevel
	JSON      bool       // default: true
	Component string     // default: "harness"
}

// NewLogger builds the structured component logger used across subsystems.
func NewLogger(opts LoggerOptions) *slog.Logger {
	if opts.Writer == nil {
		opts.Writer = os.Stderr
	}
	if opts.Component == "" {
		opts.Component = "harness"
	}
	var h slog.Handler
	if opts.JSON {
		h = slog.NewJSONHandler(opts.Writer, &slog.HandlerOptions{Level: opts.Level})
	} else {
		h = slog.NewTextHandler(opts.Writer, &slog.HandlerOptions{Level: opts.Level})
	}
	return slog.New(h).With("component", opts.Component)
}
