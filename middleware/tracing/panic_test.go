package tracing_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

const (
	formHuma    = "huma"
	formHandler = "handler"
	panicMsg    = "boom"
)

var errDomain = errors.New("domain failure")

func TestPanicIsRecordedAndRaisedAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value        any
		name         string
		wantMessage  string
		wantRecorded bool
	}{
		{name: "string", value: panicMsg, wantMessage: "panic: boom", wantRecorded: true},
		{name: "error", value: errDomain, wantMessage: "panic: domain failure", wantRecorded: true},
		{name: "abort handler is not an error", value: http.ErrAbortHandler, wantRecorded: false},
	}
	for _, form := range []string{formHuma, formHandler} {
		for _, tt := range tests {
			t.Run(form+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				exp := tracetest.NewInMemoryExporter()
				reader := sdkmetric.NewManualReader()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
				mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
				opts := []tracing.Option{
					tracing.WithTracerProvider(tp), tracing.WithMeterProvider(mp),
					tracing.WithPropagators(propagation.TraceContext{}),
				}
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom", http.NoBody)
				run := func() {
					if form == formHandler {
						h := tracing.Handler(
							http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(tt.value) }),
							opts...)
						h.ServeHTTP(httptest.NewRecorder(), req)

						return
					}
					op := &huma.Operation{OperationID: panicMsg, Path: "/boom"}
					hctx := humatest.NewContext(op, req, httptest.NewRecorder())
					tracing.New(opts...)(hctx, func(huma.Context) { panic(tt.value) })
				}
				assert.PanicsWithValue(t, tt.value, run, "the original value is raised again")

				spans := exp.GetSpans()
				require.Len(t, spans, 1, "the span is ended")
				s := spans[0]
				recorded := panicEvents(s)
				if !tt.wantRecorded {
					assert.Empty(t, recorded)

					return
				}
				assert.Equal(t, codes.Error, s.Status.Code)
				require.Len(t, recorded, 1)
				assert.Equal(t, tt.wantMessage, recorded[0]["exception.message"])
				assert.Contains(t, recorded[0]["exception.stacktrace"], "panic_test.go")
				code, ok := attr(s.Attributes, "http.response.status_code")
				require.True(t, ok)
				assert.EqualValues(t, http.StatusInternalServerError, code.AsInt64())
				if form == formHuma {
					assert.Equal(
						t,
						[]string{"/boom"},
						durationRoutes(t, reader),
						"otelhttp recorded the 500 in its metrics",
					)
				}
			})
		}
	}
}

// panicEvents returns the attributes of the exception events recorded by this package.
func panicEvents(s tracetest.SpanStub) []map[string]string {
	var out []map[string]string
	for _, ev := range s.Events {
		attrs := map[string]string{}
		for _, kv := range ev.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		if ev.Name == "exception" && attrs["exception.type"] == "*tracing.PanicError" {
			out = append(out, attrs)
		}
	}

	return out
}

// TestPanicReturnsTheViewToThePool runs its cases in order: each reuses the pooled view of the previous one.
func TestPanicReturnsTheViewToThePool(t *testing.T) { //nolint:paralleltest // the cases share one middleware in order
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	mw := tracing.New(tracing.WithTracerProvider(tp), tracing.WithPropagators(propagation.TraceContext{}))
	tests := []struct {
		name      string
		panics    bool
		wantCodes codes.Code
	}{
		{name: "panic first", panics: true, wantCodes: codes.Error},
		{name: "then a normal request", wantCodes: codes.Unset},
		{name: "then another panic", panics: true, wantCodes: codes.Error},
		{name: "and a normal one again", wantCodes: codes.Unset},
	}
	for _, tt := range tests { //nolint:paralleltest // see above
		t.Run(tt.name, func(t *testing.T) {
			exp.Reset()
			op := &huma.Operation{OperationID: "x", Path: "/x"}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			hctx := humatest.NewContext(op, req, httptest.NewRecorder())
			run := func() { mw(hctx, func(huma.Context) { panicIf(tt.panics) }) }
			if tt.panics {
				assert.Panics(t, run)
			} else {
				assert.NotPanics(t, run)
			}
			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, tt.wantCodes, spans[0].Status.Code)
		})
	}
}

func panicIf(b bool) {
	if b {
		panic("again")
	}
}
