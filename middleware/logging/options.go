package logging

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Fields are the attribute names used by the built-in request attributes and
// the default access log record. An empty name keeps the default.
type Fields struct {
	Method      string
	Path        string
	Route       string
	OperationID string
	Status      string
	Duration    string
	Size        string
	RemoteAddr  string
	UserAgent   string
	RequestID   string
	TraceID     string
	SpanID      string
	Headers     string
}

// DefaultFields returns the default attribute names.
func DefaultFields() Fields {
	return Fields{
		Method:      "method",
		Path:        "path",
		Route:       "route",
		OperationID: "operation_id",
		Status:      "status",
		Duration:    "duration",
		Size:        "response_size",
		RemoteAddr:  "remote_addr",
		UserAgent:   "user_agent",
		RequestID:   "request_id",
		TraceID:     "trace_id",
		SpanID:      "span_id",
		Headers:     "headers",
	}
}

func (f Fields) merge(o Fields) Fields {
	pick := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	pick(&f.Method, o.Method)
	pick(&f.Path, o.Path)
	pick(&f.Route, o.Route)
	pick(&f.OperationID, o.OperationID)
	pick(&f.Status, o.Status)
	pick(&f.Duration, o.Duration)
	pick(&f.Size, o.Size)
	pick(&f.RemoteAddr, o.RemoteAddr)
	pick(&f.UserAgent, o.UserAgent)
	pick(&f.RequestID, o.RequestID)
	pick(&f.TraceID, o.TraceID)
	pick(&f.SpanID, o.SpanID)
	pick(&f.Headers, o.Headers)

	return f
}

// Levels are the access log levels by status class.
type Levels struct {
	Success     slog.Level // 1xx and 2xx
	Redirect    slog.Level // 3xx
	ClientError slog.Level // 4xx
	ServerError slog.Level // 5xx
}

// DefaultLevels is Info for 1xx-3xx, Warn for 4xx and Error for 5xx.
func DefaultLevels() Levels {
	return Levels{
		Success:     slog.LevelInfo,
		Redirect:    slog.LevelInfo,
		ClientError: slog.LevelWarn,
		ServerError: slog.LevelError,
	}
}

func (l Levels) of(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return l.ServerError
	case status >= http.StatusBadRequest:
		return l.ClientError
	case status >= http.StatusMultipleChoices:
		return l.Redirect
	default:
		return l.Success
	}
}

// Option configures [New] and [Handler].
type Option func(*config)

type config struct {
	extra      func(Record) []slog.Attr
	logger     *slog.Logger
	clock      func() time.Time
	skip       func(huma.Context) bool
	reqSkip    func(*http.Request) bool
	emit       func(context.Context, *slog.Logger, Record)
	fields     Fields
	requestID  string
	message    string
	enrich     []func(huma.Context, *slog.Logger) *slog.Logger
	reqEnrich  []func(*http.Request, *slog.Logger) *slog.Logger
	headers    []string
	redacted   []string
	levels     Levels
	noAccess   bool
	noRequestA bool
}

func newConfig(opts []Option) *config {
	c := &config{
		fields:    DefaultFields(),
		levels:    DefaultLevels(),
		clock:     time.Now,
		requestID: "X-Request-ID",
		message:   "request",
		redacted: []string{
			"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
			"X-Api-Key", "Api-Key", "X-Auth-Token", "X-Csrf-Token",
		},
	}
	for _, o := range opts {
		o(c)
	}
	for i, h := range c.redacted {
		c.redacted[i] = http.CanonicalHeaderKey(h)
	}
	for i, h := range c.headers {
		c.headers[i] = http.CanonicalHeaderKey(h)
	}

	return c
}

// root returns the logger to derive from: the configured one as is, else the
// current [slog.Default], resolved at request time so a later [slog.SetDefault]
// is honoured.
func (c *config) root() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}

	return slog.Default()
}

// WithLogger sets the root logger. A nil logger, or no logger at all, means
// [slog.Default] resolved on every request.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithFields renames attributes; empty names keep the default.
func WithFields(f Fields) Option { return func(c *config) { c.fields = c.fields.merge(f) } }

// WithLevels sets the access log levels by status class.
func WithLevels(l Levels) Option { return func(c *config) { c.levels = l } }

// WithClock sets the time source used for the duration (default [time.Now]).
func WithClock(now func() time.Time) Option {
	return func(c *config) {
		if now != nil {
			c.clock = now
		}
	}
}

// WithMessage sets the access log message (default "request").
func WithMessage(msg string) Option { return func(c *config) { c.message = msg } }

// WithSkip skips the access log (the context logger is still installed) for
// requests where fn returns true. Only used by [New].
func WithSkip(fn func(huma.Context) bool) Option { return func(c *config) { c.skip = fn } }

// WithRequestSkip is [WithSkip] for [Handler].
func WithRequestSkip(fn func(*http.Request) bool) Option { return func(c *config) { c.reqSkip = fn } }

// WithoutAccessLog installs the context logger only.
func WithoutAccessLog() Option { return func(c *config) { c.noAccess = true } }

// WithAccessLogFunc replaces the default access log record: fn is called once
// per request after the response with a logger that carries the root logger,
// the enrichers' additions and the late attributes from [AddAttrs], but not
// the built-in request attributes, so fn controls every field name and the
// order exactly. The Record holds everything the default record logs.
func WithAccessLogFunc(fn func(ctx context.Context, l *slog.Logger, r Record)) Option {
	return func(c *config) { c.emit = fn }
}

// WithAccessAttrs appends attributes to the default record (ignored with
// [WithAccessLogFunc]).
func WithAccessAttrs(fn func(Record) []slog.Attr) Option { return func(c *config) { c.extra = fn } }

// WithHeaders logs the named request headers in the default record. Values of
// redacted headers (see [WithRedactedHeaders]) are replaced by "[REDACTED]".
func WithHeaders(names ...string) Option {
	return func(c *config) { c.headers = append(c.headers, names...) }
}

// WithRedactedHeaders adds header names to the redaction list. The default
// list is Authorization, Proxy-Authorization, Cookie, Set-Cookie, X-Api-Key,
// Api-Key, X-Auth-Token and X-Csrf-Token.
func WithRedactedHeaders(names ...string) Option {
	return func(c *config) { c.redacted = append(c.redacted, names...) }
}

// WithRequestIDHeader sets the request header holding the request ID (default
// "X-Request-ID"); an empty name disables the request ID attribute.
func WithRequestIDHeader(name string) Option { return func(c *config) { c.requestID = name } }

// WithoutRequestAttrs stops the context logger from carrying the built-in
// request attributes (method, route, operation ID, request and trace IDs).
func WithoutRequestAttrs() Option { return func(c *config) { c.noRequestA = true } }

// WithEnrichLogs registers fn, called once per request, before next, with the
// request-scoped logger; the logger it returns is stored in the context
// ([FromContext]) and used for the access log, so additions show on both.
// A nil fn is ignored, a nil result keeps the logger passed in, and several
// enrichers run in registration order. A panic in fn is not recovered: it
// propagates to whatever recovery middleware wraps this one. fn sees only what
// the context holds when the logging middleware runs; for values known later
// use [AddAttrs] (or run the authentication middleware first).
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
