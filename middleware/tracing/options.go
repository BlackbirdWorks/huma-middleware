package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Option configures [New] and [Handler].
type Option func(*config)

type config struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	propagator     propagation.TextMapPropagator
	nameFormatter  func(method, route string) string
	filter         func(huma.Context) bool
	requestFilter  func(*http.Request) bool
	serverName     string
	logger         *slog.Logger
	enabled        *bool
	skipOps        []string
	skipPaths      []string
	spanOptions    []trace.SpanStartOption
	public         bool
}

func newConfig(opts []Option) *config {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	if c.propagator == nil {
		c.propagator = otel.GetTextMapPropagator()
	}

	return c
}

// WithTracerProvider sets the tracer provider. The default is the global one,
// as in otelhttp.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithMeterProvider sets the meter provider used for the otelhttp HTTP server
// metrics. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// WithPropagators sets the propagator used to extract the incoming trace
// context. The default is the global one at the time the middleware is built:
// the header names it reads are fixed then, so set the OpenTelemetry globals
// first or pass the propagator.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(c *config) { c.propagator = p }
}

// WithSpanNameFormatter sets the span name. It receives the request method and
// the route template ("/users/{id}"; empty when unknown) and must return a low
// cardinality name. The default is "METHOD /route".
func WithSpanNameFormatter(f func(method, route string) string) Option {
	return func(c *config) { c.nameFormatter = f }
}

// WithFilter sets a function that decides per request whether it is traced:
// return false to skip. Only used by [New].
func WithFilter(f func(huma.Context) bool) Option {
	return func(c *config) { c.filter = f }
}

// WithRequestFilter is [WithFilter] for [Handler]: return false to skip.
func WithRequestFilter(f func(*http.Request) bool) Option {
	return func(c *config) { c.requestFilter = f }
}

// WithSkipOperations skips the operations with the given operation IDs.
func WithSkipOperations(ids ...string) Option {
	return func(c *config) { c.skipOps = append(c.skipOps, ids...) }
}

// WithSkipPaths skips requests whose route template (for [New]) or URL path
// (for [Handler]) equals one of paths, for example "/healthz".
func WithSkipPaths(paths ...string) Option {
	return func(c *config) { c.skipPaths = append(c.skipPaths, paths...) }
}

// WithPublicEndpoint makes every request start a new trace, linked to the
// incoming remote span context instead of continuing it.
func WithPublicEndpoint() Option {
	return func(c *config) { c.public = true }
}

// WithServerName sets the server name used for the server.address attribute.
func WithServerName(name string) Option {
	return func(c *config) { c.serverName = name }
}

// WithSpanOptions adds span start options to every server span.
func WithSpanOptions(o ...trace.SpanStartOption) Option {
	return func(c *config) { c.spanOptions = append(c.spanOptions, o...) }
}

// admit reports whether the request is traced by [New].
func (c *config) admit(ctx huma.Context, route string) bool {
	if slices.Contains(c.skipPaths, route) {
		return false
	}
	if len(c.skipOps) > 0 {
		if op := ctx.Operation(); op != nil && slices.Contains(c.skipOps, op.OperationID) {
			return false
		}
	}

	return c.filter == nil || c.filter(ctx)
}

func (c *config) otelOptions() []otelhttp.Option {
	opts := []otelhttp.Option{otelhttp.WithPropagators(c.propagator)}
	if c.tracerProvider != nil {
		opts = append(opts, otelhttp.WithTracerProvider(c.tracerProvider))
	}
	if c.meterProvider != nil {
		opts = append(opts, otelhttp.WithMeterProvider(c.meterProvider))
	}
	if c.nameFormatter != nil {
		f := c.nameFormatter
		opts = append(opts, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return f(r.Method, routePart(r.Pattern))
		}))
	}
	if c.requestFilter != nil || len(c.skipPaths) > 0 {
		f, paths := c.requestFilter, c.skipPaths
		opts = append(opts, otelhttp.WithFilter(func(r *http.Request) bool {
			if slices.Contains(paths, r.URL.Path) {
				return false
			}

			return f == nil || f(r)
		}))
	}
	if c.public {
		opts = append(opts, otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
	}
	if c.serverName != "" {
		opts = append(opts, otelhttp.WithServerName(c.serverName))
	}
	if len(c.spanOptions) > 0 {
		opts = append(opts, otelhttp.WithSpanOptions(c.spanOptions...))
	}

	return opts
}

// routePart strips the optional "METHOD " prefix of a [http.ServeMux] pattern.
func routePart(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}

	return ""
}

// WithLogger sets the logger that reports, once at construction, that tracing
// is disabled. A nil logger means [slog.Default].
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithEnabled forces tracing on or off, overriding the detection below.
//
// Without it, the middleware is a pass-through (no allocation, no pooled
// view) when both the tracer provider and the meter provider were passed
// explicitly, with [WithTracerProvider] and [WithMeterProvider], and are the
// no-op providers of go.opentelemetry.io/otel/trace/noop and
// go.opentelemetry.io/otel/metric/noop (as values or pointers). One line is
// logged at construction when that happens.
//
// What is not detected: the global providers (when none is passed). The global
// tracer provider is a delegate that can receive a real provider after the
// middleware was built, so it is never taken for "disabled". Types that embed a
// no-op provider are not detected either, and neither is one provider being a
// no-op while the other is not (tracing continues normally then). Pass the
// providers explicitly; the same goes for the propagator, whose header names
// are read once, at construction, from [WithPropagators] or the global
// propagator at that time.
func WithEnabled(enabled bool) Option { return func(c *config) { c.enabled = &enabled } }

// disabled decides once, at construction, whether the middleware is a
// pass-through, and logs the reason.
func (c *config) disabled() bool {
	reason := ""
	switch {
	case c.enabled != nil:
		if !*c.enabled {
			reason = "WithEnabled(false)"
		}
	case isNoopTracer(c.tracerProvider) && isNoopMeter(c.meterProvider):
		reason = "the tracer provider and the meter provider are no-op"
	}
	if reason == "" {
		return false
	}
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "tracing disabled", slog.String("reason", reason))

	return true
}

func isNoopTracer(p trace.TracerProvider) bool {
	switch p.(type) {
	case tracenoop.TracerProvider, *tracenoop.TracerProvider:
		return true
	default:
		return false
	}
}

func isNoopMeter(p metric.MeterProvider) bool {
	switch p.(type) {
	case metricnoop.MeterProvider, *metricnoop.MeterProvider:
		return true
	default:
		return false
	}
}
