# huma-middleware

Useful, router-agnostic middleware for [huma v2](https://github.com/danielgtaylor/huma). Each middleware lives in its
own package, so an application links only the dependencies of the ones it uses.

| Package | What | Kind |
| --- | --- | --- |
| `middleware/cleanpath` | Cleans the request path (`//a/../b` to `/b`) before routing | `net/http` middleware, in front of the router |
| `middleware/tracing` | OpenTelemetry server spans and HTTP server metrics, through `otelhttp` | huma middleware |
| `middleware/logging` | Request-scoped `log/slog` logger in the context, with enrichment | huma middleware |
| `middleware/accesslog` | One allocation-free access log record per request | huma middleware |
| `middleware/recovery` | Recovers panics: logs them, marks the span, answers 500 | huma middleware |
| `middleware/compression` | zstd, brotli and gzip response compression | huma middleware |

The huma middlewares are written against the `huma.Context` interface only. They import no router adapter and behave
the same under `humago`, `humachi`, `humagin` and the others (the tests run them under `humatest`, `humago` and
`humachi`). `cleanpath` is not a huma middleware on purpose: huma middleware runs after the router matched a route,
which is too late to change which route matches. It is a plain `http.Handler` wrapper that imports only `net/http`.

Every snippet below is a compiled, runnable `Example` function of its package; a test keeps the README and the examples
identical.

## Install

```shell
go get github.com/blackbirdworks/huma-middleware
```

## tracing

OpenTelemetry server spans and the otelhttp HTTP server metrics for a huma API, reusing `otelhttp`. Spans are named
`METHOD /route/{template}`, continue the incoming `traceparent`, and are visible to handlers through the context; 5xx
sets the span status to error. The server span carries `http.route` in both forms (for `Handler` it is taken from the
`ServeMux` pattern after routing, without the method and host); an unmatched request has none and a low-cardinality
name.

<!-- example: tracing ExampleNew -->
```go
func ExampleNew() {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// tracing.New() alone uses the global providers; pass them explicitly when you can.
	api.UseMiddleware(tracing.New(
		tracing.WithTracerProvider(tp),
		tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
		tracing.WithSkipPaths("/healthz"),
	))
	huma.Get(api, "/users/{id}", func(context.Context, *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		return &struct{}{}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users/42", http.NoBody)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	fmt.Println(exporter.GetSpans()[0].Name)
	// Output: GET /users/{id}
}
```

Options: `WithTracerProvider`, `WithMeterProvider`, `WithPropagators`, `WithSpanNameFormatter(func(method, route string) string)`,
`WithFilter(func(huma.Context) bool)`, `WithRequestFilter` (for `Handler`), `WithSkipOperations(ids...)`,
`WithSkipPaths(routes...)`, `WithPublicEndpoint()`, `WithServerName`, `WithSpanOptions`, `WithEnabled`, `WithLogger`.
`tracing.Handler(next, opts...)` is the same for plain `net/http` routes.

**Panics.** When the rest of the chain panics, the panic is recorded on the span (exception event with the stack,
error status, status code 500; otelhttp also records the 500 in its metrics), the span is ended, the pooled view is
returned, and the panic continues with its original value for a recovery middleware to answer. `http.ErrAbortHandler`
is passed through unrecorded. `tracing.Handler` records the panic the same way but cannot report a status to otelhttp
without writing a 500 itself, so that request is missing from the metrics.

**Disabled.** When both providers are no-op the middleware is a pass-through: it adds no allocation and no pooled
view, and logs one line at construction (level Info, through `WithLogger`, `slog.Default()` when nil).

<!-- example: tracing ExampleWithEnabled -->
```go
func ExampleWithEnabled() {
	// Both providers are no-op: the middleware is a pass-through, and says so once.
	mw := tracing.New(
		tracing.WithTracerProvider(tracenoop.NewTracerProvider()),
		tracing.WithMeterProvider(metricnoop.NewMeterProvider()),
		tracing.WithLogger(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropTime}))),
	)
	_ = mw
	// Or switch it off explicitly, whatever the providers are:
	_ = tracing.New(tracing.WithEnabled(false), tracing.WithLogger(slog.New(slog.DiscardHandler)))
	// Output: level=INFO msg="tracing disabled" reason="the tracer provider and the meter provider are no-op"
}
```

Detection, exactly: the middleware is disabled when `WithEnabled(false)` is given, or when **both** the tracer provider
and the meter provider were passed explicitly (`WithTracerProvider`, `WithMeterProvider`) and are the no-op types of
`go.opentelemetry.io/otel/trace/noop` and `go.opentelemetry.io/otel/metric/noop` (values or pointers). `WithEnabled(true)`
forces it on. Not detected: the global providers (the global tracer provider is a delegate that can receive a real
provider after the middleware was built, so it is never treated as disabled), types embedding a no-op provider, and
only one of the two being a no-op (it then works normally). Pass providers explicitly. With `WithPropagators` only the headers the
propagator declares are copied into the view; without it every request header is visible to the global propagator,
so one registered after the middleware was built still works.

How it reuses otelhttp: one `otelhttp` handler is built at construction. Per request the middleware fills a pooled
`http.Request` view of the `huma.Context` (method, URL, host, remote address, protocol, TLS, the propagator headers,
`User-Agent`, `X-Forwarded-For`, and the route as the request `Pattern`, which is how otelhttp learns `http.route`),
the inner handler calls the rest of the chain, and reports `ctx.Status()` back to otelhttp.

Limits compared with otelhttp on a real handler: request and response body sizes are 0 (huma reads and writes the
bodies); `WithMessageEvents` is not available; requests that do not reach huma (404, 405, raw `net/http` routes) are not
seen, wrap those with `tracing.Handler`.

## logging

A request-scoped `log/slog` logger in the context. It writes no access log: that is `accesslog`.

<!-- example: logging ExampleNew -->
```go
func ExampleNew() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropNoise}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// logging.New() alone logs to slog.Default().
	api.UseMiddleware(logging.New(
		logging.WithLogger(logger),
		logging.WithEnrichLogs(func(ctx huma.Context, l *slog.Logger) *slog.Logger {
			return l.With(slog.String("req", ctx.Header("X-Correlation-ID")))
		}),
	))
	huma.Get(api, "/users", func(ctx context.Context, _ *struct{}) (*struct{}, error) {
		logging.FromContext(ctx).InfoContext(ctx, "loading user")

		return &struct{}{}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users", http.NoBody)
	req.Header.Set("X-Correlation-ID", "abc")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	// Output:
	// level=INFO msg="loading user" req=abc method=GET route=/users
}
```

The logger in the context carries `method`, `route`, `operation_id`, `request_id` (header `X-Request-ID`) and, when a
span is in the context, `trace_id` and `span_id`; the attributes are added when a record is handled, so a disabled
level costs nothing. `logging.FromContext(ctx)` returns the request logger (or `slog.Default()`); `logging.NewContext`
puts a logger in a context (tests, background work); `logging.AddAttrs(ctx, attrs...)` adds attributes later in the
request (for example once the user is authenticated), to the logs written afterwards and to the access log record.
`logging.Handler(next, opts...)` is the form for plain routes.

Options: `WithLogger` (nil or absent means `slog.Default()`, resolved on every request), `WithFields` (rename the
request attributes), `WithRequestIDHeader(name)`, `WithoutRequestAttrs`, `WithEnrichLogs(fn)` (called once per request,
before the handlers; the returned logger is the one in the context; several are applied in order; a panic in `fn`
propagates) and `WithRequestEnrichLogs(fn)` for `Handler`. The enrich function sees what is in the context when the
logging middleware runs; put authentication before it, or use `AddAttrs`. The cost is one allocation per request (the
request state, which is also the context handed on) and the logger when it is first asked for.

## accesslog

One access log record per request, after the response, without allocating: no context value, no request copy, no
per-request logger or closure. The state of a request (the response wrapper, the byte counter and the log record) is
pooled, the level is checked first (a disabled level costs nothing more), and the attributes are typed.

<!-- example: accesslog ExampleNew -->
```go
func ExampleNew() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropNoise}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// accesslog.New() alone logs to slog.Default().
	api.UseMiddleware(accesslog.New(accesslog.WithLogger(logger)))
	huma.Get(api, "/users", func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })

	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/users?secret=1", http.NoBody))
	// Output: level=INFO msg=request method=GET path=/users status=204 route=/users
}
```

The record has `method`, `path` (never the query string), `route`, `operation_id`, `status` actually sent, `duration`,
`response_size` (what was handed to the writer, the compressed size when compression runs inside), `remote_addr`,
`user_agent` and `request_id`, at Info, Warn for 4xx, Error for 5xx. `accesslog.Handler(opts...)(next)` is the form for
plain routes (the route is the `ServeMux` pattern).

Options: `WithLogger` (nil or absent means `slog.Default()`, resolved on every request), `WithFields`, `WithLevels`,
`WithClock`, `WithMessage`, `WithSkip(fn)` and `WithRequestSkip(fn)`, `WithAttrs(fn)` (extra attributes),
`WithHeaders(names...)` with `WithRedactedHeaders(names...)` (Authorization, cookies and API key headers are redacted by
default), `WithRequestIDHeader(name)` (the request's ID, else the same header of the response when the record is
written) and `WithRequestIDGenerator(fn)` (creates the ID when the request has none and sets it on the response), and
`WithRecordFunc(fn)`, which replaces the record entirely with your own function, for applications that must keep an
existing log format:

<!-- example: accesslog ExampleWithRecordFunc -->
```go
func ExampleWithRecordFunc() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: dropNoise}))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(accesslog.New(
		accesslog.WithLogger(logger),
		accesslog.WithRecordFunc(func(ctx context.Context, l *slog.Logger, r *accesslog.Record) {
			l.LogAttrs(ctx, slog.LevelInfo, "Success",
				slog.String("request", r.Method+" "+r.RequestURI()),
				slog.Int("status", r.Status),
				slog.Int64("size", r.Size),
			)
		}),
	))
	huma.Get(api, "/ping", func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil })

	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ping?a=1", http.NoBody))
	// Output: level=INFO msg=Success request="GET /ping?a=1" status=204 size=0
}
```

The `*Record` your function receives is pooled state: use it during the call and copy what you keep (`Record.Header` and
`Record.RequestURI` read the request, call them before the function returns). `WithHeaders`, `WithAttrs`,
`WithRecordFunc` and `WithRequestIDGenerator` allocate what their feature needs, only when used. What the log handler
allocates to format the record is the handler's: `slog.JSONHandler` and `slog.TextHandler` allocate, a handler that does
not (as the tests use) shows the middleware adds none.

On adapters that stream through a callback (Fiber) the record is written when the handler returns, before the stream
ends.

### With `logging`

Install `logging` outside `accesslog` (before it in `UseMiddleware`). The record is then written to the root logger of
`logging` with its enrichers' additions, carries the attributes added with `logging.AddAttrs` and the `trace_id` and
`span_id`, and the request ID `accesslog` generated is the one the request logger shows. Installed the other way round,
or alone, each works as if the other were not there (the record has no late attributes and no trace IDs; the handler
logs have no generated request ID). The two packages do not import each other.

## recovery

Turns a panic into a logged error, a marked span and a 500.

<!-- example: recovery ExampleNew -->
```go
func ExampleNew() {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	// recovery.New(api) alone answers 500 in huma's error model and logs the panic.
	api.UseMiddleware(recovery.New(api,
		recovery.WithLogger(slog.New(slog.DiscardHandler)),
		recovery.WithHandler(func(ctx huma.Context, _ any) {
			ctx.SetStatus(http.StatusServiceUnavailable)
			_, _ = ctx.BodyWriter().Write([]byte("try again later"))
		}),
	))
	huma.Get(api, "/boom", func(context.Context, *struct{}) (*struct{}, error) { panic("something broke") })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom", http.NoBody))
	fmt.Println(rec.Code, rec.Body.String())
	// Output: 503 try again later
}
```

It logs at error level through the request logger (`logging.FromContext`, so the record carries the request attributes)
with the panic value and the stack, marks the active span as error if one is still recording (OpenTelemetry API only),
and, if nothing was written yet (a final status, a body write or a flush counts as written; an early hint does not), answers 500 in huma's error model (`huma.WriteErr`), or with your `WithHandler`; with a nil `api` and no handler it answers a plain text 500.
`http.ErrAbortHandler` is raised again, not swallowed. Options: `WithLogger`, `WithStack(bool)`, `WithStackSize(n)`,
`WithHandler`, `WithSkip`; `recovery.Handler(opts...)` is the `net/http` form (`WithRequestHandler`, `WithRequestSkip`).

## compression

zstd, brotli and gzip, negotiated from `Accept-Encoding`. **`compression.New()` with no option is the recommended setup**;
the options are overrides on top of it, one thing at a time.

<!-- example: compression ExampleNew -->
```go
func ExampleNew() {
	// No option is the recommended setup: text and JSON are compressed, zstd first for JSON, then br, then gzip.
	mw, err := compression.New()
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{Body: strings.Split(strings.Repeat("item ", 400), " ")}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	// Output: zstd Accept-Encoding
}
```

### Defaults

| Setting | Default | Why |
| --- | --- | --- |
| Preference | brotli, gzip, zstd | brotli compresses text best (about 20% smaller than gzip on JSON), gzip is understood everywhere, zstd is the last resort for clients that only offer it |
| Preference for JSON (`application/json`, `+json`) | zstd, brotli, gzip | zstd is as small as gzip level 9 at a fraction of the cost, which suits responses built per request; the rule follows a fallback preference that includes zstd |
| gzip level | 6 (the library default) | the same ratio as level 9 at a sixth of the time on JSON; level 5 is 4% bigger |
| brotli level | 5 | the knee of the curve: levels 6 to 8 do not compress better and cost up to twice as much, level 4 is 12% bigger, level 11 is 50 times slower |
| zstd level | 3 (zstd's default) | levels 1 and 3 are within 3% of each other; level 9 is 7% smaller for 35% more time and four times the memory |
| Minimum size | 1024 bytes | about one TCP segment: below it a response fits one packet whether compressed or not |
| Content types | text, JSON, XML, JavaScript, ... (`text/*`, `+json`, `+xml`, ...) | what shrinks well. Never images (but SVG), video, audio, archives, PDF, fonts (compressed already), and never `text/event-stream` (a stream must reach the client as it is written; no wildcard matches it, it is compressed only when named exactly) |

The levels come from `BenchmarkLevels` (`task bench`): a 19 KB JSON list of records and a 15 KB HTML text, median of 5 runs
on a 3-core machine. Ratio is compressed size over original; the times are relative (they vary between machines).

| Level | JSON ratio | JSON time | HTML ratio | HTML time |
| --- | --- | --- | --- | --- |
| gzip 1 | 0.159 | 102 us | 0.176 | 43 us |
| gzip 5 | 0.133 | 235 us | 0.156 | 81 us |
| **gzip 6** | **0.128** | 107 us | **0.157** | 93 us |
| gzip 9 | 0.128 | 760 us | 0.153 | 328 us |
| brotli 4 | 0.117 | 516 us | 0.160 | 226 us |
| **brotli 5** | **0.105** | 605 us | **0.146** | 278 us |
| brotli 8 | 0.105 | 465 us | 0.147 | 362 us |
| brotli 11 | 0.085 | 29.8 ms | 0.133 | 23.2 ms |
| zstd 1 | 0.120 | 49 us | 0.172 | 42 us |
| **zstd 3** | **0.117** | 56 us | **0.160** | 55 us |
| zstd 9 | 0.109 | 76 us | 0.158 | 74 us |

`DefaultConfig()` returns the fallback configuration with every field set (a new value on each call). To start from the
defaults and change one thing, pass the changed copy to `WithDefault`; the built-in rule for JSON stays with it:

<!-- example: compression ExampleDefaultConfig -->
```go
func ExampleDefaultConfig() {
	// Start from the defaults and change one thing: compress smaller bodies too.
	cfg := compression.DefaultConfig()
	cfg.MinSize = 16
	mw, err := compression.New(compression.WithDefault(cfg))
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{Body: strings.Split(strings.Repeat("item ", 10), " ")}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("Content-Encoding")) // JSON keeps preferring zstd
	// Output: zstd
}
```

### Your own configuration

<!-- example: compression ExampleWithSkipPathPrefixes -->
```go
func ExampleWithSkipPathPrefixes() {
	mw, err := compression.New(
		compression.WithDefault(compression.Config{MinSize: 1}), // compress even tiny bodies here
		compression.WithContentType("text/html", compression.Config{
			Preference: []compression.Encoding{compression.Gzip, compression.Brotli},
			Levels:     compression.Levels{Gzip: new(9)},
		}),
		compression.WithSkipPathPrefixes("/metrics"),
	)
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))
	api.UseMiddleware(mw)
	huma.Get(api, "/items", func(context.Context, *struct{}) (*struct{ Body []string }, error) {
		return &struct{ Body []string }{Body: []string{"a"}}, nil
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	// Output: zstd Accept-Encoding
}
```

The client's quality values come first (`q=0` excludes, `*` and `identity` are understood; a header split over several
lines is read as one list); the server preference breaks ties.

Configuration is one fallback `Config` (`WithDefault`) plus optional per-content-type configs (`WithContentType`). A
`Config` has `Preference` (order), `Levels` (`compression.Levels{Gzip, Brotli, Zstd}`, each a pointer written with `new(n)`, so that level 0 is selectable: gzip -2 to 9, brotli 0 to 11, zstd 1 to 22; nil keeps the default), `MinSize`
(1024 by default) and `Disabled`; a content type's config inherits what it leaves at the zero value. The entry for a
response is the most specific match: exact (`application/json`), then suffix (`+json`, the longest), then wildcard
(`text/*`, the longest; `*/*` last), then the default. An entry also makes its type compressible. Encoder pools are keyed
by encoding and level. Invalid configuration is an error from `New`.

Other options: `WithContentTypes(patterns...)` (the allow-list, replacing the default one), `WithSkipPathPrefixes`,
`WithSkipPathPatterns`, `WithOnlyPathPrefixes`, `WithSkipOperations`, `WithSkip(fn)`. Images other than SVG, video, audio,
archives, PDF and fonts are never compressed.

Headers: `Content-Encoding` is set; `Content-Length` is not sent for a compressed body; `Vary: Accept-Encoding` is
appended to the handler's `Vary` (once) on every response that could be compressed, including those that are not
because the client accepts nothing; no compression for HEAD, 204, 304, 206, an existing `Content-Encoding` or
`Cache-Control: no-transform`; `103 Early Hints` are sent at once.

**ETag.** A strong ETag promises identical bytes, and the compressed body has different bytes than the uncompressed one,
so the tag is marked weak (`W/"..."`). Conditional requests (`If-None-Match`) keep working, because they compare weakly.

Streaming: `text/event-stream` is not compressed by default and flushes pass straight through. A compressible response
that flushes before reaching the minimum size is compressed from that point and the encoder is flushed with it.
Adapters that stream through a callback (Fiber) are supported: the decision and the headers come before the stream.

Limits: a `huma.Context` cannot read response headers, so only headers set through the context handed down are seen,
not those set earlier by an outer middleware. HEAD responses are not compressed, so their `Content-Encoding` differs
from the GET.

## cleanpath

`cleanpath.Handler(opts...) func(http.Handler) http.Handler` cleans the path before the router sees it.

<!-- example: cleanpath ExampleHandler -->
```go
func ExampleHandler() {
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin", func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Path })
	// Outermost: every layer behind it sees the cleaned path.
	handler := cleanpath.Handler()(mux)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(
		rec,
		httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/public/../admin", http.NoBody),
	)
	fmt.Println(rec.Code, seen)
	// Output: 200 /admin
}
```

Collapses repeated slashes and resolves `.` and `..` (never above the root) with the `path.Clean` rules that `net/http`
applies. It works on the escaped path: `%2F` is data and stays inside its segment, while `%2e` and `%2E` count as dots,
so `/a/%2e%2e/b` is `/b`; this is the safe behaviour, because a path-based access rule that sees the encoded dots as a
harmless name while the router resolves them is bypassed by exactly that difference. Modes: `Rewrite` (default; served
on a shallow copy whose `URL.Path`, `URL.RawPath` and `RequestURI` are all the cleaned path, query kept), `Redirect` (301
for GET and HEAD, 308 otherwise, query kept) and `Reject` (404, or `WithRejectHandler`). `WithTrailingSlash(KeepSlash |
StripSlash | AddSlash)` must match how the router registers its routes (with `StripSlash` in front of a `ServeMux` that has only
a `/docs/` subtree pattern, `/docs/` becomes `/docs`, which the mux redirects back: a loop). `WithSkip(fn)`; `CONNECT` and
`OPTIONS *` are untouched. An already clean path costs no allocation.

## All together

```go
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
	"github.com/blackbirdworks/huma-middleware/middleware/compression"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
	"github.com/blackbirdworks/huma-middleware/middleware/tracing"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("example", "1.0.0"))

	compress, err := compression.New() // returns an error for an invalid option
	if err != nil {
		panic(err)
	}
	// The first middleware is the outermost one.
	api.UseMiddleware(
		tracing.New(),
		logging.New(logging.WithLogger(logger)),
		accesslog.New(accesslog.WithLogger(logger)),
		recovery.New(api),
		compress,
	)

	// ... register operations ...

	// cleanpath goes in front of the router, outside of everything else.
	srv := &http.Server{Addr: ":8080", Handler: cleanpath.Handler()(mux)}
	_ = srv.ListenAndServe()
}
```

### Order

`cleanpath` (outermost, `net/http`) then `tracing`, `logging`, `accesslog`, `recovery`, `compression` (huma, outermost first).

- **cleanpath first**: every later layer, including path based access rules, sees the same cleaned path, so
  `/public/../admin` cannot slip past a rule that matches `/admin`. Requests that never reach huma (404, 405, raw
  `net/http` routes) are cleaned too.
- **tracing before logging**: the span exists when logging reads the trace and span IDs, and the span covers the whole
  request.
- **logging before accesslog**: the access record carries what the request logger knows (enrichment, `AddAttrs`, trace
  and span IDs). **accesslog before recovery**: the recovered panic is logged with the request attributes, and the
  access log records the 500 that recovery wrote.
- **recovery inside tracing and logging, outside the handlers**: it recovers before the panic crosses them. What each
  combination records:
  - recovery inside tracing (above): recovery marks the span (exception `*recovery.PanicError`), writes the 500, and
    tracing and its metrics see the 500 as a normal response;
  - recovery outside tracing: tracing records the panic on the span first (exception `*tracing.PanicError`, status 500,
    the 500 also in the metrics) and raises it again; recovery logs it and answers; the span is not marked twice;
  - recovery outside accesslog: the panic crosses it, and no access log record is written for that request.
- **compression innermost**: it wraps the handler's response, and everything outside it sees the final status and the
  bytes actually sent (`accesslog` counts the compressed size).
- **authentication**: before `logging` when the logs should carry the user, or call `logging.AddAttrs(ctx, ...)` once the
  user is known.

## Performance

What each middleware adds to a request, measured against the same API without it (`bare/*`: huma on `net/http`, 5
allocations and about 440 ns per request, which is huma's own). The numbers are from `BenchmarkPerf` in `middleware/`
(`task bench`): test binaries built once for v0.1.0 and for this version, run alternately on two pinned cores, one CPU,
10 runs each, medians below (`benchstat` on the same runs says every difference marked here is significant). The bare
rows differ by about 4% between the two binaries (the same huma code): read the ns column with that offset, the
allocation columns are exact.

| Case | v0.1.0 | this version | ns | allocs v0.1.0 | allocs now | B/op v0.1.0 | B/op now |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `bare/tiny` | 426 ns | 447 ns | +5% | 5 | 5 | 144 | 144 |
| `bare/big` | 428 ns | 443 ns | +4% | 5 | 5 | 144 | 144 |
| `bare/encoded` | 544 ns | 567 ns | +4% | 7 | 7 | 160 | 160 |
| `tracing/disabled` | 436 ns | 462 ns | +6% | 5 | 5 | 144 | 144 |
| `tracing/filtered` | 443 ns | 473 ns | +7% | 5 | 5 | 144 | 144 |
| `tracing/enabled-noop` | 4.81 µs | 4.92 µs | +2% | 37 | 37 | 3616 | 3616 |
| `tracing/recording` | 6.61 µs | 6.76 µs | +2% | 41 | 41 | 6913 | 6913 |
| `accesslog/info-nop-handler` | 1.61 µs | 994 ns | -38% | 12 | 5 | 960 | 144 |
| `accesslog/info-json` | 2.83 µs | 1.99 µs | -30% | 12 | 5 | 960 | 144 |
| `accesslog/level-off` | 957 ns | 725 ns | -24% | 9 | 5 | 512 | 144 |
| `accesslog/skipped` | 819 ns | 609 ns | -26% | 9 | 5 | 512 | 144 |
| `accesslog/net-http-info-nop-handler` | 1.92 µs | 977 ns | -49% | 16 | 5 | 1312 | 144 |
| `accesslog/net-http-info-json` | 3.12 µs | 1.98 µs | -37% | 16 | 5 | 1312 | 144 |
| `accesslog/net-http-level-off` | 1.30 µs | 742 ns | -43% | 13 | 5 | 896 | 144 |
| `logging/request-state` | 814 ns | 661 ns | -19% | 9 | 6 | 512 | 496 |
| `logging/net-http-request-state` | 1.05 µs | 759 ns | -28% | 12 | 7 | 880 | 816 |
| `logging+accesslog/info-nop-handler` | 1.61 µs | 1.24 µs | -23% | 12 | 6 | 960 | 496 |
| `logging+accesslog/info-json` | 2.84 µs | 2.45 µs | -13% | 12 | 6 | 960 | 496 |
| `recovery/no-panic` | 487 ns | 508 ns | +4% | 6 | 5 | 192 | 144 |
| `compression/not-accepted` | 860 ns | 757 ns | -12% | 7 | 6 | 384 | 160 |
| `compression/not-compressible` | 859 ns | 612 ns | -29% | 9 | 5 | 416 | 144 |
| `compression/below-min` | 1.02 µs | 754 ns | -26% | 10 | 6 | 432 | 160 |
| `compression/skipped` | 445 ns | 471 ns | +6% | 5 | 5 | 144 | 144 |
| `compression/gzip-2k` | 10.65 µs | 10.29 µs | -3% | 11 | 7 | 512 | 238 |
| `compression/gzip-20k` | 53.32 µs | 53.06 µs | -0% | 11 | 7 | 771 | 497 |
| `compression/brotli-20k` | 28.20 µs | 30.95 µs | +10% | 11 | 7 | 686 | 430 |
| `compression/zstd-20k` | 11.10 µs | 10.90 µs | -2% | 11 | 7 | 559 | 284 |
| `compression/stream-gzip` | 28.71 µs | 28.39 µs | -1% | 11 | 7 | 590 | 317 |
| `compression/stream-passthrough` | 906 ns | 787 ns | -13% | 7 | 6 | 352 | 128 |
| `bare/stream` | 451 ns | 460 ns | +2% | 5 | 5 | 112 | 112 |
| `cleanpath/clean` | 468 ns | 501 ns | +7% | 5 | 5 | 144 | 144 |
| `cleanpath/encoded-clean` | 618 ns | 621 ns | +1% | 8 | 7 | 168 | 160 |
| `cleanpath/rewrite` | 860 ns | 769 ns | -11% | 9 | 7 | 664 | 600 |
| `chain/tiny` | 9.36 µs | 8.74 µs | -7% | 50 | 39 | 4769 | 3986 |
| `chain/big-gzip` | 64.52 µs | 63.74 µs | -1% | 51 | 40 | 5137 | 4354 |
| `chain/big-zstd` | 21.74 µs | 20.97 µs | -4% | 51 | 40 | 4987 | 4196 |
| `chain-tracing-off/tiny` | 3.71 µs | 3.06 µs | -18% | 18 | 7 | 1296 | 512 |

How to read it, per middleware (allocations on top of `bare`):

- `accesslog` adds **0** allocations in both forms, when the record is written, when its level is off and when the
  request is skipped (`TestAllocs` pins this against a handler that does not allocate; `slog.JSONHandler` and
  `slog.TextHandler` allocate when they format, that is theirs). v0.1.0's access log added 4 to 11 per request.
- `logging` adds 1: the request state, which is also the context handed on (the logger is made when first asked for, +1).
- `recovery` adds 0 when nothing panics.
- `compression` adds 0 when the request is skipped, not compressible or below the minimum size, 1 for the `Vary`
  header when it is not accepted, and 2 when compressing (the encoder state is pooled; the library's own writes remain).
- `cleanpath` adds 0 on a clean path, including an encoded one, and 2 on a rewrite (the request copy and the new path).
- `tracing` adds 0 when disabled or filtered. An enabled middleware adds 32 in all: 4 of its own, which its
  design needs (the context value, the request copy handed to otelhttp and huma's two context wrappers), and the rest
  `otelhttp`'s.

Every pool has a parallel `-race` test, the allocation pins skip under the race detector and run in `task allocs`
(also in CI).

## Upgrading from v0.1.0

`logging` was split: it keeps the request logger, and the access log is the new `accesslog` package. Nothing is
deprecated, the access log options are gone from `logging`.

| v0.1.0 | Now |
| --- | --- |
| `logging.New(opts...)`: logger and access log | `logging.New(...)` and `accesslog.New(...)`, in this order |
| `logging.Handler(next, opts...)` | `logging.Handler(accesslog.Handler(accessOpts...)(next), logOpts...)` |
| `WithLogger`, `WithRequestIDHeader` | the same in both packages (`logging`: the `request_id` of its logger; `accesslog`: the record) |
| `WithEnrichLogs`, `WithRequestEnrichLogs`, `WithoutRequestAttrs`, `FromContext`, `NewContext`, `AddAttrs` | unchanged, in `logging` |
| `WithFields(Fields)` | `logging.WithFields` for `Method`, `Route`, `OperationID`, `RequestID`, `TraceID`, `SpanID`; `accesslog.WithFields` for the names of the record |
| `WithLevels`, `Levels`, `DefaultLevels`, `DefaultFields`, `WithClock`, `WithMessage`, `WithHeaders`, `WithRedactedHeaders`, `WithRequestIDGenerator` | the same names in `accesslog` |
| `WithSkip`, `WithRequestSkip` | the same names in `accesslog` (they skip the record) |
| `WithAccessAttrs(func(Record) []slog.Attr)` | `accesslog.WithAttrs(func(*Record) []slog.Attr)` |
| `WithAccessLogFunc(func(ctx, *slog.Logger, Record))` | `accesslog.WithRecordFunc(func(ctx, *slog.Logger, *Record))`: the `*Record` is pooled, copy what you keep |
| `WithoutAccessLog` | do not install `accesslog` |
| `logging.Record` | `accesslog.Record` (a pointer) |

Behaviour: the access record carries the enrichers' additions, the `AddAttrs` attributes and the trace IDs only when
`logging` is installed outside `accesslog`. A request ID that `accesslog` generated is shown by the request logger only
then too. In `compression`, a wildcard (`text/*`, `*/*`) no longer matches `text/event-stream`: only its exact name does.

## Development

Tasks use [Task](https://taskfile.dev): `task` lists them. `task lint`, `task test`, `task coverage` (fails below 100%),
`task bench`, `task fuzz`, `task tidy-check`, `task vulncheck`. The rules for contributors are in
[AGENTS.md](AGENTS.md).

## License

MIT, see [LICENSE](LICENSE).
