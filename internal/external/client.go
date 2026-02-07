// Package external provides the anti-corruption layer between WatchPoint domain
// logic and third-party vendor APIs. All outbound HTTP calls are routed through
// the BaseClient, which enforces consistent resilience patterns: circuit breaking,
// retries with exponential backoff, trace propagation, and error mapping.
package external

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"watchpoint/internal/types"

	"github.com/sony/gobreaker/v2"
)

// RetryPolicy configures the retry behavior for the BaseClient.
type RetryPolicy struct {
	MaxRetries int
	MinWait    time.Duration
	MaxWait    time.Duration
}

// DefaultRetryPolicy returns sensible defaults for external API calls.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: 3,
		MinWait:    500 * time.Millisecond,
		MaxWait:    10 * time.Second,
	}
}

// BaseClient wraps an *http.Client and a circuit breaker to enforce consistent
// resilience patterns on all outbound HTTP calls. Provider clients (Stripe,
// OAuth) embed BaseClient to inherit this behavior.
type BaseClient struct {
	client      *http.Client
	breaker     *gobreaker.CircuitBreaker[*http.Response]
	retryPolicy RetryPolicy
	userAgent   string
	sleepFn     func(time.Duration) // for testability; defaults to time.Sleep
}

// BaseClientOption is a functional option for configuring a BaseClient.
type BaseClientOption func(*BaseClient)

// WithSleepFunc overrides the sleep function used between retries.
// This is intended for testing to avoid real delays.
func WithSleepFunc(fn func(time.Duration)) BaseClientOption {
	return func(c *BaseClient) {
		c.sleepFn = fn
	}
}

// NewBaseClient creates a BaseClient with the given http client, circuit breaker
// settings name, retry policy, and user agent string.
func NewBaseClient(
	httpClient *http.Client,
	breakerName string,
	retryPolicy RetryPolicy,
	userAgent string,
	opts ...BaseClientOption,
) *BaseClient {
	cb := gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
		Name:        breakerName,
		MaxRequests: 1,
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures > 5
		},
		IsSuccessful: func(err error) bool {
			return err == nil
		},
	})

	bc := &BaseClient{
		client:      httpClient,
		breaker:     cb,
		retryPolicy: retryPolicy,
		userAgent:   userAgent,
		sleepFn:     time.Sleep,
	}

	for _, opt := range opts {
		opt(bc)
	}

	return bc
}

// NewBaseClientWithBreaker creates a BaseClient with a caller-provided circuit
// breaker. This is useful for testing or when sharing a breaker across clients.
func NewBaseClientWithBreaker(
	httpClient *http.Client,
	breaker *gobreaker.CircuitBreaker[*http.Response],
	retryPolicy RetryPolicy,
	userAgent string,
	opts ...BaseClientOption,
) *BaseClient {
	bc := &BaseClient{
		client:      httpClient,
		breaker:     breaker,
		retryPolicy: retryPolicy,
		userAgent:   userAgent,
		sleepFn:     time.Sleep,
	}

	for _, opt := range opts {
		opt(bc)
	}

	return bc
}

// Do executes the HTTP request with:
//  1. Trace ID injection (X-B3-TraceId from context)
//  2. User-Agent header injection
//  3. Circuit breaker wrapping
//  4. Retry on 429/5xx (respecting Retry-After headers)
//  5. Error mapping to types.AppError
//
// On success (2xx/3xx/4xx other than 429), Do returns the response as-is.
// The caller is responsible for closing the response body.
//
// On exhausted retries or circuit breaker open, Do returns a types.AppError
// with the appropriate upstream error code.
func (c *BaseClient) Do(req *http.Request) (*http.Response, error) {
	// Inject trace ID from context if available.
	if traceID := types.GetRequestID(req.Context()); traceID != "" {
		req.Header.Set("X-B3-TraceId", traceID)
	}

	// Inject User-Agent.
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	// Snapshot the request body so we can replay it on retries.
	// For requests without a body (GET, DELETE), this is a no-op.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, types.NewAppError(
				types.ErrCodeInternalUnexpected,
				"failed to read request body for retry support",
				err,
			)
		}
		req.Body.Close()
	}

	var lastResp *http.Response
	var lastErr error

	maxAttempts := 1 + c.retryPolicy.MaxRetries
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Restore the request body for each attempt.
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		}

		resp, err := c.breaker.Execute(func() (*http.Response, error) {
			r, doErr := c.client.Do(req)
			if doErr != nil {
				return nil, doErr
			}
			// Treat 5xx as errors for the circuit breaker.
			if r.StatusCode >= 500 {
				return r, fmt.Errorf("upstream returned %d", r.StatusCode)
			}
			// Treat 429 as an error for the circuit breaker.
			if r.StatusCode == http.StatusTooManyRequests {
				return r, fmt.Errorf("upstream returned 429")
			}
			return r, nil
		})

		if err == nil {
			// Success -- 2xx/3xx/4xx (not 429).
			return resp, nil
		}

		// Track the last response/error for final error mapping.
		lastErr = err
		if resp != nil {
			// Close previous response body before retry, unless this is the last attempt.
			if attempt < maxAttempts-1 {
				resp.Body.Close()
			} else {
				lastResp = resp
			}
		}

		// If the circuit breaker is open, do not retry.
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			break
		}

		// Only retry on 429 and 5xx.
		if resp != nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			// Non-retryable error status -- return as-is.
			return resp, nil
		}

		// If there are more attempts remaining, sleep before retrying.
		if attempt < maxAttempts-1 {
			wait := c.computeBackoff(attempt, resp)
			c.sleepFn(wait)
		}
	}

	// Close the last response body if we're returning an error.
	if lastResp != nil {
		lastResp.Body.Close()
	}

	return nil, c.mapError(lastResp, lastErr)
}

// computeBackoff determines the wait duration before the next retry attempt.
// It respects the Retry-After header if present, otherwise uses exponential
// backoff with jitter clamped to [MinWait, MaxWait].
func (c *BaseClient) computeBackoff(attempt int, resp *http.Response) time.Duration {
	// Check Retry-After header.
	if resp != nil {
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
				wait := time.Duration(seconds) * time.Second
				if wait > c.retryPolicy.MaxWait {
					wait = c.retryPolicy.MaxWait
				}
				return wait
			}
			// Try parsing as HTTP-date.
			if t, err := http.ParseTime(retryAfter); err == nil {
				wait := time.Until(t)
				if wait <= 0 {
					return c.retryPolicy.MinWait
				}
				if wait > c.retryPolicy.MaxWait {
					wait = c.retryPolicy.MaxWait
				}
				return wait
			}
		}
	}

	// Exponential backoff with full jitter: [0, min(MaxWait, MinWait * 2^attempt)]
	base := float64(c.retryPolicy.MinWait) * math.Pow(2, float64(attempt))
	maxWait := float64(c.retryPolicy.MaxWait)
	if base > maxWait {
		base = maxWait
	}

	// Full jitter: random value in [MinWait, base].
	minWait := float64(c.retryPolicy.MinWait)
	if base <= minWait {
		return c.retryPolicy.MinWait
	}
	jittered := minWait + rand.Float64()*(base-minWait)
	return time.Duration(jittered)
}

// mapError translates HTTP-level failures into domain-level AppErrors.
func (c *BaseClient) mapError(resp *http.Response, err error) *types.AppError {
	// Circuit breaker open.
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return types.NewAppError(
			types.ErrCodeUpstreamRateLimited,
			"circuit breaker is open; upstream service unavailable",
			err,
		)
	}

	// Check for specific HTTP status codes from the last response.
	if resp != nil {
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return types.NewAppError(
				types.ErrCodeUpstreamRateLimited,
				"upstream rate limit exceeded",
				err,
			)
		case resp.StatusCode >= 500:
			return types.NewAppError(
				types.ErrCodeUpstreamUnavailable,
				fmt.Sprintf("upstream returned %d after retries", resp.StatusCode),
				err,
			)
		}
	}

	// Generic upstream failure (network error, DNS failure, etc.).
	return types.NewAppError(
		types.ErrCodeInternalUnexpected,
		"upstream request failed",
		err,
	)
}
