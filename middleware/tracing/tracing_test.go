package tracing_test

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

const (
	spanUser   = "GET /users/{id}"
	pathHealth = "/healthz"
)

type out struct {
	Body struct {
		TraceID string `json:"traceId"`
	}
}

type idIn struct {
	ID string `path:"id"`
}

type fixture struct {
	handler http.Handler
	spans   *tracetest.InMemoryExporter
	reader  *sdkmetric.ManualReader
}

type builder func(t *testing.T, mw func(huma.Context, func(huma.Context))) (http.Handler, huma.API)

func builders() map[string]builder {
	return map[string]builder{
		"humatest": func(t *testing.T, _ func(huma.Context, func(huma.Context))) (http.Handler, huma.API) {
			t.Helper()
			h, api := humatest.New(t)

			return h, api
		},
		"humago": func(_ *testing.T, _ func(huma.Context, func(huma.Context))) (http.Handler, huma.API) {
			mux := http.NewServeMux()

			return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
		},
		"humachi": func(_ *testing.T, _ func(huma.Context, func(huma.Context))) (http.Handler, huma.API) {
			r := chi.NewRouter()

			return r, humachi.New(r, huma.DefaultConfig("t", "1"))
		},
	}
}

func newFixture(t *testing.T, b builder, opts ...tracing.Option) fixture {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		assert.NoError(t, tp.Shutdown(context.Background()))
		assert.NoError(t, mp.Shutdown(context.Background()))
	})

	base := make([]tracing.Option, 0, 3+len(opts))
	base = append(base,
		tracing.WithTracerProvider(tp),
		tracing.WithMeterProvider(mp),
		tracing.WithPropagators(propagation.TraceContext{}),
	)
	h, api := b(t, nil)
	api.UseMiddleware(tracing.New(append(base, opts...)...))

	huma.Get(api, "/users/{id}", func(ctx context.Context, in *idIn) (*out, error) {
		switch in.ID {
		case "boom":
			return nil, huma.Error500InternalServerError("boom")
		case "missing":
			return nil, huma.Error404NotFound("missing")
		}
		o := &out{}
		o.Body.TraceID = trace.SpanContextFromContext(ctx).TraceID().String()

		return o, nil
	})
	huma.Get(api, pathHealth, func(context.Context, *struct{}) (*out, error) { return &out{}, nil })

	return fixture{handler: h, spans: exp, reader: reader}
}

func (f fixture) do(path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	return rec
}

func attr(kvs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}

	return attribute.Value{}, false
}

// durationRoutes returns the http.route label of every request duration data point.
func durationRoutes(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var routes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || m.Name != "http.server.request.duration" {
				continue
			}
			for _, dp := range h.DataPoints {
				if v, found := dp.Attributes.Value("http.route"); found {
					routes = append(routes, v.AsString())
				}
			}
		}
	}

	return routes
}

func panicIf(b bool) {
	if b {
		panic("again")
	}
}

// TestNew covers the middleware: spans, parents, filters, names, options, panics and the pooled views.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "spans", run: runSpans},
		{name: "parent and handler context", run: runParentAndHandlerContext},
		{name: "default propagator", run: runDefaultPropagatorAndNoOperation},
		{name: "pooled views", run: runPooledViewsAreNeverShared},
		{name: "panic", run: runPanicIsRecordedAndRaisedAgain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runSpans(t *testing.T) {
	t.Helper()

	tests := []struct {
		name       string
		path       string
		wantName   string
		wantStatus codes.Code
		wantCode   int64
		wantHTTP   int
	}{
		{"ok", "/users/42", spanUser, codes.Unset, 200, http.StatusOK},
		{"server error", "/users/boom", spanUser, codes.Error, 500, http.StatusInternalServerError},
		{"client error unset", "/users/missing", spanUser, codes.Unset, 404, http.StatusNotFound},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, b)
				rec := f.do(tt.path)
				require.Equal(t, tt.wantHTTP, rec.Code)

				spans := f.spans.GetSpans()
				require.Len(t, spans, 1)
				s := spans[0]
				assert.Equal(t, tt.wantName, s.Name)
				assert.Equal(t, trace.SpanKindServer, s.SpanKind)
				assert.Equal(t, tt.wantStatus, s.Status.Code)
				route, ok := attr(s.Attributes, "http.route")
				require.True(t, ok)
				assert.Equal(t, "/users/{id}", route.AsString())
				code, ok := attr(s.Attributes, "http.response.status_code")
				require.True(t, ok)
				assert.Equal(t, tt.wantCode, code.AsInt64())
				method, _ := attr(s.Attributes, "http.request.method")
				assert.Equal(t, "GET", method.AsString())
			})
		}
	}
}

func runParentAndHandlerContext(t *testing.T) {
	t.Helper()

	const (
		wantTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		wantSpan  = "00f067aa0ba902b7"
	)
	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, b)
			rec := f.do("/users/42", "Traceparent", "00-"+wantTrace+"-"+wantSpan+"-01")
			require.Equal(t, http.StatusOK, rec.Code)

			spans := f.spans.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, wantTrace, spans[0].SpanContext.TraceID().String())
			assert.Equal(t, wantTrace, spans[0].Parent.TraceID().String())
			assert.Equal(t, wantSpan, spans[0].Parent.SpanID().String())
			assert.True(t, spans[0].Parent.IsRemote())
			assert.Contains(t, rec.Body.String(), wantTrace, "handler sees the span context")
		})
	}
}

func runDefaultPropagatorAndNoOperation(t *testing.T) {
	t.Helper()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	mw := tracing.New(tracing.WithTracerProvider(tp), tracing.WithSpanNameFormatter(func(m, r string) string {
		return m + "|" + r
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	hctx := humatest.NewContext(nil, req, httptest.NewRecorder())
	called := false
	mw(hctx, func(huma.Context) { called = true })
	assert.True(t, called)
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET|", spans[0].Name, "no operation, no route")
}

// runHumaFormFiltersByTemplate checks that the path and request filters of the huma form see the route
// template only: the concrete URL path "/users/42" must not match a skip for "/users/42".
// runPooledViewsAreNeverShared serves requests from many goroutines through one middleware, some of which panic:
// a pooled view must never be used by two requests at once. Run it with -race.
func runPooledViewsAreNeverShared(t *testing.T) {
	t.Helper()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	mw := tracing.New(tracing.WithTracerProvider(tp), tracing.WithPropagators(propagation.TraceContext{}))
	var wg sync.WaitGroup
	const goroutines, perGoroutine = 8, 25
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				route := "/r" + strconv.Itoa(g)
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, route, http.NoBody)
				req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
				req.Header.Add("X-Multi", "a")
				hctx := humatest.NewContext(&huma.Operation{OperationID: "r", Path: route}, req, httptest.NewRecorder())
				run := func() { mw(hctx, func(huma.Context) { panicIf(i%5 == 0) }) }
				if i%5 == 0 {
					assert.Panics(t, run)
				} else {
					run()
				}
			}
		})
	}
	wg.Wait()
	spans := exp.GetSpans()
	assert.Len(t, spans, goroutines*perGoroutine)
	for _, s := range spans {
		assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", s.SpanContext.TraceID().String())
		route, ok := attr(s.Attributes, "http.route")
		assert.True(t, ok)
		assert.Equal(t, "GET "+route.AsString(), s.Name)
	}
}

func runPanicIsRecordedAndRaisedAgain(t *testing.T) {
	t.Helper()

	tests := []struct {
		value        any
		name         string
		wantMessage  string
		wantRecorded bool
	}{
		{name: "string", value: panicMsg, wantMessage: "panic: boom", wantRecorded: true},
		{name: "error", value: errDomain, wantMessage: "panic: domain failure", wantRecorded: true},
		{name: "abort handler is not an error", value: http.ErrAbortHandler, wantRecorded: false},
	}
	for _, form := range []string{formHuma, formHandler} {
		for _, tt := range tests {
			t.Run(form+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				exp := tracetest.NewInMemoryExporter()
				reader := sdkmetric.NewManualReader()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
				mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
				opts := []tracing.Option{
					tracing.WithTracerProvider(tp), tracing.WithMeterProvider(mp),
					tracing.WithPropagators(propagation.TraceContext{}),
				}
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom", http.NoBody)
				run := func() {
					if form == formHandler {
						h := tracing.Handler(
							http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(tt.value) }),
							opts...)
						h.ServeHTTP(httptest.NewRecorder(), req)

						return
					}
					op := &huma.Operation{OperationID: panicMsg, Path: "/boom"}
					hctx := humatest.NewContext(op, req, httptest.NewRecorder())
					tracing.New(opts...)(hctx, func(huma.Context) { panic(tt.value) })
				}
				assert.PanicsWithValue(t, tt.value, run, "the original value is raised again")

				spans := exp.GetSpans()
				require.Len(t, spans, 1, "the span is ended")
				s := spans[0]
				recorded := panicEvents(s)
				if !tt.wantRecorded {
					assert.Empty(t, recorded)

					return
				}
				assert.Equal(t, codes.Error, s.Status.Code)
				require.Len(t, recorded, 1)
				assert.Equal(t, tt.wantMessage, recorded[0]["exception.message"])
				assert.Contains(t, recorded[0]["exception.stacktrace"], "tracing_test.go")
				code, ok := attr(s.Attributes, "http.response.status_code")
				require.True(t, ok)
				assert.EqualValues(t, http.StatusInternalServerError, code.AsInt64())
				if form == formHuma {
					assert.Equal(
						t,
						[]string{"/boom"},
						durationRoutes(t, reader),
						"otelhttp recorded the 500 in its metrics",
					)
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
		{name: "wrapper", run: runHandlerWrapper},
		{name: "no pattern", run: runHandlerNameFormatterWithoutPattern},
		{name: "route on the span", run: runHandlerPutsTheRouteOnTheSpan},
		{name: "multipart and pattern", run: runHandlerGivesBackWhatItsCopyLearned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runHandlerWrapper(t *testing.T) {
	t.Helper()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /raw/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := tracing.Handler(mux,
		tracing.WithTracerProvider(tp),
		tracing.WithPropagators(propagation.TraceContext{}),
		tracing.WithSkipPaths("/skip"),
		tracing.WithRequestFilter(func(r *http.Request) bool { return r.URL.Path != "/nofilter" }),
	)

	for _, p := range []string{"/raw/1", "/skip", "/nofilter"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, p, http.NoBody))
	}
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /raw/{id}", spans[0].Name)
}

func runHandlerNameFormatterWithoutPattern(t *testing.T) {
	t.Helper()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	h := tracing.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		tracing.WithTracerProvider(tp),
		tracing.WithSpanNameFormatter(func(m, r string) string { return m + "|" + r }))
	h.ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody),
	)
	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET|", spans[0].Name)
}

func runHandlerPutsTheRouteOnTheSpan(t *testing.T) {
	t.Helper()

	tests := []struct {
		name      string
		target    string
		host      string
		wantName  string
		wantRoute string
		panics    bool
	}{
		{name: "matched pattern", target: "/raw/7", wantName: "GET /raw/{id}", wantRoute: "/raw/{id}"},
		{
			name:      "method and host are stripped",
			target:    "/h/7",
			host:      "example.com",
			wantName:  "GET /h/{id}",
			wantRoute: "/h/{id}",
		},
		{name: "unmatched has no route and a low cardinality name", target: "/nothing/1234", wantName: "GET"},
		{
			name:      "a panic keeps the route",
			target:    "/boom/7",
			wantName:  "GET /boom/{id}",
			wantRoute: "/boom/{id}",
			panics:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			mux := http.NewServeMux()
			mux.HandleFunc("GET /raw/{id}", func(http.ResponseWriter, *http.Request) {})
			mux.HandleFunc("GET example.com/h/{id}", func(http.ResponseWriter, *http.Request) {})
			mux.HandleFunc("GET /boom/{id}", func(http.ResponseWriter, *http.Request) { panic("boom") })
			h := tracing.Handler(mux, tracing.WithTracerProvider(tp))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.target, http.NoBody)
			if tt.host != "" {
				req.Host = tt.host
			}
			run := func() { h.ServeHTTP(httptest.NewRecorder(), req) }
			if tt.panics {
				assert.Panics(t, run)
			} else {
				run()
			}
			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, tt.wantName, spans[0].Name)
			route, ok := attr(spans[0].Attributes, "http.route")
			assert.Equal(t, tt.wantRoute != "", ok)
			assert.Equal(t, tt.wantRoute, route.AsString())
		})
	}
}

// runHandlerGivesBackWhatItsCopyLearned checks the multipart form and the routing pattern of otelhttp's copy.
func runHandlerGivesBackWhatItsCopyLearned(t *testing.T) {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	var seen *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen = r
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		assert.NoError(t, r.ParseMultipartForm(1)) //nolint:gosec // G120: the test body is a few bytes
	})
	h := tracing.Handler(mux, tracing.WithTracerProvider(tp))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/upload/7", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(httptest.NewRecorder(), req)
	t.Cleanup(func() { // the handler is called directly, so the server that removes the upload files is not there
		if req.MultipartForm != nil {
			assert.NoError(t, req.MultipartForm.RemoveAll())
		}
	})

	require.NotNil(t, seen)
	require.NotNil(t, req.MultipartForm)
	assert.Same(t, seen.MultipartForm, req.MultipartForm)
	assert.Equal(t, "POST /upload/{id}", req.Pattern)
}

const (
	reasonNoop     = "the tracer provider and the meter provider are no-op"
	reasonDisabled = "WithEnabled(false)"
)

type detectionCase struct {
	build       func(exp *tracetest.InMemoryExporter) []tracing.Option
	name        string
	wantReason  string
	wantSpans   int
	wantDormant bool
}

// providers returns an SDK tracer provider exporting to exp, and a meter provider.
func providers(exp *tracetest.InMemoryExporter) (*sdktrace.TracerProvider, *sdkmetric.MeterProvider) {
	return sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), sdkmetric.NewMeterProvider()
}

func detectionCases() []detectionCase {
	return []detectionCase{
		{
			name: "both no-op values",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return []tracing.Option{
					tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
					tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
				}
			},
			wantReason: reasonNoop, wantDormant: true,
		},
		{
			name: "both no-op pointers",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				tp, mp := tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider()

				return []tracing.Option{tracing.WithTracerProvider(&tp), tracing.WithMeterProvider(&mp)}
			},
			wantReason: reasonNoop, wantDormant: true,
		},
		{
			name: "only the tracer is no-op",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				_, mp := providers(tracetest.NewInMemoryExporter())

				return []tracing.Option{
					tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
					tracing.WithMeterProvider(mp),
				}
			},
		},
		{
			name: "only the meter is no-op",
			build: func(exp *tracetest.InMemoryExporter) []tracing.Option {
				tp, _ := providers(exp)

				return []tracing.Option{
					tracing.WithTracerProvider(tp),
					tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
				}
			},
			wantSpans: 1,
		},
		{
			name: "no-op tracer without an explicit meter provider",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return []tracing.Option{tracing.WithTracerProvider(tracenoop.NewTracerProvider())}
			},
		},
		{
			name: "nothing passed: the global delegate is never taken for disabled",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return nil
			},
		},
		{
			name: "disabled explicitly",
			build: func(exp *tracetest.InMemoryExporter) []tracing.Option {
				tp, mp := providers(exp)

				return []tracing.Option{
					tracing.WithTracerProvider(tp),
					tracing.WithMeterProvider(mp),
					tracing.WithEnabled(false),
				}
			},
			wantReason: reasonDisabled, wantDormant: true,
		},
		{
			name: "enabled explicitly despite no-op providers",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return []tracing.Option{
					tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
					tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
					tracing.WithEnabled(true),
				}
			},
		},
		{
			name: "enabled explicitly with real providers",
			build: func(exp *tracetest.InMemoryExporter) []tracing.Option {
				tp, mp := providers(exp)

				return []tracing.Option{
					tracing.WithTracerProvider(tp),
					tracing.WithMeterProvider(mp),
					tracing.WithEnabled(true),
				}
			},
			wantSpans: 1,
		},
	}
}

const (
	formHuma    = "huma"
	formHandler = "handler"
	panicMsg    = "boom"
)

var errDomain = errors.New("domain failure")

// panicEvents returns the attributes of the exception events recorded by this package.
func panicEvents(s tracetest.SpanStub) []map[string]string {
	var out []map[string]string
	for _, ev := range s.Events {
		attrs := map[string]string{}
		for _, kv := range ev.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		if ev.Name == "exception" && attrs["exception.type"] == "*tracing.PanicError" {
			out = append(out, attrs)
		}
	}

	return out
}
