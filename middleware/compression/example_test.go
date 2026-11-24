package compression_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func ExampleNew() {
	mw, err := compression.New(
		compression.WithLevel(compression.Gzip, 6),
		compression.WithMinSize(512),
		compression.WithSkipPathPrefixes("/metrics"),
	)
	if err != nil {
		fmt.Println(err)

		return
	}

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*listOut, error) {
		out := &listOut{}
		for range 200 {
			out.Body.Items = append(out.Body.Items, "item")
		}

		return out, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", allAE)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	fmt.Println(rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	// Output: zstd Accept-Encoding
}

func ExampleNegotiate() {
	enc, ok := compression.Negotiate("gzip;q=0.5, br;q=0.8, zstd;q=0",
		[]compression.Encoding{compression.Zstd, compression.Brotli, compression.Gzip})
	fmt.Println(enc, ok)
	// Output: br true
}

func ExampleWithContentTypePreference() {
	mw, err := compression.New(
		// HTML is mostly served to browsers: prefer gzip, then brotli.
		compression.WithContentTypePreference(ctHTML, compression.Gzip, compression.Brotli),
	)
	if err != nil {
		fmt.Println(err)

		return
	}
	rec := httptest.NewRecorder()
	serveRec(mw, http.MethodGet, "/", []string{"Accept-Encoding", "gzip, br"}, rec,
		reply(http.StatusOK, []string{"Content-Type", ctHTML}, big()))
	fmt.Println(rec.Header().Get("Content-Encoding"))
	// Output: gzip
}
