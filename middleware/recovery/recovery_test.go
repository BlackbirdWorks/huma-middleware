package recovery_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

const (
	pathBoom    = "/boom"
	pathLate    = "/late"
	pathAbort   = "/abort"
	pathOK      = "/ok"
	msgPanic    = "panic recovered"
	problemCT   = "application/problem+json"
	plainCT     = "text/plain; charset=utf-8"
	teapotBody  = "short and stout"
	bodyPartial = "partial"
)

var errDomain = errors.New("domain failure")

type empty struct{}

// syncBuffer is a log sink safe for the request goroutine and the test.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if l == "" {
			continue
		}
		m := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		out = append(out, m)
	}

	return out
}

func newLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}

	return slog.New(slog.NewJSONHandler(buf, nil)), buf
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

func registerRoutes(api huma.API) {
	huma.Get(api, pathOK, func(context.Context, *empty) (*empty, error) { return &empty{}, nil })
	huma.Get(api, pathBoom, func(context.Context, *empty) (*empty, error) { panic(errDomain) })
	huma.Get(api, pathAbort, func(context.Context, *empty) (*empty, error) { panic(http.ErrAbortHandler) })
	huma.Register(api, huma.Operation{OperationID: "late", Method: http.MethodGet, Path: pathLate},
		func(context.Context, *empty) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(ctx huma.Context) {
				ctx.SetHeader("Content-Type", "text/plain")
				ctx.SetStatus(http.StatusAccepted)
				_, _ = ctx.BodyWriter().Write([]byte(bodyPartial))
				panic("after the response started")
			}}, nil
		})
}

func get(h http.Handler, path string) (*httptest.ResponseRecorder, any) {
	rec := httptest.NewRecorder()
	var panicValue any
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	func() {
		defer func() { panicValue = recover() }()
		h.ServeHTTP(rec, req)
	}()

	return rec, panicValue
}

// exceptionTypes returns the exception.type of the exception events of a span.
func exceptionTypes(s tracetest.SpanStub) []string {
	var types []string
	for _, ev := range s.Events {
		for _, kv := range ev.Attributes {
			if kv.Key == "exception.type" {
				types = append(types, kv.Value.String())
			}
		}
	}

	return types
}

func hasStatusMetric(t *testing.T, reader sdkmetric.Reader, status int64) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range hist.DataPoints {
				if v, found := dp.Attributes.Value("http.response.status_code"); found && v.AsInt64() == status {
					return true
				}
			}
		}
	}

	return false
}

// statusRecorder keeps the last status written, informational ones included.
type statusRecorder struct {
	*httptest.ResponseRecorder

	last int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.last = code
	s.ResponseRecorder.WriteHeader(code)
}

func get2(h http.Handler, w http.ResponseWriter) (http.ResponseWriter, any) {
	var panicValue any
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	func() {
		defer func() { panicValue = recover() }()
		h.ServeHTTP(w, req)
	}()

	return w, panicValue
}

// hijackRecorder is a response writer that can be hijacked.
type hijackRecorder struct {
	*statusRecorder
}

func (hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

// TestNew covers the middleware: recovered responses, options, skips, tracing, streams and the pooled guards.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "recovered responses", run: runRecoveredResponses},
		{name: "abort handler", run: runAbortHandlerIsRaisedAgain},
		{name: "request logger", run: runRequestLogger},
		{name: "tracing", run: runWithTracing},
		{name: "started streams", run: runStartedStreamsAreLeftAlone},
		{name: "early hints", run: runEarlyHintsDoNotStartTheResponse},
		{name: "adapter unwrap", run: runAdapterUnwrapSeesThroughTheGuard},
		{name: "stream body hook", run: runStreamBodyHook},
		{name: "pooled guards", run: runPooledGuardsAreNeverShared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runRecoveredResponses(t *testing.T) {
	t.Helper()

	tests := []struct {
		opts       func(logger *slog.Logger) []recovery.Option
		name       string
		path       string
		wantType   string
		wantInBody string
		wantStatus int
		nilAPI     bool
	}{
		{
			name: "default huma error body", path: pathBoom, wantStatus: 500, wantType: problemCT,
			wantInBody: "Internal Server Error",
		},
		{
			name: "plain text without an api", path: pathBoom, nilAPI: true, wantStatus: 500, wantType: plainCT,
			wantInBody: "Internal Server Error",
		},
		{
			name:       "custom handler",
			path:       pathBoom,
			wantStatus: http.StatusTeapot,
			wantType:   "text/teapot",
			wantInBody: teapotBody,
			opts: func(*slog.Logger) []recovery.Option {
				return []recovery.Option{recovery.WithHandler(func(ctx huma.Context, recovered any) {
					ctx.SetHeader("Content-Type", "text/teapot")
					ctx.SetStatus(http.StatusTeapot)
					_, _ = ctx.BodyWriter().Write([]byte(teapotBody))
					assert.Equal(t, errDomain, recovered)
				})}
			},
		},
		{
			name: "response already started is left alone", path: pathLate, wantStatus: http.StatusAccepted,
			wantType: "text/plain", wantInBody: bodyPartial,
		},
	}
	for an, mk := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, api := mk(t)
				logger, buf := newLogger()
				opts := []recovery.Option{recovery.WithLogger(logger)}
				if tt.opts != nil {
					opts = append(opts, tt.opts(logger)...)
				}
				ra := api
				if tt.nilAPI {
					ra = nil
				}
				api.UseMiddleware(recovery.New(ra, opts...))
				registerRoutes(api)

				rec, p := get(h, tt.path)
				require.Nil(t, p, "the panic is recovered")
				assert.Equal(t, tt.wantStatus, rec.Code)
				assert.Equal(t, tt.wantType, rec.Header().Get("Content-Type"))
				assert.Contains(t, rec.Body.String(), tt.wantInBody)
				if tt.wantStatus == http.StatusAccepted {
					assert.NotContains(t, rec.Body.String(), "Internal Server Error")
				}

				records := buf.records(t)
				require.Len(t, records, 1)
				assert.Equal(t, "ERROR", records[0]["level"])
				assert.Equal(t, msgPanic, records[0]["msg"])
				assert.NotEmpty(t, records[0]["panic"])
				assert.Contains(t, records[0]["stack"], "recovery_test.go")
			})
		}
	}
}

func runAbortHandlerIsRaisedAgain(t *testing.T) {
	t.Helper()

	for an, mk := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			h, api := mk(t)
			logger, buf := newLogger()
			api.UseMiddleware(recovery.New(api, recovery.WithLogger(logger)))
			registerRoutes(api)
			_, p := get(h, pathAbort)
			assert.Equal(t, http.ErrAbortHandler, p)
			assert.Empty(t, buf.records(t), "an abort is not an error to log")
		})
	}
}

// runRequestLogger checks that without WithLogger the request logger of the logging middleware is used.
func runRequestLogger(t *testing.T) {
	t.Helper()

	h, api := humatest.New(t)
	logger, buf := newLogger()
	api.UseMiddleware(logging.New(logging.WithLogger(logger)), recovery.New(api))
	registerRoutes(api)
	rec, p := get(h, pathBoom)
	require.Nil(t, p)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	records := buf.records(t)
	require.Len(t, records, 1)
	assert.Equal(t, msgPanic, records[0]["msg"])
	assert.Equal(t, pathBoom, records[0]["route"], "the record carries the request attributes")
}

// runWithTracing checks what each order records.
func runWithTracing(t *testing.T) {
	t.Helper()

	tests := []struct {
		name             string
		recoveryInside   bool
		wantRecoveryType bool
		wantMetric500    bool
	}{
		{
			name:             "recovery inside tracing marks the span and the metrics see the 500",
			recoveryInside:   true,
			wantRecoveryType: true,
			wantMetric500:    true,
		},
		{name: "recovery outside tracing: tracing records and reports the 500 too", wantMetric500: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			trace := tracing.New(tracing.WithTracerProvider(tp), tracing.WithMeterProvider(mp),
				tracing.WithPropagators(propagation.TraceContext{}))
			h, api := humatest.New(t)
			logger, _ := newLogger()
			rec := recovery.New(api, recovery.WithLogger(logger))
			if tt.recoveryInside {
				api.UseMiddleware(trace, rec)
			} else {
				api.UseMiddleware(rec, trace)
			}
			registerRoutes(api)

			resp, p := get(h, pathBoom)
			require.Nil(t, p)
			assert.Equal(t, http.StatusInternalServerError, resp.Code)

			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, codes.Error, spans[0].Status.Code)
			types := exceptionTypes(spans[0])
			if tt.wantRecoveryType {
				assert.Equal(t, []string{"*recovery.PanicError"}, types)
			} else {
				assert.Contains(t, types, "*tracing.PanicError")
				assert.NotContains(t, types, "*recovery.PanicError")
			}
			assert.Equal(t, tt.wantMetric500, hasStatusMetric(t, reader, 500))
		})
	}
}

// runStartedStreamsAreLeftAlone checks that a body written without a status, or a flush, counts as a started response.
func runStartedStreamsAreLeftAlone(t *testing.T) {
	t.Helper()

	tests := []struct {
		name     string
		path     string
		wantBody string
	}{
		{name: "a write without a status", path: "/nostatus", wantBody: bodyPartial},
		{name: "a flush", path: "/flushed"},
	}
	for an, mk := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, api := mk(t)
				logger, buf := newLogger()
				api.UseMiddleware(recovery.New(api, recovery.WithLogger(logger)))
				registerStreams(api)
				rec, p := get(h, tt.path)
				require.Nil(t, p)
				assert.Equal(t, http.StatusOK, rec.Code)
				assert.Equal(t, tt.wantBody, rec.Body.String(), "no error body is appended to the stream")
				assert.Len(t, buf.records(t), 1, "the panic is still logged")
			})
		}
	}
}

func runEarlyHintsDoNotStartTheResponse(t *testing.T) {
	t.Helper()

	logger, _ := newLogger()
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec)
	var codes []int
	recovery.New(nil, recovery.WithLogger(logger))(hctx, func(c huma.Context) {
		c.SetStatus(http.StatusEarlyHints)
		codes = append(codes, rec.last)
		panic("boom")
	})
	assert.Equal(t, []int{http.StatusEarlyHints}, codes)
	assert.Equal(t, http.StatusInternalServerError, rec.last, "the 500 follows the early hints")
}

func runAdapterUnwrapSeesThroughTheGuard(t *testing.T) {
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
			unwrap: func(c huma.Context) *http.Request {
				r, _ := humago.Unwrap(c)

				return r
			},
		},
		{
			name: "humachi",
			build: func() (http.Handler, huma.API) {
				r := chi.NewRouter()

				return r, humachi.New(r, huma.DefaultConfig("t", "1"))
			},
			unwrap: func(c huma.Context) *http.Request {
				r, _ := humachi.Unwrap(c)

				return r
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := tt.build()
			logger, _ := newLogger()
			var got *http.Request
			api.UseMiddleware(
				recovery.New(api, recovery.WithLogger(logger)),
				func(ctx huma.Context, next func(huma.Context)) {
					got = tt.unwrap(ctx)
					next(ctx)
				},
			)
			registerRoutes(api)
			rec, p := get(h, pathOK)
			require.Nil(t, p)
			assert.Equal(t, http.StatusNoContent, rec.Code)
			require.NotNil(t, got)
			assert.Equal(t, pathOK, got.URL.Path)
		})
	}
}

func runStreamBodyHook(t *testing.T) {
	t.Helper()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name      string
		streaming bool
	}{
		{name: "an adapter with a stream callback", streaming: true},
		{name: "an adapter without one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			rec := httptest.NewRecorder()
			sink := &deadlineSink{}
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec)
			if tt.streaming {
				hctx = streamerCtx{ctxBase: hctx, sink: sink}
			}
			var deadlineErr error
			recovery.New(nil, recovery.WithLogger(logger))(hctx, func(c huma.Context) {
				bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
				require.True(t, ok, "the hook is forwarded")
				bs.StreamBody(func(w io.Writer) {
					_, _ = w.Write([]byte("data"))
					dl, isDL := w.(interface{ SetWriteDeadline(time.Time) error })
					require.True(t, isDL)
					deadlineErr = dl.SetWriteDeadline(deadline)
				})
			})
			if tt.streaming {
				assert.Equal(t, "data", sink.body.String())
				require.NoError(t, deadlineErr)
				assert.Equal(t, deadline, sink.deadline)
			} else {
				assert.Equal(t, "data", rec.Body.String(), "the stream goes to the body writer; no error body follows")
				require.ErrorIs(t, deadlineErr, http.ErrNotSupported)
			}
		})
	}
}

// runPooledGuardsAreNeverShared runs, through one middleware and from many goroutines, requests that finish
// normally, that panic and that stream from a goroutine outliving the handler: a pooled guard must never be used
// by two requests at once, nor by a stream after it was given back. Run it with -race.
func runPooledGuardsAreNeverShared(t *testing.T) {
	t.Helper()

	logger, _ := newLogger()
	mw := recovery.New(nil, recovery.WithLogger(logger))
	scenarios := []struct {
		run  func(t *testing.T, i int)
		name string
	}{
		{name: "normal", run: func(t *testing.T, i int) {
			t.Helper()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			mw(humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec), func(ctx huma.Context) {
				ctx.SetStatus(http.StatusOK)
				_, _ = io.WriteString(ctx.BodyWriter(), strconv.Itoa(i))
			})
			assert.Equal(t, strconv.Itoa(i), rec.Body.String())
		}},
		{name: "panic", run: func(t *testing.T, i int) {
			t.Helper()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			mw(
				humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec),
				func(huma.Context) { panic(i) },
			)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
		}},
		{name: "stream outliving the handler", run: func(t *testing.T, i int) {
			t.Helper()
			rec := httptest.NewRecorder()
			var done sync.WaitGroup
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec)
			mw(&asyncStreamerCtx{ctxBase: base, sink: rec, done: &done}, func(ctx huma.Context) {
				ctx.SetStatus(http.StatusOK)
				bs, ok := ctx.(interface{ StreamBody(func(io.Writer)) })
				assert.True(t, ok, "the hook is forwarded")
				bs.StreamBody(func(w io.Writer) { _, _ = io.WriteString(w, strconv.Itoa(i)) })
			})
			// Other requests run, and are given back, while the stream is still being written.
			other := httptest.NewRecorder()
			mw(humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, other), func(ctx huma.Context) {
				ctx.SetStatus(http.StatusOK)
			})
			done.Wait()
			assert.Equal(t, strconv.Itoa(i), rec.Body.String())
		}},
	}
	for _, sc := range scenarios {
		for g := range 4 {
			t.Run(sc.name+"/"+strconv.Itoa(g), func(t *testing.T) {
				t.Parallel()
				for i := range 40 {
					sc.run(t, g*100+i)
				}
			})
		}
	}
}

// TestHandler covers the net/http form.
func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "form", run: runHandlerForm},
		{name: "interim status and hijack", run: runHandlerInterimStatusAndHijack},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runHandlerForm(t *testing.T) {
	t.Helper()

	tests := []struct {
		next       http.HandlerFunc
		name       string
		wantBody   string
		opts       []recovery.Option
		wantStatus int
		wantPanic  bool
	}{
		{
			name: "default plain 500", next: func(http.ResponseWriter, *http.Request) { panic("boom") },
			wantStatus: 500, wantBody: "Internal Server Error\n",
		},
		{
			name: "response already started", wantStatus: http.StatusAccepted, wantBody: bodyPartial,
			next: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(bodyPartial))
				panic("late")
			},
		},
		{
			name: "started by a write only", wantStatus: http.StatusOK, wantBody: "x",
			next: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("x"))
				panic("late")
			},
		},
		{
			name: "started by a flush only", wantStatus: http.StatusOK,
			next: func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, http.NewResponseController(w).Flush())
				panic("late")
			},
		},
		{
			name:       "custom handler",
			wantStatus: http.StatusTeapot,
			wantBody:   teapotBody,
			opts: []recovery.Option{
				recovery.WithRequestHandler(func(w http.ResponseWriter, _ *http.Request, recovered any) {
					assert.Equal(t, "boom", recovered)
					w.WriteHeader(http.StatusTeapot)
					_, _ = w.Write([]byte(teapotBody))
				}),
			},
			next: func(http.ResponseWriter, *http.Request) { panic("boom") },
		},
		{
			name: "skipped", wantPanic: true,
			opts: []recovery.Option{recovery.WithRequestSkip(func(*http.Request) bool { return true })},
			next: func(http.ResponseWriter, *http.Request) { panic("boom") },
		},
		{
			name: "abort is raised again", wantPanic: true,
			next: func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) },
		},
		{
			name: "skipped without a panic", wantStatus: http.StatusOK, wantBody: "fine",
			opts: []recovery.Option{recovery.WithRequestSkip(func(*http.Request) bool { return true })},
			next: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("fine")) },
		},
		{
			name: "unwrap reaches the underlying writer", wantStatus: http.StatusOK,
			next: func(w http.ResponseWriter, _ *http.Request) {
				assert.ErrorIs(t, http.NewResponseController(w).SetReadDeadline(time.Time{}), http.ErrNotSupported)
			},
		},
		{
			name: "no panic", wantStatus: http.StatusOK, wantBody: "fine",
			next: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("fine")) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger()
			h := recovery.Handler(append([]recovery.Option{recovery.WithLogger(logger)}, tt.opts...)...)(tt.next)
			rec, p := get(h, "/x")
			assert.Equal(t, tt.wantPanic, p != nil)
			if tt.wantPanic {
				return
			}
			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantBody, rec.Body.String())
			if tt.name == "no panic" || strings.HasPrefix(tt.name, "skipped") || strings.HasPrefix(tt.name, "unwrap") {
				assert.Empty(t, buf.records(t))
			} else {
				assert.Len(t, buf.records(t), 1)
			}
		})
	}
}

func runHandlerInterimStatusAndHijack(t *testing.T) {
	t.Helper()

	tests := []struct {
		next       http.HandlerFunc
		name       string
		wantStatus int
		hijackable bool
	}{
		{
			name: "an early hint does not count as a started response", wantStatus: http.StatusInternalServerError,
			next: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusEarlyHints)
				panic("boom")
			},
		},
		{
			name: "101 does", wantStatus: http.StatusSwitchingProtocols,
			next: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusSwitchingProtocols)
				panic("boom")
			},
		},
		{
			name:       "an unsupported hijack leaves the response unstarted and is not advertised",
			wantStatus: http.StatusInternalServerError,
			next: func(w http.ResponseWriter, _ *http.Request) {
				_, ok := w.(http.Hijacker)
				assert.False(t, ok, "the capability is not advertised when the writer lacks it")
				_, _, err := http.NewResponseController(w).Hijack()
				assert.ErrorIs(t, err, http.ErrNotSupported)
				panic("boom")
			},
		},
		{
			name: "a hijack starts the response: no 500 is written over it", wantStatus: 0, hijackable: true,
			next: func(w http.ResponseWriter, _ *http.Request) {
				h, ok := w.(http.Hijacker)
				if assert.True(t, ok, "a direct assertion works when the writer is a hijacker") {
					_, _, err := h.Hijack()
					assert.NoError(t, err)
				}
				panic("boom")
			},
		},
		{
			name: "a flush starts the response", wantStatus: 0,
			next: func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, http.NewResponseController(w).Flush())
				panic("boom")
			},
		},
		{
			name: "a plain flusher assertion works too", wantStatus: 0,
			next: func(w http.ResponseWriter, _ *http.Request) {
				if f, ok := w.(http.Flusher); assert.True(t, ok) {
					f.Flush()
				}
				panic("boom")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger()
			h := recovery.Handler(recovery.WithLogger(logger))(tt.next)
			rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
			var w http.ResponseWriter = rec
			if tt.hijackable {
				w = hijackRecorder{rec}
			}
			_, p := get2(h, w)
			require.Nil(t, p)
			assert.Equal(t, tt.wantStatus, rec.last)
		})
	}
}

// TestPanicError_Error covers the message of a recovered panic.
func TestPanicError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "message", run: runPanicErrorMessage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runPanicErrorMessage(t *testing.T) {
	t.Helper()

	tests := []struct {
		value any
		want  string
	}{
		{value: "boom", want: "panic: boom"},
		{value: errDomain, want: "panic: domain failure"},
		{value: 42, want: "panic: 42"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, (&recovery.PanicError{Value: tt.value}).Error())
		})
	}
}

func registerStreams(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "nostatus", Method: http.MethodGet, Path: "/nostatus"},
		func(context.Context, *empty) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(ctx huma.Context) {
				ctx.SetHeader("Content-Type", "text/plain")
				_, _ = ctx.BodyWriter().Write([]byte(bodyPartial))
				panic("after an implicit 200")
			}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "flushed", Method: http.MethodGet, Path: "/flushed"},
		func(context.Context, *empty) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(ctx huma.Context) {
				if f, ok := ctx.BodyWriter().(http.Flusher); ok {
					f.Flush()
				}
				panic("after a flush")
			}}, nil
		})
}

// deadlineSink is a stream writer with write deadlines, like the one of Fiber's callback.
type deadlineSink struct {
	deadline time.Time
	body     bytes.Buffer
}

func (d *deadlineSink) Write(p []byte) (int, error) { return d.body.Write(p) }

func (d *deadlineSink) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

// ctxBase lets the test contexts embed a context.
type ctxBase huma.Context

// streamerCtx is the context of an adapter that streams through a callback.
type streamerCtx struct {
	ctxBase

	sink io.Writer
}

func (s streamerCtx) StreamBody(fn func(io.Writer)) { fn(s.sink) }

// asyncStreamerCtx runs the stream callback on another goroutine after the handler returned, as fasthttp does.
type asyncStreamerCtx struct {
	ctxBase

	sink io.Writer
	done *sync.WaitGroup
}

func (a *asyncStreamerCtx) StreamBody(fn func(io.Writer)) {
	a.done.Go(func() { fn(a.sink) })
}
