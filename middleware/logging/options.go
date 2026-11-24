package logging

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Fields are the names of the request attributes of the request logger. Empty names keep the default.
type Fields struct {
	Method      string
	Route       string
	OperationID string
	RequestID   string
	TraceID     string
	SpanID      string
}

// DefaultFields returns the default attribute names.
func DefaultFields() Fields {
	return Fields{
		Method:      "method",
		Route:       "route",
		OperationID: "operation_id",
		RequestID:   "request_id",
		TraceID:     "trace_id",
		SpanID:      "span_id",
	}
}

func (f Fields) merge(o Fields) Fields {
	pick := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	pick(&f.Method, o.Method)
	pick(&f.Route, o.Route)
	pick(&f.OperationID, o.OperationID)
	pick(&f.RequestID, o.RequestID)
	pick(&f.TraceID, o.TraceID)
	pick(&f.SpanID, o.SpanID)

	return f
}

// Option configures [New] and [Handler].
type Option func(*config)

type config struct {
	logger    *slog.Logger
	fields    Fields
	requestID string
	// idKey is requestID in the canonical form of header keys, so that looking it up does not allocate.
	idKey      string
	enrich     []func(huma.Context, *slog.Logger) *slog.Logger
	reqEnrich  []func(*http.Request, *slog.Logger) *slog.Logger
	noRequestA bool
}

func newConfig(opts []Option) *config {
	c := &config{fields: DefaultFields(), requestID: "X-Request-ID"}
	for _, o := range opts {
		o(c)
	}
	c.idKey = http.CanonicalHeaderKey(c.requestID)

	return c
}

// root returns the logger to derive from: the configured one as is, else the current [slog.Default], resolved at
// request time so a later [slog.SetDefault] is honoured.
func (c *config) root() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}

	return slog.Default()
}

// WithLogger sets the root logger. A nil logger, or no logger at all, means [slog.Default] resolved on every
// request.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithFields renames the request attributes; empty names keep the default.
func WithFields(f Fields) Option { return func(c *config) { c.fields = c.fields.merge(f) } }

// WithRequestIDHeader sets the request header holding the request ID (default "X-Request-ID"); an empty name
// disables the request ID attribute. The accesslog middleware tells this one the ID it generated when the request
// had none.
func WithRequestIDHeader(name string) Option { return func(c *config) { c.requestID = name } }

// WithoutRequestAttrs stops the request logger from carrying the built-in request attributes (method, route,
// operation ID, request and trace IDs).
func WithoutRequestAttrs() Option { return func(c *config) { c.noRequestA = true } }

// WithEnrichLogs registers fn, called once per request, before next, with the request-scoped logger; the logger it
// returns is stored in the context ([FromContext]) and used for the access log, so additions show on both. A nil fn
// is ignored, a nil result keeps the logger passed in, and several enrichers run in registration order. A panic in fn
// is not recovered: it propagates to whatever recovery middleware wraps this one. fn sees only what the context holds
// when the logging middleware runs; for values known later use [AddAttrs] (or run the authentication middleware
// first).
func WithEnrichLogs(fn func(ctx huma.Context, l *slog.Logger) *slog.Logger) Option {
	return func(c *config) {
		if fn != nil {
			c.enrich = append(c.enrich, fn)
		}
	}
}

// WithRequestEnrichLogs is [WithEnrichLogs] for [Handler].
func WithRequestEnrichLogs(fn func(r *http.Request, l *slog.Logger) *slog.Logger) Option {
	return func(c *config) {
		if fn != nil {
			c.reqEnrich = append(c.reqEnrich, fn)
		}
	}
}
