// Package logging puts a request-scoped [log/slog] logger into the context. It only uses the huma.Context
// interface, [log/slog] and the OpenTelemetry API (to read trace and span IDs; the SDK is not imported).
//
// # Request-scoped logger
//
// For each request New derives a logger from the root logger given with [WithLogger] (or [slog.Default], resolved
// per request, when none was given) and stores it in the request context; [FromContext] returns it. It carries the
// method, route template, operation ID, request ID (header "X-Request-ID" by default) and, when a span is in the
// context, trace_id and span_id. The attributes are added when a record is handled, not when the logger is created,
// so a disabled level costs nothing and nothing is formatted twice. Install the tracing middleware before this one
// so the span exists.
//
// [WithEnrichLogs] lets the application add its own attributes (user, tenant, custom request IDs) before the
// handlers run. [AddAttrs] adds attributes later in the request, for example once authentication has run; they
// appear on the handler logs written afterwards and on the access log.
//
// # With the access log
//
// The access log is the accesslog package. When this middleware is installed before it (outside it), the access
// record carries what this one knows about the request: the root logger with the enrichers' additions, the
// attributes added with [AddAttrs] and the trace and span IDs. Installed the other way round, or alone, each works
// as if the other were not there.
//
// [Handler] applies the same logic to a plain [http.Handler], for routes that do not go through huma.
package logging

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/trace"
)

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

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
			reqID = hctx.Header(cfg.idKey)
		}
		parent := hctx.Context()
		st := newState(parent, cfg, cfg.root(), hctx.Method(), route, opID, reqID, trace.SpanContextFromContext(parent))
		for _, fn := range cfg.enrich {
			if l := fn(hctx, st.requestLogger()); l != nil {
				st.logger = st.adopt(cfg, l)
			}
		}
		var ctx context.Context = st
		st.wrap = wrapper{base: hctx, ctx: ctx}

		next(&st.wrap)
	}
}

// Handler wraps a plain [http.Handler] with the same request logger, for routes that bypass huma. The route is
// known to the access log only (the [http.Request] Pattern is set by [http.ServeMux] after routing); the operation
// ID is empty. The pattern and the parsed multipart form of the request the handler received are copied back to the
// request this handler received, so wrappers around it (and net/http, which removes the temporary files of uploads)
// see them.
func Handler(next http.Handler, opts ...Option) http.Handler {
	cfg := newConfig(opts)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqID string
		if v := r.Header[cfg.idKey]; cfg.requestID != "" && len(v) > 0 {
			reqID = v[0]
		}
		st := newState(r.Context(), cfg, cfg.root(), r.Method, "", "", reqID, trace.SpanContextFromContext(r.Context()))
		for _, fn := range cfg.reqEnrich {
			if l := fn(r, st.requestLogger()); l != nil {
				st.logger = st.adopt(cfg, l)
			}
		}
		rr := r.WithContext(st)
		defer giveBack(r, rr)
		next.ServeHTTP(w, rr)
	})
}

// giveBack hands the multipart form (whose temporary files net/http removes through the original request) and
// the routing pattern that the copy of a request learned to the request it was made from.
func giveBack(r, cp *http.Request) {
	if cp.MultipartForm != nil {
		r.MultipartForm = cp.MultipartForm
	}
	if cp.Pattern != "" {
		r.Pattern = cp.Pattern
	}
}
