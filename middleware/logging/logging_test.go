package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

const (
	redactedValue = "[REDACTED]"
	msgRequest    = "request"
	routeUser     = "/users/{id}"
	wantTrace     = "4bf92f3577b34da6a3ce929d0e0e4736"
	wantSpan      = "00f067aa0ba902b7"
	wantAgent     = "test-agent"
	wantReqID     = "req-1"
	remoteAddr    = "192.0.2.7:1234"
)

type out struct {
	Body struct {
		Name string `json:"name"`
	}
}

type idIn struct {
	ID string `path:"id"`
}

// syncBuffer lets the JSON handler write from the request goroutine while the
// test reads afterwards.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var res []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if l == "" {
			continue
		}
		m := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		res = append(res, m)
	}

	return res
}

func newLogger(level slog.Level) (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	})

	return slog.New(h), buf
}

// stepClock returns a clock advancing by step on every call.
func stepClock(step time.Duration) func() time.Time {
	var n atomic.Int64
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	return func() time.Time { return base.Add(time.Duration(n.Add(1)-1) * step) }
}

type builder func(t *testing.T) (http.Handler, huma.API)

func builders() map[string]builder {
	return map[string]builder{
		"humatest": func(t *testing.T) (http.Handler, huma.API) {
			t.Helper()
			h, api := humatest.New(t)

			return h, api
		},
		"humago": func(*testing.T) (http.Handler, huma.API) {
			mux := http.NewServeMux()

			return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
		},
		"humachi": func(*testing.T) (http.Handler, huma.API) {
			r := chi.NewRouter()

			return r, humachi.New(r, huma.DefaultConfig("t", "1"))
		},
	}
}

func register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-user", Method: http.MethodGet, Path: routeUser,
	}, func(ctx context.Context, in *idIn) (*out, error) {
		logging.FromContext(ctx).InfoContext(ctx, "handler", slog.String("id", in.ID))
		switch in.ID {
		case "boom":
			return nil, huma.Error500InternalServerError("boom")
		case "missing":
			return nil, huma.Error404NotFound("missing")
		}
		logging.AddAttrs(ctx, slog.String("user", "u-"+in.ID))
		logging.FromContext(ctx).InfoContext(ctx, "after auth")
		o := &out{}
		o.Body.Name = "n"

		return o, nil
	})
}

func serve(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	req.RemoteAddr = remoteAddr
	req.Header.Set("User-Agent", wantAgent)
	req.Header.Set("X-Request-ID", wantReqID)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func setup(t *testing.T, b builder, opts ...logging.Option) (http.Handler, *syncBuffer) {
	t.Helper()
	l, buf := newLogger(slog.LevelDebug)
	h, api := b(t)
	api.UseMiddleware(logging.New(append([]logging.Option{logging.WithLogger(l)}, opts...)...))
	register(api)

	return h, buf
}

func find(lines []map[string]any, msg string) map[string]any {
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}

	return nil
}

func TestAccessLogDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		wantLevel string
		wantCode  float64
	}{
		{"ok", "/users/1", "INFO", 200},
		{"client error", "/users/missing", "WARN", 404},
		{"server error", "/users/boom", "ERROR", 500},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, buf := setup(t, b, logging.WithClock(stepClock(5*time.Millisecond)))
				serve(h, tt.path+"?secret=1")

				rec := find(buf.lines(t), msgRequest)
				require.NotNil(t, rec)
				assert.Equal(t, tt.wantLevel, rec["level"])
				assert.InDelta(t, tt.wantCode, rec["status"], 0)
				assert.Equal(t, "GET", rec["method"])
				assert.Equal(t, strings.TrimPrefix(tt.path, ""), rec["path"], "no query string in the path")
				assert.Equal(t, routeUser, rec["route"])
				assert.Equal(t, "get-user", rec["operation_id"])
				assert.InDelta(t, float64(5*time.Millisecond), rec["duration"], 0)
				assert.Equal(t, wantAgent, rec["user_agent"])
				assert.Equal(t, wantReqID, rec["request_id"])
				assert.Equal(t, remoteAddr, rec["remote_addr"])
				assert.Positive(t, rec["response_size"])
				assert.NotContains(t, rec, "headers")
			})
		}
	}
}

func TestContextLogger(t *testing.T) {
	t.Parallel()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			h, buf := setup(t, b)
			serve(h, "/users/9")
			lines := buf.lines(t)

			hl := find(lines, "handler")
			require.NotNil(t, hl)
			assert.Equal(t, "9", hl["id"])
			assert.Equal(t, "GET", hl["method"])
			assert.Equal(t, routeUser, hl["route"])
			assert.Equal(t, "get-user", hl["operation_id"])
			assert.Equal(t, wantReqID, hl["request_id"])
			assert.NotContains(t, hl, "user", "late attribute not yet added")

			after := find(lines, "after auth")
			require.NotNil(t, after)
			assert.Equal(t, "u-9", after["user"])

			access := find(lines, msgRequest)
			require.NotNil(t, access)
			assert.Equal(t, "u-9", access["user"], "late attributes reach the access log")
		})
	}
}

func TestTraceIDs(t *testing.T) {
	t.Parallel()

	tid, err := trace.TraceIDFromHex(wantTrace)
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex(wantSpan)
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})

	l, buf := newLogger(slog.LevelDebug)
	h, api := humatest.New(t)
	api.UseMiddleware(
		func(ctx huma.Context, next func(huma.Context)) {
			next(huma.WithContext(ctx, trace.ContextWithSpanContext(ctx.Context(), sc)))
		},
		logging.New(logging.WithLogger(l)),
	)
	register(api)
	serve(h, "/users/1")

	for _, msg := range []string{"handler", msgRequest} {
		line := find(buf.lines(t), msg)
		require.NotNil(t, line, msg)
		assert.Equal(t, wantTrace, line["trace_id"], msg)
		assert.Equal(t, wantSpan, line["span_id"], msg)
	}
}

type optionCase struct {
	check func(t *testing.T, lines []map[string]any)
	name  string
	opts  []logging.Option
}

func optionCases() []optionCase {
	return []optionCase{

		{
			name: "fields renamed",
			opts: []logging.Option{logging.WithFields(logging.Fields{Status: "code", Route: "r", Method: "verb"})},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				rec := find(lines, msgRequest)
				assert.InDelta(t, 200, rec["code"], 0)
				assert.Equal(t, routeUser, rec["r"])
				assert.Equal(t, "GET", rec["verb"])
				assert.Contains(t, rec, "path", "unset names keep their default")
			},
		},
		{
			name: "levels",
			opts: []logging.Option{logging.WithLevels(logging.Levels{Success: slog.LevelDebug})},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Equal(t, "DEBUG", find(lines, msgRequest)["level"])
			},
		},
		{
			name: "message",
			opts: []logging.Option{logging.WithMessage("access")},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotNil(t, find(lines, "access"))
				assert.Nil(t, find(lines, msgRequest))
			},
		},
		{
			name: "skip",
			opts: []logging.Option{logging.WithSkip(func(huma.Context) bool { return true })},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Nil(t, find(lines, msgRequest))
				assert.NotNil(t, find(lines, "handler"), "context logger still installed")
			},
		},
		{
			name: "no access log",
			opts: []logging.Option{logging.WithoutAccessLog()},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Nil(t, find(lines, msgRequest))
			},
		},
		{
			name: "headers redacted",
			opts: []logging.Option{
				logging.WithHeaders("authorization", "X-Tenant", "x-custom-secret", "X-Absent", "Cookie"),
				logging.WithRedactedHeaders("X-Custom-Secret"),
			},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				hs, ok := find(lines, msgRequest)["headers"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, map[string]any{
					"Authorization": redactedValue, "X-Tenant": "acme", "X-Custom-Secret": redactedValue,
					"Cookie": redactedValue,
				}, hs)
			},
		},
		{
			name: "extra access attrs",
			opts: []logging.Option{logging.WithAccessAttrs(func(r logging.Record) []slog.Attr {
				return []slog.Attr{slog.String("tenant", r.Header("X-Tenant")), slog.String("uri", r.RequestURI())}
			})},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				rec := find(lines, msgRequest)
				assert.Equal(t, "acme", rec["tenant"])
				assert.Equal(t, "/users/1?q=1", rec["uri"])
			},
		},
	}
}

// requestIDOptionCases are the options about request IDs and request attributes.
func requestIDOptionCases() []optionCase {
	return []optionCase{
		{
			name: "request id header",
			opts: []logging.Option{logging.WithRequestIDHeader("X-Tenant")},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Equal(t, "acme", find(lines, msgRequest)["request_id"])
			},
		},
		{
			name: "request id disabled",
			opts: []logging.Option{logging.WithRequestIDHeader("")},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotContains(t, find(lines, msgRequest), "request_id")
			},
		},
		{
			name: "no request attrs",
			opts: []logging.Option{logging.WithoutRequestAttrs()},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotContains(t, find(lines, "handler"), "method")
			},
		},
		{
			name: "nil clock ignored",
			opts: []logging.Option{logging.WithClock(nil)},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotNil(t, find(lines, msgRequest))
			},
		},
	}
}

func TestOptions(t *testing.T) {
	t.Parallel()

	tests := append(optionCases(), requestIDOptionCases()...)
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, buf := setup(t, b, tt.opts...)
				serve(h, "/users/1?q=1", "Authorization", "Bearer s3cr3t", "X-Tenant", "acme",
					"X-Custom-Secret", "x", "Cookie", "a=b")
				lines := buf.lines(t)
				raw, err := json.Marshal(lines)
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "s3cr3t")
				tt.check(t, lines)
			})
		}
	}
}

func TestCustomAccessLogFunc(t *testing.T) {
	t.Parallel()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			var got logging.Record
			h, buf := setup(
				t,
				b,
				logging.WithEnrichLogs(
					func(_ huma.Context, l *slog.Logger) *slog.Logger { return l.With(slog.String("svc", "x")) },
				),
				logging.WithAccessLogFunc(func(ctx context.Context, l *slog.Logger, r logging.Record) {
					got = r
					l.LogAttrs(ctx, slog.LevelInfo, "custom", slog.Int("status", r.Status), slog.Int64("size", r.Size))
				}),
			)
			serve(h, "/users/1?q=1")

			line := find(buf.lines(t), "custom")
			require.NotNil(t, line)
			assert.Equal(t, "x", line["svc"], "enrichment kept")
			assert.Equal(t, "u-1", line["user"], "late attribute kept")
			assert.NotContains(t, line, "method", "built-in attributes are not forced on a custom record")
			assert.Nil(t, find(buf.lines(t), msgRequest))
			assert.Equal(t, http.StatusOK, got.Status)
			assert.Equal(t, "/users/1?q=1", got.RequestURI())
			assert.Equal(t, routeUser, got.Route)
			assert.Equal(t, "get-user", got.OperationID)
			assert.Equal(t, wantAgent, got.UserAgent)
			assert.Equal(t, wantAgent, got.Header("User-Agent"))
			assert.Equal(t, remoteAddr, got.RemoteAddr)
			assert.NotEmpty(t, got.Host)
			assert.Len(t, got.Extra, 1)
		})
	}
}

func TestEnrich(t *testing.T) {
	t.Parallel()

	add := func(k, v string) func(huma.Context, *slog.Logger) *slog.Logger {
		return func(_ huma.Context, l *slog.Logger) *slog.Logger { return l.With(slog.String(k, v)) }
	}
	tests := []struct {
		want map[string]any
		name string
		opts []logging.Option
	}{
		{
			name: "adds to both",
			opts: []logging.Option{logging.WithEnrichLogs(add("a", "1"))},
			want: map[string]any{"a": "1"},
		},
		{
			name: "in order",
			opts: []logging.Option{
				logging.WithEnrichLogs(add("a", "1")),
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
				logging.WithEnrichLogs(add("a", "1")),
			},
			want: map[string]any{"a": "1"},
		},
		{
			name: "nil fn ignored",
			opts: []logging.Option{logging.WithEnrichLogs(nil), logging.WithEnrichLogs(add("a", "1"))},
			want: map[string]any{"a": "1"},
		},
		{
			name: "header values",
			opts: []logging.Option{logging.WithEnrichLogs(func(c huma.Context, l *slog.Logger) *slog.Logger {
				return l.With(slog.String("tenant", c.Header("X-Tenant")))
			})},
			want: map[string]any{"tenant": "acme"},
		},
	}
	for an, b := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, buf := setup(t, b, tt.opts...)
				serve(h, "/users/1", "X-Tenant", "acme")
				lines := buf.lines(t)
				for _, msg := range []string{"handler", msgRequest} {
					line := find(lines, msg)
					require.NotNil(t, line)
					for k, v := range tt.want {
						assert.Equal(t, v, line[k], "%s %s", msg, k)
					}
					assert.Equal(t, "GET", line["method"], "request attributes stay at the top level")
				}
			})
		}
	}
}

// TestDefaultLoggerFallback sets the global default logger, so it must not run
// in parallel with other tests that read it.
func TestDefaultLoggerFallback(t *testing.T) { //nolint:paralleltest // mutates the global slog default
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
		assert.NotNil(t, find(firstBuf.lines(t), msgRequest))
		slog.SetDefault(second)
		serve(h, "/users/2")
		assert.NotNil(t, find(secondBuf.lines(t), msgRequest))
		slog.SetDefault(first)
	}
}

func TestFromContext(t *testing.T) {
	t.Parallel()

	l, buf := newLogger(slog.LevelDebug)
	ctx := logging.NewContext(context.Background(), l)
	assert.Same(t, l, logging.FromContext(ctx))
	logging.AddAttrs(ctx, slog.String("k", "v"))
	logging.AddAttrs(context.Background(), slog.String("ignored", "x"))
	assert.Same(t, slog.Default(), logging.FromContext(context.Background()))
	logging.FromContext(ctx).Info("m")
	assert.Equal(t, "m", buf.lines(t)[0]["msg"])
}

func TestLevelsDisabled(t *testing.T) {
	t.Parallel()

	l, buf := newLogger(slog.LevelError)
	h, api := humatest.New(t)
	api.UseMiddleware(logging.New(logging.WithLogger(l)))
	register(api)
	serve(h, "/users/1")
	assert.Empty(t, buf.lines(t))
	serve(h, "/users/boom")
	assert.Len(t, buf.lines(t), 1)
}

func TestStatusZeroIsOK(t *testing.T) {
	t.Parallel()

	l, buf := newLogger(slog.LevelDebug)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	hctx := humatest.NewContext(nil, req, httptest.NewRecorder())
	logging.New(logging.WithLogger(l))(hctx, func(huma.Context) {})
	rec := find(buf.lines(t), msgRequest)
	require.NotNil(t, rec)
	assert.InDelta(t, 200, rec["status"], 0)
	assert.NotContains(t, rec, "route")
}

func TestStreamingFlushAndSize(t *testing.T) {
	t.Parallel()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(slog.LevelDebug)
			h, api := b(t)
			api.UseMiddleware(logging.New(logging.WithLogger(l)))
			huma.Register(api, huma.Operation{OperationID: "stream", Method: http.MethodGet, Path: "/stream"},
				func(context.Context, *struct{}) (*huma.StreamResponse, error) {
					return &huma.StreamResponse{Body: func(ctx huma.Context) {
						w := ctx.BodyWriter()
						_, err := w.Write([]byte("hello"))
						require.NoError(t, err)
						f, ok := w.(http.Flusher)
						assert.True(t, ok, "flusher preserved")
						u, isU := w.(interface{ Unwrap() http.ResponseWriter })
						assert.True(t, isU)
						if isU {
							assert.NotNil(t, u.Unwrap())
						}
						if ok {
							f.Flush()
						}
						_, err = w.Write([]byte("world"))
						assert.NoError(t, err)
					}}, nil
				})
			rec := serve(h, "/stream")
			assert.Equal(t, "helloworld", rec.Body.String())
			assert.True(t, rec.Flushed, "the flush reached the response writer")
			line := find(buf.lines(t), msgRequest)
			require.NotNil(t, line)
			assert.InDelta(t, 10, line["response_size"], 0)
		})
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()

	l, buf := newLogger(slog.LevelDebug)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /raw/{id}", func(w http.ResponseWriter, r *http.Request) {
		logging.FromContext(r.Context()).InfoContext(r.Context(), "inside")
		w.WriteHeader(http.StatusTeapot)
		_, err := w.Write([]byte("tea"))
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		assert.ErrorIs(t, http.NewResponseController(w).SetReadDeadline(time.Time{}), http.ErrNotSupported)
	})
	mux.HandleFunc("GET /plain", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) })
	mux.HandleFunc("GET /noflush", func(http.ResponseWriter, *http.Request) {})
	h := logging.Handler(mux,
		logging.WithLogger(l),
		logging.WithClock(stepClock(time.Second)),
		logging.WithRequestSkip(func(r *http.Request) bool { return r.URL.Path == "/noflush" }),
		logging.WithRequestEnrichLogs(func(r *http.Request, lg *slog.Logger) *slog.Logger {
			return lg.With(slog.String("tenant", r.Header.Get("X-Tenant")))
		}),
		logging.WithRequestEnrichLogs(nil),
		logging.WithRequestEnrichLogs(func(*http.Request, *slog.Logger) *slog.Logger { return nil }),
		logging.WithHeaders("User-Agent"),
	)
	for _, p := range []string{"/raw/7", "/plain", "/noflush"} {
		serve(h, p, "X-Tenant", "acme")
	}
	lines := buf.lines(t)
	inside := find(lines, "inside")
	require.NotNil(t, inside)
	assert.Equal(t, "acme", inside["tenant"])

	var recs []map[string]any
	for _, l := range lines {
		if l["msg"] == msgRequest {
			recs = append(recs, l)
		}
	}
	require.Len(t, recs, 2)
	assert.InDelta(t, http.StatusTeapot, recs[0]["status"], 0)
	assert.Equal(t, "/raw/{id}", recs[0]["route"])
	assert.InDelta(t, 3, recs[0]["response_size"], 0)
	assert.InDelta(t, float64(time.Second), recs[0]["duration"], 0)
	hdrs, ok := recs[0]["headers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, wantAgent, hdrs["User-Agent"])
	assert.InDelta(t, http.StatusOK, recs[1]["status"], 0)
	assert.Equal(t, "/plain", recs[1]["route"])
}

func TestHandlerNoRoute(t *testing.T) {
	t.Parallel()

	l, buf := newLogger(slog.LevelDebug)
	h := logging.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), logging.WithLogger(l),
		logging.WithoutAccessLog())
	serve(h, "/x")
	assert.Empty(t, buf.lines(t))

	h = logging.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), logging.WithLogger(l))
	serve(h, "/x")
	rec := find(buf.lines(t), msgRequest)
	require.NotNil(t, rec)
	assert.NotContains(t, rec, "route")
}

func TestHandlerCustomRecord(t *testing.T) {
	t.Parallel()

	l, _ := newLogger(slog.LevelDebug)
	var got logging.Record
	h := logging.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), logging.WithLogger(l),
		logging.WithAccessLogFunc(func(_ context.Context, _ *slog.Logger, r logging.Record) { got = r }))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x?a=b", http.NoBody)
	req.Header.Set("X-A", "1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, "/x?a=b", got.RequestURI())
	assert.Equal(t, "1", got.Header("X-A"))
}

func TestWithGroupEmptyAndEnabled(t *testing.T) {
	t.Parallel()

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

func TestEnrichGroup(t *testing.T) {
	t.Parallel()

	h, buf := setup(t, builders()["humago"],
		logging.WithEnrichLogs(func(_ huma.Context, l *slog.Logger) *slog.Logger {
			return l.WithGroup("g").WithGroup("").With(slog.String("k", "v"))
		}))
	serve(h, "/users/1")
	line := find(buf.lines(t), "handler")
	require.NotNil(t, line)
	assert.Equal(t, "GET", line["method"], "request attributes stay at the top level")
	grp, ok := line["g"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "v", grp["k"])
	assert.Equal(t, "1", grp["id"])
}

func TestLevelsByClass(t *testing.T) {
	t.Parallel()

	l, buf := newLogger(slog.LevelDebug)
	status := http.StatusFound
	h := logging.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }),
		logging.WithLogger(l),
		logging.WithLevels(logging.Levels{Redirect: slog.LevelDebug, ClientError: slog.LevelError}))
	serve(h, "/x")
	assert.Equal(t, "DEBUG", find(buf.lines(t), msgRequest)["level"])
}

func TestForeignLoggerKeptAsIs(t *testing.T) {
	t.Parallel()

	other, otherBuf := newLogger(slog.LevelDebug)
	h, _ := setup(t, builders()["humago"],
		logging.WithEnrichLogs(func(huma.Context, *slog.Logger) *slog.Logger { return other }),
		logging.WithAccessLogFunc(func(ctx context.Context, l *slog.Logger, _ logging.Record) {
			l.InfoContext(ctx, "custom")
		}))
	serve(h, "/users/1")
	assert.NotNil(t, find(otherBuf.lines(t), "custom"))
}
