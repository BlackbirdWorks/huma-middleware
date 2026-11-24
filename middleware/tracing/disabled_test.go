package tracing_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

const (
	reasonNoop     = "the tracer provider and the meter provider are no-op"
	reasonDisabled = "WithEnabled(false)"
)

type detectionCase struct {
	build       func(exp *tracetest.InMemoryExporter) []tracing.Option
	name        string
	wantReason  string
	wantSpans   int
	wantDormant bool
}

// providers returns an SDK tracer provider exporting to exp, and a meter provider.
func providers(exp *tracetest.InMemoryExporter) (*sdktrace.TracerProvider, *sdkmetric.MeterProvider) {
	return sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)), sdkmetric.NewMeterProvider()
}

func detectionCases() []detectionCase {
	return []detectionCase{
		{
			name: "both no-op values",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return []tracing.Option{
					tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
					tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
				}
			},
			wantReason: reasonNoop, wantDormant: true,
		},
		{
			name: "both no-op pointers",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				tp, mp := tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider()

				return []tracing.Option{tracing.WithTracerProvider(&tp), tracing.WithMeterProvider(&mp)}
			},
			wantReason: reasonNoop, wantDormant: true,
		},
		{
			name: "only the tracer is no-op",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				_, mp := providers(tracetest.NewInMemoryExporter())

				return []tracing.Option{
					tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
					tracing.WithMeterProvider(mp),
				}
			},
		},
		{
			name: "only the meter is no-op",
			build: func(exp *tracetest.InMemoryExporter) []tracing.Option {
				tp, _ := providers(exp)

				return []tracing.Option{
					tracing.WithTracerProvider(tp),
					tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
				}
			},
			wantSpans: 1,
		},
		{
			name: "no-op tracer without an explicit meter provider",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return []tracing.Option{tracing.WithTracerProvider(tracenoop.NewTracerProvider())}
			},
		},
		{
			name: "nothing passed: the global delegate is never taken for disabled",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return nil
			},
		},
		{
			name: "disabled explicitly",
			build: func(exp *tracetest.InMemoryExporter) []tracing.Option {
				tp, mp := providers(exp)

				return []tracing.Option{
					tracing.WithTracerProvider(tp),
					tracing.WithMeterProvider(mp),
					tracing.WithEnabled(false),
				}
			},
			wantReason: reasonDisabled, wantDormant: true,
		},
		{
			name: "enabled explicitly despite no-op providers",
			build: func(*tracetest.InMemoryExporter) []tracing.Option {
				return []tracing.Option{
					tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
					tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
					tracing.WithEnabled(true),
				}
			},
		},
		{
			name: "enabled explicitly with real providers",
			build: func(exp *tracetest.InMemoryExporter) []tracing.Option {
				tp, mp := providers(exp)

				return []tracing.Option{
					tracing.WithTracerProvider(tp),
					tracing.WithMeterProvider(mp),
					tracing.WithEnabled(true),
				}
			},
			wantSpans: 1,
		},
	}
}

func TestDisabledDetection(t *testing.T) {
	t.Parallel()

	tests := detectionCases()
	for _, form := range []string{formHuma, formHandler} {
		for _, tt := range tests {
			t.Run(form+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				exp := tracetest.NewInMemoryExporter()
				var logs bytes.Buffer
				opts := append(tt.build(exp), tracing.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
				called := false
				if form == formHandler {
					h := tracing.Handler(
						http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
						opts...)
					h.ServeHTTP(httptest.NewRecorder(), req)
				} else {
					op := &huma.Operation{OperationID: "x", Path: "/x"}
					tracing.New(
						opts...)(
						humatest.NewContext(op, req, httptest.NewRecorder()),
						func(huma.Context) { called = true },
					)
				}
				assert.True(t, called)
				assert.Len(t, exp.GetSpans(), tt.wantSpans)
				lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
				if tt.wantReason == "" {
					assert.Empty(t, logs.String())

					return
				}
				require.Len(t, lines, 1, "exactly one line, at construction")
				assert.Contains(t, lines[0], "level=INFO")
				assert.Contains(t, lines[0], "tracing disabled")
				assert.Contains(t, lines[0], tt.wantReason)
			})
		}
	}
}

// TestDisabledLogsToTheDefaultLogger sets the global slog default, so it is not parallel.
func TestDisabledLogsToTheDefaultLogger(t *testing.T) { //nolint:paralleltest // mutates the global slog default
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	tests := []struct {
		name string
		nilL bool
	}{
		{name: "no logger option"},
		{name: "nil logger", nilL: true},
	}
	for _, tt := range tests { //nolint:paralleltest // see above
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			opts := []tracing.Option{tracing.WithEnabled(false)}
			if tt.nilL {
				opts = append(opts, tracing.WithLogger(nil))
			}
			tracing.New(opts...)
			assert.Contains(t, logs.String(), "tracing disabled")
			assert.Equal(t, 1, strings.Count(logs.String(), "\n"))
		})
	}
}

// TestDisabledAddsNothing is sequential: [testing.AllocsPerRun] panics in parallel tests.
func TestDisabledAddsNothing(t *testing.T) { //nolint:paralleltest // AllocsPerRun needs a serial test
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
	for _, tt := range tests { //nolint:paralleltest // see above
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
