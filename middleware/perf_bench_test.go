package middleware_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
	"github.com/blackbirdworks/huma-middleware/middleware/compression"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

// sink discards what a log handler writes, so formatting is measured but not output.
type sink struct{}

func (sink) Write(p []byte) (int, error) { return len(p), nil }

// nullWriter is a response writer that keeps nothing.
type nullWriter struct{ h http.Header }

func (w nullWriter) Header() http.Header       { return w.h }
func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (nullWriter) WriteHeader(int)             {}

// body returns about n bytes of compressible JSON-like text.
func perfBody(n int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i := 0; sb.Len() < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":`)
		sb.WriteString(strings.Repeat("7", 1+i%5))
		sb.WriteString(`,"name":"User Example","email":"user@example.com","status":"active"}`)
	}
	sb.WriteString(`]}`)

	return []byte(sb.String())
}

// perfAPI builds a huma API on humago with the middleware given (built from the API when needed) and four routes:
// /tiny (a 100 byte JSON body), /small (2 KB), /big (20 KB) and /image (20 KB, not compressible).
const (
	encGzip    = "gzip"
	ctJSON     = "application/json"
	pathTiny   = "/tiny"
	pathBig    = "/big"
	pathStream = "/stream"
	acceptAll  = "gzip, br, zstd"
)

func perfAPI(mws func(api huma.API) []func(huma.Context, func(huma.Context))) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("perf", "1"))
	if mws != nil {
		api.UseMiddleware(mws(api)...)
	}
	routes := []struct {
		path, ct string
		body     []byte
	}{
		{pathTiny, ctJSON, perfBody(100)},
		{"/small", ctJSON, perfBody(2048)},
		{pathBig, ctJSON, perfBody(20000)},
		{"/image", "image/png", perfBody(20000)},
	}
	for _, r := range routes {
		huma.Register(
			api,
			huma.Operation{
				OperationID: "get" + strings.ReplaceAll(r.path, "/", "-"),
				Method:      http.MethodGet,
				Path:        r.path,
			},
			func(context.Context, *struct{}) (*huma.StreamResponse, error) {
				return &huma.StreamResponse{Body: func(ctx huma.Context) {
					ctx.SetHeader("Content-Type", r.ct)
					ctx.SetStatus(http.StatusOK)
					_, _ = ctx.BodyWriter().Write(r.body)
				}}, nil
			},
		)
	}

	chunk := perfBody(1024)
	huma.Register(api, huma.Operation{OperationID: "get-stream", Method: http.MethodGet, Path: pathStream},
		func(context.Context, *struct{}) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(ctx huma.Context) {
				ctx.SetHeader("Content-Type", "text/plain")
				ctx.SetStatus(http.StatusOK)
				w := ctx.BodyWriter()
				for range 8 {
					_, _ = w.Write(chunk)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				}
			}}, nil
		})

	return mux
}

type perfCase struct {
	name   string
	build  func(tb testing.TB) http.Handler
	target string
	ae     string
}

func perfRequest(target, ae string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	req.Header.Set("User-Agent", "bench")
	if ae != "" {
		req.Header.Set("Accept-Encoding", ae)
	}

	return req
}

func runPerf(b *testing.B, h http.Handler, target, ae string) {
	b.Helper()
	req := perfRequest(target, ae)
	w := nullWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		h.ServeHTTP(w, req)
	}
}

func runPerfParallel(b *testing.B, h http.Handler, target, ae string) {
	b.Helper()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		req := perfRequest(target, ae)
		w := nullWriter{h: http.Header{}}
		for pb.Next() {
			clear(w.h)
			h.ServeHTTP(w, req)
		}
	})
}

func newCompression(tb testing.TB, opts ...compression.Option) compression.Middleware {
	tb.Helper()
	m, err := compression.New(opts...)
	require.NoError(tb, err)

	return m
}

func mw(m ...func(huma.Context, func(huma.Context))) func(huma.API) []func(huma.Context, func(huma.Context)) {
	return func(huma.API) []func(huma.Context, func(huma.Context)) { return m }
}

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(sink{}, nil)) }

func perfCases() []perfCase {
	all := make([]perfCase, 0, 40)
	all = append(all, bareCases()...)
	all = append(all, tracingCases()...)
	all = append(all, loggingCases()...)
	all = append(all, recoveryCases()...)
	all = append(all, compressionCases()...)
	all = append(all, streamCases()...)
	all = append(all, chainCases()...)

	return all
}

func bareCases() []perfCase {
	return []perfCase{
		{name: "bare/tiny", build: func(testing.TB) http.Handler { return perfAPI(nil) }, target: pathTiny},
		{name: "bare/big", build: func(testing.TB) http.Handler { return perfAPI(nil) }, target: pathBig},
		{name: "bare/encoded", build: func(testing.TB) http.Handler { return perfAPI(nil) }, target: "/t%69ny"},
	}
}

func tracingCases() []perfCase {
	noopTracer, noopMeter := tracenoop.NewTracerProvider(), metricnoop.NewMeterProvider()
	silent := tracing.WithLogger(slog.New(slog.DiscardHandler))
	recording := sdktrace.NewTracerProvider()

	return []perfCase{
		{name: "tracing/disabled", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(tracing.New(tracing.WithEnabled(false), silent)))
		}},
		{name: "tracing/filtered", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(tracing.New(tracing.WithEnabled(true), tracing.WithSkipPaths(pathTiny))))
		}},
		{name: "tracing/enabled-noop", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(
				mw(
					tracing.New(
						tracing.WithEnabled(true),
						tracing.WithTracerProvider(noopTracer),
						tracing.WithMeterProvider(noopMeter),
					),
				),
			)
		}},
		{name: "tracing/recording", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(tracing.New(tracing.WithTracerProvider(recording), tracing.WithMeterProvider(noopMeter))))
		}},
	}
}

// nopHandler accepts every record and writes none, so that what the benchmarks of the log middleware show is the
// middleware, not a log handler.
type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (h nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h nopHandler) WithGroup(string) slog.Handler           { return h }

func nopLogger() *slog.Logger { return slog.New(nopHandler{}) }

func errorsOnly() *slog.Logger {
	return slog.New(slog.NewJSONHandler(sink{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

func loggingCases() []perfCase {
	return []perfCase{
		{name: "accesslog/info-nop-handler", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(accesslog.New(accesslog.WithLogger(nopLogger()))))
		}},
		{name: "accesslog/info-json", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(accesslog.New(accesslog.WithLogger(quietLogger()))))
		}},
		{name: "accesslog/level-off", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(accesslog.New(accesslog.WithLogger(errorsOnly()))))
		}},
		{name: "accesslog/skipped", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(accesslog.New(
				accesslog.WithLogger(nopLogger()), accesslog.WithSkip(func(huma.Context) bool { return true }))))
		}},
		{name: "accesslog/net-http-info-nop-handler", target: pathTiny, build: func(testing.TB) http.Handler {
			return accesslog.Handler(accesslog.WithLogger(nopLogger()))(perfAPI(nil))
		}},
		{name: "accesslog/net-http-info-json", target: pathTiny, build: func(testing.TB) http.Handler {
			return accesslog.Handler(accesslog.WithLogger(quietLogger()))(perfAPI(nil))
		}},
		{name: "accesslog/net-http-level-off", target: pathTiny, build: func(testing.TB) http.Handler {
			return accesslog.Handler(accesslog.WithLogger(errorsOnly()))(perfAPI(nil))
		}},
		{name: "logging/request-state", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(logging.New(logging.WithLogger(nopLogger()))))
		}},
		{name: "logging/net-http-request-state", target: pathTiny, build: func(testing.TB) http.Handler {
			return logging.Handler(perfAPI(nil), logging.WithLogger(nopLogger()))
		}},
		{name: "logging+accesslog/info-nop-handler", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(
				logging.New(logging.WithLogger(nopLogger())),
				accesslog.New(accesslog.WithLogger(nopLogger())),
			))
		}},
		{name: "logging+accesslog/info-json", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(mw(
				logging.New(logging.WithLogger(quietLogger())),
				accesslog.New(accesslog.WithLogger(quietLogger())),
			))
		}},
	}
}

func recoveryCases() []perfCase {
	return []perfCase{
		{name: "recovery/no-panic", target: pathTiny, build: func(testing.TB) http.Handler {
			return perfAPI(func(api huma.API) []func(huma.Context, func(huma.Context)) {
				return []func(huma.Context, func(huma.Context)){
					recovery.New(api, recovery.WithLogger(slog.New(slog.DiscardHandler))),
				}
			})
		}},
	}
}

func compressionCases() []perfCase {
	return []perfCase{
		{
			name:   "compression/not-accepted",
			target: pathBig,
			build:  compressed(),
		},
		{
			name:   "compression/not-compressible",
			target: "/image",
			ae:     acceptAll,
			build:  compressed(),
		},
		{
			name:   "compression/below-min",
			target: pathTiny,
			ae:     acceptAll,
			build:  compressed(),
		},
		{
			name:   "compression/skipped",
			target: pathBig,
			ae:     encGzip,
			build:  compressed(compression.WithSkipPathPrefixes(pathBig)),
		},
		{
			name:   "compression/gzip-2k",
			target: "/small",
			ae:     encGzip,
			build:  compressed(),
		},
		{
			name:   "compression/gzip-20k",
			target: pathBig,
			ae:     encGzip,
			build:  compressed(),
		},
		{
			name:   "compression/brotli-20k",
			target: pathBig,
			ae:     "br",
			build:  compressed(),
		},
		{
			name:   "compression/zstd-20k",
			target: pathBig,
			ae:     "zstd",
			build:  compressed(),
		},
	}
}

func streamCases() []perfCase {
	return []perfCase{
		{
			name:   "compression/stream-gzip",
			target: pathStream,
			ae:     encGzip,
			build:  compressed(),
		},
		{
			name:   "compression/stream-passthrough",
			target: pathStream,
			build:  compressed(),
		},
		{name: "bare/stream", target: pathStream, build: func(testing.TB) http.Handler { return perfAPI(nil) }},
		{
			name:   "cleanpath/clean",
			target: pathTiny,
			build:  func(testing.TB) http.Handler { return cleanpath.Handler()(perfAPI(nil)) },
		},
		{
			name:   "cleanpath/encoded-clean",
			target: "/t%69ny",
			build:  func(testing.TB) http.Handler { return cleanpath.Handler()(perfAPI(nil)) },
		},
		{
			name:   "cleanpath/rewrite",
			target: "//tiny",
			build:  func(testing.TB) http.Handler { return cleanpath.Handler()(perfAPI(nil)) },
		},
	}
}

func chainCases() []perfCase {
	return []perfCase{
		{name: "chain/tiny", target: pathTiny, ae: acceptAll, build: perfChain(false)},
		{name: "chain/big-gzip", target: pathBig, ae: encGzip, build: perfChain(false)},
		{name: "chain/big-zstd", target: pathBig, ae: acceptAll, build: perfChain(false)},
		{name: "chain-tracing-off/tiny", target: pathTiny, ae: acceptAll, build: perfChain(true)},
	}
}

// compressed builds an API behind the compression middleware with the options given.
func compressed(opts ...compression.Option) func(testing.TB) http.Handler {
	return func(tb testing.TB) http.Handler {
		tb.Helper()

		return perfAPI(mw(newCompression(tb, opts...)))
	}
}

// perfChain is the recommended stack: cleanpath, then tracing, logging, accesslog, recovery and compression.
func perfChain(tracingOff bool) func(testing.TB) http.Handler {
	return func(tb testing.TB) http.Handler {
		tb.Helper()
		topts := []tracing.Option{
			tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
			tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
			tracing.WithEnabled(!tracingOff),
		}
		compress := newCompression(tb)

		return cleanpath.Handler()(perfAPI(func(api huma.API) []func(huma.Context, func(huma.Context)) {
			return []func(huma.Context, func(huma.Context)){
				tracing.New(append(topts, tracing.WithLogger(slog.New(slog.DiscardHandler)))...),
				logging.New(logging.WithLogger(quietLogger())),
				accesslog.New(accesslog.WithLogger(quietLogger())),
				recovery.New(api, recovery.WithLogger(slog.New(slog.DiscardHandler))),
				compress,
			}
		}))
	}
}

func BenchmarkPerf(b *testing.B) {
	for _, c := range perfCases() {
		b.Run(c.name, func(b *testing.B) { runPerf(b, c.build(b), c.target, c.ae) })
	}
}

// BenchmarkPerfParallel runs the same cases from all CPUs to expose pool and lock contention.
func BenchmarkPerfParallel(b *testing.B) {
	for _, c := range perfCases() {
		b.Run(c.name, func(b *testing.B) { runPerfParallel(b, c.build(b), c.target, c.ae) })
	}
}
