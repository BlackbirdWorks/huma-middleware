package tracing_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

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

func TestSpans(t *testing.T) {
	t.Parallel()

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

func TestParentAndHandlerContext(t *testing.T) {
	t.Parallel()

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

func TestFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		opts      []tracing.Option
		wantSpans int
	}{
		{name: "no filter", path: pathHealth, wantSpans: 1},
		{name: "skip path", opts: []tracing.Option{tracing.WithSkipPaths(pathHealth)}, path: pathHealth},
		{
			name: "skip path keeps others", opts: []tracing.Option{tracing.WithSkipPaths(pathHealth)},
			path: "/users/1", wantSpans: 1,
		},
		{
			name: "skip operation", opts: []tracing.Option{tracing.WithSkipOperations("get-healthz")},
			path: pathHealth,
		},
		{
			name: "filter false",
			path: pathHealth,
			opts: []tracing.Option{
				tracing.WithFilter(func(c huma.Context) bool { return c.Operation().Path != pathHealth }),
			},
		},
		{
			name: "filter true", path: pathHealth, wantSpans: 1,
			opts: []tracing.Option{tracing.WithFilter(func(huma.Context) bool { return true })},
		},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, b, tt.opts...)
				require.Equal(t, http.StatusOK, f.do(tt.path).Code)
				assert.Len(t, f.spans.GetSpans(), tt.wantSpans)
			})
		}
	}
}

func TestSpanNameFormatter(t *testing.T) {
	t.Parallel()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, b, tracing.WithSpanNameFormatter(func(m, r string) string { return m + "|" + r }))
			f.do("/users/1")
			spans := f.spans.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "GET|/users/{id}", spans[0].Name)
		})
	}
}

func TestMetricsHaveRoute(t *testing.T) {
	t.Parallel()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, b)
			f.do("/users/7")

			assert.Equal(t, []string{"/users/{id}"}, durationRoutes(t, f.reader))
		})
	}
}

func TestHandlerWrapper(t *testing.T) {
	t.Parallel()

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

func TestOptionsAccepted(t *testing.T) {
	t.Parallel()

	f := newFixture(t, builders()["humago"],
		tracing.WithPublicEndpoint(),
		tracing.WithServerName("svc.example"),
		tracing.WithSpanOptions(trace.WithAttributes(attribute.String("k", "v"))),
	)
	f.do("/users/1", "Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	spans := f.spans.GetSpans()
	require.Len(t, spans, 1)
	assert.False(t, spans[0].Parent.IsValid(), "public endpoint starts a new root")
	assert.Len(t, spans[0].Links, 1)
	v, ok := attr(spans[0].Attributes, "k")
	require.True(t, ok)
	assert.Equal(t, "v", v.AsString())
	addr, _ := attr(spans[0].Attributes, "server.address")
	assert.Equal(t, "svc.example", addr.AsString())
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

func TestDefaultPropagatorAndNoOperation(t *testing.T) {
	t.Parallel()

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

func TestHandlerNameFormatterWithoutPattern(t *testing.T) {
	t.Parallel()

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

func TestHandlerPutsTheRouteOnTheSpan(t *testing.T) {
	t.Parallel()

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

// TestHumaFormFiltersByTemplate checks that the path and request filters of the huma form see the route
// template only: the concrete URL path "/users/42" must not match a skip for "/users/42".
func TestHumaFormFiltersByTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      []tracing.Option
		wantSpans int
	}{
		{
			name:      "a concrete path does not match a template route",
			opts:      []tracing.Option{tracing.WithSkipPaths("/users/42")},
			wantSpans: 1,
		},
		{name: "the template matches", opts: []tracing.Option{tracing.WithSkipPaths("/users/{id}")}},
		{
			name:      "the request filter belongs to the net/http form",
			opts:      []tracing.Option{tracing.WithRequestFilter(func(*http.Request) bool { return false })},
			wantSpans: 1,
		},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, b, tt.opts...)
				require.Equal(t, http.StatusOK, f.do("/users/42").Code)
				assert.Len(t, f.spans.GetSpans(), tt.wantSpans)
			})
		}
	}
}

// TestGlobalPropagatorRegisteredAfterConstruction sets the global propagator, so it is not parallel.
func TestGlobalPropagatorRegisteredAfterConstruction(t *testing.T) { //nolint:paralleltest // sets the global propagator
	prev := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	for an, b := range builders() { //nolint:paralleltest // see above
		t.Run(an, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			h, api := b(t, nil)
			// No explicit propagator: the middleware is built before the global one is registered.
			api.UseMiddleware(tracing.New(tracing.WithTracerProvider(tp)))
			huma.Get(api, "/users/{id}", func(context.Context, *idIn) (*out, error) { return &out{}, nil })
			otel.SetTextMapPropagator(propagation.TraceContext{})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/1", http.NoBody)
			req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			req.Header.Add("X-Multi", "a")
			req.Header.Add("X-Multi", "b")
			h.ServeHTTP(httptest.NewRecorder(), req)

			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", spans[0].SpanContext.TraceID().String())
		})
	}
}

// TestHandlerGivesBackWhatItsCopyLearned checks the multipart form and the routing pattern of otelhttp's copy.
func TestHandlerGivesBackWhatItsCopyLearned(t *testing.T) {
	t.Parallel()

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

	require.NotNil(t, seen)
	require.NotNil(t, req.MultipartForm)
	assert.Same(t, seen.MultipartForm, req.MultipartForm)
	assert.Equal(t, "POST /upload/{id}", req.Pattern)
}
