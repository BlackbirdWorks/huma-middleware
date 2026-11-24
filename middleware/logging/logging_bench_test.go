package logging_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

type discardWriter struct{ h http.Header }

func (w discardWriter) Header() http.Header       { return w.h }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}

// sink is a writer that discards, so formatting is measured but not output.
type sink struct{}

func (sink) Write(p []byte) (int, error) { return len(p), nil }

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
	req.Header.Set("User-Agent", "bench")
	w := discardWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		h.ServeHTTP(w, req)
	}
}

func BenchmarkBare(b *testing.B) { runBench(b, benchHandler()) }

func BenchmarkLoggingRequestState(b *testing.B) {
	l := slog.New(slog.NewJSONHandler(sink{}, nil))
	runBench(b, benchHandler(logging.New(logging.WithLogger(l))))
}
