package compression_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

const (
	ctJSON     = "application/json"
	ctText     = "text/plain; charset=utf-8"
	hdrAE      = "Accept-Encoding"
	hdrCE      = "Content-Encoding"
	hdrCL      = "Content-Length"
	hdrVary    = "Vary"
	hdrETag    = "ETag"
	hdrCT      = "Content-Type"
	encGzip    = "gzip"
	encBrotli  = "br"
	encZstd    = "zstd"
	varyAE     = "Accept-Encoding"
	strongETag = `"abc"`
	weakETag   = `W/"abc"`
	allAE      = "gzip, br, zstd"

	ctHTML         = "text/html"
	ctSSE          = "text/event-stream"
	ctPNG          = "image/png"
	ctOctet        = "application/octet-stream"
	pathList       = "/list"
	pathAPI        = "/api/data"
	origin         = "Origin"
	ctCSS          = "text/css"
	encFake        = "deflate"
	opID           = "op-x"
	ctJS           = "application/javascript"
	noCT           = "no content type"
	pathMetrics    = "/metrics"
	pathTxtPattern = "/files/*.txt"
	pathAPIPrefix  = "/api"
	ctVendor       = "application/vnd.api+json"
	aeGzipBr       = "gzip, br"
	ctZip          = "application/zip"
	encIdent       = "identity"
)

// big is a compressible body above the default minimum size.
func big() string { return strings.Repeat("hello compressible world ", 200) }

func mustNew(t *testing.T, opts ...compression.Option) compression.Middleware {
	t.Helper()
	mw, err := compression.New(opts...)
	require.NoError(t, err)

	return mw
}

// reply sets the headers (name, value pairs), the status and writes the chunks.
func reply(status int, headers []string, chunks ...string) func(huma.Context) {
	return func(ctx huma.Context) {
		for i := 0; i+1 < len(headers); i += 2 {
			ctx.SetHeader(headers[i], headers[i+1])
		}
		ctx.SetStatus(status)
		for _, c := range chunks {
			_, _ = ctx.BodyWriter().Write([]byte(c))
		}
	}
}

// result is a finished response: headers, status and the body as sent.
type result struct {
	Header     http.Header
	Raw        []byte
	StatusCode int
}

// resultOf reads a recorded response.
func resultOf(t *testing.T, rec *httptest.ResponseRecorder) result {
	t.Helper()
	res := rec.Result()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	return result{Header: res.Header, Raw: raw, StatusCode: res.StatusCode}
}

// decoded returns the body decoded according to Content-Encoding.
func (r result) decoded(t *testing.T) string {
	t.Helper()

	return decode(t, r.Header.Get(hdrCE), r.Raw)
}

// serve runs the middleware around next on a humatest context.
func serve(
	t *testing.T, mw compression.Middleware, method string, reqHeaders []string, next func(huma.Context),
) result {
	t.Helper()
	rec := httptest.NewRecorder()
	serveRec(mw, method, "/x", reqHeaders, rec, next)

	return resultOf(t, rec)
}

func serveRec(
	mw compression.Middleware, method, target string, reqHeaders []string, w http.ResponseWriter,
	next func(huma.Context),
) {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)
	for i := 0; i+1 < len(reqHeaders); i += 2 {
		req.Header.Set(reqHeaders[i], reqHeaders[i+1])
	}
	op := &huma.Operation{OperationID: opID, Path: target}
	mw(humatest.NewContext(op, req, w), next)
}

func decode(t *testing.T, enc string, raw []byte) string {
	t.Helper()
	var r io.Reader
	switch enc {
	case "":
		return string(raw)
	case encGzip:
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		require.NoError(t, err)
		r = gr
	case encBrotli:
		r = brotli.NewReader(bytes.NewReader(raw))
	case encZstd:
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		require.NoError(t, err)
		defer zr.Close()
		r = zr
	default:
		require.Failf(t, "unknown encoding", "%q", enc)
	}
	out, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(out)
}

// decodeNoRequire is decode for use off the test goroutine; it returns an empty string on failure.
func decodeNoRequire(enc string, raw []byte) string {
	var r io.Reader
	switch enc {
	case encGzip:
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return ""
		}
		r = gr
	case encBrotli:
		r = brotli.NewReader(bytes.NewReader(raw))
	default:
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return ""
		}
		defer zr.Close()
		r = zr
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return ""
	}

	return string(out)
}

// levelOpt sets the default compression level of one encoding.
func levelOpt(enc compression.Encoding, level int) compression.Option {
	var levels compression.Levels
	switch enc {
	case compression.Brotli:
		levels.Brotli = new(level)
	case compression.Zstd:
		levels.Zstd = new(level)
	default:
		levels.Gzip = new(level)
	}

	return compression.WithDefault(compression.Config{Levels: levels})
}

type preferenceCase struct {
	name string
	ct   string
	ae   string
	want string
	opts []compression.Option
}

func preferenceCases() []preferenceCase {
	return []preferenceCase{
		{
			name: "global gzip only",
			ct:   ctJSON,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Gzip}}),
			},
			want: encGzip,
		},
		{
			name: "global order",
			ct:   ctText,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithDefault(
					compression.Config{Preference: []compression.Encoding{compression.Zstd, compression.Gzip}},
				),
			},
			want: encZstd,
		},
		{
			name: "global replaces the json rule",
			ct:   ctJSON,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Brotli}}),
			},
			want: encBrotli,
		},
	}
}

// typePreferenceCases are the cases of preferences set per content type.
func typePreferenceCases() []preferenceCase {
	return []preferenceCase{
		{
			name: "per type",
			ct:   ctHTML,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					ctHTML,
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type enables an encoding outside the global list", ct: ctHTML, ae: allAE,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Gzip}}),
				compression.WithContentType(
					ctHTML,
					compression.Config{Preference: []compression.Encoding{compression.Zstd}},
				),
			},
			want: encZstd,
		},
		{
			name: "per type wildcard",
			ct:   ctCSS,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"text/*",
					compression.Config{Preference: []compression.Encoding{compression.Gzip, compression.Brotli}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type wins over the json rule",
			ct:   ctJSON,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"application/json",
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type suffix",
			ct:   "application/x+xml",
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"+xml",
					compression.Config{Preference: []compression.Encoding{compression.Zstd}},
				),
			},
			want: encZstd,
		},
		{
			name: "per type with params",
			ct:   "TEXT/HTML; charset=utf-8",
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"Text/Html",
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type other type unaffected",
			ct:   ctText,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					ctHTML,
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encBrotli,
		},
		{
			name: "client accepts only a disabled encoding",
			ct:   ctText,
			ae:   encZstd,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Gzip}}),
			},
		},
	}
}

type invalidCase struct {
	wantErr error
	opt     compression.Option
	name    string
}

func invalidCases() []invalidCase {
	return []invalidCase{
		{
			name:    "gzip level too low",
			opt:     levelOpt(compression.Gzip, -3),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "gzip level too high",
			opt:     levelOpt(compression.Gzip, 10),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "brotli level too low",
			opt:     levelOpt(compression.Brotli, -1),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "brotli level too high",
			opt:     levelOpt(compression.Brotli, 12),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "zstd level negative",
			opt:     levelOpt(compression.Zstd, -1),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "zstd level too high",
			opt:     levelOpt(compression.Zstd, 23),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "unknown encoding preference",
			opt:     compression.WithDefault(compression.Config{Preference: []compression.Encoding{encFake}}),
			wantErr: compression.ErrUnknownEncoding,
		},
		{
			name:    "empty preference",
			opt:     compression.WithDefault(compression.Config{Preference: []compression.Encoding{}}),
			wantErr: compression.ErrEmptyPreference,
		},
		{
			name: "unknown encoding in a type preference",
			opt: compression.WithContentType(
				"text/*",
				compression.Config{Preference: []compression.Encoding{encFake}},
			),
			wantErr: compression.ErrUnknownEncoding,
		},
		{
			name: "type level out of range",
			opt: compression.WithContentType(
				"text/*",
				compression.Config{Levels: compression.Levels{Zstd: new(99)}},
			),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "type negative min size",
			opt:     compression.WithContentType("text/*", compression.Config{MinSize: -1}),
			wantErr: compression.ErrInvalidMinSize,
		},
		{
			name: "bad type preference pattern",
			opt: compression.WithContentType(
				"html",
				compression.Config{Preference: []compression.Encoding{compression.Gzip}},
			),
			wantErr: compression.ErrInvalidPattern,
		},
		{name: "empty content type", opt: compression.WithContentTypes(""), wantErr: compression.ErrInvalidPattern},
		{name: "bare plus", opt: compression.WithContentTypes("+"), wantErr: compression.ErrInvalidPattern},
		{
			name:    "negative min size",
			opt:     compression.WithDefault(compression.Config{MinSize: -1}),
			wantErr: compression.ErrInvalidMinSize,
		},
		{
			name:    "bad path pattern",
			opt:     compression.WithSkipPathPatterns("[a-"),
			wantErr: compression.ErrInvalidPattern,
		},
	}
}

// flushRecorder records the body at the time of each flush.
type flushRecorder struct {
	*httptest.ResponseRecorder

	atFlush []int
}

func (f *flushRecorder) Flush() {
	f.atFlush = append(f.atFlush, f.Body.Len())
	f.ResponseRecorder.Flush()
}

// plainWriter is an [http.ResponseWriter] that is not an [http.Flusher].
type plainWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (*plainWriter) WriteHeader(int)               {}

var errSink = errors.New("sink failed")

// failingWriter is a body writer that always fails.
type failingWriter struct{}

func (*failingWriter) Write([]byte) (int, error) { return 0, errSink }

func unwrapGo(c huma.Context) *http.Request {
	r, _ := humago.Unwrap(c)

	return r
}

func unwrapChi(c huma.Context) *http.Request {
	r, _ := humachi.Unwrap(c)

	return r
}

// deadlineWriter is a body writer that records the write deadline it is given.
type deadlineWriter struct {
	deadline time.Time
	body     bytes.Buffer
}

func (d *deadlineWriter) Write(p []byte) (int, error) { return d.body.Write(p) }

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

// deadlineRW is a response writer that records the write deadlines set on it.
type deadlineRW struct {
	*httptest.ResponseRecorder

	deadlines []time.Time
}

func (d *deadlineRW) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)

	return nil
}

func (d *deadlineRW) count() int { return len(d.deadlines) }

type message struct {
	Text string `json:"text"`
}

func prefer(encs ...compression.Encoding) compression.Config {
	return compression.Config{Preference: encs}
}

// failFlusher is a response writer that records informational statuses.
type statusRecorder struct {
	header http.Header
	codes  []int
	body   []byte
}

func (s *statusRecorder) Header() http.Header { return s.header }

func (s *statusRecorder) WriteHeader(code int) { s.codes = append(s.codes, code) }

func (s *statusRecorder) Write(p []byte) (int, error) {
	s.body = append(s.body, p...)

	return len(p), nil
}

// streamerContext is a context of an adapter that streams the body through a callback, like Fiber.
type streamerContext struct {
	ctxBase

	sink     io.Writer
	headers  func() http.Header
	onStream func(headers http.Header)
}

func (s *streamerContext) StreamBody(fn func(io.Writer)) {
	s.onStream(s.headers())
	fn(s.sink)
}

const itemValue = "item-value"

type listOut struct {
	Body struct {
		Items []string `json:"items"`
	}
}

type streamIn struct{}

func adapterAPIs() map[string]func(t *testing.T) (http.Handler, huma.API) {
	return map[string]func(t *testing.T) (http.Handler, huma.API){
		"humatest": func(t *testing.T) (http.Handler, huma.API) {
			t.Helper()
			h, api := humatest.New(t)

			return h, api
		},
		"humago": func(*testing.T) (http.Handler, huma.API) {
			mux := http.NewServeMux()

			return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
		},
		"humachi": func(*testing.T) (http.Handler, huma.API) {
			r := chi.NewRouter()

			return r, humachi.New(r, huma.DefaultConfig("t", "1"))
		},
	}
}

func registerRoutes(api huma.API) {
	huma.Get(api, pathList, func(context.Context, *struct{}) (*listOut, error) {
		o := &listOut{}
		for range 300 {
			o.Body.Items = append(o.Body.Items, itemValue)
		}

		return o, nil
	})
	huma.Get(api, "/stream", func(context.Context, *streamIn) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			ctx.SetHeader(hdrCT, "text/plain")
			w := ctx.BodyWriter()
			_, _ = w.Write([]byte("first;"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write([]byte("second"))
		}}, nil
	})
}

// asyncStreamer runs the stream callback on another goroutine after the handler returned, as fasthttp does.
type asyncStreamer struct {
	ctxBase

	sink io.Writer
	done *sync.WaitGroup
}

func (a *asyncStreamer) StreamBody(fn func(io.Writer)) {
	a.done.Go(func() { fn(a.sink) })
}

// ctxBase lets bodyOverride embed a context and still define a Context method.
type ctxBase huma.Context

// bodyOverride is a huma.Context with another body writer.
type bodyOverride struct {
	ctxBase

	w io.Writer
}

func (b bodyOverride) BodyWriter() io.Writer { return b.w }

func newContextWithBody(req *http.Request, rec *httptest.ResponseRecorder, w io.Writer) huma.Context {
	op := &huma.Operation{OperationID: opID, Path: "/x"}

	return bodyOverride{ctxBase: humatest.NewContext(op, req, rec), w: w}
}

// newContextNoOp is a context without an operation.
func newContextNoOp(req *http.Request, rec *httptest.ResponseRecorder) huma.Context {
	return humatest.NewContext(nil, req, rec)
}
