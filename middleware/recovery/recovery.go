// Package recovery turns a panic in a handler into a logged error and a 500
// response, and keeps the process serving. It is a huma middleware ([New]) and
// has a plain [net/http] form ([Handler]) sharing the same core.
//
// When a panic is recovered:
//
//   - it is logged at error level with the panic value and the stack, through
//     the request logger of the logging package ([logging.FromContext], which
//     falls back to [slog.Default]) or the logger given with [WithLogger];
//   - the active span, if there is one that is still recording, gets the
//     exception (with the stack), the error status and status code 500 (the
//     OpenTelemetry API only; no SDK is imported);
//   - if nothing was written yet, a 500 response is written: through huma's
//     error model by default (huma.WriteErr), or by the function given with
//     [WithHandler] / [WithRequestHandler].
//
// [http.ErrAbortHandler] is not recovered: it is the standard way for a handler
// to abort a response, and it is raised again as the net/http contract says.
//
// # Order
//
// Put recovery inside tracing and logging, outside the handlers and the other
// middleware that can panic: api.UseMiddleware(tracing, logging, recovery, ...).
// The span is still recording when recovery runs, so recovery marks it, and the
// 500 it writes is the status that tracing, logging (its access log record) and
// the HTTP server metrics report.
//
// With recovery outside tracing, tracing records the panic on the span itself
// and reports a 500 to its metrics, then raises the panic again; recovery sees
// no span in the context and only logs and writes the 500. With recovery
// outside logging, the panic crosses the logging middleware, which writes no
// access log record for that request.
package recovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackbirdworks/huma-middleware/internal/bodywriter"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

const defaultStackSize = 4096

// Option configures [New] and [Handler].
type Option func(*config)

type config struct {
	logger     *slog.Logger
	handler    func(huma.Context, any)
	reqHandler func(http.ResponseWriter, *http.Request, any)
	skip       func(huma.Context) bool
	reqSkip    func(*http.Request) bool
	stackSize  int
	noStack    bool
}

func newConfig(opts []Option) *config {
	c := &config{stackSize: defaultStackSize}
	for _, o := range opts {
		o(c)
	}

	return c
}

// WithLogger sets the logger for the recovered panics, instead of the request
// logger from [logging.FromContext].
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithStack turns the stack in the log record and on the span on or off (default on).
func WithStack(enabled bool) Option { return func(c *config) { c.noStack = !enabled } }

// WithStackSize sets the size in bytes of the buffer the stack is captured in
// (default 4096). Longer stacks are cut.
func WithStackSize(n int) Option { return func(c *config) { c.stackSize = max(n, 1) } }

// WithHandler sets the function that writes the response for a recovered panic
// in [New], instead of the default 500 in huma's error model. It is only called
// when nothing was written yet. recovered is the value passed to panic.
func WithHandler(fn func(ctx huma.Context, recovered any)) Option {
	return func(c *config) { c.handler = fn }
}

// WithRequestHandler is [WithHandler] for [Handler].
func WithRequestHandler(fn func(w http.ResponseWriter, r *http.Request, recovered any)) Option {
	return func(c *config) { c.reqHandler = fn }
}

// WithSkip lets the panics of the requests for which fn returns true through
// unrecovered. Only used by [New].
func WithSkip(fn func(huma.Context) bool) Option { return func(c *config) { c.skip = fn } }

// WithRequestSkip is [WithSkip] for [Handler].
func WithRequestSkip(fn func(*http.Request) bool) Option { return func(c *config) { c.reqSkip = fn } }

// PanicError is the error recorded on a span for a recovered panic.
type PanicError struct {
	// Value is what was passed to panic.
	Value any
}

// Error implements error.
func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// isAbort reports whether v is [http.ErrAbortHandler].
func isAbort(v any) bool {
	err, ok := v.(error)

	return ok && errors.Is(err, http.ErrAbortHandler)
}

// report logs the panic and marks the active span.
func (c *config) report(ctx context.Context, v any) {
	logger := c.logger
	if logger == nil {
		logger = logging.FromContext(ctx)
	}
	attrs := make([]slog.Attr, 0, 2) //nolint:mnd // the panic value and the stack
	attrs = append(attrs, slog.String("panic", fmt.Sprint(v)))
	if !c.noStack {
		attrs = append(attrs, slog.String("stack", c.stack()))
	}
	logger.LogAttrs(ctx, slog.LevelError, "panic recovered", attrs...)

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		err := &PanicError{Value: v}
		span.RecordError(err, trace.WithStackTrace(!c.noStack))
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.Int("http.response.status_code", http.StatusInternalServerError))
	}
}

func (c *config) stack() string {
	buf := make([]byte, c.stackSize)

	return string(buf[:runtime.Stack(buf, false)])
}

// New returns the recovery middleware for huma. api is used to write the
// default 500 response in huma's error model; it may be nil when [WithHandler]
// writes the response, and then a plain text 500 is written otherwise.
func New(api huma.API, opts ...Option) Middleware {
	cfg := newConfig(opts)
	pool := &sync.Pool{New: func() any {
		g := &guard{}
		g.body.g = g

		return g
	}}

	return func(ctx huma.Context, next func(huma.Context)) {
		if cfg.skip != nil && cfg.skip(ctx) {
			next(ctx)

			return
		}
		g, _ := pool.Get().(*guard)
		g.ctxBase, g.retained = ctx, false
		g.body.W = ctx.BodyWriter()
		g.wrote.Store(false)
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if isAbort(v) {
				panic(v)
			}
			cfg.report(ctx.Context(), v)
			if g.wrote.Load() {
				return // the response is already on its way, there is nothing left to write
			}
			switch {
			case cfg.handler != nil:
				cfg.handler(ctx, v)
			case api != nil:
				_ = huma.WriteErr(
					api,
					ctx,
					http.StatusInternalServerError,
					http.StatusText(http.StatusInternalServerError),
				)
			default:
				ctx.SetHeader("Content-Type", "text/plain; charset=utf-8")
				ctx.SetStatus(http.StatusInternalServerError)
				_, _ = ctx.BodyWriter().Write([]byte(http.StatusText(http.StatusInternalServerError) + "\n"))
			}
		}()
		next(g)
		if !g.retained {
			// Reached without a panic only: after one, whatever still holds the guard keeps it.
			pool.Put(g)
		}
	}
}

// ctxBase lets guard embed the context and still define its own methods.
type ctxBase huma.Context

// guard is the context given to the rest of the chain: it notes when the
// response was started, by a final status, a body write or a flush. Looking at
// the status alone is not enough, as writing a body without setting a status
// starts a 200 response on most adapters.
type guard struct {
	ctxBase

	body  tracker
	wrote atomic.Bool
	// retained is set when a stream callback may outlive the handler: the guard is then not reused.
	retained bool
}

// tracker is the body writer of a guard.
type tracker struct {
	bodywriter.Passthrough

	g *guard
}

func (t *tracker) Write(p []byte) (int, error) {
	t.g.wrote.Store(true)

	return t.W.Write(p) //nolint:wrapcheck // transparent pass-through of the response writer error
}

// Flush flushes the response and notes that it started.
func (t *tracker) Flush() { _ = t.FlushError() }

// FlushError flushes the response and notes that it started.
func (t *tracker) FlushError() error {
	t.g.wrote.Store(true)

	return t.Passthrough.FlushError()
}

// SetStatus notes the status; an informational one (103 Early Hints) does not start the final response.
func (g *guard) SetStatus(code int) {
	if !interim(code) {
		g.wrote.Store(true)
	}
	g.ctxBase.SetStatus(code)
}

// BodyWriter returns the tracking body writer.
func (g *guard) BodyWriter() io.Writer { return &g.body }

// Unwrap returns the context this one wraps, which adapters' Unwrap helpers follow.
func (g *guard) Unwrap() huma.Context { return g.ctxBase }

// bodyStreamer is implemented by the contexts of adapters that stream the body through a callback.
type bodyStreamer interface {
	StreamBody(func(io.Writer))
}

// StreamBody keeps streaming through a callback working on the adapters that need it.
func (g *guard) StreamBody(fn func(io.Writer)) {
	g.retained = true
	bs, ok := g.ctxBase.(bodyStreamer)
	if !ok {
		fn(&g.body)

		return
	}
	bs.StreamBody(func(sink io.Writer) {
		g.body.W = sink
		fn(&g.body)
	})
}

// Handler is the [net/http] form of the middleware, for routes that bypass
// huma. The default response is a plain text 500.
func Handler(opts ...Option) func(http.Handler) http.Handler {
	cfg := newConfig(opts)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.reqSkip != nil && cfg.reqSkip(r) {
				next.ServeHTTP(w, r)

				return
			}
			tw := &trackWriter{ResponseWriter: w}
			var out http.ResponseWriter = tw
			if _, ok := w.(http.Hijacker); ok {
				out = hijackTracker{tw}
			}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if isAbort(v) {
					panic(v)
				}
				cfg.report(r.Context(), v)
				switch {
				case tw.wrote:
				case cfg.reqHandler != nil:
					cfg.reqHandler(tw, r, v)
				default:
					http.Error(tw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(out, r)
		})
	}
}

// interim reports whether the status is informational (103 Early Hints): not the final one.
// 101 Switching Protocols ends the exchange and is final.
func interim(code int) bool {
	return code >= http.StatusContinue && code < http.StatusOK && code != http.StatusSwitchingProtocols
}

// trackWriter remembers whether the response was started.
type trackWriter struct {
	http.ResponseWriter

	wrote bool
}

func (w *trackWriter) WriteHeader(code int) {
	// An informational status (103 Early Hints) does not start the final response.
	w.wrote = w.wrote || !interim(code)
	w.ResponseWriter.WriteHeader(code)
}

// hijackTracker is the writer given to handlers when the underlying writer is an
// [http.Hijacker], so that a direct w.(http.Hijacker) assertion succeeds exactly
// when it would have on the underlying writer.
type hijackTracker struct {
	*trackWriter
}

// Hijack lets handlers take over the connection (upgrades).
func (w hijackTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.wrote = true

	return http.NewResponseController(w.ResponseWriter).Hijack() //nolint:wrapcheck // the writer's own error
}

func (w *trackWriter) Write(p []byte) (int, error) {
	w.wrote = true

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // transparent pass-through of the response writer error
}

// Flush keeps streaming responses working.
func (w *trackWriter) Flush() { _ = w.FlushError() }

// FlushError flushes the response, noting that it started, and returns what the
// underlying writer reports, for [http.ResponseController].
func (w *trackWriter) FlushError() error {
	w.wrote = true

	return http.NewResponseController(w.ResponseWriter).Flush() //nolint:wrapcheck // the writer's own error
}

// Unwrap exposes the underlying writer to [http.ResponseController].
func (w *trackWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
