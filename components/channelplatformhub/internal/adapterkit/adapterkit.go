// Package adapterkit holds small helpers shared by the chat-platform adapters (A-100
// phase 3: Slack, Discord, Microsoft Teams), the adapter-kit.ts of core/ts. No
// dependencies: HTTP is net/http.
package adapterkit

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// RequestTimeout bounds one platform API call.
	RequestTimeout = 15 * time.Second
	// MaxBackoff caps the reconnect backoff.
	MaxBackoff = 30 * time.Second
	// MaxRetryAfter: a 429 on a send is retried once when the platform asks for at most
	// this many seconds of waiting.
	MaxRetryAfter = 30
)

// EmDash is U+2014, built from a rune value so the adapters' source stays ASCII-only.
var EmDash = string(rune(0x2014))

// APIError is an error from a platform's HTTP API. RetryAfter is in seconds.
type APIError struct {
	Status     int
	Msg        string
	RetryAfter float64
	// Code is the platform's own error code, e.g. Slack's "invalid_auth".
	Code string
}

func (e *APIError) Error() string { return e.Msg }

// Sleep waits d, or returns false when ctx ends first.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Backoff doubles from 1 s up to MaxBackoff; Reset after a success.
type Backoff struct{ d time.Duration }

// Next returns the wait before the next attempt.
func (b *Backoff) Next() time.Duration {
	if b.d == 0 {
		b.d = time.Second
	}
	d := b.d
	b.d = min(b.d*2, MaxBackoff)
	return d
}

// Reset starts again from 1 s.
func (b *Backoff) Reset() { b.d = 0 }

// RetryAfterHeader is a Retry-After header in seconds, or 0 when absent.
func RetryAfterHeader(res *http.Response) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(res.Header.Get("Retry-After")), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// WithRetry runs call, and once more after the platform's wait when it answered 429 with
// a short Retry-After.
func WithRetry(ctx context.Context, call func() error) error {
	err := call()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.RetryAfter <= 0 || apiErr.RetryAfter > MaxRetryAfter {
		return err
	}
	if !Sleep(ctx, time.Duration(apiErr.RetryAfter*float64(time.Second))) {
		return ctx.Err()
	}
	return call()
}

// Cause strips the *url.Error around a transport error: its message carries the URL,
// which may carry a secret.
func Cause(err error) error {
	var u interface{ Unwrap() error }
	if errors.As(err, &u) && u.Unwrap() != nil {
		return u.Unwrap()
	}
	return err
}
