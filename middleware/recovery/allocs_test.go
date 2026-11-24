package recovery_test

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
	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
)

// nullWriter is a response writer that keeps nothing.
type nullWriter struct{ h http.Header }

func (w *nullWriter) Header() http.Header       { return w.h }
func (*nullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*nullWriter) WriteHeader(int)             {}

// TestAllocs pins what the middleware allocates when nothing panics.
func TestAllocs(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in parallel tests
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "nothing panics", run: runNoPanicAddsNothing},
	}
	for _, tt := range tests { //nolint:paralleltest // AllocsPerRun panics in parallel tests
		t.Run(tt.name, tt.run)
	}
}

// runNoPanicAddsNothing checks that a request without a panic costs no allocation. It is sequential:
// [testing.AllocsPerRun] panics in parallel tests.
func runNoPanicAddsNothing(t *testing.T) {
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
	mw := recovery.New(nil, recovery.WithLogger(slog.New(slog.DiscardHandler)))
	bare := testing.AllocsPerRun(50, func() {
		next(humatest.NewContext(op, req, &nullWriter{h: http.Header{}}))
	})
	wrapped := testing.AllocsPerRun(50, func() {
		mw(humatest.NewContext(op, req, &nullWriter{h: http.Header{}}), next)
	})
	assert.InDelta(t, bare, wrapped, 0)
}
