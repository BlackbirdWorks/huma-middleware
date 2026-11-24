package accesslog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/internal/reqlog"
	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
)

const (
	redactedValue = "[REDACTED]"
	msgRequest    = "request"
	routeUser     = "/users/{id}"
	wantTrace     = "4bf92f3577b34da6a3ce929d0e0e4736"
	wantSpan      = "00f067aa0ba902b7"
	wantAgent     = "test-agent"
	wantReqID     = "req-1"
	remoteAddr    = "192.0.2.7:1234"
)

type out struct {
	Body struct {
		Name string `json:"name"`
	}
}

type idIn struct {
	ID string `path:"id"`
}

// syncBuffer lets the JSON handler write from the request goroutine while the
// test reads afterwards.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var res []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if l == "" {
			continue
		}
		m := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		res = append(res, m)
	}

	return res
}

func newLogger(level slog.Level) (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	})

	return slog.New(h), buf
}

// stepClock returns a clock advancing by step on every call.
func stepClock(step time.Duration) func() time.Time {
	var n atomic.Int64
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	return func() time.Time { return base.Add(time.Duration(n.Add(1)-1) * step) }
}

type builder func(t *testing.T) (http.Handler, huma.API)

func builders() map[string]builder {
	return map[string]builder{
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

func register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-user", Method: http.MethodGet, Path: routeUser,
	}, func(_ context.Context, in *idIn) (*out, error) {
		switch in.ID {
		case "boom":
			return nil, huma.Error500InternalServerError("boom")
		case "missing":
			return nil, huma.Error404NotFound("missing")
		}
		o := &out{}
		o.Body.Name = "n"

		return o, nil
	})
}

func serve(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	req.RemoteAddr = remoteAddr
	req.Header.Set("User-Agent", wantAgent)
	req.Header.Set("X-Request-ID", wantReqID)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func setup(t *testing.T, b builder, opts ...accesslog.Option) (http.Handler, *syncBuffer) {
	t.Helper()
	l, buf := newLogger(slog.LevelDebug)
	h, api := b(t)
	api.UseMiddleware(accesslog.New(append([]accesslog.Option{accesslog.WithLogger(l)}, opts...)...))
	register(api)

	return h, buf
}

func find(lines []map[string]any, msg string) map[string]any {
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}

	return nil
}

// TestNew covers the middleware: the request logger, the access log, streaming and the request state.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "access log", run: runAccessLogDefault},
		{name: "request logger source", run: runSource},
		{name: "pooled state", run: runPooledState},

		{name: "streaming", run: runStreamingFlushAndSize},
		{name: "flush errors", run: runFlushErrorsAreReported},
		{name: "stream body hook", run: runStreamBodyHook},
		{name: "counter race", run: runCounterIsSafeForStreamingAdapters},
		{name: "write deadline", run: runWriteDeadlineThroughTheCounter},
		{name: "adapter unwrap", run: runAdapterUnwrapSeesThroughTheWrapper},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runAccessLogDefault(t *testing.T) {
	t.Helper()

	tests := []struct {
		name      string
		path      string
		wantLevel string
		wantCode  float64
	}{
		{"ok", "/users/1", "INFO", 200},
		{"client error", "/users/missing", "WARN", 404},
		{"server error", "/users/boom", "ERROR", 500},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, buf := setup(t, b, accesslog.WithClock(stepClock(5*time.Millisecond)))
				serve(h, tt.path+"?secret=1")

				rec := find(buf.lines(t), msgRequest)
				require.NotNil(t, rec)
				assert.Equal(t, tt.wantLevel, rec["level"])
				assert.InDelta(t, tt.wantCode, rec["status"], 0)
				assert.Equal(t, "GET", rec["method"])
				assert.Equal(t, strings.TrimPrefix(tt.path, ""), rec["path"], "no query string in the path")
				assert.Equal(t, routeUser, rec["route"])
				assert.Equal(t, "get-user", rec["operation_id"])
				assert.InDelta(t, float64(5*time.Millisecond), rec["duration"], 0)
				assert.Equal(t, wantAgent, rec["user_agent"])
				assert.Equal(t, wantReqID, rec["request_id"])
				assert.Equal(t, remoteAddr, rec["remote_addr"])
				assert.Positive(t, rec["response_size"])
				assert.NotContains(t, rec, "headers")
			})
		}
	}
}

func runStreamingFlushAndSize(t *testing.T) {
	t.Helper()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(slog.LevelDebug)
			h, api := b(t)
			api.UseMiddleware(accesslog.New(accesslog.WithLogger(l)))
			huma.Register(api, huma.Operation{OperationID: "stream", Method: http.MethodGet, Path: "/stream"},
				func(context.Context, *struct{}) (*huma.StreamResponse, error) {
					return &huma.StreamResponse{Body: func(ctx huma.Context) {
						w := ctx.BodyWriter()
						_, err := w.Write([]byte("hello"))
						require.NoError(t, err)
						f, ok := w.(http.Flusher)
						assert.True(t, ok, "flusher preserved")
						u, isU := w.(interface{ Unwrap() http.ResponseWriter })
						assert.True(t, isU)
						if isU {
							assert.NotNil(t, u.Unwrap())
						}
						if ok {
							f.Flush()
						}
						_, err = w.Write([]byte("world"))
						assert.NoError(t, err)
					}}, nil
				})
			rec := serve(h, "/stream")
			assert.Equal(t, "helloworld", rec.Body.String())
			assert.True(t, rec.Flushed, "the flush reached the response writer")
			line := find(buf.lines(t), msgRequest)
			require.NotNil(t, line)
			assert.InDelta(t, 10, line["response_size"], 0)
		})
	}
}

func runFlushErrorsAreReported(t *testing.T) {
	t.Helper()

	tests := []struct {
		wantErr   error
		body      func() io.Writer
		name      string
		wantFlush bool
	}{
		{name: "a flushing response writer", body: func() io.Writer { return httptest.NewRecorder() }, wantFlush: true},
		{
			name:    "a response writer that cannot flush",
			body:    func() io.Writer { return &responseOnly{header: http.Header{}} },
			wantErr: http.ErrNotSupported,
		},
		{
			name:      "a flusher that is not a response writer",
			body:      func() io.Writer { return &flushOnly{} },
			wantFlush: true,
		},
		{name: "a writer that cannot flush", body: func() io.Writer { return &plainWriter{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
			hctx := &bodyContext{ctxBase: base, body: tt.body()}
			var flushErr error
			var flusher http.Flusher
			accesslog.New(accesslog.WithLogger(logger))(hctx, func(c huma.Context) {
				fe, ok := c.BodyWriter().(interface{ FlushError() error })
				require.True(t, ok)
				flushErr = fe.FlushError()
				flusher, _ = c.BodyWriter().(http.Flusher)
				flusher.Flush()
			})
			require.ErrorIs(t, flushErr, tt.wantErr)
			if fo, ok := hctx.body.(*flushOnly); ok {
				assert.Equal(t, 2, fo.flushed, "FlushError and Flush both reached the writer")
			}
		})
	}
}

func runStreamBodyHook(t *testing.T) {
	t.Helper()

	tests := []struct {
		name      string
		wantBody  string
		wantSize  int
		streaming bool
	}{
		{name: "adapter with a stream callback", streaming: true, wantSize: 11},
		{name: "adapter without one", wantSize: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger(slog.LevelDebug)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
			body, streamed := &plainWriter{}, &plainWriter{}
			var hctx huma.Context = &bodyContext{ctxBase: base, body: body}
			if tt.streaming {
				hctx = streamingContext{&bodyContext{ctxBase: base, body: body, streamed: streamed}}
			}
			accesslog.New(accesslog.WithLogger(logger))(hctx, func(c huma.Context) {
				bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
				require.True(t, ok, "the hook is forwarded")
				bs.StreamBody(func(w io.Writer) { _, _ = w.Write([]byte("data: one\n\n")[:11]) })
			})
			if tt.streaming {
				assert.Equal(t, "data: one\n\n", streamed.String())
				assert.Empty(t, body.String())
			} else {
				assert.Equal(t, "data: one\n\n", body.String())
			}
			rec := find(buf.lines(t), msgRequest)
			require.NotNil(t, rec)
			assert.InDelta(t, tt.wantSize, rec["response_size"], 0, "the streamed bytes are counted")
		})
	}
}

// runCounterIsSafeForStreamingAdapters writes from another goroutine while the access log reads the size.
func runCounterIsSafeForStreamingAdapters(t *testing.T) {
	t.Helper()

	logger, _ := newLogger(slog.LevelDebug)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
	var wg sync.WaitGroup
	sink := &syncSink{}
	hctx := &asyncStreamer{ctxBase: base, sink: sink, wg: &wg}
	accesslog.New(accesslog.WithLogger(logger))(hctx, func(c huma.Context) {
		bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
		require.True(t, ok)
		bs.StreamBody(func(w io.Writer) {
			for range 200 {
				_, _ = w.Write([]byte("x"))
				runtime.Gosched()
			}
		})
	})
	wg.Wait()
	assert.Equal(t, 200, sink.len())
}

func runWriteDeadlineThroughTheCounter(t *testing.T) {
	t.Helper()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	logger, _ := newLogger(slog.LevelDebug)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
	sink := &deadlineStream{}
	hctx := streamingContext{&bodyContext{ctxBase: base, body: &bytes.Buffer{}, streamed: sink}}
	var got error
	accesslog.New(accesslog.WithLogger(logger))(hctx, func(c huma.Context) {
		bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
		require.True(t, ok, "the hook is forwarded")
		bs.StreamBody(func(w io.Writer) {
			dl, isDL := w.(interface{ SetWriteDeadline(time.Time) error })
			require.True(t, isDL)
			got = dl.SetWriteDeadline(deadline)
		})
	})
	require.NoError(t, got)
	assert.Equal(t, deadline, sink.deadline)
}

func runAdapterUnwrapSeesThroughTheWrapper(t *testing.T) {
	t.Helper()

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
			logger, _ := newLogger(slog.LevelDebug)
			var got *http.Request
			api.UseMiddleware(
				accesslog.New(accesslog.WithLogger(logger)),
				func(ctx huma.Context, next func(huma.Context)) {
					got = tt.unwrap(ctx)
					next(ctx)
				},
			)
			register(api)
			serve(h, "/users/1")
			require.NotNil(t, got)
			assert.Equal(t, "/users/1", got.URL.Path)
		})
	}
}

// TestHandler covers the net/http form: records, flushing, status, hijacking and what the copy gives back.
func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "records", run: runHandler},
		{name: "request logger source", run: runHandlerSource},
		{name: "pooled state", run: runHandlerPooledState},
		{name: "no route", run: runHandlerNoRoute},
		{name: "custom record", run: runHandlerCustomRecord},
		{name: "flush errors", run: runHandlerFlushErrors},
		{name: "hijack by capability", run: runHandlerOnlyAdvertisesHijackWhenItCan},
		{name: "status and hijack", run: runHandlerStatusAndHijack},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runHandler(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /raw/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, err := w.Write([]byte("tea"))
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		assert.ErrorIs(t, http.NewResponseController(w).SetReadDeadline(time.Time{}), http.ErrNotSupported)
	})
	mux.HandleFunc("GET /plain", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) })
	mux.HandleFunc("GET /noflush", func(http.ResponseWriter, *http.Request) {})
	h := accesslog.Handler(accesslog.WithLogger(l),
		accesslog.WithClock(stepClock(time.Second)),
		accesslog.WithRequestSkip(func(r *http.Request) bool { return r.URL.Path == "/noflush" }),
		accesslog.WithHeaders("User-Agent"))(mux)
	for _, p := range []string{"/raw/7", "/plain", "/noflush"} {
		serve(h, p, "X-Tenant", "acme")
	}
	lines := buf.lines(t)
	var recs []map[string]any
	for _, l := range lines {
		if l["msg"] == msgRequest {
			recs = append(recs, l)
		}
	}
	require.Len(t, recs, 2)
	assert.InDelta(t, http.StatusTeapot, recs[0]["status"], 0)
	assert.Equal(t, "/raw/{id}", recs[0]["route"])
	assert.InDelta(t, 3, recs[0]["response_size"], 0)
	assert.InDelta(t, float64(time.Second), recs[0]["duration"], 0)
	hdrs, ok := recs[0]["headers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, wantAgent, hdrs["User-Agent"])
	assert.InDelta(t, http.StatusOK, recs[1]["status"], 0)
	assert.Equal(t, "/plain", recs[1]["route"])
}

func runHandlerNoRoute(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	h := accesslog.Handler(accesslog.WithLogger(l))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serve(h, "/x")
	rec := find(buf.lines(t), msgRequest)
	require.NotNil(t, rec)
	assert.NotContains(t, rec, "route")
}

func runHandlerCustomRecord(t *testing.T) {
	t.Helper()

	l, _ := newLogger(slog.LevelDebug)
	var got accesslog.Record
	h := accesslog.Handler(
		accesslog.WithLogger(l),
		accesslog.WithRecordFunc(
			func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { got = *r },
		),
	)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x?a=b", http.NoBody)
	req.Header.Set("X-A", "1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, "/x?a=b", got.RequestURI())
	assert.Equal(t, "1", got.Header("X-A"))
}

func runHandlerFlushErrors(t *testing.T) {
	t.Helper()

	tests := []struct {
		wantErr error
		w       func() http.ResponseWriter
		name    string
	}{
		{name: "flushes", w: func() http.ResponseWriter { return httptest.NewRecorder() }},
		{
			name:    "reports a writer that cannot flush",
			w:       func() http.ResponseWriter { return &responseOnly{header: http.Header{}} },
			wantErr: http.ErrNotSupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var got error
			h := accesslog.Handler(
				accesslog.WithLogger(logger),
			)(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					got = http.NewResponseController(w).Flush()
					f, ok := w.(http.Flusher)
					assert.True(t, ok, "plain flusher assertions keep working")
					f.Flush()
				}),
			)
			h.ServeHTTP(tt.w(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))
			require.ErrorIs(t, got, tt.wantErr)
		})
	}
}

func runHandlerOnlyAdvertisesHijackWhenItCan(t *testing.T) {
	t.Helper()

	tests := []struct {
		w         http.ResponseWriter
		name      string
		wantAdver bool
	}{
		{name: "a hijacker", w: hijackable{httptest.NewRecorder()}, wantAdver: true},
		{name: "a writer that cannot be hijacked", w: httptest.NewRecorder()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var advertised bool
			var hijackErr error
			h := accesslog.Handler(
				accesslog.WithLogger(logger),
			)(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hj, ok := w.(http.Hijacker)
					advertised = ok
					if ok {
						_, _, hijackErr = hj.Hijack()
					}
				}),
			)
			h.ServeHTTP(tt.w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))
			assert.Equal(t, tt.wantAdver, advertised)
			assert.NoError(t, hijackErr)
		})
	}
}

func runHandlerStatusAndHijack(t *testing.T) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Close())
	})
	tests := []struct {
		handler    http.HandlerFunc
		w          http.ResponseWriter
		name       string
		wantStatus float64
	}{
		{
			name: "103 then a final status", wantStatus: 500,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusEarlyHints)
				w.WriteHeader(http.StatusInternalServerError)
			},
			w: httptest.NewRecorder(),
		},
		{
			name: "101 is final", wantStatus: 101,
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusSwitchingProtocols) },
			w:       httptest.NewRecorder(),
		},
		{
			name: "a flush commits the implicit 200", wantStatus: 200,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, http.NewResponseController(w).Flush())
				w.WriteHeader(http.StatusInternalServerError)
			},
			w: httptest.NewRecorder(),
		},
		{
			name: "a failed flush commits nothing", wantStatus: 500,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				assert.ErrorIs(t, http.NewResponseController(w).Flush(), http.ErrNotSupported)
				w.WriteHeader(http.StatusInternalServerError)
			},
			w: &responseOnly{header: http.Header{}},
		},
		{
			name: "hijack is not supported by the underlying writer", wantStatus: 200,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _, err := http.NewResponseController(w).Hijack()
				assert.ErrorIs(t, err, http.ErrNotSupported)
			},
			w: httptest.NewRecorder(),
		},
		{
			name: "hijack reaches a writer that supports it", wantStatus: 200,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				assert.NoError(t, err)
				assert.Equal(t, server, conn)
			},
			w: &hijackWriter{ResponseRecorder: httptest.NewRecorder(), conn: server},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger(slog.LevelDebug)
			h := accesslog.Handler(accesslog.WithLogger(logger))(tt.handler)
			h.ServeHTTP(tt.w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))
			rec := find(buf.lines(t), msgRequest)
			require.NotNil(t, rec)
			assert.InDelta(t, tt.wantStatus, rec["status"], 0)
		})
	}
}

// fakeSource is the request state of the logging middleware as the access log sees it.
type fakeSource struct {
	handler slog.Handler
	gotID   string
	ids     bool
}

func (f *fakeSource) Handler() slog.Handler { return f.handler }

func (f *fakeSource) AddIDs(rec *slog.Record, buf []slog.Attr) {
	f.ids = true
	buf = append(buf[:0], slog.String("trace_id", "t"), slog.String("span_id", "s"))
	rec.AddAttrs(slog.Attr{Value: slog.GroupValue(buf...)})
}

func (f *fakeSource) SetRequestID(id string) { f.gotID = id }

// sourceRequest is a request whose context carries the source.
func sourceRequest(src *fakeSource) *http.Request {
	ctx := context.WithValue(context.Background(), reqlog.Key{}, reqlog.Source(src))

	return httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", http.NoBody)
}

// runSource checks the huma form against the request state of the logging middleware: the record goes to its
// handler, carries the IDs, and it learns the generated request ID.
func runSource(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	src := &fakeSource{handler: l.Handler()}
	hctx := humatest.NewContext(
		&huma.Operation{OperationID: "x", Path: "/x"},
		sourceRequest(src),
		httptest.NewRecorder(),
	)
	accesslog.New(accesslog.WithRequestIDGenerator(func() string { return "gen-1" }))(hctx, func(huma.Context) {})
	rec := find(buf.lines(t), msgRequest)
	require.NotNil(t, rec)
	assert.Equal(t, "t", rec["trace_id"])
	assert.Equal(t, "s", rec["span_id"])
	assert.Equal(t, "gen-1", rec["request_id"])
	assert.Equal(t, "gen-1", src.gotID)
	assert.True(t, src.ids)
}

// runHandlerSource is runSource for the net/http form, where the ID is found on the response.
func runHandlerSource(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	src := &fakeSource{handler: l.Handler()}
	h := accesslog.Handler(accesslog.WithRequestIDGenerator(func() string { return "gen-2" }))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), sourceRequest(src))
	rec := find(buf.lines(t), msgRequest)
	require.NotNil(t, rec)
	assert.Equal(t, "t", rec["trace_id"])
	assert.Equal(t, "gen-2", src.gotID)
}

// collector keeps the records the access log hands to WithRecordFunc.
type collector struct {
	paths map[string]int64
	mu    sync.Mutex
}

func (c *collector) add(path string, size int64) {
	c.mu.Lock()
	c.paths[path] = size
	c.mu.Unlock()
}

// runPooledState sends many concurrent requests through one middleware and checks that every record is its own
// request's: the pooled state is never shared between two requests in flight.
func runPooledState(t *testing.T) {
	t.Helper()

	got := &collector{paths: map[string]int64{}}
	mw := accesslog.New(accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) {
		got.add(r.Path, r.Size)
	}))
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 20 {
				path := "/p" + strconv.Itoa(g) + "-" + strconv.Itoa(i)
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
				hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: path}, req, httptest.NewRecorder())
				mw(hctx, func(c huma.Context) {
					_, _ = c.BodyWriter().Write(make([]byte, g+1))
				})
			}
		})
	}
	wg.Wait()
	require.Len(t, got.paths, 320)
	for g := range 16 {
		assert.Equal(t, int64(g+1), got.paths["/p"+strconv.Itoa(g)+"-0"])
	}
}

// runHandlerPooledState is runPooledState for the net/http form.
func runHandlerPooledState(t *testing.T) {
	t.Helper()

	got := &collector{paths: map[string]int64{}}
	h := accesslog.Handler(accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) {
		got.add(r.Path, r.Size)
	}))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, len(r.URL.Path)))
	}))
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 20 {
				path := "/p" + strconv.Itoa(g) + "-" + strconv.Itoa(i)
				h.ServeHTTP(httptest.NewRecorder(),
					httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
			}
		})
	}
	wg.Wait()
	require.Len(t, got.paths, 320)
	for path, size := range got.paths {
		assert.Equal(t, int64(len(path)), size)
	}
}
