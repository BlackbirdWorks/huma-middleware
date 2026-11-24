package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

const (
	nameAccess  = "accesslog"
	nameLogging = "logging"
	traceHex    = "4bf92f3577b34da6a3ce929d0e0e4736"
	spanHex     = "00f067aa0ba902b7"
)

// TestLogs covers the request logger and the access log together: in both orders, and with one of them only.
func TestLogs(t *testing.T) {
	t.Parallel()

	enrich := logging.WithEnrichLogs(func(_ huma.Context, l *slog.Logger) *slog.Logger {
		return l.With(slog.String("svc", "x"))
	})
	tests := []struct {
		name          string
		order         []string
		wantSvc       bool
		wantLate      bool
		wantTrace     bool
		wantHandlerID bool
	}{
		{
			name:          "logging outside",
			order:         []string{nameLogging, nameAccess},
			wantSvc:       true,
			wantLate:      true,
			wantTrace:     true,
			wantHandlerID: true,
		},
		{name: "accesslog outside", order: []string{nameAccess, nameLogging}},
		{name: "accesslog only", order: []string{nameAccess}},
		{name: "logging only", order: []string{nameLogging}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for name, mk := range adapterBuilders() {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					buf := &lockedBuffer{}
					logger := slog.New(slog.NewJSONHandler(buf, nil))
					router, api := mk()
					mws := map[string]func(huma.Context, func(huma.Context)){
						nameLogging: logging.New(logging.WithLogger(logger), enrich),
						nameAccess: accesslog.New(accesslog.WithLogger(logger),
							accesslog.WithRequestIDGenerator(func() string { return "gen-1" })),
					}
					tid, err := trace.TraceIDFromHex(traceHex)
					require.NoError(t, err)
					sid, err := trace.SpanIDFromHex(spanHex)
					require.NoError(t, err)
					sc := trace.NewSpanContext(trace.SpanContextConfig{
						TraceID:    tid,
						SpanID:     sid,
						TraceFlags: trace.FlagsSampled,
					})
					api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
						next(huma.WithContext(ctx, trace.ContextWithSpanContext(ctx.Context(), sc)))
					})
					for _, o := range tt.order {
						api.UseMiddleware(mws[o])
					}
					huma.Get(api, "/u", func(ctx context.Context, _ *struct{}) (*struct{}, error) {
						logging.AddAttrs(ctx, slog.String("user", "u-1"))
						logging.FromContext(ctx).InfoContext(ctx, "handler")

						return &struct{}{}, nil
					})
					req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/u", http.NoBody)
					router.ServeHTTP(httptest.NewRecorder(), req)

					lines := buf.lines(t)
					access := findMsg(lines, "request")
					handler := findMsg(lines, "handler")
					if len(tt.order) == 1 && tt.order[0] == nameLogging {
						require.Nil(t, access)
						require.NotNil(t, handler)
						assert.NotContains(t, handler, "request_id")

						return
					}
					require.NotNil(t, access)
					assert.Equal(t, "gen-1", access["request_id"])
					assert.Equal(t, tt.wantSvc, access["svc"] == "x")
					assert.Equal(t, tt.wantLate, access["user"] == "u-1")
					assert.Equal(t, tt.wantTrace, access["trace_id"] == traceHex)
					assert.Equal(t, "/u", access["path"])
					if handler != nil {
						assert.Equal(t, traceHex, handler["trace_id"])
						assert.Equal(t, tt.wantHandlerID, handler["request_id"] == "gen-1")
					}
				})
			}
		})
	}
}

// lockedBuffer is a buffer the log handler writes to from the request goroutine and the test reads afterwards.
type lockedBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for l := range bytes.SplitSeq(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		m := map[string]any{}
		require.NoError(t, json.Unmarshal(l, &m))
		out = append(out, m)
	}

	return out
}

func findMsg(lines []map[string]any, msg string) map[string]any {
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}

	return nil
}
