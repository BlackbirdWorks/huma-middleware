package recovery_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	pathBoom   = "/boom"
	pathLate   = "/late"
	pathAbort  = "/abort"
	pathOK     = "/ok"
	msgPanic   = "panic recovered"
	problemCT  = "application/problem+json"
	plainCT    = "text/plain; charset=utf-8"
	teapotBody = "short and stout"
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
				_, _ = ctx.BodyWriter().Write([]byte("partial"))
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

func TestRecoveredResponses(t *testing.T) {
	t.Parallel()

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
			wantType: "text/plain", wantInBody: "partial",
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

func TestAbortHandlerIsRaisedAgain(t *testing.T) {
	t.Parallel()

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

func TestOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check func(t *testing.T, r map[string]any)
		name  string
		opts  []recovery.Option
	}{
		{
			name: "no stack",
			opts: []recovery.Option{recovery.WithStack(false)},
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				assert.NotContains(t, r, "stack")
			},
		},
		{
			name: "stack size",
			opts: []recovery.Option{recovery.WithStackSize(64)},
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				assert.Len(t, r["stack"], 64)
			},
		},
		{
			name: "stack size is at least one",
			opts: []recovery.Option{recovery.WithStackSize(-5)},
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				assert.Len(t, r["stack"], 1)
			},
		},
		{
			name: "default stack",
			check: func(t *testing.T, r map[string]any) {
				t.Helper()
				assert.NotEmpty(t, r["stack"])
				assert.Equal(t, "domain failure", r["panic"])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := humatest.New(t)
			logger, buf := newLogger()
			api.UseMiddleware(recovery.New(api, append([]recovery.Option{recovery.WithLogger(logger)}, tt.opts...)...))
			registerRoutes(api)
			rec, p := get(h, pathBoom)
			require.Nil(t, p)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			records := buf.records(t)
			require.Len(t, records, 1)
			tt.check(t, records[0])
		})
	}
}

func TestSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		skip       bool
		wantPanic  bool
		wantStatus int
	}{
		{name: "skipped requests keep their panic", path: pathBoom, skip: true, wantPanic: true},
		{name: "skipped requests without a panic", path: pathOK, skip: true, wantStatus: http.StatusNoContent},
		{name: "other requests are recovered", path: pathBoom, wantStatus: http.StatusInternalServerError},
		{name: "requests without a panic", path: pathOK, wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := humatest.New(t)
			logger, buf := newLogger()
			api.UseMiddleware(recovery.New(api, recovery.WithLogger(logger),
				recovery.WithSkip(func(huma.Context) bool { return tt.skip })))
			registerRoutes(api)
			rec, p := get(h, tt.path)
			assert.Equal(t, tt.wantPanic, p != nil)
			if !tt.wantPanic {
				assert.Equal(t, tt.wantStatus, rec.Code)
			}
			assert.Equal(t, tt.wantStatus == http.StatusInternalServerError, len(buf.records(t)) == 1)
		})
	}
}

// TestRequestLogger checks that without WithLogger the request logger of the logging middleware is used.
func TestRequestLogger(t *testing.T) {
	t.Parallel()

	h, api := humatest.New(t)
	logger, buf := newLogger()
	api.UseMiddleware(logging.New(logging.WithLogger(logger), logging.WithoutAccessLog()), recovery.New(api))
	registerRoutes(api)
	rec, p := get(h, pathBoom)
	require.Nil(t, p)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	records := buf.records(t)
	require.Len(t, records, 1)
	assert.Equal(t, msgPanic, records[0]["msg"])
	assert.Equal(t, pathBoom, records[0]["route"], "the record carries the request attributes")
}

// TestWithTracing checks what each order records.
func TestWithTracing(t *testing.T) {
	t.Parallel()

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

func TestHandlerForm(t *testing.T) {
	t.Parallel()

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
			name: "response already started", wantStatus: http.StatusAccepted, wantBody: "partial",
			next: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte("partial"))
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

func TestPanicErrorMessage(t *testing.T) {
	t.Parallel()

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

func TestHandlerInterimStatusAndHijack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		next       http.HandlerFunc
		name       string
		wantStatus int
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
			name: "a hijack attempt counts as a started response: no 500 is written over it", wantStatus: 0,
			next: func(w http.ResponseWriter, _ *http.Request) {
				_, _, err := http.NewResponseController(w).Hijack()
				assert.ErrorIs(t, err, http.ErrNotSupported)
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
			_, p := get2(h, rec)
			require.Nil(t, p)
			assert.Equal(t, tt.wantStatus, rec.last)
		})
	}
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
