package cleanpath_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
)

type discard struct{ h http.Header }

func (d discard) Header() http.Header       { return d.h }
func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) WriteHeader(int)             {}

func benchPath(b *testing.B, h http.Handler, target string) {
	b.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	w := discard{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(w, req)
	}
}

func BenchmarkBare(b *testing.B) {
	benchPath(b, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "/a/b/c")
}

func BenchmarkCleanPath(b *testing.B) {
	h := cleanpath.Handler()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	benchPath(b, h, "/a/b/c")
}

func BenchmarkUncleanRewrite(b *testing.B) {
	h := cleanpath.Handler()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	benchPath(b, h, "/a//b/../c")
}
