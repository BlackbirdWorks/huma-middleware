package logging

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/trace"
)

type stateKey struct{}

// state is the per-request state: one allocation holds the lazy log handler,
// the body counter and the late attributes.
type state struct {
	hwrap   wrapper
	logger  *slog.Logger
	handler lazyHandler
	counter counter
	route   string
	method  string
	opID    string
	reqID   string
	late    []slog.Attr
	// mu guards route and late.
	mu       sync.Mutex
	traceID  trace.TraceID
	spanID   trace.SpanID
	hasTrace bool
}

// lazyHandler adds the request attributes when a record is handled, so a
// disabled level costs nothing and an enabled one formats nothing earlier.
type lazyHandler struct {
	inner slog.Handler
	st    *state
	cfg   *config
	quiet bool
}

func (h *lazyHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *lazyHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.quiet {
		r.AddAttrs(h.st.requestAttrs(h.cfg)...)
	}
	h.st.mu.Lock()
	r.AddAttrs(h.st.late...)
	h.st.mu.Unlock()

	return h.inner.Handle(ctx, r)
}

// WithAttrs implements [slog.Handler].
func (h *lazyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &lazyHandler{inner: h.inner.WithAttrs(attrs), st: h.st, cfg: h.cfg, quiet: h.quiet}
}

// WithGroup implements [slog.Handler]; the request attributes are attached
// before the group opens so they stay at the top level.
func (h *lazyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	inner := h.inner
	if !h.quiet {
		inner = inner.WithAttrs(h.st.requestAttrs(h.cfg))
	}

	return &lazyHandler{inner: inner.WithGroup(name), st: h.st, cfg: h.cfg, quiet: true}
}

func (s *state) requestAttrs(cfg *config) []slog.Attr {
	if cfg.noRequestA {
		return nil
	}
	s.mu.Lock()
	route := s.route
	s.mu.Unlock()
	attrs := make([]slog.Attr, 0, 6) //nolint:mnd // method, route, operation, request ID, trace and span
	attrs = append(attrs, slog.String(cfg.fields.Method, s.method))
	if route != "" {
		attrs = append(attrs, slog.String(cfg.fields.Route, route))
	}
	if s.opID != "" {
		attrs = append(attrs, slog.String(cfg.fields.OperationID, s.opID))
	}
	if s.reqID != "" {
		attrs = append(attrs, slog.String(cfg.fields.RequestID, s.reqID))
	}
	if s.hasTrace {
		attrs = append(attrs,
			slog.String(cfg.fields.TraceID, s.traceID.String()),
			slog.String(cfg.fields.SpanID, s.spanID.String()))
	}

	return attrs
}

func newState(cfg *config, root *slog.Logger, method, route, opID, reqID string, sc trace.SpanContext) *state {
	s := &state{method: method, route: route, opID: opID, reqID: reqID}
	if sc.IsValid() {
		s.traceID, s.spanID, s.hasTrace = sc.TraceID(), sc.SpanID(), true
	}
	s.handler = lazyHandler{inner: root.Handler(), st: s, cfg: cfg}
	s.logger = slog.New(&s.handler)

	return s
}

// quietLogger returns l without the built-in request attributes when it was
// derived from the request logger, else l.
func quietLogger(l *slog.Logger) *slog.Logger {
	if h, ok := l.Handler().(*lazyHandler); ok && !h.quiet {
		q := *h
		q.quiet = true

		return slog.New(&q)
	}

	return l
}

// FromContext returns the request-scoped logger installed by the middleware.
// Without one it returns [slog.Default], so the result is always usable.
func FromContext(ctx context.Context) *slog.Logger {
	if s, ok := ctx.Value(stateKey{}).(*state); ok {
		return s.logger
	}

	return slog.Default()
}

// NewContext returns ctx carrying l as the logger returned by [FromContext].
// The middleware does this itself; use it in tests or background work.
func NewContext(ctx context.Context, l *slog.Logger) context.Context {
	s := &state{logger: l}

	return context.WithValue(ctx, stateKey{}, s)
}

// AddAttrs adds attributes to the current request: they are added to every
// later record of the request logger and to the access log record. It is safe
// for concurrent use and does nothing outside a request handled by the
// middleware. Use it for values only known after the middleware ran, such as
// the authenticated user.
func AddAttrs(ctx context.Context, attrs ...slog.Attr) {
	s, ok := ctx.Value(stateKey{}).(*state)
	if !ok {
		return
	}
	s.mu.Lock()
	s.late = append(s.late, attrs...)
	s.mu.Unlock()
}

func (s *state) setRoute(route string) {
	s.mu.Lock()
	s.route = route
	s.mu.Unlock()
}

func (s *state) lateAttrs() []slog.Attr {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]slog.Attr(nil), s.late...)
}

// counter counts the bytes written to the response body.
type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)

	return n, err //nolint:wrapcheck // transparent pass-through of the response writer error
}

// Flush forwards to the underlying writer so streaming keeps working.
func (c *counter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to [http.ResponseController].
func (c *counter) Unwrap() http.ResponseWriter {
	rw, _ := c.w.(http.ResponseWriter)

	return rw
}

// wrapper is the huma.Context given to the rest of the chain: it carries the
// request context and counts the response bytes.
// base lets wrapper embed the context while overriding its Context method.
type base huma.Context

type wrapper struct {
	base

	ctx context.Context
	st  *state
}

func (w *wrapper) Context() context.Context { return w.ctx }

func (w *wrapper) BodyWriter() io.Writer { return &w.st.counter }
