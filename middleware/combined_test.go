package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
	"github.com/blackbirdworks/huma-middleware/middleware/compression"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

type listOut struct {
	Body struct {
		Items []string `json:"items"`
	}
}

// TestAllFourTogether registers the recommended stack and checks that the layers agree on one request.
func TestAllFourTogether(t *testing.T) {
	t.Parallel()

	builders := map[string]func() (http.Handler, huma.API){
		"humago": func() (http.Handler, huma.API) {
			mux := http.NewServeMux()

			return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
		},
		"humachi": func() (http.Handler, huma.API) {
			r := chi.NewRouter()

			return r, humachi.New(r, huma.DefaultConfig("t", "1"))
		},
	}
	for name, mk := range builders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))

			router, api := mk()
			compress, err := compression.New(compression.WithMinSize(1))
			require.NoError(t, err)
			api.UseMiddleware(
				tracing.New(tracing.WithTracerProvider(tp), tracing.WithPropagators(propagation.TraceContext{})),
				logging.New(logging.WithLogger(logger)),
				compress,
			)
			huma.Get(api, "/items/{id}", func(context.Context, *struct {
				ID string `path:"id"`
			}) (*listOut, error) {
				out := &listOut{}
				for range 100 {
					out.Body.Items = append(out.Body.Items, "item-value")
				}

				return out, nil
			})
			h := cleanpath.Handler()(router)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"//items/../items/7",
				http.NoBody,
			)
			req.Header.Set("Accept-Encoding", "zstd, gzip")
			req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "zstd", rec.Header().Get("Content-Encoding"))
			assert.Equal(t, []string{"Accept-Encoding"}, rec.Header().Values("Vary"))

			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, "GET /items/{id}", spans[0].Name)

			var access map[string]any
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &access))
			assert.Equal(t, "/items/7", access["path"], "the logged path is the cleaned one")
			assert.Equal(t, "/items/{id}", access["route"])
			assert.InDelta(t, http.StatusOK, access["status"], 0)
			assert.Equal(t, spans[0].SpanContext.TraceID().String(), access["trace_id"])
			assert.Less(t, access["response_size"], float64(1000), "the access log counts the compressed bytes")
		})
	}
}
