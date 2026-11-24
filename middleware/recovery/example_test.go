package recovery_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
)

func ExampleNew() {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// recovery.New(api) alone answers 500 in huma's error model and logs the panic.
	api.UseMiddleware(recovery.New(api,
		recovery.WithLogger(slog.New(slog.DiscardHandler)),
		recovery.WithHandler(func(ctx huma.Context, _ any) {
			ctx.SetStatus(http.StatusServiceUnavailable)
			_, _ = ctx.BodyWriter().Write([]byte("try again later"))
		}),
	))
	huma.Get(api, "/boom", func(context.Context, *struct{}) (*struct{}, error) { panic("something broke") })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom", http.NoBody))
	fmt.Println(rec.Code, rec.Body.String())
	// Output: 503 try again later
}

func ExampleHandler() {
	h := recovery.Handler(recovery.WithLogger(slog.New(slog.DiscardHandler)))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("something broke") }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody))
	fmt.Println(rec.Code)
	// Output: 500
}
