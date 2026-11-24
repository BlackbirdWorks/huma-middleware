package tracing_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

func ExampleNew() {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background()) //nolint:errcheck // example

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(tracing.New(
		tracing.WithTracerProvider(tp),
		tracing.WithPropagators(propagation.TraceContext{}),
		tracing.WithSkipPaths("/healthz"),
	))
	huma.Get(api, "/users/{id}", func(context.Context, *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		return &struct{}{}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/42", http.NoBody)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	for _, s := range exporter.GetSpans() {
		fmt.Println(s.Name, s.SpanKind)
	}
	// Output: GET /users/{id} server
}

func ExampleHandler() {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background()) //nolint:errcheck // example

	mux := http.NewServeMux()
	mux.HandleFunc("GET /raw/{id}", func(http.ResponseWriter, *http.Request) {})
	h := tracing.Handler(mux, tracing.WithTracerProvider(tp))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/raw/1", http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), req)

	fmt.Println(exporter.GetSpans()[0].Name)
	// Output: GET /raw/{id}
}
