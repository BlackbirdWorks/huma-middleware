package accesslog_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
)

func dropNoise(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey, "duration", "remote_addr", "operation_id", "user_agent", "response_size":
		return slog.Attr{}
	default:
		return a
	}
}

func ExampleNew() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropNoise}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// accesslog.New() alone logs to slog.Default().
	api.UseMiddleware(accesslog.New(accesslog.WithLogger(logger)))
	huma.Get(api, "/users", func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })

	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users?secret=1", http.NoBody))
	// Output: level=INFO msg=request method=GET path=/users status=204 route=/users
}

// ExampleHandler logs the requests of routes that do not go through huma.
func ExampleHandler() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropNoise}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	handler := accesslog.Handler(accesslog.WithLogger(logger))(mux)

	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", http.NoBody))
	// Output: level=INFO msg=request method=GET path=/health status=200 route=/health
}

// ExampleWithRecordFunc keeps an established access log format.
func ExampleWithRecordFunc() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropNoise}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(accesslog.New(
		accesslog.WithLogger(logger),
		accesslog.WithRecordFunc(func(ctx context.Context, l *slog.Logger, r *accesslog.Record) {
			l.LogAttrs(ctx, slog.LevelInfo, "Success",
				slog.String("request", r.Method+" "+r.RequestURI()),
				slog.Int("status", r.Status),
				slog.Int64("size", r.Size),
			)
		}),
	))
	huma.Get(api, "/ping", func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })

	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ping?a=1", http.NoBody))
	// Output: level=INFO msg=Success request="GET /ping?a=1" status=204 size=0
}
