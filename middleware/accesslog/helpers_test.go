package accesslog_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
)

const (
	nameSkipped    = "skipped"
	hdrID          = "X-Request-ID"
	idGenerated    = "generated-1"
	idFromRequest  = "from-request"
	idFromResponse = "from-response"
	idLower        = "lower"
	idTrace        = "t-1"
	idOuter        = "outer-1"
	idInner        = "inner-1"
)

type idCase struct {
	reqHeaders  map[string]string
	respSet     func(h func(name, value string))
	name        string
	wantAccess  string
	wantInCtx   string
	wantRespHdr string
	wantRecord  string
	opts        []accesslog.Option
}

func idCases() []idCase {
	gen := accesslog.WithRequestIDGenerator(func() string { return idGenerated })

	return []idCase{
		{
			name:        "request header wins",
			reqHeaders:  map[string]string{hdrID: idFromRequest},
			respSet:     func(h func(string, string)) { h(hdrID, idFromResponse) },
			wantAccess:  idFromRequest,
			wantInCtx:   idFromRequest,
			wantRecord:  idFromRequest,
			wantRespHdr: idFromResponse,
		},
		{
			name:        "response header is the fallback",
			respSet:     func(h func(string, string)) { h(hdrID, idFromResponse) },
			wantAccess:  idFromResponse,
			wantInCtx:   idFromResponse,
			wantRecord:  idFromResponse,
			wantRespHdr: idFromResponse,
		},
		{
			name:       "response header is matched case-insensitively",
			respSet:    func(h func(string, string)) { h("x-request-id", idLower) },
			wantAccess: idLower, wantInCtx: idLower, wantRecord: idLower, wantRespHdr: idLower,
		},
		{name: "neither"},
		{
			name: "another header", opts: []accesslog.Option{accesslog.WithRequestIDHeader("X-Trace")},
			respSet:    func(h func(string, string)) { h("X-Trace", idTrace); h(hdrID, "ignored") },
			wantAccess: idTrace, wantInCtx: idTrace, wantRecord: idTrace, wantRespHdr: "ignored",
		},
		{
			name: "generator when neither has one", opts: []accesslog.Option{gen},
			wantAccess: idGenerated, wantInCtx: idGenerated, wantRecord: idGenerated, wantRespHdr: idGenerated,
		},
		{
			name: "generator is not used when the request has an ID", opts: []accesslog.Option{gen},
			reqHeaders: map[string]string{hdrID: idFromRequest},
			wantAccess: idFromRequest, wantInCtx: idFromRequest, wantRecord: idFromRequest,
		},
		{
			name: "generator without a header name does nothing",
			opts: []accesslog.Option{accesslog.WithRequestIDHeader(""), gen},
		},
	}
}

// TestAdapterUnwrapSeesThroughTheWrapper checks that the adapters' Unwrap helpers still reach the request.
func unwrapGo(c huma.Context) *http.Request {
	r, _ := humago.Unwrap(c)

	return r
}

func unwrapChi(c huma.Context) *http.Request {
	r, _ := humachi.Unwrap(c)

	return r
}

// hijackWriter is a response writer that can be hijacked.
type hijackWriter struct {
	*httptest.ResponseRecorder

	conn net.Conn
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return h.conn, nil, nil }

const (
	msgHandler = "handler"
	tenantAcme = "acme"
	hdrTenant  = "X-Tenant"
)

func newAPI(t *testing.T) (http.Handler, huma.API) {
	t.Helper()

	return builders()["humago"](t)
}

// hijackable is a response writer that can be hijacked.
type hijackable struct {
	*httptest.ResponseRecorder
}

func (hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

// syncSink is a stream writer.
type syncSink struct {
	mu sync.Mutex // guards buf
	n  int
}

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n += len(p)

	return len(p), nil
}

func (s *syncSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.n
}

// asyncStreamer runs the stream callback on another goroutine, as fasthttp does.
type asyncStreamer struct {
	ctxBase

	sink io.Writer
	wg   *sync.WaitGroup
}

func (a *asyncStreamer) StreamBody(fn func(io.Writer)) {
	a.wg.Go(func() { fn(a.sink) })
}

// deadlineStream is a stream writer with write deadlines.
type deadlineStream struct {
	deadline time.Time
	buf      bytes.Buffer
}

func (d *deadlineStream) Write(p []byte) (int, error) { return d.buf.Write(p) }

func (d *deadlineStream) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

type idRuleCase struct {
	reqHeaders map[string]string
	outer      string
	innerSet   string
	name       string
	wantRecord string
	wantHeader string
	opts       []accesslog.Option
}

func idRuleCases() []idRuleCase {
	return []idRuleCase{
		{
			name:  "an ID an outer wrapper set wins over the generator",
			outer: idOuter,
			opts: []accesslog.Option{
				accesslog.WithRequestIDGenerator(func() string { return idGenerated }),
			},
			wantRecord: idOuter,
			wantHeader: idOuter,
		},
		{
			name:       "the generator is last",
			opts:       []accesslog.Option{accesslog.WithRequestIDGenerator(func() string { return idGenerated })},
			wantRecord: idGenerated,
			wantHeader: idGenerated,
		},
		{
			name: "the record reports what the handler left on the response", innerSet: idInner,
			opts:       []accesslog.Option{accesslog.WithRequestIDGenerator(func() string { return idGenerated })},
			wantRecord: idInner, wantHeader: idInner,
		},
		{
			name:       "the request header wins over everything",
			reqHeaders: map[string]string{hdrID: idFromRequest},
			outer:      idOuter,
			innerSet:   idInner,
			opts:       []accesslog.Option{accesslog.WithRequestIDGenerator(func() string { return idGenerated })},
			wantRecord: idFromRequest,
			wantHeader: idInner,
		},
	}
}

// ctxBase lets the test contexts embed a context and override its methods.
type ctxBase huma.Context

// bodyContext is a context with another body writer, optionally with a stream callback like Fiber's.
type bodyContext struct {
	ctxBase

	body     io.Writer
	streamed io.Writer
}

func (c *bodyContext) BodyWriter() io.Writer { return c.body }

// streamingContext adds the StreamBody hook of adapters that stream through a callback.
type streamingContext struct {
	*bodyContext
}

func (c streamingContext) StreamBody(fn func(io.Writer)) { fn(c.streamed) }

// flushOnly is a writer that is an [http.Flusher] but not an [http.ResponseWriter].
type flushOnly struct {
	bytes.Buffer

	flushed int
}

func (f *flushOnly) Flush() { f.flushed++ }

// plainWriter is neither.
type plainWriter struct{ bytes.Buffer }

// responseOnly is an [http.ResponseWriter] that cannot flush.
type responseOnly struct {
	header http.Header
	body   bytes.Buffer
}

func (r *responseOnly) Header() http.Header         { return r.header }
func (r *responseOnly) Write(p []byte) (int, error) { return r.body.Write(p) }
func (*responseOnly) WriteHeader(int)               {}
