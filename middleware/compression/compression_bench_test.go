package compression_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

type discardWriter struct{ h http.Header }

func (w discardWriter) Header() http.Header       { return w.h }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}

func benchHandler(mw ...func(huma.Context, func(huma.Context))) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("b", "1"))
	api.UseMiddleware(mw...)
	huma.Get(api, pathList, func(context.Context, *struct{}) (*listOut, error) {
		o := &listOut{}
		for range 200 {
			o.Body.Items = append(o.Body.Items, "item-value-0123456789")
		}

		return o, nil
	})

	return mux
}

func runBench(b *testing.B, h http.Handler, ae string) {
	b.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, pathList, http.NoBody)
	req.Header.Set("Accept-Encoding", ae)
	w := discardWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		h.ServeHTTP(w, req)
	}
}

func newBenchMW(b *testing.B) compression.Middleware {
	b.Helper()
	mw, err := compression.New()
	require.NoError(b, err)

	return mw
}

func BenchmarkBare(b *testing.B) { runBench(b, benchHandler(), encGzip) }

func BenchmarkNotAccepted(b *testing.B) { runBench(b, benchHandler(newBenchMW(b)), "") }

func BenchmarkGzip(b *testing.B) { runBench(b, benchHandler(newBenchMW(b)), encGzip) }

func BenchmarkBrotli(b *testing.B) { runBench(b, benchHandler(newBenchMW(b)), encBrotli) }

func BenchmarkZstd(b *testing.B) { runBench(b, benchHandler(newBenchMW(b)), encZstd) }

func BenchmarkPerContentTypeConfig(b *testing.B) {
	mw, err := compression.New(
		compression.WithDefault(compression.Config{Levels: compression.Levels{Gzip: new(1)}}),
		compression.WithContentType("application/json", compression.Config{
			Preference: []compression.Encoding{compression.Gzip},
			Levels:     compression.Levels{Gzip: new(9)},
		}),
	)
	require.NoError(b, err)
	runBench(b, benchHandler(mw), "gzip")
}
