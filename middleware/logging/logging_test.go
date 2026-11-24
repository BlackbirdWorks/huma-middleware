package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

	"github.com/blackbirdworks/huma-middleware/internal/reqlog"
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
		logging.FromContext(ctx).InfoContext(ctx, msgHandler, slog.String("id", in.ID))
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

func serve(h http.Handler, path string, hdr ...string) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
	req.RemoteAddr = remoteAddr
	req.Header.Set("User-Agent", wantAgent)
	req.Header.Set("X-Request-ID", wantReqID)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
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

// TestNew covers the middleware: the request logger, the access log, streaming and the request state.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "context logger", run: runContextLogger},
		{name: "parent values", run: runParentValues},
		{name: "request source", run: runSource},
		{name: "trace ids", run: runTraceIDs},
		{name: "adapter unwrap", run: runAdapterUnwrapSeesThroughTheWrapper},
		{name: "state race", run: runRequestStateIsSafeForConcurrentUse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

type ctxKey struct{}

// runParentValues checks that the request state still answers for the values of the incoming context.
func runParentValues(t *testing.T) {
	t.Helper()

	l, _ := newLogger(slog.LevelDebug)
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("t", "1"))
	api.UseMiddleware(logging.New(logging.WithLogger(l)))
	var got any
	huma.Get(api, "/p", func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		got = ctx.Value(ctxKey{})

		return &struct{}{}, nil
	})
	req := httptest.NewRequestWithContext(context.WithValue(context.Background(), ctxKey{}, "kept"),
		http.MethodGet, "/p", http.NoBody)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, "kept", got)
}

func runContextLogger(t *testing.T) {
	t.Helper()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			h, buf := setup(t, b)
			serve(h, "/users/9")
			lines := buf.lines(t)

			hl := find(lines, msgHandler)
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
		})
	}
}

func runTraceIDs(t *testing.T) {
	t.Helper()

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

	line := find(buf.lines(t), msgHandler)
	require.NotNil(t, line)
	assert.Equal(t, wantTrace, line["trace_id"])
	assert.Equal(t, wantSpan, line["span_id"])
}

func runAdapterUnwrapSeesThroughTheWrapper(t *testing.T) {
	t.Helper()

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

// runRequestStateIsSafeForConcurrentUse uses the request logger and AddAttrs from many goroutines of one request,
// some of which outlive the handler, while other requests run: the request logger is built lazily, and its state
// is read when the access log is written. Run it with -race.
func runRequestStateIsSafeForConcurrentUse(t *testing.T) {
	t.Helper()

	logger, buf := newLogger(slog.LevelDebug)
	mw := logging.New(logging.WithLogger(logger))
	op := &huma.Operation{OperationID: "x", Path: "/x"}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 20 {
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
				var late sync.WaitGroup
				mw(humatest.NewContext(op, req, httptest.NewRecorder()), func(ctx huma.Context) {
					for j := range 4 {
						late.Go(func() {
							logging.AddAttrs(ctx.Context(), slog.Int("k"+strconv.Itoa(j), g))
							logging.FromContext(ctx.Context()).
								InfoContext(ctx.Context(), "from a goroutine", slog.Int("i", i))
						})
					}
				})
				late.Wait()
			}
		})
	}
	wg.Wait()
	assert.NotEmpty(t, buf.lines(t))
}

// TestHandler covers the net/http form: records, flushing, status, hijacking and what the copy gives back.
func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "records", run: runHandler},
		{name: "multipart and pattern", run: runHandlerGivesBackWhatItsCopyLearned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runHandler(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /raw/{id}", func(_ http.ResponseWriter, r *http.Request) {
		logging.FromContext(r.Context()).InfoContext(r.Context(), "inside")
		logging.AddAttrs(r.Context(), slog.String("user", "u-1"))
		logging.FromContext(r.Context()).InfoContext(r.Context(), "after")
	})
	h := logging.Handler(mux,
		logging.WithLogger(l),
		logging.WithRequestEnrichLogs(func(r *http.Request, lg *slog.Logger) *slog.Logger {
			return lg.With(slog.String("tenant", r.Header.Get("X-Tenant")))
		}),
		logging.WithRequestEnrichLogs(nil),
		logging.WithRequestEnrichLogs(func(*http.Request, *slog.Logger) *slog.Logger { return nil }),
	)
	serve(h, "/raw/7", "X-Tenant", "acme")
	lines := buf.lines(t)
	inside := find(lines, "inside")
	require.NotNil(t, inside)
	assert.Equal(t, "acme", inside["tenant"])
	assert.Equal(t, "GET", inside["method"])
	assert.Equal(t, wantReqID, inside["request_id"])
	assert.NotContains(t, inside, "user")
	after := find(lines, "after")
	require.NotNil(t, after)
	assert.Equal(t, "u-1", after["user"])
}

// runHandlerGivesBackWhatItsCopyLearned checks the multipart form (whose temporary files net/http removes through
// the original request) and the routing pattern (read by the wrappers around).
func runHandlerGivesBackWhatItsCopyLearned(t *testing.T) {
	t.Helper()

	logger, _ := newLogger(slog.LevelDebug)
	var seen *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen = r
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		assert.NoError(t, r.ParseMultipartForm(1)) //nolint:gosec // G120: the test body is a few bytes
	})
	h := logging.Handler(mux, logging.WithLogger(logger))
	req := multipartRequest(t, "/upload/7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NotNil(t, seen)
	require.NotNil(t, req.MultipartForm, "the original request holds the form, so the server removes its files")
	assert.Same(t, seen.MultipartForm, req.MultipartForm)
	assert.Equal(t, "POST /upload/{id}", req.Pattern)
}

// runSource checks the request state as the access log reads it: its handler leaves out the request attributes and
// keeps what the enrichers and AddAttrs added, it knows the trace IDs and takes a request ID the request had none of.
func runSource(t *testing.T) {
	t.Helper()

	tid, err := trace.TraceIDFromHex(wantTrace)
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex(wantSpan)
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})

	tests := []struct {
		name    string
		enrich  bool
		traced  bool
		asked   bool
		foreign bool
	}{
		{name: "plain"},
		{name: "traced", traced: true},
		{name: "logger asked for", asked: true},
		{name: "enriched", enrich: true},
		{name: "foreign logger", enrich: true, foreign: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(slog.LevelDebug)
			other, otherBuf := newLogger(slog.LevelDebug)
			opts := []logging.Option{logging.WithLogger(l), logging.WithRequestIDHeader("X-Missing")}
			if tt.enrich {
				opts = append(opts, logging.WithEnrichLogs(func(_ huma.Context, lg *slog.Logger) *slog.Logger {
					if tt.foreign {
						return other
					}

					return lg.With(slog.String("svc", "x"))
				}))
			}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			ctx := req.Context()
			if tt.traced {
				ctx = trace.ContextWithSpanContext(ctx, sc)
			}
			hctx := humatest.NewContext(
				&huma.Operation{OperationID: "x", Path: "/x"},
				req.WithContext(ctx),
				httptest.NewRecorder(),
			)
			logging.New(opts...)(hctx, func(c huma.Context) {
				src, ok := c.Context().Value(reqlog.Key{}).(reqlog.Source)
				require.True(t, ok)
				if tt.asked {
					_ = logging.FromContext(c.Context())
				}
				logging.AddAttrs(c.Context(), slog.String("user", "u-1"))
				src.SetRequestID("gen")
				src.SetRequestID("ignored")
				h := src.Handler()
				rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "access", 0)
				src.AddIDs(&rec, make([]slog.Attr, 0, 2))
				require.NoError(t, h.Handle(context.Background(), rec))
				logging.FromContext(c.Context()).InfoContext(c.Context(), "after")
			})
			dest := buf
			if tt.foreign {
				dest = otherBuf
			}
			access := find(dest.lines(t), "access")
			require.NotNil(t, access)
			assert.Equal(t, "u-1", access["user"])
			assert.NotContains(t, access, "method", "the access log writes its own request attributes")
			assert.Equal(t, tt.enrich && !tt.foreign, access["svc"] == "x")
			assert.Equal(t, tt.traced, access["trace_id"] == wantTrace)
			after := find(dest.lines(t), "after")
			require.NotNil(t, after)
			assert.Equal(t, "gen", after["request_id"])
		})
	}
}
