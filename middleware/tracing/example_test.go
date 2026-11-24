package tracing_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

func ExampleNew() {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// tracing.New() alone uses the global providers; pass them explicitly when you can.
	api.UseMiddleware(tracing.New(
		tracing.WithTracerProvider(tp),
		tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
		tracing.WithSkipPaths("/healthz"),
	))
	huma.Get(api, "/users/{id}", func(context.Context, *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		return &struct{}{}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/42", http.NoBody)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	fmt.Println(exporter.GetSpans()[0].Name)
	// Output: GET /users/{id}
}

func ExampleWithEnabled() {
	// Both providers are no-op: the middleware is a pass-through, and says so once.
	mw := tracing.New(
		tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
		tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
		tracing.WithLogger(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropTime}))),
	)
	_ = mw
	// Or switch it off explicitly, whatever the providers are:
	_ = tracing.New(tracing.WithEnabled(false), tracing.WithLogger(slog.New(slog.DiscardHandler)))
	// Output: level=INFO msg="tracing disabled" reason="the tracer provider and the meter provider are no-op"
}

func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}

	return a
}

func ExampleHandler() {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /raw/{id}", func(http.ResponseWriter, *http.Request) {})
	h := tracing.Handler(mux, tracing.WithTracerProvider(tp))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/raw/1", http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), req)

	fmt.Println(exporter.GetSpans()[0].Name)
	// Output: GET /raw/{id}
}
