package logging_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"

	"github.com/blackbirdworks/huma-middleware/internal/racedetect"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

// nullWriter is a response writer that keeps nothing.
type nullWriter struct{ h http.Header }

func (w *nullWriter) Header() http.Header       { return w.h }
func (*nullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*nullWriter) WriteHeader(int)             {}

// nopHandler accepts every record and writes none.
type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (h nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h nopHandler) WithGroup(string) slog.Handler           { return h }

// TestAllocs pins what the middleware allocates on top of the handler.
func TestAllocs(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in parallel tests
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "added allocations", run: runAddedAllocations},
	}
	for _, tt := range tests { //nolint:paralleltest // AllocsPerRun panics in parallel tests
		t.Run(tt.name, tt.run)
	}
}

// runAddedAllocations pins what the middleware allocates on top of the handler: one object per request, which is
// both the request state and the context handed on, and the logger the first time it is asked for. It is
// sequential: [testing.AllocsPerRun] panics in parallel tests.
func runAddedAllocations(t *testing.T) {
	t.Helper()
	if racedetect.Enabled {
		t.Skip("allocation counts mean nothing under the race detector; see task allocs")
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	op := &huma.Operation{OperationID: "x", Path: "/x"}
	payload := []byte("ok")
	next := func(ctx huma.Context) {
		ctx.SetStatus(http.StatusOK)
		_, _ = ctx.BodyWriter().Write(payload)
	}
	asked := func(ctx huma.Context) {
		next(ctx)
		_ = logging.FromContext(ctx.Context())
	}
	tests := []struct {
		next  func(huma.Context)
		name  string
		added float64
	}{
		{name: "request state", next: next, added: 1},
		{name: "request logger asked for", next: asked, added: 2},
	}
	bare := testing.AllocsPerRun(50, func() {
		next(humatest.NewContext(op, req, &nullWriter{h: http.Header{}}))
	})
	mw := logging.New(logging.WithLogger(slog.New(nopHandler{})))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := testing.AllocsPerRun(50, func() {
				mw(humatest.NewContext(op, req, &nullWriter{h: http.Header{}}), tt.next)
			})
			assert.InDelta(t, tt.added, wrapped-bare, 0)
		})
	}
}
