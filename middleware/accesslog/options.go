package accesslog

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Fields are the names of the attributes of the access log record. Empty names keep the default.
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
	attrs     func(*Record) []slog.Attr
	logger    *slog.Logger
	clock     func() time.Time
	skip      func(huma.Context) bool
	reqSkip   func(*http.Request) bool
	emit      func(context.Context, *slog.Logger, *Record)
	idGen     func() string
	fields    Fields
	requestID string
	// idKey is requestID in the canonical form of header keys, so that looking it up does not allocate.
	idKey    string
	message  string
	headers  []string
	redacted []string
	levels   Levels
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
	c.idKey = http.CanonicalHeaderKey(c.requestID)
	for i, h := range c.redacted {
		c.redacted[i] = http.CanonicalHeaderKey(h)
	}
	for i, h := range c.headers {
		c.headers[i] = http.CanonicalHeaderKey(h)
	}

	return c
}

// root returns the logger to write to when no request logger is installed: the configured one as is, else the
// current [slog.Default], resolved at request time so a later [slog.SetDefault] is honoured.
func (c *config) root() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}

	return slog.Default()
}

// WithLogger sets the logger the access log is written to. A nil logger, or no logger at all, means [slog.Default]
// resolved on every request. When the logging middleware is installed outside this one, its root logger is used
// instead, with the additions of its enrichers.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithFields renames attributes; empty names keep the default.
func WithFields(f Fields) Option { return func(c *config) { c.fields = c.fields.merge(f) } }

// WithLevels sets the levels by status class.
func WithLevels(l Levels) Option { return func(c *config) { c.levels = l } }

// WithClock sets the time source used for the duration (default [time.Now]).
func WithClock(now func() time.Time) Option {
	return func(c *config) {
		if now != nil {
			c.clock = now
		}
	}
}

// WithMessage sets the message of the record (default "request").
func WithMessage(msg string) Option { return func(c *config) { c.message = msg } }

// WithSkip skips the record for requests where fn returns true. Only used by [New].
func WithSkip(fn func(huma.Context) bool) Option { return func(c *config) { c.skip = fn } }

// WithRequestSkip is [WithSkip] for [Handler].
func WithRequestSkip(fn func(*http.Request) bool) Option { return func(c *config) { c.reqSkip = fn } }

// WithRecordFunc replaces the default record: fn is called once per request, after the response, with the logger
// the record goes to and the [Record] of the request. Unlike the default record it allocates (a logger is made for
// the call). The Record is pooled state: it is only valid during the call, copy what you keep.
func WithRecordFunc(fn func(ctx context.Context, l *slog.Logger, r *Record)) Option {
	return func(c *config) { c.emit = fn }
}

// WithAttrs appends attributes to the default record (ignored with [WithRecordFunc]). The slice fn returns is the
// application's allocation.
func WithAttrs(fn func(r *Record) []slog.Attr) Option { return func(c *config) { c.attrs = fn } }

// WithHeaders logs the named request headers in the default record. Values of redacted headers (see
// [WithRedactedHeaders]) are replaced by "[REDACTED]". It allocates the group of the headers on every request.
func WithHeaders(names ...string) Option {
	return func(c *config) { c.headers = append(c.headers, names...) }
}

// WithRedactedHeaders adds header names to the redaction list. The default list is Authorization,
// Proxy-Authorization, Cookie, Set-Cookie, X-Api-Key, Api-Key, X-Auth-Token and X-Csrf-Token.
func WithRedactedHeaders(names ...string) Option {
	return func(c *config) { c.redacted = append(c.redacted, names...) }
}

// WithRequestIDHeader sets the header holding the request ID (default "X-Request-ID"); an empty name disables the
// request ID attribute. The request header is used; when the request has none, the same header of the response, as
// set by the handlers or the middleware inside this one, as it is when the record is written.
func WithRequestIDHeader(name string) Option { return func(c *config) { c.requestID = name } }

// WithRequestIDGenerator creates a request ID when the request has none: it is set as the response header named by
// [WithRequestIDHeader] and carried by the record. It is the one place the middleware allocates by itself, and only
// when it runs.
func WithRequestIDGenerator(fn func() string) Option { return func(c *config) { c.idGen = fn } }
