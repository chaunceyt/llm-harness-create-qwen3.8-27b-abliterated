package llm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

var errSSEDone = fmt.Errorf("sse done sentinel")

// pumpLines reads newline-delimited records from r until EOF or context
// cancellation, invoking onLine for each record. It closes r on cancellation
// and reports ctx.Err() when cancellation won the race. onLine errors abort
// the pump immediately.
func pumpLines(ctx context.Context, r io.ReadCloser, onLine func(string) error) error {
	done := make(chan struct{})
	var (
		scanErr error
		wg      sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			if err := onLine(string(sc.Bytes())); err != nil {
				scanErr = err
				return
			}
		}
		scanErr = sc.Err()
	}()
	select {
	case <-ctx.Done():
		_ = r.Close()
		wg.Wait()
		return fmt.Errorf("llm: stream cancelled: %w", ctx.Err())
	case <-done:
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = r.Close()
			return fmt.Errorf("llm: stream cancelled: %w", ctxErr)
		}
		if scanErr != nil {
			_ = r.Close()
		}
		return scanErr
	}
}

// pumpSSE pumps a server-sent-events stream, invoking onData with the payload
// of each `data:` field. Comment (`:`) and event-name lines are ignored; a
// `data: [DONE]` marker ends the stream cleanly. The three supported wire
// dialects (OpenAI-compatible, Anthropic) emit one JSON document per data
// field, which this framing preserves exactly.
func pumpSSE(ctx context.Context, r io.ReadCloser, onData func(string) error) error {
	return pumpLines(ctx, r, func(line string) error {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			return nil
		}
		var payload string
		switch {
		case strings.HasPrefix(line, "data:"):
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case strings.HasPrefix(line, "data"):
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data"))
		default:
			// event:/id:/retry: fields carry no payload we need.
			return nil
		}
		if payload == "[DONE]" {
			return errSSEDone
		}
		return onData(payload)
	})
}
