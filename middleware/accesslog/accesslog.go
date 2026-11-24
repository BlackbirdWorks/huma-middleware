// Package accesslog writes one access log record per request, without allocating. It only uses the huma.Context
// interface and [log/slog].
//
// After the response one record is written at the level of the status class ([Levels], Error for 5xx) with the
// method, path (never the query string, which can hold secrets), route, operation ID, status actually sent, duration,
// response size, remote address, user agent and request ID. Selected request headers can be added with
// [WithHeaders]; Authorization, cookies and API key headers are redacted by default. [WithRecordFunc] replaces the
// record entirely, for applications with an established log format.
//
// The response size counts the bytes handed to the next writer. When compression runs inside this middleware that is
// the compressed size.
//
// # Cost
//
// The default record adds no allocation per request to the handler it wraps, in the huma and in the net/http form,
// when the level is on and when it is off (a disabled level is checked first and costs nothing more). What the log
// handler allocates to format the record is the handler's: [slog.JSONHandler] and [slog.TextHandler] do. Opting in to
// [WithHeaders], [WithAttrs], [WithRecordFunc] or [WithRequestIDGenerator] adds the allocations of that feature.
//
// The record is made of attributes that are groups with an empty key (every handler inlines them) over slices that
// are pooled with the request state, which is what makes it allocation free. A log handler that keeps the record
// after Handle returns (an asynchronous one) must be told with [WithRetainedRecords], which allocates the slices per
// record instead.
//
// # With the logging middleware
//
// When the logging middleware is installed outside this one the record is written to its root logger with the
// additions of its enrichers, carries the attributes added with logging.AddAttrs and the trace and span IDs, and the
// request ID this middleware resolves is the one the request logger shows. Installed inside the logging middleware
// or alone, it writes to [WithLogger] (or [slog.Default]) with its own attributes only. Neither package imports the
// other.
//
// [Handler] is the same for a plain [http.Handler].
package accesslog

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/blackbirdworks/huma-middleware/internal/bodywriter"
	"github.com/blackbirdworks/huma-middleware/internal/reqlog"
)

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

const redacted = "[REDACTED]"

// Record is what the access log knows about a finished request. It is the pooled state of the request: a function
// that receives it ([WithRecordFunc], [WithAttrs]) must not keep it, copy the fields instead. The methods
// [Record.Header] and [Record.RequestURI] read the request and are valid only during that call too.
type Record struct {
	hctx        huma.Context
	req         *http.Request
	UserAgent   string
	OperationID string
	Host        string
	RemoteAddr  string
	Method      string
	RequestID   string
	Route       string
	Path        string
	Duration    time.Duration
	// Size is the number of response body bytes written.
	Size   int64
	Status int
}

// Header returns a request header value.
func (r *Record) Header(name string) string {
	if r.req != nil {
		return r.req.Header.Get(name)
	}

	return r.hctx.Header(name)
}

// RequestURI returns the request target (path and raw query).
func (r *Record) RequestURI() string {
	if r.req != nil {
		return r.req.RequestURI
	}
	u := r.hctx.URL()

	return u.RequestURI()
}

// accessLog is the middleware: its configuration and the pools of the per-request state.
type accessLog struct {
	cfg   *config
	calls sync.Pool
	plain sync.Pool
}

func newAccessLog(opts []Option) *accessLog {
	a := &accessLog{cfg: newConfig(opts)}
	a.calls.New = func() any { return &call{a: a} }
	a.plain.New = func() any { return newPlain() }

	return a
}

// New returns the access log middleware for huma.
func New(opts ...Option) Middleware { return newAccessLog(opts).serve }

// base lets wrapper embed the context while overriding some of its methods.
type base huma.Context

// call is the state of one request of the huma form: the wrapper handed to the rest of the chain, the counter of the
// response bytes, the record and the log record that is written. It is pooled, so none of it is allocated per request.
type call struct {
	out      output
	wrap     wrapper
	a        *accessLog
	counter  counter
	idHeader string
	respID   string
	record   Record
	streamed bool
}

// output is the log record that is written, and the attributes it is made of: they are pooled with the request
// state. A [slog.Record] holds five attributes without allocating, so the record is made of a few attributes that
// are groups with an empty key, which every handler inlines, over slices of the pooled array.
type output struct {
	attrs [maxAttrs]slog.Attr
	rec   slog.Record
}

const (
	maxAttrs = 16
	coreEnd  = 7  // method, path, status, duration, size, remote address, user agent
	optEnd   = 10 // route, operation ID, request ID
	idsEnd   = 12 // trace and span ID
)

// counter counts the bytes written to the response body. Adapters that stream through a callback write from another
// goroutine, so the count is atomic.
type counter struct {
	bodywriter.Passthrough

	n atomic.Int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	c.n.Add(int64(n))

	return n, err //nolint:wrapcheck // transparent pass-through of the response writer error
}

// wrapper is the huma.Context given to the rest of the chain: it counts the response bytes and watches the header
// that carries the request ID.
type wrapper struct {
	base

	c *call
}

// Unwrap returns the context this one wraps, which adapters' Unwrap helpers (humago.Unwrap, humachi.Unwrap) follow
// to reach the request and the writer.
func (w *wrapper) Unwrap() huma.Context { return w.base }

// SetHeader sets a response header, watching the one that carries the request ID.
func (w *wrapper) SetHeader(name, value string) {
	w.c.watch(name, value)
	w.base.SetHeader(name, value)
}

// AppendHeader appends to a response header, watching the one that carries the request ID.
func (w *wrapper) AppendHeader(name, value string) {
	w.c.watch(name, value)
	w.base.AppendHeader(name, value)
}

func (w *wrapper) BodyWriter() io.Writer { return &w.c.counter }

// bodyStreamer is implemented by the contexts of adapters that stream the body through a callback (Fiber), which
// huma's SSE support looks for.
type bodyStreamer interface {
	StreamBody(func(io.Writer))
}

// StreamBody keeps streaming through a callback working on the adapters that need it, counting what is streamed. The
// record is written when the handler returns, so on those adapters it does not wait for the stream to end, and the
// state is not pooled again.
func (w *wrapper) StreamBody(fn func(io.Writer)) {
	bs, ok := w.base.(bodyStreamer)
	if !ok {
		fn(w.BodyWriter())

		return
	}
	w.c.streamed = true
	bs.StreamBody(func(sink io.Writer) {
		w.c.counter.W = sink
		fn(&w.c.counter)
	})
}

func (c *call) watch(name, value string) {
	if c.idHeader != "" && strings.EqualFold(name, c.idHeader) {
		c.respID = value
	}
}

func (a *accessLog) acquire(hctx huma.Context) *call {
	c, _ := a.calls.Get().(*call)
	c.wrap = wrapper{base: hctx, c: c}
	c.counter.W = hctx.BodyWriter()
	c.counter.n.Store(0)
	c.idHeader, c.respID, c.streamed = a.cfg.idKey, "", false
	if a.cfg.requestID == "" {
		c.idHeader = ""
	}

	return c
}

func (a *accessLog) serve(hctx huma.Context, next func(huma.Context)) {
	cfg := a.cfg
	var reqID string
	if cfg.requestID != "" {
		reqID = hctx.Header(cfg.idKey)
	}
	ctx := hctx.Context()
	src, _ := ctx.Value(reqlog.Key{}).(reqlog.Source)
	c := a.acquire(hctx)
	start := cfg.clock()
	if reqID == "" && cfg.requestID != "" && cfg.idGen != nil {
		// A generated ID lives on the response: what the handlers leave there is what is logged.
		id := cfg.idGen()
		hctx.SetHeader(cfg.requestID, id)
		c.respID = id
		if src != nil {
			src.SetRequestID(id)
		}
	}

	next(&c.wrap)

	if cfg.skip == nil || !cfg.skip(hctx) {
		if reqID == "" {
			reqID = c.respID
		}
		u := hctx.URL()
		r := &c.record
		*r = Record{
			hctx:       hctx,
			Method:     hctx.Method(),
			Path:       u.Path,
			Host:       hctx.Host(),
			RemoteAddr: hctx.RemoteAddr(),
			UserAgent:  hctx.Header("User-Agent"),
			RequestID:  reqID,
			Duration:   cfg.clock().Sub(start),
			Size:       c.counter.n.Load(),
			Status:     statusOf(hctx.Status()),
		}
		if op := hctx.Operation(); op != nil {
			r.Route, r.OperationID = op.Path, op.OperationID
		}
		a.write(ctx, src, &c.out, r)
	}
	if !c.streamed {
		c.wrap = wrapper{}
		c.record = Record{}
		a.calls.Put(c)
	}
}

func statusOf(s int) int {
	if s == 0 {
		return http.StatusOK
	}

	return s
}

// write writes the record of a finished request: to the handler of the logging middleware when it is there, else to
// the root logger.
func (a *accessLog) write(ctx context.Context, src reqlog.Source, o *output, r *Record) {
	cfg := a.cfg
	var h slog.Handler
	if src != nil {
		h = src.Handler()
	} else {
		h = cfg.root().Handler()
	}
	if cfg.emit != nil {
		cfg.emit(ctx, slog.New(h), r)

		return
	}
	level := cfg.levels.of(r.Status)
	if !h.Enabled(ctx, level) {
		return
	}
	f := &cfg.fields
	o.rec = slog.NewRecord(time.Now(), level, cfg.message, 0)
	attrs := o.attrs[:]
	if cfg.retain {
		attrs = make([]slog.Attr, maxAttrs) // a handler that keeps the record needs slices nobody reuses
	}
	core := attrs[:0:coreEnd]
	core = append(core,
		slog.String(f.Method, r.Method),
		slog.String(f.Path, r.Path),
		slog.Int(f.Status, r.Status),
		slog.Duration(f.Duration, r.Duration),
		slog.Int64(f.Size, r.Size),
		slog.String(f.RemoteAddr, r.RemoteAddr),
		slog.String(f.UserAgent, r.UserAgent),
	)
	o.rec.AddAttrs(slog.Attr{Value: slog.GroupValue(core...)}) // an empty key inlines the group
	opt := attrs[coreEnd:coreEnd:optEnd]
	opt = addNonEmpty(opt, f.Route, r.Route)
	opt = addNonEmpty(opt, f.OperationID, r.OperationID)
	opt = addNonEmpty(opt, f.RequestID, r.RequestID)
	if len(opt) > 0 {
		o.rec.AddAttrs(slog.Attr{Value: slog.GroupValue(opt...)})
	}
	if len(cfg.headers) > 0 {
		o.rec.AddAttrs(cfg.headerGroup(r))
	}
	if cfg.attrs != nil {
		o.rec.AddAttrs(cfg.attrs(r)...)
	}
	if src != nil {
		src.AddIDs(&o.rec, attrs[optEnd:optEnd:idsEnd])
	}
	_ = h.Handle(ctx, o.rec) // the handler's own error has nobody to go to, as with Logger.LogAttrs
}

func (c *config) headerGroup(r *Record) slog.Attr {
	hs := make([]any, 0, len(c.headers))
	for _, name := range c.headers {
		v := r.Header(name)
		if v == "" {
			continue
		}
		if slices.Contains(c.redacted, name) {
			v = redacted
		}
		hs = append(hs, slog.String(name, v))
	}

	return slog.Group(c.fields.Headers, hs...)
}

// addNonEmpty appends a string attribute to dst unless the value is empty.
func addNonEmpty(dst []slog.Attr, key, value string) []slog.Attr {
	if value != "" {
		dst = append(dst, slog.String(key, value))
	}

	return dst
}
