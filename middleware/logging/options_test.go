package logging_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

// TestWithLogger covers the root logger, and the default logger when there is none.
func TestWithLogger(t *testing.T) { //nolint:paralleltest // it sets the process wide default logger
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "default logger", run: runDefaultLoggerFallback},
	}
	for _, tt := range tests { //nolint:paralleltest // it sets the process wide default logger
		t.Run(tt.name, tt.run)
	}
}

// runDefaultLoggerFallback sets the global default logger, so it must not run
// in parallel with other tests that read it.
func runDefaultLoggerFallback(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	first, firstBuf := newLogger(slog.LevelDebug)
	slog.SetDefault(first)

	for _, withNil := range []bool{false, true} {
		h, api := humatest.New(t)
		if withNil {
			api.UseMiddleware(logging.New(logging.WithLogger(nil)))
		} else {
			api.UseMiddleware(logging.New())
		}
		register(api)

		// A default set after construction is honoured.
		second, secondBuf := newLogger(slog.LevelDebug)
		serve(h, "/users/1")
		assert.NotNil(t, find(firstBuf.lines(t), msgHandler))
		slog.SetDefault(second)
		serve(h, "/users/2")
		assert.NotNil(t, find(secondBuf.lines(t), msgHandler))
		slog.SetDefault(first)
	}
}

// TestWithEnrichLogs covers the enrichment of the request logger.
func TestWithEnrichLogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "enrichers", run: runEnrich},
		{name: "group", run: runEnrichGroup},
		{name: "replaced loggers", run: runEnrichedLoggers},
		{name: "groups and levels", run: runWithGroupEmptyAndEnabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runEnrich(t *testing.T) {
	t.Helper()

	addA := func(_ huma.Context, l *slog.Logger) *slog.Logger { return l.With(slog.String("a", "1")) }
	tests := []struct {
		want map[string]any
		name string
		opts []logging.Option
	}{
		{
			name: "adds to both",
			opts: []logging.Option{logging.WithEnrichLogs(addA)},
			want: map[string]any{"a": "1"},
		},
		{
			name: "in order",
			opts: []logging.Option{
				logging.WithEnrichLogs(addA),
				logging.WithEnrichLogs(func(_ huma.Context, l *slog.Logger) *slog.Logger {
					return l.With(slog.String("b", "after"))
				}),
			},
			want: map[string]any{"a": "1", "b": "after"},
		},
		{
			name: "nil result keeps logger",
			opts: []logging.Option{
				logging.WithEnrichLogs(func(huma.Context, *slog.Logger) *slog.Logger { return nil }),
				logging.WithEnrichLogs(addA),
			},
			want: map[string]any{"a": "1"},
		},
		{
			name: "nil fn ignored",
			opts: []logging.Option{logging.WithEnrichLogs(nil), logging.WithEnrichLogs(addA)},
			want: map[string]any{"a": "1"},
		},
		{
			name: "header values",
			opts: []logging.Option{logging.WithEnrichLogs(func(c huma.Context, l *slog.Logger) *slog.Logger {
				return l.With(slog.String("tenant", c.Header(hdrTenant)))
			})},
			want: map[string]any{"tenant": tenantAcme},
		},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, buf := setup(t, b, tt.opts...)
				serve(h, "/users/1", hdrTenant, tenantAcme)
				lines := buf.lines(t)
				line := find(lines, msgHandler)
				require.NotNil(t, line)
				for k, v := range tt.want {
					assert.Equal(t, v, line[k], k)
				}
				assert.Equal(t, "GET", line[attrMethod], "request attributes stay at the top level")
			})
		}
	}
}

func runEnrichGroup(t *testing.T) {
	t.Helper()

	h, buf := setup(t, builders()["humago"],
		logging.WithEnrichLogs(func(_ huma.Context, l *slog.Logger) *slog.Logger {
			return l.WithGroup("g").WithGroup("").With(slog.String("k", "v"))
		}))
	serve(h, "/users/1")
	line := find(buf.lines(t), msgHandler)
	require.NotNil(t, line)
	assert.Equal(t, "GET", line[attrMethod], "request attributes stay at the top level")
	grp, ok := line["g"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "v", grp["k"])
	assert.Equal(t, "1", grp["id"])
}

func runEnrichedLoggers(t *testing.T) {
	t.Helper()

	tests := []struct {
		enrich    func(other *slog.Logger) func(huma.Context, *slog.Logger) *slog.Logger
		name      string
		intoOther bool
	}{
		{
			name: "an independent logger keeps the request layer and the late attributes",
			enrich: func(other *slog.Logger) func(huma.Context, *slog.Logger) *slog.Logger {
				return func(huma.Context, *slog.Logger) *slog.Logger { return other }
			},
			intoOther: true,
		},
		{
			name: "a derived logger is used as it is",
			enrich: func(*slog.Logger) func(huma.Context, *slog.Logger) *slog.Logger {
				return func(_ huma.Context, l *slog.Logger) *slog.Logger { return l.With(slog.String("k", "v")) }
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root, rootBuf := newLogger(slog.LevelDebug)
			other, otherBuf := newLogger(slog.LevelDebug)
			dest := rootBuf
			if tt.intoOther {
				dest = otherBuf
			}
			h, api := newAPI(t)
			api.UseMiddleware(logging.New(logging.WithLogger(root), logging.WithEnrichLogs(tt.enrich(other))))
			register(api)
			serve(h, "/users/5")
			lines := dest.lines(t)
			handler := find(lines, msgHandler)
			require.NotNil(t, handler)
			assert.Equal(t, "GET", handler[attrMethod])
		})
	}
}

func runWithGroupEmptyAndEnabled(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelWarn)
	var inLogger *slog.Logger
	h, api := humatest.New(t)
	api.UseMiddleware(
		logging.New(logging.WithLogger(l)),
		func(ctx huma.Context, next func(huma.Context)) {
			inLogger = logging.FromContext(ctx.Context())
			next(ctx)
		})
	register(api)
	serve(h, "/users/1")
	assert.False(t, inLogger.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, inLogger.Enabled(context.Background(), slog.LevelError))
	require.NoError(t, inLogger.Handler().WithGroup("").
		Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelError, "m", 0)))
	assert.Len(t, buf.lines(t), 1)
}

// optionCase is one row of an option test: the options, and what the lines logged for one request must show.
type optionCase struct {
	check func(t *testing.T, lines []map[string]any)
	name  string
	opts  []logging.Option
}

// runOption serves one request through every adapter with the options and checks the lines logged.
func runOption(t *testing.T, tt optionCase) {
	t.Helper()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			h, buf := setup(t, b, tt.opts...)
			serve(h, "/users/1?q=1", "Authorization", "Bearer s3cr3t", hdrTenant, tenantAcme,
				"X-Custom-Secret", "x", "Cookie", "a=b")
			lines := buf.lines(t)
			raw, err := json.Marshal(lines)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "s3cr3t")
			tt.check(t, lines)
		})
	}
}

// requestLine is the record of the handler of the request.
func requestLine(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	rec := find(lines, msgHandler)
	require.NotNil(t, rec)

	return rec
}

// TestWithFields covers the renamed attributes.
func TestWithFields(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "fields renamed",
			opts: []logging.Option{logging.WithFields(logging.Fields{RequestID: "rid", Route: "r", Method: "verb"})},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				rec := requestLine(t, lines)
				assert.Equal(t, wantReqID, rec["rid"])
				assert.Equal(t, routeUser, rec["r"])
				assert.Equal(t, "GET", rec["verb"])
				assert.Contains(t, rec, "operation_id", "unset names keep their default")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithoutRequestAttrs covers the request logger without the request attributes.
func TestWithoutRequestAttrs(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "no request attrs",
			opts: []logging.Option{logging.WithoutRequestAttrs()},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotContains(t, find(lines, msgHandler), attrMethod)
			},
		},
		{
			name: "default has them",
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Contains(t, find(lines, msgHandler), attrMethod)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithRequestEnrichLogs covers the enrichment of the request logger in the net/http form.
func TestWithRequestEnrichLogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want   any
		enrich func(*http.Request, *slog.Logger) *slog.Logger
		name   string
	}{
		{
			name: "enriched",
			enrich: func(r *http.Request, l *slog.Logger) *slog.Logger {
				return l.With(slog.String("tenant", r.Header.Get(hdrTenant)))
			},
			want: tenantAcme,
		},
		{name: "nil enricher ignored"},
		{name: "nil logger ignored", enrich: func(*http.Request, *slog.Logger) *slog.Logger { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(slog.LevelDebug)
			h := logging.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				logging.FromContext(r.Context()).InfoContext(r.Context(), "inside")
			}), logging.WithLogger(l), logging.WithRequestEnrichLogs(tt.enrich))
			serve(h, "/raw", hdrTenant, tenantAcme)
			inside := find(buf.lines(t), "inside")
			require.NotNil(t, inside)
			assert.Equal(t, tt.want, inside["tenant"])
		})
	}
}

// TestDefaultFields covers the default names of the request attributes.
func TestDefaultFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		got  func(logging.Fields) string
		name string
		want string
	}{
		{name: attrMethod, got: func(f logging.Fields) string { return f.Method }, want: attrMethod},
		{name: "route", got: func(f logging.Fields) string { return f.Route }, want: "route"},
		{name: "operation", got: func(f logging.Fields) string { return f.OperationID }, want: "operation_id"},
		{name: "request id", got: func(f logging.Fields) string { return f.RequestID }, want: "request_id"},
		{name: "trace", got: func(f logging.Fields) string { return f.TraceID }, want: "trace_id"},
		{name: "span", got: func(f logging.Fields) string { return f.SpanID }, want: "span_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.got(logging.DefaultFields()))
		})
	}
}

// TestWithRequestIDHeader covers where the request ID of the request logger comes from.
func TestWithRequestIDHeader(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "default header",
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Equal(t, wantReqID, requestLine(t, lines)["request_id"])
			},
		},
		{
			name: "other header",
			opts: []logging.Option{logging.WithRequestIDHeader(hdrTenant)},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Equal(t, tenantAcme, requestLine(t, lines)["request_id"])
			},
		},
		{
			name: "disabled",
			opts: []logging.Option{logging.WithRequestIDHeader("")},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotContains(t, requestLine(t, lines), "request_id")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}
