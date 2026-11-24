package logging_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

type userKey struct{}

func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey || a.Key == "duration" || a.Key == "remote_addr" ||
		a.Key == "operation_id" || a.Key == "user_agent" || a.Key == "response_size" {
		return slog.Attr{}
	}

	return a
}

// ExampleWithEnrichLogs adds a request ID taken from a header and a user ID
// taken from the context to every log record of the request, including the
// access log.
func ExampleWithEnrichLogs() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropTime}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// Authentication runs first and stores the user in the context.
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		next(huma.WithValue(ctx, userKey{}, "u-7"))
	})
	api.UseMiddleware(logging.New(
		logging.WithLogger(logger),
		logging.WithEnrichLogs(func(ctx huma.Context, l *slog.Logger) *slog.Logger {
			user, _ := ctx.Context().Value(userKey{}).(string)

			return l.With(slog.String("req", ctx.Header("X-Correlation-ID")), slog.String("user", user))
		}),
	))
	huma.Get(api, "/users/{id}", func(ctx context.Context, _ *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		logging.FromContext(ctx).InfoContext(ctx, "loading user")

		return &struct{}{}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/7", http.NoBody)
	req.Header.Set("X-Correlation-ID", "abc")
	req.Header.Set("User-Agent", "example")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	// Output:
	// level=INFO msg="loading user" req=abc user=u-7 method=GET route=/users/{id}
	// level=INFO msg=request req=abc user=u-7 path=/users/7 status=204 method=GET route=/users/{id}
}

// ExampleWithAccessLogFunc keeps an established access log format.
func ExampleWithAccessLogFunc() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropTime}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(logging.New(
		logging.WithLogger(logger),
		logging.WithAccessLogFunc(func(ctx context.Context, l *slog.Logger, r logging.Record) {
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
