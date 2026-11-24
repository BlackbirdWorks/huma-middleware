// Package logging puts a request-scoped [log/slog] logger into the context and
// writes one access log record per request. It only uses the huma.Context
// interface, [log/slog] and the OpenTelemetry API (to read trace and span IDs;
// the SDK is not imported).
//
// # Request-scoped logger
//
// For each request New derives a logger from the root logger given with
// [WithLogger] (or [slog.Default], resolved per request, when none was given)
// and stores it in the request context; [FromContext] returns it. It carries
// the method, route template, operation ID, request ID (header "X-Request-ID"
// by default) and, when a span is in the context, trace_id and span_id. The
// attributes are added when a record is handled, not when the logger is
// created, so a disabled level costs nothing and nothing is formatted twice.
// Install the tracing middleware before this one so the span exists.
//
// [WithEnrichLogs] lets the application add its own attributes (user, tenant,
// custom request IDs) before the handlers run. [AddAttrs] adds attributes later
// in the request, for example once authentication has run; they appear on the
// handler logs written afterwards and on the access log.
//
// # Access log
//
// After the response one record is written at the level of the status class
// ([Levels], Error for 5xx) with the path (never the query string, which can
// hold secrets), status actually sent, duration, response size, remote address
// and user agent. Selected request headers can be added with [WithHeaders];
// Authorization, cookies and API key headers are redacted by default.
// [WithAccessLogFunc] replaces the record entirely, for applications with an
// established log format.
//
// The response size counts the bytes handed to the next writer. When
// compression runs inside this middleware that is the compressed size.
//
// [Handler] applies the same logic to a plain [http.Handler], for routes that
// do not go through huma.
package logging

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/trace"
)

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

// Record is what the access log knows about a finished request. It is passed
// by value and is safe to keep.
type Record struct {
	hctx        huma.Context
	req         *http.Request
	UserAgent   string
	OperationID string
	Host        string
	RemoteAddr  string
	Method      string
	RequestID   string
	Route       string
	Path        string
	// Extra holds the attributes added with [AddAttrs].
	Extra    []slog.Attr
	Duration time.Duration
	// Size is the number of response body bytes written.
	Size   int64
	Status int
}

// Header returns a request header value.
func (r Record) Header(name string) string {
	if r.req != nil {
		return r.req.Header.Get(name)
	}

	return r.hctx.Header(name)
}

// RequestURI returns the request target (path and raw query).
func (r Record) RequestURI() string {
	if r.req != nil {
		return r.req.RequestURI
	}
	u := r.hctx.URL()

	return u.RequestURI()
}

// New returns the logging middleware for huma.
func New(opts ...Option) Middleware {
	cfg := newConfig(opts)

	return func(hctx huma.Context, next func(huma.Context)) {
		var route, opID string
		if op := hctx.Operation(); op != nil {
			route, opID = op.Path, op.OperationID
		}
		var reqID string
		if cfg.requestID != "" {
			reqID = hctx.Header(cfg.requestID)
		}
		start := cfg.clock()
		st := newState(cfg, cfg.root(), hctx.Method(), route, opID, reqID, trace.SpanContextFromContext(hctx.Context()))
		for _, fn := range cfg.enrich {
			if l := fn(hctx, st.logger); l != nil {
				st.logger = l
			}
		}
		ctx := context.WithValue(hctx.Context(), stateKey{}, st)
		st.counter.w = hctx.BodyWriter()
		st.hwrap = wrapper{base: hctx, ctx: ctx, st: st}

		next(&st.hwrap)

		if cfg.noAccess || (cfg.skip != nil && cfg.skip(hctx)) {
			return
		}
		u := hctx.URL()
		cfg.access(ctx, st, Record{
			Method:      st.method,
			Path:        u.Path,
			Route:       route,
			OperationID: opID,
			Host:        hctx.Host(),
			RemoteAddr:  hctx.RemoteAddr(),
			UserAgent:   hctx.Header("User-Agent"),
			RequestID:   reqID,
			Duration:    cfg.clock().Sub(start),
			Size:        st.counter.n,
			Status:      statusOf(hctx.Status()),
			hctx:        hctx,
		})
	}
}

func statusOf(s int) int {
	if s == 0 {
		return http.StatusOK
	}

	return s
}

// Handler wraps a plain [http.Handler] with the same request logger and access
// log, for routes that bypass huma. The route is the [http.Request] Pattern
// set by [http.ServeMux] after routing, so it is only available to the access
// log record; the operation ID is empty.
func Handler(next http.Handler, opts ...Option) http.Handler {
	cfg := newConfig(opts)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqID string
		if cfg.requestID != "" {
			reqID = r.Header.Get(cfg.requestID)
		}
		start := cfg.clock()
		st := newState(cfg, cfg.root(), r.Method, "", "", reqID, trace.SpanContextFromContext(r.Context()))
		for _, fn := range cfg.reqEnrich {
			if l := fn(r, st.logger); l != nil {
				st.logger = l
			}
		}
		ctx := context.WithValue(r.Context(), stateKey{}, st)
		rw := &respWriter{ResponseWriter: w}

		rr := r.WithContext(ctx)
		next.ServeHTTP(rw, rr)

		if cfg.noAccess || (cfg.reqSkip != nil && cfg.reqSkip(r)) {
			return
		}
		route := routePart(rr.Pattern)
		st.setRoute(route)
		cfg.access(ctx, st, Record{
			Method:     r.Method,
			Path:       r.URL.Path,
			Route:      route,
			Host:       r.Host,
			RemoteAddr: r.RemoteAddr,
			UserAgent:  r.UserAgent(),
			RequestID:  reqID,
			Duration:   cfg.clock().Sub(start),
			Size:       rw.n,
			Status:     statusOf(rw.status),
			req:        r,
		})
	})
}

// routePart strips the optional "METHOD " prefix of a [http.ServeMux] pattern.
func routePart(pattern string) string {
	for i := range len(pattern) {
		if pattern[i] == '/' {
			return pattern[i:]
		}
	}

	return ""
}

// respWriter records the status and size of a plain handler response.
type respWriter struct {
	http.ResponseWriter

	n      int64
	status int
}

func (w *respWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)

	return n, err //nolint:wrapcheck // transparent pass-through of the response writer error
}

// Flush keeps streaming responses working.
func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to [http.ResponseController].
func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
