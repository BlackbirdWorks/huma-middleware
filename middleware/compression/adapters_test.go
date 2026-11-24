package compression_test

import (
	"context"
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

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

const itemValue = "item-value"

type listOut struct {
	Body struct {
		Items []string `json:"items"`
	}
}

type streamIn struct{}

func adapterAPIs() map[string]func(t *testing.T) (http.Handler, huma.API) {
	return map[string]func(t *testing.T) (http.Handler, huma.API){
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
	huma.Get(api, pathList, func(context.Context, *struct{}) (*listOut, error) {
		o := &listOut{}
		for range 300 {
			o.Body.Items = append(o.Body.Items, itemValue)
		}

		return o, nil
	})
	huma.Get(api, "/stream", func(context.Context, *streamIn) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			ctx.SetHeader(hdrCT, "text/plain")
			w := ctx.BodyWriter()
			_, _ = w.Write([]byte("first;"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write([]byte("second"))
		}}, nil
	})
}

func TestUnderRealAdapters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		ae           string
		wantEncoding string
		wantContains string
	}{
		{name: "json zstd", path: pathList, ae: allAE, wantEncoding: encZstd, wantContains: itemValue},
		{name: "json brotli", path: pathList, ae: aeGzipBr, wantEncoding: encBrotli, wantContains: itemValue},
		{name: "json gzip", path: pathList, ae: encGzip, wantEncoding: encGzip, wantContains: itemValue},
		{name: "json identity", path: pathList, ae: "", wantContains: itemValue},
		{name: "flushed stream", path: "/stream", ae: encGzip, wantEncoding: encGzip, wantContains: "first;second"},
	}
	for an, mk := range adapterAPIs() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, api := mk(t)
				api.UseMiddleware(mustNew(t, compression.WithDefault(compression.Config{MinSize: 1})))
				registerRoutes(api)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.path, http.NoBody)
				req.Header.Set(hdrAE, tt.ae)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				res := resultOf(t, rec)
				require.Equal(t, http.StatusOK, res.StatusCode)
				assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
				assert.Equal(t, []string{varyAE}, res.Header.Values(hdrVary))
				assert.Contains(t, res.decoded(t), tt.wantContains)
			})
		}
	}
}
