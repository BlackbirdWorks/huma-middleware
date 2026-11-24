package compression_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func ExampleNew() {
	// compression.New() alone compresses text and JSON, zstd for JSON, then br, then gzip.
	mw, err := compression.New(
		compression.WithFallback(compression.Config{MinSize: 1}), // compress even tiny bodies here
		compression.WithContentType("text/html", compression.Config{
			Preference: []compression.Encoding{compression.Gzip, compression.Brotli},
			Levels:     compression.Levels{Gzip: new(9)},
		}),
		compression.WithSkipPathPrefixes("/metrics"),
	)
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*listOut, error) { return &listOut{}, nil })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
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

func ExampleWithContentType() {
	mw, err := compression.New(
		// HTML is mostly served to browsers: prefer gzip, then brotli.
		compression.WithContentType(
			ctHTML,
			compression.Config{Preference: []compression.Encoding{compression.Gzip, compression.Brotli}},
		),
	)
	if err != nil {
		fmt.Println(err)

		return
	}
	rec := httptest.NewRecorder()
	serveRec(mw, http.MethodGet, "/", []string{"Accept-Encoding", "gzip, br"}, rec,
		reply(http.StatusOK, []string{hdrCT, ctHTML}, big()))
	fmt.Println(rec.Header().Get("Content-Encoding"))
	// Output: gzip
}

func ExampleWithDefaults() {
	// compression.New() alone is the recommendation; WithDefaults() says so explicitly.
	mw, err := compression.New(compression.WithDefaults())
	if err != nil {
		panic(err)
	}
	_ = mw

	// The values are readable: Recommended, RecommendedContentTypes and RecommendedAllowList.
	cfg := compression.Recommended()
	fmt.Println(cfg.Preference, *cfg.Levels.Gzip, *cfg.Levels.Brotli, *cfg.Levels.Zstd, cfg.MinSize)
	fmt.Println(compression.RecommendedContentTypes()["+json"].Preference)
	// Output:
	// [br gzip zstd] 6 5 3 1024
	// [zstd br gzip]
}

func ExampleRecommended() {
	// Start from the recommendation and change one thing: compress smaller bodies too.
	cfg := compression.Recommended()
	cfg.MinSize = 256
	mw, err := compression.New(compression.WithDefaults(), compression.WithFallback(cfg))
	if err != nil {
		panic(err)
	}

	rec := httptest.NewRecorder()
	serveRec(mw, http.MethodGet, "/", []string{"Accept-Encoding", "gzip, zstd"}, rec,
		reply(http.StatusOK, []string{hdrCT, "application/json"}, strings.Repeat("a", 300)))
	fmt.Println(rec.Header().Get("Content-Encoding")) // JSON keeps preferring zstd
	// Output: zstd
}
