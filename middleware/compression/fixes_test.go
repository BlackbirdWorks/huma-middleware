package compression_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func unwrapGo(c huma.Context) *http.Request {
	r, _ := humago.Unwrap(c)

	return r
}

func unwrapChi(c huma.Context) *http.Request {
	r, _ := humachi.Unwrap(c)

	return r
}

func TestAdapterUnwrapSeesThroughTheWrapper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		build  func() (http.Handler, huma.API)
		unwrap func(huma.Context) *http.Request
		name   string
	}{
		{
			name: "humago",
			build: func() (http.Handler, huma.API) {
				mux := http.NewServeMux()

				return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
			},
			unwrap: unwrapGo,
		},
		{
			name: "humachi",
			build: func() (http.Handler, huma.API) {
				r := chi.NewRouter()

				return r, humachi.New(r, huma.DefaultConfig("t", "1"))
			},
			unwrap: unwrapChi,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := tt.build()
			var got *http.Request
			api.UseMiddleware(mustNew(t), func(ctx huma.Context, next func(huma.Context)) {
				got = tt.unwrap(ctx)
				next(ctx)
			})
			registerRoutes(api)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, pathList, http.NoBody)
			h.ServeHTTP(httptest.NewRecorder(), req)
			require.NotNil(t, got)
			assert.Equal(t, pathList, got.URL.Path)
		})
	}
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

func TestWriteDeadlineReachesTheWriter(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	dw := &deadlineWriter{}
	tests := []struct {
		sink    func() io.Writer
		wantErr error
		name    string
	}{
		{name: "a writer with deadlines", sink: func() io.Writer { return dw }},
		{
			name:    "a response writer without deadlines",
			sink:    func() io.Writer { return httptest.NewRecorder() },
			wantErr: http.ErrNotSupported,
		},
		{name: "a plain writer", sink: func() io.Writer { return &bytes.Buffer{} }, wantErr: http.ErrNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			base := newContextWithBody(req, httptest.NewRecorder(), tt.sink())
			var got error
			mustNew(t)(base, func(c huma.Context) {
				dl, ok := c.BodyWriter().(interface{ SetWriteDeadline(time.Time) error })
				require.True(t, ok, "the body writer keeps the capability")
				got = dl.SetWriteDeadline(deadline)
			})
			require.ErrorIs(t, got, tt.wantErr)
			if tt.wantErr == nil {
				assert.Equal(t, deadline, dw.deadline)
			}
		})
	}
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

// TestServerSentEventsKeepTheirWriteDeadline runs huma's SSE under a real adapter, through the middleware, to a
// writer that records deadlines: huma sets one before each event.
func TestServerSentEventsKeepTheirWriteDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		wantEncoding string
		opts         []compression.Option
	}{
		{name: "not compressed by default"},
		{
			name: "compressed on request", wantEncoding: encGzip,
			opts: []compression.Option{
				compression.WithContentTypes(ctSSE),
				compression.WithFallback(compression.Config{MinSize: 1}),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("t", "1"))
			api.UseMiddleware(mustNew(t, tt.opts...))
			sse.Register(api, huma.Operation{OperationID: "events", Method: http.MethodGet, Path: "/events"},
				map[string]any{"message": &message{}},
				func(_ context.Context, _ *struct{}, send sse.Sender) {
					assert.NoError(t, send.Data(&message{Text: "one"}))
					assert.NoError(t, send.Data(&message{Text: "two"}))
				})

			w := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events", http.NoBody)
			req.Header.Set(hdrAE, encGzip)
			mux.ServeHTTP(w, req)

			res := resultOf(t, w.ResponseRecorder)
			assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
			assert.Contains(t, res.decoded(t), "one")
			assert.Contains(t, res.decoded(t), "two")
			assert.GreaterOrEqual(t, w.count(), 2, "a write deadline was set for each event")
		})
	}
}

func TestDisabledDefaultDisablesJSONToo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		wantEnc string
		opts    []compression.Option
	}{
		{
			name: "the built-in JSON rule does not enable it",
			opts: []compression.Option{compression.WithFallback(compression.Config{Disabled: true})},
		},
		{
			name: "an entry of the caller does, with the default preference", wantEnc: encBrotli,
			opts: []compression.Option{
				compression.WithFallback(compression.Config{Disabled: true}),
				compression.WithContentType(ctJSON, compression.Config{}),
			},
		},
		{name: "enabled by default", wantEnc: encZstd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, tt.opts...),
				http.MethodGet,
				[]string{hdrAE, allAE},
				reply(200, []string{hdrCT, ctJSON}, big()),
			)
			assert.Equal(t, tt.wantEnc, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

func TestAcceptEncodingSplitAcrossLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		want  string
		lines []string
	}{
		{name: "one line", lines: []string{"gzip;q=0.2, br"}, want: encBrotli},
		{name: "a better coding on a later line", lines: []string{"gzip;q=0.2", "br"}, want: encBrotli},
		{name: "three lines", lines: []string{"gzip;q=0.5", "br;q=0.1", "zstd;q=0.4"}, want: encGzip},
		{name: "the first line decides alone when it is the best", lines: []string{"gzip", "br;q=0.5"}, want: encGzip},
		{name: "an excluded coding on a later line", lines: []string{"br, gzip", "br;q=0"}, want: encBrotli},
		{name: "no header", lines: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			for _, l := range tt.lines {
				req.Header.Add(hdrAE, l)
			}
			mustNew(t)(humatest.NewContext(&huma.Operation{OperationID: opID, Path: "/x"}, req, rec),
				reply(200, []string{hdrCT, ctText}, big()))
			res := resultOf(t, rec)
			assert.Equal(t, tt.want, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}
