// Package httpx provides the REST server, middleware chain and JSON helpers.
//
// REST is the platform's secondary transport: gRPC is the default for
// service-to-service calls, and this package exists so that switching a service —
// or a single endpoint — to REST is a matter of mounting a second inbound adapter
// over the same use cases, not of rewriting anything.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/google/uuid"
)

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so that the first argument is the outermost layer.
func Chain(middlewares ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			next = middlewares[i](next)
		}
		return next
	}
}

// Header names used by the platform.
const (
	// HeaderRequestID is the caller-supplied or generated request identifier.
	HeaderRequestID = "X-Request-Id"
	// HeaderCorrelationID ties several requests into one business transaction.
	HeaderCorrelationID = "X-Correlation-Id"
	// HeaderIdempotencyKey deduplicates money-moving requests.
	HeaderIdempotencyKey = "Idempotency-Key"
)

// RequestID assigns every request an identifier and echoes it back.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(HeaderRequestID)
			if requestID == "" {
				requestID = uuid.NewString()
			}
			correlationID := r.Header.Get(HeaderCorrelationID)
			if correlationID == "" {
				correlationID = requestID
			}

			w.Header().Set(HeaderRequestID, requestID)
			w.Header().Set(HeaderCorrelationID, correlationID)

			ctx := logx.WithCorrelationID(r.Context(), correlationID)
			ctx = withRequestID(ctx, requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type requestIDKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request identifier, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Logging emits one structured line per request.
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Probe traffic would drown out everything else at info level.
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			ctx := logx.WithLogger(r.Context(), logger)
			r = r.WithContext(ctx)

			start := time.Now()
			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)
			duration := time.Since(start)

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Int64("duration_ms", duration.Milliseconds()),
				slog.Int64("bytes", recorder.written),
				slog.String("request_id", RequestIDFrom(ctx)),
			}
			if principal, ok := authn.PrincipalFrom(ctx); ok {
				attrs = append(attrs,
					slog.String("user_id", principal.UserID),
					slog.String("role", principal.Role.String()),
				)
			}

			entry := logx.FromContext(ctx)
			switch {
			case recorder.status >= 500:
				entry.Error("request failed", attrs...)
			case recorder.status >= 400:
				entry.Warn("request rejected", attrs...)
			default:
				entry.Info("request handled", attrs...)
			}
		})
	}
}

// Recover turns a panic into a 500 instead of taking the process down.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				// A client disconnecting mid-write is normal, not a bug: net/http uses
				// this sentinel panic to abort a response, so let it through.
				if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(recovered)
				}
				logx.FromContext(r.Context()).Error("panic while handling request",
					slog.Any("panic", recovered),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
				errs.WriteProblem(w, errs.Internal("internal error"), logx.TraceID(r.Context()))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler execution.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MaxBodyBytes caps request bodies so that a large upload cannot exhaust memory.
func MaxBodyBytes(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics is the sink the middleware reports to. pkg/metrics implements it.
type Metrics interface {
	ObserveRPC(transport, method, code string, duration time.Duration)
	IncInFlight(transport string)
	DecInFlight(transport string)
}

// Instrument records RED metrics per route.
func Instrument(m Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			m.IncInFlight("http")
			defer m.DecInFlight("http")

			start := time.Now()
			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)

			m.ObserveRPC("http", r.Method+" "+routePattern(r), strconv.Itoa(recorder.status), time.Since(start))
		})
	}
}

// Authenticate verifies the bearer token and puts the Principal on the context.
//
// Unauthenticated requests are allowed through with no principal attached, so
// that a handler can decide for itself whether it needs one. Every wallet handler
// does require one — that decision belongs with the use case, not the transport.
func Authenticate(verifier *authn.Verifier) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				next.ServeHTTP(w, r)
				return
			}

			token, err := authn.BearerToken(header)
			if err != nil {
				errs.WriteProblem(w, err, logx.TraceID(r.Context()))
				return
			}
			principal, err := verifier.Verify(token)
			if err != nil {
				errs.WriteProblem(w, err, logx.TraceID(r.Context()))
				return
			}
			next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), principal)))
		})
	}
}

// SecurityHeaders sets conservative defaults on every response.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			// The API returns JSON only; a strict CSP costs nothing and blocks a
			// reflected-content class of attack outright.
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			next.ServeHTTP(w, r)
		})
	}
}

// responseRecorder captures the status code and byte count for logging and
// metrics.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Flush forwards to the wrapped writer when it supports flushing, so that
// streaming responses are not buffered by the recorder.
func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func isProbePath(path string) bool {
	switch path {
	case "/livez", "/readyz", "/healthz", "/metrics":
		return true
	default:
		return false
	}
}

// routePattern returns the registered route pattern for a request, falling back
// to the raw path. Go 1.22+ exposes the pattern on the request itself.
func routePattern(r *http.Request) string {
	if pattern := r.Pattern; pattern != "" {
		// The pattern includes the method; strip it so the caller can prefix its own.
		if idx := strings.Index(pattern, " "); idx >= 0 {
			return strings.TrimSpace(pattern[idx+1:])
		}
		return pattern
	}
	return r.URL.Path
}

// DecodeJSON reads a JSON request body into target with strict field checking.
//
// Unknown fields are rejected rather than silently dropped: a client that
// misspells "amount_minor" should be told so, not have its payment interpreted as
// a zero-amount request.
func DecodeJSON(r *http.Request, target any) error {
	if r.Body == nil {
		return errs.InvalidArgument("a request body is required")
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return errs.InvalidArgument("Content-Type must be application/json")
	}

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		var maxBytes *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return errs.InvalidArgument("the request body is empty")
		case errors.As(err, &maxBytes):
			return errs.InvalidArgument("the request body exceeds %d bytes", maxBytes.Limit)
		default:
			return errs.InvalidArgument("the request body is not valid JSON: %s", err.Error()).WithCause(err)
		}
	}
	// A second value in the stream means the client sent two documents.
	if decoder.More() {
		return errs.InvalidArgument("the request body must contain exactly one JSON document")
	}
	return nil
}

// WriteJSON renders a successful response.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError renders a failure as a problem document.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	logger := logx.FromContext(r.Context())
	code := errs.CodeOf(err)
	if code == errs.CodeInternal {
		// The full cause is logged here because it is deliberately withheld from the
		// response body.
		logger.Error("unhandled error", slog.String("error", err.Error()))
	} else {
		logger.Debug("request rejected",
			slog.String("code", string(code)),
			slog.String("error", err.Error()),
		)
	}
	errs.WriteProblem(w, err, logx.TraceID(r.Context()))
}

// IdempotencyKey returns the request's Idempotency-Key header, or "".
func IdempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(HeaderIdempotencyKey))
}

// PathValue reads a path parameter and rejects an empty one.
func PathValue(r *http.Request, name string) (string, error) {
	value := strings.TrimSpace(r.PathValue(name))
	if value == "" {
		return "", errs.InvalidArgument("the path parameter %q is required", name)
	}
	return value, nil
}

// PathUUID reads a path parameter that must be a UUID.
//
// Without this, a malformed id travels all the way to PostgreSQL and comes back as
// `invalid input syntax for type uuid` — a 500 with a database error in the log, for a request
// the caller simply got wrong. The status was the visible half of the problem; the worse half is
// that an unvalidated path parameter reaching a query is the shape of a whole class of bugs, so
// this is checked at the edge where the value enters the system.
//
// 400, not 404. A well-formed id that does not exist is a missing resource and is already a 404
// from the repository; "null" is not an id at all, and telling a client the resource was not
// found would send them looking for it.
func PathUUID(r *http.Request, name string) (string, error) {
	value, err := PathValue(r, name)
	if err != nil {
		return "", err
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		// The offending value is echoed because it came from the caller's own URL — there is
		// nothing to leak, and "the id is not a UUID" without saying which id is not actionable
		// on a page that makes several of these calls.
		return "", errs.InvalidArgument("the path parameter %q must be a UUID, got %q", name, value)
	}
	// The canonical form, so a valid but oddly-cased or brace-wrapped id becomes the same string
	// every layer below sees. uuid.Parse accepts `{...}` and `urn:uuid:...`; the database does
	// not, and passing the raw value through would move this bug one layer down.
	return parsed.String(), nil
}

// QueryInt reads an integer query parameter, returning def when absent.
func QueryInt(r *http.Request, name string, def int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errs.InvalidArgument("the query parameter %q must be an integer, got %q", name, raw)
	}
	return value, nil
}
