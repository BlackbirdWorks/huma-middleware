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
	"net/http"
	"net/url"
	"slices"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
)

// operationName is the otelhttp "operation" argument. It is unused because the
// span name always comes from the span name formatter.
const operationName = "huma"

type callKey struct{}

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

// call is the pooled request/response view handed to otelhttp.
type call struct {
	req  http.Request
	hctx huma.Context
	hdr  http.Header
	rhdr http.Header
	next func(huma.Context)
	url  url.URL
	vals [][1]string
	// status is the last status reported to the view.
	status int
}

// Header implements [http.ResponseWriter].
func (c *call) Header() http.Header { return c.rhdr }

// Write implements [http.ResponseWriter]; the body is not observable.
func (c *call) Write(p []byte) (int, error) { return len(p), nil }

// WriteHeader implements [http.ResponseWriter].
func (c *call) WriteHeader(code int) { c.status = code }

// New returns the tracing middleware for huma.
func New(opts ...Option) Middleware {
	cfg := newConfig(opts)
	keys := headerKeys(cfg.propagator)
	pool := &sync.Pool{New: func() any {
		return &call{hdr: make(http.Header, len(keys)), vals: make([][1]string, len(keys)), rhdr: http.Header{}}
	}}

	inner := otelhttp.NewHandler(http.HandlerFunc(serveCall), operationName, cfg.otelOptions()...)

	return func(hctx huma.Context, next func(huma.Context)) {
		route := routeOf(hctx)
		if !cfg.admit(hctx, route) {
			next(hctx)

			return
		}

		c := pool.Get().(*call) //nolint:errcheck // the pool only holds *call
		defer func() {
			c.reset()
			pool.Put(c)
		}()
		c.fill(hctx, next, route, keys)

		r := c.req.WithContext(context.WithValue(hctx.Context(), callKey{}, c))
		inner.ServeHTTP(c, r)
	}
}

// serveCall runs the rest of the huma chain from inside the otelhttp handler
// and reports the status to otelhttp. A request that does not carry a call is
// not one of ours and is ignored.
func serveCall(w http.ResponseWriter, r *http.Request) {
	c, ok := r.Context().Value(callKey{}).(*call)
	if !ok {
		return
	}
	c.next(huma.WithContext(c.hctx, r.Context()))
	if s := c.hctx.Status(); s > 0 {
		w.WriteHeader(s)
	}
}

// Handler wraps a plain [http.Handler] with the same instrumentation and
// options, for routes that bypass huma and for requests huma never routes.
// The route comes from the request Pattern set by [http.ServeMux]; without it
// spans are named after the method only. Options that take a [huma.Context]
// have no effect here; use [WithRequestFilter] instead of [WithFilter].
func Handler(next http.Handler, opts ...Option) http.Handler {
	cfg := newConfig(opts)

	return otelhttp.NewHandler(next, operationName, cfg.otelOptions()...)
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
	for i, k := range keys {
		if val := hctx.Header(k); val != "" {
			c.vals[i][0] = val
			c.hdr[k] = c.vals[i][:]
		}
	}
}

func (c *call) reset() {
	clear(c.hdr)
	clear(c.rhdr)
	clear(c.vals)
	c.req = http.Request{}
	c.url = url.URL{}
	c.hctx, c.next, c.status = nil, nil, 0
}
