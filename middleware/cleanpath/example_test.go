package cleanpath_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
)

func ExampleHandler() {
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin", func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Path })
	// Outermost: every layer behind it sees the cleaned path.
	handler := cleanpath.Handler()(mux)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(
		rec,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/public/../admin", http.NoBody),
	)
	fmt.Println(rec.Code, seen)
	// Output: 200 /admin
}

func ExampleWithMode() {
	handler := cleanpath.Handler(
		cleanpath.WithMode(cleanpath.Redirect),
		cleanpath.WithTrailingSlash(cleanpath.StripSlash),
	)(http.NotFoundHandler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(
		rec,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/a//b/?q=1", http.NoBody),
	)
	fmt.Println(rec.Code, rec.Header().Get("Location"))
	// Output: 301 /a/b?q=1
}
