package middleware_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

// TestStack stacks the net/http forms in both orders: the router sets the pattern
// on the innermost request copy, and every layer must still see the route.
func TestStack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		loggingOutside bool
		cleanFirst     bool
	}{
		{name: "logging outside tracing", loggingOutside: true},
		{name: "tracing outside logging"},
		{name: "cleanpath, logging, tracing", loggingOutside: true, cleanFirst: true},
		{name: "cleanpath, tracing, logging", cleanFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			var rec accesslog.Record
			trace := func(next http.Handler) http.Handler {
				return tracing.Handler(next, tracing.WithTracerProvider(tp), tracing.WithMeterProvider(mp))
			}
			logs := func(next http.Handler) http.Handler {
				access := accesslog.Handler(
					accesslog.WithLogger(slog.New(slog.DiscardHandler)),
					accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { rec = *r }))

				return logging.Handler(access(next), logging.WithLogger(slog.New(slog.DiscardHandler)))
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /raw/{id}", func(http.ResponseWriter, *http.Request) {})
			var h http.Handler = mux
			if tt.loggingOutside {
				h = logs(trace(h))
			} else {
				h = trace(logs(h))
			}
			target := "/raw/7"
			if tt.cleanFirst {
				h = cleanpath.Handler()(h)
				target = "//raw/./7"
			}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
			h.ServeHTTP(httptest.NewRecorder(), req)

			assert.Equal(t, "/raw/{id}", rec.Route, "the access log sees the route")
			assert.Equal(t, "/raw/7", rec.Path)
			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "GET /raw/{id}", spans[0].Name)
			route := routeAttr(spans[0])
			assert.Equal(t, "/raw/{id}", route, "the span has http.route")
			assert.Equal(t, "GET /raw/{id}", req.Pattern, "and so does the request that came in")

			routes := durationRoutes(t, reader)
			assert.Equal(t, []string{"/raw/{id}"}, routes, "and the metrics")
		})
	}
}

// routeAttr returns the http.route attribute of a span.
func routeAttr(s tracetest.SpanStub) string {
	for _, kv := range s.Attributes {
		if kv.Key == "http.route" {
			return kv.Value.AsString()
		}
	}

	return ""
}

// durationRoutes returns the http.route label of every request duration data point.
func durationRoutes(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var routes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || m.Name != "http.server.request.duration" {
				continue
			}
			for _, dp := range hist.DataPoints {
				if v, found := dp.Attributes.Value("http.route"); found {
					routes = append(routes, v.AsString())
				}
			}
		}
	}

	return routes
}
