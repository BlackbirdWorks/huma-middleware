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
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
	"github.com/blackbirdworks/huma-middleware/middleware/compression"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

const pathBoom = "/boom"

type listOut struct {
	Body struct {
		Items []string `json:"items"`
	}
}

// adapterBuilders returns a router and a huma API on it, for two adapters.
func adapterBuilders() map[string]func() (http.Handler, huma.API) {
	return map[string]func() (http.Handler, huma.API){
		"humago": func() (http.Handler, huma.API) {
			mux := http.NewServeMux()

			return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
		},
		"humachi": func() (http.Handler, huma.API) {
			r := chi.NewRouter()

			return r, humachi.New(r, huma.DefaultConfig("t", "1"))
		},
	}
}

// TestAllTogether registers the recommended stack and checks that the layers agree on one request.
func TestAllTogether(t *testing.T) {
	t.Parallel()

	builders := adapterBuilders()
	tests := []struct {
		name         string
		target       string
		wantEncoding string
		wantRoute    string
		wantPath     string
		wantLevel    string
		wantStatus   int
		wantSpanCode codes.Code
	}{
		{
			name: "a normal request", target: "//items/../items/7", wantEncoding: "zstd", wantRoute: "/items/{id}",
			wantPath: "/items/7", wantSpanCode: codes.Unset, wantStatus: http.StatusOK, wantLevel: "INFO",
		},
		{
			name: "a panic", target: pathBoom, wantRoute: pathBoom, wantPath: pathBoom,
			wantSpanCode: codes.Error, wantStatus: http.StatusInternalServerError, wantLevel: "ERROR",
		},
	}
	for name, mk := range builders {
		for _, tt := range tests {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				exp := tracetest.NewInMemoryExporter()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
				var logs bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&logs, nil))

				router, api := mk()
				compress, err := compression.New(compression.WithDefault(compression.Config{MinSize: 1}))
				require.NoError(t, err)
				api.UseMiddleware(
					tracing.New(tracing.WithTracerProvider(tp), tracing.WithPropagators(propagation.TraceContext{})),
					logging.New(logging.WithLogger(logger)),
					recovery.New(api),
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
				huma.Get(api, pathBoom, func(context.Context, *struct{}) (*listOut, error) { panic("boom") })
				h := cleanpath.Handler()(router)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.target, http.NoBody)
				req.Header.Set("Accept-Encoding", "zstd, gzip")
				req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				require.Equal(t, tt.wantStatus, rec.Code)
				assert.Equal(t, tt.wantEncoding, rec.Header().Get("Content-Encoding"))

				spans := exp.GetSpans()
				require.Len(t, spans, 1)
				assert.Equal(t, "GET "+tt.wantRoute, spans[0].Name)
				assert.Equal(t, tt.wantSpanCode, spans[0].Status.Code)

				lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
				records := make([]map[string]any, 0, len(lines))
				for _, line := range lines {
					m := map[string]any{}
					require.NoError(t, json.Unmarshal(line, &m))
					records = append(records, m)
				}
				access := records[len(records)-1]
				assert.Equal(t, "request", access["msg"])
				assert.Equal(t, tt.wantPath, access["path"], "the logged path is the cleaned one")
				assert.Equal(t, tt.wantRoute, access["route"])
				assert.InDelta(t, tt.wantStatus, access["status"], 0, "the access log sees the 500 recovery wrote")
				assert.Equal(t, tt.wantLevel, access["level"])
				assert.Equal(t, spans[0].SpanContext.TraceID().String(), access["trace_id"])
				if tt.wantStatus == http.StatusOK {
					assert.Less(t, access["response_size"], float64(1000), "the access log counts the compressed bytes")
				} else {
					require.Len(t, records, 2)
					assert.Equal(t, "panic recovered", records[0]["msg"])
				}
			})
		}
	}
}
