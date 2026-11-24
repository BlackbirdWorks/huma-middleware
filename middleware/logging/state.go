package logging

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackbirdworks/huma-middleware/internal/reqlog"
)

// state is the per-request state: one allocation holds the context handed on (it is a [context.Context] itself,
// so no value wrapper is allocated), the lazy log handler and the late attributes. The request logger is built the
// first time it is asked for. It is the [reqlog.Source] of the access log.
type state struct {
	context.Context

	wrap     wrapper
	logger   *slog.Logger
	handler  lazyHandler
	quiet    lazyHandler
	reqID    string
	method   string
	opID     string
	route    string
	traceHex string
	spanHex  string
	late     []slog.Attr
	mu       sync.Mutex
	traceID  trace.TraceID
	spanID   trace.SpanID
	hasTrace bool
}

// Value returns the state for the key of the package, and the parent context's values for the others.
func (s *state) Value(key any) any {
	if _, ok := key.(reqlog.Key); ok {
		return s
	}

	return s.Context.Value(key)
}

// requestLogger returns the request logger, building it on first use.
func (s *state) requestLogger() *slog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logger == nil {
		s.logger = slog.New(&s.handler)
	}

	return s.logger
}

// Handler implements [reqlog.Source]: the request logger without the request attributes.
func (s *state) Handler() slog.Handler {
	s.mu.Lock()
	l := s.logger
	s.mu.Unlock()
	if l == nil {
		return &s.quiet
	}
	h, ok := l.Handler().(*lazyHandler)
	if !ok {
		return l.Handler()
	}
	q := *h
	q.quiet = true

	return &q
}

// AddIDs implements [reqlog.Source].
func (s *state) AddIDs(rec *slog.Record, buf []slog.Attr) {
	if !s.hasTrace {
		return
	}
	s.mu.Lock()
	if s.traceHex == "" {
		s.traceHex, s.spanHex = s.traceID.String(), s.spanID.String()
	}
	traceHex, spanHex := s.traceHex, s.spanHex
	s.mu.Unlock()
	f := &s.handler.cfg.fields
	buf = append(buf[:0], slog.String(f.TraceID, traceHex), slog.String(f.SpanID, spanHex))
	rec.AddAttrs(slog.Attr{Value: slog.GroupValue(buf...)}) // an empty key inlines the group
}

// SetRequestID implements [reqlog.Source].
func (s *state) SetRequestID(id string) {
	if s.handler.cfg.requestID == "" {
		return // the request ID attribute is disabled
	}
	s.mu.Lock()
	if s.reqID == "" {
		s.reqID = id
	}
	s.mu.Unlock()
}

// lazyHandler adds the request attributes when a record is handled, so a disabled level costs nothing and an
// enabled one formats nothing earlier.
//
// It keeps two derived handlers: bare has only what the application added (the attributes and groups of its With
// calls), full also has the request attributes, bound just before the first group so they stay at the top level.
// Until a group is opened the request attributes are added to each record instead, which is cheaper to derive.
type lazyHandler struct {
	full    slog.Handler
	bare    slog.Handler
	st      *state
	cfg     *config
	grouped bool
	// quiet drops the request attributes: the handler of the access log.
	quiet bool
}

// Enabled asks the handler that Handle writes to: the quiet one writes through bare, which has not bound the request
// attributes, so a handler that decides on them must not decide for it.
func (h *lazyHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if h.quiet {
		return h.bare.Enabled(ctx, l)
	}

	return h.full.Enabled(ctx, l)
}

func (h *lazyHandler) Handle(ctx context.Context, r slog.Record) error {
	inner := h.full
	if h.quiet {
		inner = h.bare
	} else if !h.grouped {
		var buf [maxRequestAttrs]slog.Attr
		r.AddAttrs(h.st.appendRequestAttrs(buf[:0], h.cfg)...)
	}
	h.st.mu.Lock()
	r.AddAttrs(h.st.late...)
	h.st.mu.Unlock()

	return inner.Handle(ctx, r)
}

// maxRequestAttrs is the number of request attributes: method, route, operation, request ID, trace and span.
const maxRequestAttrs = 6

// WithAttrs implements [slog.Handler].
func (h *lazyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	d := *h
	d.full, d.bare = h.full.WithAttrs(attrs), h.bare.WithAttrs(attrs)

	return &d
}

// WithGroup implements [slog.Handler]; the request attributes are bound before the first group opens so they stay
// at the top level.
func (h *lazyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	d := *h
	full := h.full
	if !h.grouped {
		full = full.WithAttrs(h.st.requestAttrs(h.cfg))
	}
	d.full, d.bare, d.grouped = full.WithGroup(name), h.bare.WithGroup(name), true

	return &d
}

func (s *state) requestAttrs(cfg *config) []slog.Attr {
	return s.appendRequestAttrs(make([]slog.Attr, 0, maxRequestAttrs), cfg)
}

// appendRequestAttrs appends the request attributes to dst.
func (s *state) appendRequestAttrs(dst []slog.Attr, cfg *config) []slog.Attr {
	if cfg.noRequestA {
		return dst
	}
	s.mu.Lock()
	route, reqID := s.route, s.reqID
	if s.hasTrace && s.traceHex == "" {
		s.traceHex, s.spanHex = s.traceID.String(), s.spanID.String()
	}
	traceHex, spanHex := s.traceHex, s.spanHex
	s.mu.Unlock()
	dst = append(dst, slog.String(cfg.fields.Method, s.method))
	if route != "" {
		dst = append(dst, slog.String(cfg.fields.Route, route))
	}
	if s.opID != "" {
		dst = append(dst, slog.String(cfg.fields.OperationID, s.opID))
	}
	if reqID != "" {
		dst = append(dst, slog.String(cfg.fields.RequestID, reqID))
	}
	if s.hasTrace {
		dst = append(dst, slog.String(cfg.fields.TraceID, traceHex), slog.String(cfg.fields.SpanID, spanHex))
	}

	return dst
}

func newState(
	parent context.Context, cfg *config, root *slog.Logger, method, route, opID, reqID string, sc trace.SpanContext,
) *state {
	s := &state{Context: parent, method: method, route: route, opID: opID, reqID: reqID}
	if sc.IsValid() {
		s.traceID, s.spanID, s.hasTrace = sc.TraceID(), sc.SpanID(), true
	}
	s.handler = lazyHandler{full: root.Handler(), bare: root.Handler(), st: s, cfg: cfg}
	s.quiet = lazyHandler{full: s.handler.full, bare: s.handler.bare, st: s, cfg: cfg, quiet: true}

	return s
}

// adopt returns l when it derives from the request logger of s; a logger the application built from scratch gets the
// request layer (request and late attributes), keeping its own destination.
func (s *state) adopt(cfg *config, l *slog.Logger) *slog.Logger {
	if h, ok := l.Handler().(*lazyHandler); ok && h.st == s {
		return l
	}

	return slog.New(&lazyHandler{full: l.Handler(), bare: l.Handler(), st: s, cfg: cfg})
}

// FromContext returns the request-scoped logger installed by the middleware.
// Without one it returns [slog.Default], so the result is always usable.
func FromContext(ctx context.Context) *slog.Logger {
	if s, ok := ctx.Value(reqlog.Key{}).(*state); ok {
		return s.requestLogger()
	}

	return slog.Default()
}

// NewContext returns ctx carrying l as the logger returned by [FromContext].
// The middleware does this itself; use it in tests or background work.
func NewContext(ctx context.Context, l *slog.Logger) context.Context {
	return &state{Context: ctx, logger: l}
}

// AddAttrs adds attributes to the current request: they are added to every later record of the request logger and
// to the access log record. It is safe for concurrent use and does nothing outside a request handled by the
// middleware. Use it for values only known after the middleware ran, such as the authenticated user.
func AddAttrs(ctx context.Context, attrs ...slog.Attr) {
	s, ok := ctx.Value(reqlog.Key{}).(*state)
	if !ok {
		return
	}
	s.mu.Lock()
	s.late = append(s.late, attrs...)
	s.mu.Unlock()
}

// base lets wrapper embed the context while overriding its Context method.
type base huma.Context

// wrapper is the huma.Context given to the rest of the chain: it carries the request context.
type wrapper struct {
	base

	ctx context.Context
}

func (w *wrapper) Context() context.Context { return w.ctx }

// bodyStreamer is implemented by the contexts of adapters that stream the body through a callback (Fiber), which
// huma's SSE support looks for on the context it is given.
type bodyStreamer interface {
	StreamBody(func(io.Writer))
}

// StreamBody keeps streaming through a callback working on the adapters that need it: the callback goes to the
// context this one wraps, and to its body writer on the others.
func (w *wrapper) StreamBody(fn func(io.Writer)) {
	if bs, ok := w.base.(bodyStreamer); ok {
		bs.StreamBody(fn)

		return
	}
	fn(w.base.BodyWriter())
}

// Unwrap returns the context this one wraps, which adapters' Unwrap helpers (humago.Unwrap, humachi.Unwrap) follow
// to reach the request and the writer.
func (w *wrapper) Unwrap() huma.Context { return w.base }
