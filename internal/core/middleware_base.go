package core

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"watchpoint/internal/types"
)

// responseCapture wraps an http.ResponseWriter to capture the status code
// written by downstream handlers. This is necessary for logging and metrics
// middleware that needs to observe the response status after the handler chain
// completes.
type responseCapture struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

// WriteHeader captures the status code and delegates to the wrapped writer.
func (rc *responseCapture) WriteHeader(code int) {
	if !rc.written {
		rc.statusCode = code
		rc.written = true
	}
	rc.ResponseWriter.WriteHeader(code)
}

// Write ensures the status code is captured even when WriteHeader is not
// called explicitly (the default is 200 per the net/http spec).
func (rc *responseCapture) Write(b []byte) (int, error) {
	if !rc.written {
		rc.statusCode = http.StatusOK
		rc.written = true
	}
	return rc.ResponseWriter.Write(b)
}

// Unwrap returns the underlying ResponseWriter, enabling http.ResponseController
// and other standard library helpers to access it for features like Flush and Hijack.
func (rc *responseCapture) Unwrap() http.ResponseWriter {
	return rc.ResponseWriter
}

// Recoverer catches panics in the handler chain, logs the stack trace
// internally, and writes a standardized types.APIErrorResponse (500) to the
// client. This middleware MUST be the outermost handler in the chain to ensure
// all panics are caught.
func (s *Server) Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rvr := recover(); rvr != nil {
				// Capture stack trace for internal logging.
				stack := debug.Stack()

				s.Logger.Error("panic recovered",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("panic", fmt.Sprintf("%v", rvr)),
					slog.String("stack", string(stack)),
				)

				// Write a standardized 500 JSON error response.
				requestID := types.GetRequestID(r.Context())
				resp := APIErrorResponse{
					Error: ErrorDetail{
						Code:      string(types.ErrCodeInternalUnexpected),
						Message:   "an unexpected error occurred",
						RequestID: requestID,
					},
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				// Best-effort write; if encoding also fails there is nothing
				// more we can do.
				_ = writeJSON(w, resp)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// RequestLogger logs request metadata (method, path, status, duration).
// It explicitly redacts headers defined in redactedHeaders (e.g., Authorization).
// The logger parameter is the application-wide structured logger; redactedHeaders
// is a list of header names (case-insensitive) whose values should be masked
// in log output.
func RequestLogger(logger *slog.Logger, redactedHeaders []string) func(http.Handler) http.Handler {
	// Pre-compute a set of lowercased header names for O(1) lookup.
	redactSet := make(map[string]struct{}, len(redactedHeaders))
	for _, h := range redactedHeaders {
		redactSet[strings.ToLower(h)] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap the response writer to capture the status code.
			rc := &responseCapture{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			next.ServeHTTP(rc, r)

			duration := time.Since(start)

			// Build log attributes.
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rc.statusCode),
				slog.Duration("duration", duration),
				slog.String("remote_addr", r.RemoteAddr),
			}

			// Add request ID if present in context.
			if reqID := types.GetRequestID(r.Context()); reqID != "" {
				attrs = append(attrs, slog.String("request_id", reqID))
			}

			// Add redacted headers for debugging (values masked).
			headerAttrs := []slog.Attr{}
			for name, values := range r.Header {
				lowerName := strings.ToLower(name)
				if _, redact := redactSet[lowerName]; redact {
					headerAttrs = append(headerAttrs, slog.String(name, "[REDACTED]"))
				} else {
					headerAttrs = append(headerAttrs, slog.String(name, strings.Join(values, ", ")))
				}
			}
			if len(headerAttrs) > 0 {
				attrs = append(attrs, slog.Group("headers", attrsToAny(headerAttrs)...))
			}

			// Log at appropriate level based on status code.
			args := attrsToAny(attrs)
			switch {
			case rc.statusCode >= 500:
				logger.Error("request completed", args...)
			case rc.statusCode >= 400:
				logger.Warn("request completed", args...)
			default:
				logger.Info("request completed", args...)
			}
		})
	}
}

// attrsToAny converts a slice of slog.Attr to []any for use with slog methods.
func attrsToAny(attrs []slog.Attr) []any {
	result := make([]any, len(attrs))
	for i, a := range attrs {
		result[i] = a
	}
	return result
}

// MetricsMiddleware records request latency and count metrics for observability.
// It wraps handlers to capture start time, call the next handler, capture
// response status code, and call s.Metrics.RecordRequest.
//
// If s.Metrics is nil (e.g., during tests that don't inject a collector),
// the middleware passes through without recording.
func (s *Server) MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no metrics collector is configured, pass through.
		if s.Metrics == nil {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()

		rc := &responseCapture{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(rc, r)

		duration := time.Since(start)
		status := strconv.Itoa(rc.statusCode)

		s.Metrics.RecordRequest(r.Method, r.URL.Path, status, duration)
	})
}

// SecurityHeadersMiddleware sets standard security response headers on all API
// responses. It executes early in the middleware chain (after RequestID) to
// ensure headers are present regardless of downstream processing or errors.
//
// Headers set:
//   - X-Content-Type-Options: nosniff   (prevents MIME type sniffing)
//   - X-Frame-Options: DENY             (prevents clickjacking)
//   - X-XSS-Protection: 1; mode=block   (enables browser XSS filtering)
func (s *Server) SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		next.ServeHTTP(w, r)
	})
}

// NewCORSMiddleware configures CORS based on the provided allowed origins.
// It handles OPTIONS preflight requests directly and sets Access-Control
// headers on all responses.
//
// Behavior:
//   - If allowedOrigins contains "*", all origins are allowed.
//   - Otherwise, the request Origin header is checked against the allowed list.
//   - Preflight OPTIONS requests receive a 204 No Content response with all
//     necessary CORS headers.
//   - Non-preflight requests receive the CORS headers added before continuing
//     to the next handler.
func NewCORSMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	// Pre-compute whether wildcard is in the allowed list.
	allowAll := false
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
			break
		}
		originSet[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Determine if this origin is allowed.
			var allowedOrigin string
			if allowAll {
				allowedOrigin = "*"
			} else if origin != "" {
				if _, ok := originSet[origin]; ok {
					allowedOrigin = origin
				}
			}

			// Set CORS headers if origin is allowed.
			if allowedOrigin != "" {
				w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token, Idempotency-Key, X-Request-ID")
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.Header().Set("Access-Control-Allow-Credentials", "true")

				// If not wildcard, also set Vary: Origin to ensure
				// proper caching behavior for different origins.
				if allowedOrigin != "*" {
					w.Header().Set("Vary", "Origin")
				}
			}

			// Handle preflight requests.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeJSON is a minimal JSON encoder used only within the Recoverer middleware
// to avoid importing encoding/json in the fast path. Since we are in a panic
// recovery context, we must not risk another panic from json.Marshal.
// Instead, we format the known-safe APIErrorResponse manually.
func writeJSON(w http.ResponseWriter, resp APIErrorResponse) error {
	// Escape any special characters in the fields.
	code := escapeJSON(resp.Error.Code)
	message := escapeJSON(resp.Error.Message)
	requestID := escapeJSON(resp.Error.RequestID)

	s := fmt.Sprintf(
		`{"error":{"code":"%s","message":"%s","request_id":"%s"}}`,
		code, message, requestID,
	)
	_, err := w.Write([]byte(s))
	return err
}

// escapeJSON performs minimal JSON string escaping for known-safe strings
// (error codes and messages that we control). It handles the characters
// that would break JSON parsing.
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
