package accesslog_test

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
	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
)

// nullWriter is a response writer that keeps nothing.
type nullWriter struct{ h http.Header }

func (w *nullWriter) Header() http.Header       { return w.h }
func (*nullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*nullWriter) WriteHeader(int)             {}

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

// runAddedAllocations pins what the middleware allocates on top of the handler: nothing, in the huma and in the
// net/http form, whether the record is written, its level is off or the request is skipped. It is sequential:
// [testing.AllocsPerRun] panics in parallel tests.
func runAddedAllocations(t *testing.T) {
	t.Helper()
	if racedetect.Enabled {
		t.Skip("allocation counts mean nothing under the race detector; see task allocs")
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set("User-Agent", "test")
	req.Header.Set("X-Request-ID", "r-1")
	op := &huma.Operation{OperationID: "x", Path: "/x"}
	payload := []byte("ok")
	next := func(ctx huma.Context) {
		ctx.SetStatus(http.StatusOK)
		_, _ = ctx.BodyWriter().Write(payload)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})
	info := slog.New(&levelOnly{nopHandler{}, slog.LevelInfo})
	errorsOnly := slog.New(&levelOnly{nopHandler{}, slog.LevelError})

	tests := []struct {
		name string
		opts []accesslog.Option
	}{
		{name: "record written", opts: []accesslog.Option{accesslog.WithLogger(info)}},
		{name: "level disabled", opts: []accesslog.Option{accesslog.WithLogger(errorsOnly)}},
		{
			name: nameSkipped,
			opts: []accesslog.Option{
				accesslog.WithLogger(info),
				accesslog.WithSkip(func(huma.Context) bool { return true }),
				accesslog.WithRequestSkip(func(*http.Request) bool { return true }),
			},
		},
		{
			name: "no request id",
			opts: []accesslog.Option{accesslog.WithLogger(info), accesslog.WithRequestIDHeader("")},
		},
	}
	bareHuma := testing.AllocsPerRun(50, func() {
		next(humatest.NewContext(op, req, &nullWriter{h: http.Header{}}))
	})
	w := &nullWriter{h: http.Header{}}
	barePlain := testing.AllocsPerRun(50, func() { handler.ServeHTTP(w, req) })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := accesslog.New(tt.opts...)
			wrapped := testing.AllocsPerRun(50, func() {
				mw(humatest.NewContext(op, req, &nullWriter{h: http.Header{}}), next)
			})
			assert.InDelta(t, bareHuma, wrapped, 0, "huma form")
			h := accesslog.Handler(tt.opts...)(handler)
			assert.InDelta(t, barePlain, testing.AllocsPerRun(50, func() { h.ServeHTTP(w, req) }), 0, "net/http form")
		})
	}
}

// levelOnly is a handler that drops the records below its level.
type levelOnly struct {
	slog.Handler

	min slog.Level
}

func (l *levelOnly) Enabled(_ context.Context, lv slog.Level) bool { return lv >= l.min }

// nopHandler accepts every record and writes none: unlike [slog.DiscardHandler] it is enabled, so the middleware
// builds the records.
type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (h nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h nopHandler) WithGroup(string) slog.Handler           { return h }
