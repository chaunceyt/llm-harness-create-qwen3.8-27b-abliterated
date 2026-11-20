package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"math/rand/v2"
)

// RetryConfig bounds transport-level retry behavior. Retries apply to
// pre-stream failures only (connection setup, headers, rate limits); once a
// stream has begun emitting, failures surface to the caller rather than
// re-driving a partially-consumed completion.
type RetryConfig struct {
	Attempts  int           // total attempts including the first; default 5
	BaseDelay time.Duration // first backoff delay; default 500ms
	MaxDelay  time.Duration // backoff ceiling; default 15s
}

// DefaultRetry is the built-in retry policy.
var DefaultRetry = RetryConfig{
	Attempts:  5,
	BaseDelay: 500 * time.Millisecond,
	MaxDelay:  15 * time.Second,
}

func (c RetryConfig) normalized() RetryConfig {
	if c.Attempts <= 0 {
		c.Attempts = DefaultRetry.Attempts
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = DefaultRetry.BaseDelay
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = DefaultRetry.MaxDelay
	}
	return c
}

// statusError classifies a non-2xx HTTP response.
type statusError struct {
	Code       int
	URL        string
	RetryAfter time.Duration
	Body       string
}

func (e *statusError) Error() string {
	var b string
	if len(e.Body) > 512 {
		b = e.Body[:512] + "…"
	} else {
		b = e.Body
	}
	return fmt.Sprintf("llm: provider returned HTTP %d from %s: %s", e.Code, e.URL, b)
}

// retryableErr reports whether a pre-stream failure merits another attempt.
func retryableErr(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return transientTransport(err)
}

func transientTransport(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE,
			syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return true
		}
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return transientTransport(pe.Err)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return transientTransport(ue.Err)
	}
	return false
}

// classifyResponse converts a non-2xx response into a classified error,
// honoring Retry-After (seconds or HTTP-date) on rate-limited responses.
func classifyResponse(resp *http.Response) error {
	body := make([]byte, 0, 1024)
	if resp.Body != nil {
		br, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err == nil {
			body = br
		}
		_ = resp.Body.Close()
	}
	var ra time.Duration
	if h := resp.Header.Get("Retry-After"); h != "" {
		if sec, perr := time.ParseDuration(h + "s"); perr == nil {
			ra = sec
		} else if t, perr := http.ParseTime(h); perr == nil {
			ra = time.Until(t)
		}
	}
	return &statusError{
		Code:       resp.StatusCode,
		URL:        resp.Request.URL.String(),
		RetryAfter: ra,
		Body:       string(body),
	}
}

// backoffDelay computes the pre-attempt delay for the attempt number (0-based
// count of already-failed attempts) with full jitter.
func backoffDelay(cfg RetryConfig, failed int) time.Duration {
	cfg = cfg.normalized()
	var d time.Duration
	if failed >= 32 {
		d = cfg.MaxDelay
	} else {
		d = cfg.BaseDelay << failed
		if d > cfg.MaxDelay || d <= 0 {
			d = cfg.MaxDelay
		}
	}
	return time.Duration(rand.Float64() * float64(d))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// withRetries runs attempt() under the retry policy, sleeping with
// exponential backoff between retryable failures. Non-retryable failures and
// context cancellation abort immediately. The final error wraps the last
// underlying failure.
func withRetries(ctx context.Context, what string, cfg RetryConfig, attempt func() error) error {
	cfg = cfg.normalized()
	var last error
	for i := 0; i < cfg.Attempts; i++ {
		last = attempt()
		if last == nil {
			return nil
		}
		if !retryableErr(last) {
			return fmt.Errorf("%s: %w", what, last)
		}
		if i == cfg.Attempts-1 {
			break
		}
		delay := backoffDelay(cfg, i)
		var se *statusError
		if errors.As(last, &se) && se.RetryAfter > delay {
			delay = se.RetryAfter
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return fmt.Errorf("%s: failed after %d attempts: %w", what, cfg.Attempts, last)
}
