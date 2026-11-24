package logging

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/trace"
)

type stateKey struct{}

// state is the per-request state: one allocation holds the lazy log handler,
// the body counter and the late attributes.
type state struct {
	hwrap    wrapper
	logger   *slog.Logger
	handler  lazyHandler
	counter  counter
	route    string
	method   string
	opID     string
	reqID    string
	respID   string
	idHeader string
	late     []slog.Attr
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
	route, reqID := s.route, s.reqID
	if reqID == "" {
		reqID = s.respID
	}
	s.mu.Unlock()
	attrs := make([]slog.Attr, 0, 6) //nolint:mnd // method, route, operation, request ID, trace and span
	attrs = append(attrs, slog.String(cfg.fields.Method, s.method))
	if route != "" {
		attrs = append(attrs, slog.String(cfg.fields.Route, route))
	}
	if s.opID != "" {
		attrs = append(attrs, slog.String(cfg.fields.OperationID, s.opID))
	}
	if reqID != "" {
		attrs = append(attrs, slog.String(cfg.fields.RequestID, reqID))
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

// resolvedID returns the request ID: the request's, else the one the response got.
func (s *state) resolvedID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reqID != "" {
		return s.reqID
	}

	return s.respID
}

// trackHeader remembers the response header that carries the request ID.
func (s *state) trackHeader(name, value string) {
	if s.idHeader != "" && strings.EqualFold(name, s.idHeader) {
		s.mu.Lock()
		s.respID = value
		s.mu.Unlock()
	}
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
func (c *counter) Flush() { _ = c.FlushError() }

// FlushError flushes the underlying writer and returns what it reports, for
// [http.ResponseController].
func (c *counter) FlushError() error {
	switch w := c.w.(type) {
	case http.ResponseWriter:
		return http.NewResponseController(w).Flush() //nolint:wrapcheck // the writer's own error
	case http.Flusher:
		w.Flush()
	}

	return nil
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

// SetHeader sets a response header, watching the one that carries the request ID.
func (w *wrapper) SetHeader(name, value string) {
	w.st.trackHeader(name, value)
	w.base.SetHeader(name, value)
}

// AppendHeader appends to a response header, watching the one that carries the request ID.
func (w *wrapper) AppendHeader(name, value string) {
	w.st.trackHeader(name, value)
	w.base.AppendHeader(name, value)
}

func (w *wrapper) BodyWriter() io.Writer { return &w.st.counter }

// bodyStreamer is implemented by the contexts of adapters that stream the body
// through a callback (Fiber), which huma's SSE support looks for.
type bodyStreamer interface {
	StreamBody(func(io.Writer))
}

// StreamBody keeps streaming through a callback working on the adapters that
// need it, counting what is streamed. The access log record is written when the
// handler returns, so on those adapters it does not wait for the stream to end.
func (w *wrapper) StreamBody(fn func(io.Writer)) {
	bs, ok := w.base.(bodyStreamer)
	if !ok {
		fn(w.BodyWriter())

		return
	}
	bs.StreamBody(func(sink io.Writer) {
		w.st.counter.w = sink
		fn(&w.st.counter)
	})
}
