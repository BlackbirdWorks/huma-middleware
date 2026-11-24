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

func ExampleWithSkipPathPrefixes() {
	mw, err := compression.New(
		compression.WithDefault(compression.Config{MinSize: 1}), // compress even tiny bodies here
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
	huma.Get(api, "/items", func(context.Context, *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{Body: []string{"a"}}, nil
	})

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
	serveRec(mw, http.MethodGet, "/", []string{"Accept-Encoding", aeGzipBr}, rec,
		reply(http.StatusOK, []string{hdrCT, ctHTML}, big()))
	fmt.Println(rec.Header().Get("Content-Encoding"))
	// Output: gzip
}

func ExampleNew() {
	// No option is the recommended setup: text and JSON are compressed, zstd first for JSON, then br, then gzip.
	mw, err := compression.New()
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{Body: strings.Split(strings.Repeat("item ", 400), " ")}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	// Output: zstd Accept-Encoding
}

func ExampleDefaultConfig() {
	// Start from the defaults and change one thing: compress smaller bodies too.
	cfg := compression.DefaultConfig()
	cfg.MinSize = 16
	mw, err := compression.New(compression.WithDefault(cfg))
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{Body: strings.Split(strings.Repeat("item ", 10), " ")}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("Content-Encoding")) // JSON keeps preferring zstd
	// Output: zstd
}
