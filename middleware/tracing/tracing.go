// Package tracing is OpenTelemetry server instrumentation for huma v2 built on
// [go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp].
//
// # How otelhttp is reused
//
// otelhttp only offers an [http.Handler]; huma middleware sees a [huma.Context].
// New therefore builds one otelhttp handler at construction time and, per
// request, drives it through a thin, pooled [http.Request] and
// [http.ResponseWriter] view of the huma context: method, URL, host, remote
// address, protocol, TLS state and the few request headers otelhttp reads (the
// propagator fields, User-Agent and X-Forwarded-For) go in; the status code
// comes back out. The inner handler calls next with the span context attached
// via [huma.WithContext] and reports [huma.Context.Status] to otelhttp, so
// span naming, attributes, status mapping (5xx is an error), propagation and
// the HTTP server metrics are otelhttp's own code, not a copy of it.
//
// The route template is handed to otelhttp as the request Pattern, which is
// how otelhttp learns http.route for net/http routers. The span is therefore
// named "METHOD /path/{template}" from the start (low cardinality) and the
// metrics carry the same http.route label.
//
// # Limits
//
// With [Handler], a panicking request is recorded on its span but is missing
// from the HTTP server metrics: reporting the 500 to otelhttp would mean writing
// it to the client before a recovery middleware decides on the response. [New]
// reports it, because its response view is not the client's.
//
// A huma middleware sees neither the raw request body nor the raw response
// writer, so compared with otelhttp on a real [http.Handler]:
//
//   - http.server.request.body.size and http.server.response.body.size
//     (and the matching span attributes) are reported as 0: the bytes are
//     read and written by huma, not through the view.
//   - WithMessageEvents ("read"/"write" span events) is not offered.
//   - Requests that never reach huma (unmatched routes: 404/405, raw
//     net/http handlers) are not seen; wrap those with [Handler].
//   - Only the request headers listed above are visible to the view, so a
//     custom propagator that reads other headers than the ones it declares in
//     Fields will not find them.
//   - A panic in a handler is not recorded as an error status by otelhttp.
//
// # Cost
//
// The view is pooled, headers are copied without allocating and a filtered
// request never enters otelhttp. See the benchmarks in this package.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// operationName is the otelhttp "operation" argument. It is unused because the
// span name always comes from the span name formatter.
const operationName = "huma"

type callKey struct{}

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

// call is the pooled request/response view handed to otelhttp.
type call struct {
	req        http.Request
	hctx       huma.Context
	panicValue any
	hdr        http.Header
	rhdr       http.Header
	next       func(huma.Context)
	// each is addHeader as a value, created once so that copying headers does not allocate.
	each func(name, value string)
	url  url.URL
	vals [][1]string
	// used counts the entries of vals taken by addHeader.
	used     int
	status   int
	panicked bool
}

// Header implements [http.ResponseWriter].
func (c *call) Header() http.Header { return c.rhdr }

// Write implements [http.ResponseWriter]; the body is not observable.
func (c *call) Write(p []byte) (int, error) { return len(p), nil }

// WriteHeader implements [http.ResponseWriter].
func (c *call) WriteHeader(code int) { c.status = code }

// New returns the tracing middleware for huma.
//
// When tracing and metrics are both disabled (see [WithEnabled]) it returns a
// pass-through that adds no allocation. When the rest of the chain panics, the
// panic is recorded on the span (exception event with a stack trace, error
// status, status code 500, so otelhttp also records the 500 in its metrics),
// the span is ended, and the panic then continues with its original value for
// a recovery middleware to handle.
func New(opts ...Option) Middleware {
	cfg := newConfig(opts)
	if cfg.disabled() {
		return passthrough
	}
	var keys []string
	if cfg.explicitPropagator {
		keys = headerKeys(cfg.propagator)
	}

	return newMiddleware(cfg, keys, newPoolStore(keys))
}

// viewStore hands out the request views and takes them back.
type viewStore interface {
	get() *call
	put(c *call)
}

// poolStore keeps the views in a [sync.Pool].
type poolStore struct {
	pool sync.Pool
}

func newPoolStore(keys []string) *poolStore {
	return &poolStore{pool: sync.Pool{New: func() any { return newCall(keys) }}}
}

func newCall(keys []string) *call {
	c := &call{hdr: make(http.Header, len(keys)), vals: make([][1]string, len(keys)), rhdr: http.Header{}}
	c.each = c.addHeader

	return c
}

func (p *poolStore) get() *call {
	c, _ := p.pool.Get().(*call)

	return c
}

func (p *poolStore) put(c *call) { p.pool.Put(c) }

// newMiddleware builds the huma middleware around a store of views.
func newMiddleware(cfg *config, keys []string, store viewStore) Middleware {
	inner := otelhttp.NewHandler(http.HandlerFunc(serveCall), operationName, cfg.otelOptions(false)...)

	return func(hctx huma.Context, next func(huma.Context)) {
		route := routeOf(hctx)
		if !cfg.admit(hctx, route) {
			next(hctx)

			return
		}

		c := store.get()
		defer func() {
			c.reset()
			store.put(c)
		}()
		c.fill(hctx, next, route, keys)

		r := c.req.WithContext(context.WithValue(hctx.Context(), callKey{}, c))
		inner.ServeHTTP(c, r)
		if c.panicked {
			panic(c.panicValue)
		}
	}
}

// passthrough is the middleware when tracing is disabled.
func passthrough(ctx huma.Context, next func(huma.Context)) { next(ctx) }

// serveCall runs the rest of the huma chain from inside the otelhttp handler
// and reports the status to otelhttp. A request that does not carry a call is
// not one of ours and is ignored.
func serveCall(w http.ResponseWriter, r *http.Request) {
	c, ok := r.Context().Value(callKey{}).(*call)
	if !ok {
		return
	}
	defer func() {
		// Recover so otelhttp finishes the span and records the metrics; the
		// panic is raised again by New once otelhttp returned.
		if v := recover(); v != nil {
			if isAbort(v) {
				panic(v)
			}
			c.panicked, c.panicValue = true, v
			recordPanic(trace.SpanFromContext(r.Context()), v)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	c.next(huma.WithContext(c.hctx, r.Context()))
	if s := c.hctx.Status(); s > 0 {
		w.WriteHeader(s)
	}
}

// PanicError is the error recorded on a span for a panic.
type PanicError struct {
	// Value is what was passed to panic.
	Value any
}

// Error implements error.
func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// isAbort reports whether the panic value is [http.ErrAbortHandler], a deliberate
// abort of the response rather than a failure: it is not recorded and goes
// straight through.
func isAbort(v any) bool {
	err, ok := v.(error)

	return ok && errors.Is(err, http.ErrAbortHandler)
}

// recordPanic records a panic on the span: an exception event with the stack,
// the error status and status code 500.
func recordPanic(span trace.Span, v any) {
	err := &PanicError{Value: v}
	span.RecordError(err, trace.WithStackTrace(true))
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(attribute.Int("http.response.status_code", http.StatusInternalServerError))
}

// Handler wraps a plain [http.Handler] with the same instrumentation and
// options, for routes that bypass huma and for requests huma never routes.
// The route comes from the request Pattern set by [http.ServeMux]; without it
// spans are named after the method only. Options that take a [huma.Context]
// have no effect here; use [WithRequestFilter] instead of [WithFilter].
//
// A panic in next is recorded on the span like in [New] and raised again. Unlike
// [New], the response status is not reported to otelhttp (writing a 500 would
// pre-empt the recovery middleware), so the panicking request is missing from
// the HTTP server metrics, and an SDK that records panics when a span ends
// adds its own exception event next to this one.
func Handler(next http.Handler, opts ...Option) http.Handler {
	cfg := newConfig(opts)
	if cfg.disabled() {
		return next
	}

	inner := otelhttp.NewHandler(recordingHandler(next, cfg.nameFormatter), operationName, cfg.otelOptions(true)...)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// otelhttp serves a copy of the request, on which the router sets the pattern and a handler
		// parses the multipart form: give both back to the request this handler received, so the
		// wrappers around it (and net/http, for the upload files) see them.
		h := &routeHolder{}
		r2 := r.WithContext(context.WithValue(r.Context(), routeKey{}, h))
		defer func() {
			if r2.MultipartForm != nil {
				r.MultipartForm = r2.MultipartForm
			}
			if h.pattern != "" {
				r.Pattern = h.pattern
			}
		}()
		inner.ServeHTTP(w, r2)
	})
}

type routeKey struct{}

// routeHolder carries the routing pattern out of the request copy of otelhttp.
type routeHolder struct {
	pattern string
}

// setRoute sets http.route on the span from the pattern the router matched: the method and
// the host are removed, "GET example.com/x/{id}" is "/x/{id}". Unmatched requests get none.
//
// otelhttp renames the span after the handler returns, but not when it panicked, so the
// name is set here as well.
func setRoute(span trace.Span, r *http.Request, formatter func(method, route string) string) {
	route := routePart(r.Pattern)
	if route == "" {
		return
	}
	span.SetAttributes(attribute.String("http.route", route))
	if formatter == nil {
		formatter = func(method, route string) string { return method + " " + route }
	}
	span.SetName(formatter(r.Method, route))
}

// recordingHandler records a panic of next on the active span and raises it again.
func recordingHandler(next http.Handler, formatter func(method, route string) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The mux sets r.Pattern on this request while routing, after the span started: put the
		// route on the span too (otelhttp only uses it for the span name and the metrics).
		defer setRoute(trace.SpanFromContext(r.Context()), r, formatter)
		defer func() {
			// Only Handler builds this handler, and it always puts the holder in the context.
			h, _ := r.Context().Value(routeKey{}).(*routeHolder)
			h.pattern = r.Pattern
		}()
		defer func() {
			if v := recover(); v != nil {
				if !isAbort(v) {
					recordPanic(trace.SpanFromContext(r.Context()), v)
				}
				panic(v)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func routeOf(ctx huma.Context) string {
	if op := ctx.Operation(); op != nil {
		return op.Path
	}

	return ""
}

func headerKeys(p propagation.TextMapPropagator) []string {
	keys := []string{"User-Agent", "X-Forwarded-For"}
	for _, f := range p.Fields() {
		k := http.CanonicalHeaderKey(f)
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}

	return keys
}

func (c *call) fill(hctx huma.Context, next func(huma.Context), route string, keys []string) {
	c.hctx, c.next = hctx, next
	c.url = hctx.URL()
	v := hctx.Version()
	c.req = http.Request{
		Method:     hctx.Method(),
		URL:        &c.url,
		Proto:      v.Proto,
		ProtoMajor: v.ProtoMajor,
		ProtoMinor: v.ProtoMinor,
		Header:     c.hdr,
		Host:       hctx.Host(),
		RemoteAddr: hctx.RemoteAddr(),
		TLS:        hctx.TLS(),
		Pattern:    route,
	}
	if keys == nil {
		hctx.EachHeader(c.each)

		return
	}
	for i, k := range keys {
		if val := hctx.Header(k); val != "" {
			c.vals[i][0] = val
			c.hdr[k] = c.vals[i][:]
		}
	}
}

// addHeader copies one request header into the view, for propagators that are not known up front.
func (c *call) addHeader(name, value string) {
	key := http.CanonicalHeaderKey(name)
	if _, dup := c.hdr[key]; dup {
		c.hdr.Add(key, value)

		return
	}
	if c.used == len(c.vals) {
		c.vals = append(c.vals, [1]string{})
	}
	c.vals[c.used][0] = value
	c.hdr[key] = c.vals[c.used][:]
	c.used++
}

func (c *call) reset() {
	c.used = 0
	clear(c.hdr)
	clear(c.rhdr)
	clear(c.vals)
	c.req = http.Request{}
	c.url = url.URL{}
	c.hctx, c.next, c.status = nil, nil, 0
	c.panicValue, c.panicked = nil, false
}
