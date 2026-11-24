package tracing_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

// TestWithEnabled covers when the middleware is off.
func TestWithEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "detection", run: runDisabledDetection},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runDisabledDetection(t *testing.T) {
	t.Helper()

	tests := detectionCases()
	for _, form := range []string{formHuma, formHandler} {
		for _, tt := range tests {
			t.Run(form+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				exp := tracetest.NewInMemoryExporter()
				var logs bytes.Buffer
				opts := append(tt.build(exp), tracing.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
				called := false
				if form == formHandler {
					h := tracing.Handler(
						http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
						opts...)
					h.ServeHTTP(httptest.NewRecorder(), req)
				} else {
					op := &huma.Operation{OperationID: "x", Path: "/x"}
					tracing.New(
						opts...)(
						humatest.NewContext(op, req, httptest.NewRecorder()),
						func(huma.Context) { called = true },
					)
				}
				assert.True(t, called)
				assert.Len(t, exp.GetSpans(), tt.wantSpans)
				lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
				if tt.wantReason == "" {
					assert.Empty(t, logs.String())

					return
				}
				require.Len(t, lines, 1, "exactly one line, at construction")
				assert.Contains(t, lines[0], "level=INFO")
				assert.Contains(t, lines[0], "tracing disabled")
				assert.Contains(t, lines[0], tt.wantReason)
			})
		}
	}
}

// TestWithLogger covers the default logger, which says why tracing is off.
func TestWithLogger(t *testing.T) { //nolint:paralleltest // it sets the process wide default logger
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "default logger", run: runDisabledLogsToTheDefaultLogger},
	}
	for _, tt := range tests { //nolint:paralleltest // it sets the process wide default logger
		t.Run(tt.name, tt.run)
	}
}

// runDisabledLogsToTheDefaultLogger sets the global slog default, so it is not parallel.
func runDisabledLogsToTheDefaultLogger(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	tests := []struct {
		name string
		nilL bool
	}{
		{name: "no logger option"},
		{name: "nil logger", nilL: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			opts := []tracing.Option{tracing.WithEnabled(false)}
			if tt.nilL {
				opts = append(opts, tracing.WithLogger(nil))
			}
			tracing.New(opts...)
			assert.Contains(t, logs.String(), "tracing disabled")
			assert.Equal(t, 1, strings.Count(logs.String(), "\n"))
		})
	}
}

// TestWithPropagators covers a global propagator registered after construction.
func TestWithPropagators(t *testing.T) { //nolint:paralleltest // it sets the process wide propagator
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "global propagator", run: runGlobalPropagatorRegisteredAfterConstruction},
	}
	for _, tt := range tests { //nolint:paralleltest // it sets the process wide propagator
		t.Run(tt.name, tt.run)
	}
}

// runGlobalPropagatorRegisteredAfterConstruction sets the global propagator, so it is not parallel.
func runGlobalPropagatorRegisteredAfterConstruction(t *testing.T) {
	t.Helper()
	prev := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	for an, b := range builders() {
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

// eachBuilder runs one row of the tests below for every adapter.
func eachBuilder(t *testing.T, opts []tracing.Option, check func(t *testing.T, f fixture)) {
	t.Helper()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			check(t, newFixture(t, b, opts...))
		})
	}
}

// spanCount is a check: the number of spans a request to the path leaves.
func spanCount(path string, want int) func(t *testing.T, f fixture) {
	return func(t *testing.T, f fixture) {
		t.Helper()
		require.Equal(t, http.StatusOK, f.do(path).Code)
		assert.Len(t, f.spans.GetSpans(), want)
	}
}

// TestWithSkipPaths covers the paths that are not traced.
func TestWithSkipPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check func(t *testing.T, f fixture)
		name  string
		opts  []tracing.Option
	}{
		{name: "skip path", opts: []tracing.Option{tracing.WithSkipPaths(pathHealth)}, check: spanCount(pathHealth, 0)},
		{
			name:  "keeps others",
			opts:  []tracing.Option{tracing.WithSkipPaths(pathHealth)},
			check: spanCount("/users/1", 1),
		},
		{name: "no filter", check: spanCount(pathHealth, 1)},
		{
			name:  "concrete path misses template",
			opts:  []tracing.Option{tracing.WithSkipPaths("/users/42")},
			check: spanCount("/users/42", 1),
		},
		{
			name:  "template matches",
			opts:  []tracing.Option{tracing.WithSkipPaths("/users/{id}")},
			check: spanCount("/users/42", 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, tt.opts, tt.check)
		})
	}
}

// TestWithSkipOperations covers the operations that are not traced.
func TestWithSkipOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check func(t *testing.T, f fixture)
		name  string
		opts  []tracing.Option
	}{
		{
			name:  "skip operation",
			opts:  []tracing.Option{tracing.WithSkipOperations("get-healthz")},
			check: spanCount(pathHealth, 0),
		},
		{
			name:  "keeps others",
			opts:  []tracing.Option{tracing.WithSkipOperations("get-healthz")},
			check: spanCount("/users/1", 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, tt.opts, tt.check)
		})
	}
}

// TestWithFilter covers the filter of the huma form.
func TestWithFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check func(t *testing.T, f fixture)
		name  string
		opts  []tracing.Option
	}{
		{
			name: "filter false",
			opts: []tracing.Option{
				tracing.WithFilter(func(c huma.Context) bool { return c.Operation().Path != pathHealth }),
			},
			check: spanCount(pathHealth, 0),
		},
		{
			name:  "filter true",
			opts:  []tracing.Option{tracing.WithFilter(func(huma.Context) bool { return true })},
			check: spanCount(pathHealth, 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, tt.opts, tt.check)
		})
	}
}

// TestWithRequestFilter covers the filter of the net/http form, which the huma form ignores.
func TestWithRequestFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		check func(t *testing.T, f fixture)
		name  string
		opts  []tracing.Option
	}{
		{
			name:  "ignored by the huma form",
			opts:  []tracing.Option{tracing.WithRequestFilter(func(*http.Request) bool { return false })},
			check: spanCount("/users/42", 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, tt.opts, tt.check)
		})
	}
}

// TestWithSpanNameFormatter covers the span names.
func TestWithSpanNameFormatter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   func(method, route string) string
		want string
	}{
		{name: "method and route", fn: func(m, r string) string { return m + "|" + r }, want: "GET|/users/{id}"},
		{name: "constant", fn: func(string, string) string { return "fixed" }, want: "fixed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, []tracing.Option{tracing.WithSpanNameFormatter(tt.fn)}, func(t *testing.T, f fixture) {
				t.Helper()
				f.do("/users/1")
				spans := f.spans.GetSpans()
				require.Len(t, spans, 1)
				assert.Equal(t, tt.want, spans[0].Name)
			})
		})
	}
}

// TestWithTracerProvider covers where the spans go.
func TestWithTracerProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "one request one span", path: "/users/1", want: 1},
		{name: "health", path: pathHealth, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, nil, spanCount(tt.path, tt.want))
		})
	}
}

// TestWithMeterProvider covers where the metrics go: the duration is recorded with the route.
func TestWithMeterProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "route template", path: "/users/7", want: []string{"/users/{id}"}},
		{name: "other route", path: pathHealth, want: []string{pathHealth}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, nil, func(t *testing.T, f fixture) {
				t.Helper()
				f.do(tt.path)
				assert.Equal(t, tt.want, durationRoutes(t, f.reader))
			})
		})
	}
}

// firstSpan serves a request with a parent and returns the one span it leaves.
func firstSpan(t *testing.T, f fixture) tracetest.SpanStub {
	t.Helper()

	f.do("/users/1", "Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	spans := f.spans.GetSpans()
	require.Len(t, spans, 1)

	return spans[0]
}

// TestWithPublicEndpoint covers the incoming trace that is linked, not continued.
func TestWithPublicEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		opts       []tracing.Option
		wantParent bool
		wantLinks  int
	}{
		{name: "public endpoint", opts: []tracing.Option{tracing.WithPublicEndpoint()}, wantLinks: 1},
		{name: "default continues the trace", wantParent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, tt.opts, func(t *testing.T, f fixture) {
				t.Helper()
				sp := firstSpan(t, f)
				assert.Equal(t, tt.wantParent, sp.Parent.IsValid())
				assert.Len(t, sp.Links, tt.wantLinks)
			})
		})
	}
}

// TestWithServerName covers the server address attribute.
func TestWithServerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
	}{
		{name: "server name", want: "svc.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, []tracing.Option{tracing.WithServerName(tt.want)}, func(t *testing.T, f fixture) {
				t.Helper()
				addr, _ := attr(firstSpan(t, f).Attributes, "server.address")
				assert.Equal(t, tt.want, addr.AsString())
			})
		})
	}
}

// TestWithSpanOptions covers the options every span starts with.
func TestWithSpanOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		val  string
	}{
		{name: "attribute", key: "k", val: "v"},
	}
	for _, tt := range tests {
		opts := []tracing.Option{tracing.WithSpanOptions(trace.WithAttributes(attribute.String(tt.key, tt.val)))}
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			eachBuilder(t, opts, func(t *testing.T, f fixture) {
				t.Helper()
				v, ok := attr(firstSpan(t, f).Attributes, tt.key)
				require.True(t, ok)
				assert.Equal(t, tt.val, v.AsString())
			})
		})
	}
}

// TestPanicError_Error covers the message of a recovered panic.
func TestPanicError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value any
		name  string
		want  string
	}{
		{name: "string", value: "boom", want: "panic: boom"},
		{name: "error", value: assert.AnError, want: "panic: " + assert.AnError.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.EqualError(t, &tracing.PanicError{Value: tt.value}, tt.want)
		})
	}
}
