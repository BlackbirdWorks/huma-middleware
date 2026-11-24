package tracing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

type discardWriter struct{ h http.Header }

func (w discardWriter) Header() http.Header       { return w.h }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}

type benchOut struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func benchHandler(mw ...func(huma.Context, func(huma.Context))) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("b", "1"))
	api.UseMiddleware(mw...)
	huma.Get(api, "/users/{id}", func(context.Context, *idIn) (*benchOut, error) {
		o := &benchOut{}
		o.Body.OK = true

		return o, nil
	})

	return mux
}

func runBench(b *testing.B, h http.Handler) {
	b.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/42", http.NoBody)
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	w := discardWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		h.ServeHTTP(w, req)
	}
}

func BenchmarkBare(b *testing.B) { runBench(b, benchHandler()) }

func BenchmarkTracingNoop(b *testing.B) {
	runBench(b, benchHandler(tracing.New(tracing.WithTracerProvider(noop.NewTracerProvider()))))
}

func BenchmarkTracingRecording(b *testing.B) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	b.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	runBench(b, benchHandler(tracing.New(tracing.WithTracerProvider(tp))))
}

func BenchmarkTracingFiltered(b *testing.B) {
	runBench(b, benchHandler(tracing.New(tracing.WithSkipPaths("/users/{id}"))))
}

func BenchmarkOtelhttpDirectNoop(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(http.ResponseWriter, *http.Request) {})
	runBench(b, otelhttp.NewHandler(mux, "x", otelhttp.WithTracerProvider(noop.NewTracerProvider())))
}
