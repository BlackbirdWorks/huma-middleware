package tracing_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/blackbirdworks/huma-middleware/internal/racedetect"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

// TestAllocs pins that a disabled middleware adds nothing.
func TestAllocs(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in parallel tests
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "disabled adds nothing", run: runDisabledAddsNothing},
	}
	for _, tt := range tests { //nolint:paralleltest // AllocsPerRun panics in parallel tests
		t.Run(tt.name, tt.run)
	}
}

// runDisabledAddsNothing is sequential: [testing.AllocsPerRun] panics in parallel tests.
func runDisabledAddsNothing(t *testing.T) {
	t.Helper()
	if racedetect.Enabled {
		t.Skip("allocation counts mean nothing under the race detector; see task allocs")
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	op := &huma.Operation{OperationID: "x", Path: "/x"}
	hctx := humatest.NewContext(op, req, httptest.NewRecorder())
	next := func(huma.Context) {}
	silent := tracing.WithLogger(slog.New(slog.DiscardHandler))

	tests := []struct {
		name    string
		opts    []tracing.Option
		wantMin float64
	}{
		{name: "disabled", opts: []tracing.Option{tracing.WithEnabled(false), silent}},
		{
			name: "filtered by route",
			opts: []tracing.Option{tracing.WithEnabled(true), tracing.WithSkipPaths("/x"), silent},
		},
		{
			name: "filtered by operation",
			opts: []tracing.Option{tracing.WithEnabled(true), tracing.WithSkipOperations("x"), silent},
		},
		{
			name: "filtered by function",
			opts: []tracing.Option{
				tracing.WithEnabled(true),
				tracing.WithFilter(func(huma.Context) bool { return false }),
				silent,
			},
		},
		{
			name: "no-op providers",
			opts: []tracing.Option{
				tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
				tracing.WithMeterProvider(metricnoop.NewMeterProvider()), silent,
			},
		},
		{
			name:    "enabled for contrast",
			opts:    []tracing.Option{tracing.WithTracerProvider(tracenoop.NewTracerProvider()), silent},
			wantMin: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := tracing.New(tt.opts...)
			allocs := testing.AllocsPerRun(100, func() { mw(hctx, next) })
			if tt.wantMin == 0 {
				assert.InDelta(t, 0, allocs, 0)
			} else {
				assert.GreaterOrEqual(t, allocs, tt.wantMin)
			}
		})
	}
}
