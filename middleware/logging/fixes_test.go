package logging_test

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

// TestAdapterUnwrapSeesThroughTheWrapper checks that the adapters' Unwrap helpers still reach the request.
func unwrapGo(c huma.Context) *http.Request {
	r, _ := humago.Unwrap(c)

	return r
}

func unwrapChi(c huma.Context) *http.Request {
	r, _ := humachi.Unwrap(c)

	return r
}

func TestAdapterUnwrapSeesThroughTheWrapper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		build  func() (http.Handler, huma.API)
		unwrap func(huma.Context) *http.Request
		name   string
	}{
		{
			name: "humago",
			build: func() (http.Handler, huma.API) {
				mux := http.NewServeMux()

				return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
			},
			unwrap: unwrapGo,
		},
		{
			name: "humachi",
			build: func() (http.Handler, huma.API) {
				r := chi.NewRouter()

				return r, humachi.New(r, huma.DefaultConfig("t", "1"))
			},
			unwrap: unwrapChi,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := tt.build()
			logger, _ := newLogger(slog.LevelDebug)
			var got *http.Request
			api.UseMiddleware(logging.New(logging.WithLogger(logger)), func(ctx huma.Context, next func(huma.Context)) {
				got = tt.unwrap(ctx)
				next(ctx)
			})
			register(api)
			serve(h, "/users/1")
			require.NotNil(t, got)
			assert.Equal(t, "/users/1", got.URL.Path)
		})
	}
}

// hijackWriter is a response writer that can be hijacked.
type hijackWriter struct {
	*httptest.ResponseRecorder

	conn net.Conn
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return h.conn, nil, nil }

func TestHandlerStatusAndHijack(t *testing.T) {
	t.Parallel()

	client, server := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Close())
	})
	tests := []struct {
		handler    http.HandlerFunc
		w          http.ResponseWriter
		name       string
		wantStatus float64
	}{
		{
			name: "103 then a final status", wantStatus: 500,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusEarlyHints)
				w.WriteHeader(http.StatusInternalServerError)
			},
			w: httptest.NewRecorder(),
		},
		{
			name: "101 is final", wantStatus: 101,
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusSwitchingProtocols) },
			w:       httptest.NewRecorder(),
		},
		{
			name: "a flush commits the implicit 200", wantStatus: 200,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, http.NewResponseController(w).Flush())
				w.WriteHeader(http.StatusInternalServerError)
			},
			w: httptest.NewRecorder(),
		},
		{
			name: "a failed flush commits nothing", wantStatus: 500,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				assert.ErrorIs(t, http.NewResponseController(w).Flush(), http.ErrNotSupported)
				w.WriteHeader(http.StatusInternalServerError)
			},
			w: &responseOnly{header: http.Header{}},
		},
		{
			name: "hijack is not supported by the underlying writer", wantStatus: 200,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _, err := http.NewResponseController(w).Hijack()
				assert.ErrorIs(t, err, http.ErrNotSupported)
			},
			w: httptest.NewRecorder(),
		},
		{
			name: "hijack reaches a writer that supports it", wantStatus: 200,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				assert.NoError(t, err)
				assert.Equal(t, server, conn)
			},
			w: &hijackWriter{ResponseRecorder: httptest.NewRecorder(), conn: server},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger(slog.LevelDebug)
			h := logging.Handler(tt.handler, logging.WithLogger(logger))
			h.ServeHTTP(tt.w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))
			rec := find(buf.lines(t), msgRequest)
			require.NotNil(t, rec)
			assert.InDelta(t, tt.wantStatus, rec["status"], 0)
		})
	}
}

func TestEnrichedLoggers(t *testing.T) {
	t.Parallel()

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
			handler := find(lines, "handler")
			require.NotNil(t, handler)
			assert.Equal(t, "GET", handler["method"])
			access := find(lines, msgRequest)
			require.NotNil(t, access)
			assert.Equal(t, "u-5", access["user"], "the late attribute reaches the access log")
			assert.Equal(t, "GET", access["method"])
		})
	}
}

func newAPI(t *testing.T) (http.Handler, huma.API) {
	t.Helper()

	return builders()["humago"](t)
}

func TestGroupedLoggersStayRemovableForCustomRecords(t *testing.T) {
	t.Parallel()

	h, api := newAPI(t)
	logger, buf := newLogger(slog.LevelDebug)
	api.UseMiddleware(logging.New(
		logging.WithLogger(logger),
		logging.WithEnrichLogs(func(_ huma.Context, l *slog.Logger) *slog.Logger { return l.WithGroup("g") }),
		logging.WithAccessLogFunc(func(ctx context.Context, l *slog.Logger, r logging.Record) {
			l.LogAttrs(ctx, slog.LevelInfo, "custom", slog.Int("status", r.Status))
		}),
	))
	register(api)
	serve(h, "/users/1")

	lines := buf.lines(t)
	handler := find(lines, "handler")
	require.NotNil(t, handler)
	assert.Equal(t, "GET", handler["method"], "ordinary records keep the request attributes at the top level")
	custom := find(lines, "custom")
	require.NotNil(t, custom)
	assert.NotContains(t, custom, "method", "a custom record gets none of the built-in fields")
	assert.NotContains(t, custom, "route")
	grp, ok := custom["g"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, 200, grp["status"], 0)
}
