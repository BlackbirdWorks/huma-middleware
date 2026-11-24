# huma-middleware

Useful, router-agnostic middleware for [huma v2](https://github.com/danielgtaylor/huma). Each middleware lives in its
own package, so an application links only the dependencies of the ones it uses.

| Package | What | Kind |
| --- | --- | --- |
| `middleware/cleanpath` | Cleans the request path (`//a/../b` to `/b`) before routing | `net/http` middleware, in front of the router |
| `middleware/tracing` | OpenTelemetry server spans and HTTP server metrics, through `otelhttp` | huma middleware |
| `middleware/logging` | Request-scoped `log/slog` logger and an access log | huma middleware |
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

A request-scoped `log/slog` logger in the context, and one access log record per request.

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
	// level=INFO msg=request req=abc path=/users status=204 method=GET route=/users
}
```

The logger in the context carries `method`, `route`, `operation_id`, `request_id` (header `X-Request-ID`) and, when a
span is in the context, `trace_id` and `span_id`; the attributes are added when a record is handled, so a disabled
level costs nothing. After the response one access log record is written: path (never the query string), status
actually sent, duration, response size, remote address and user agent, at Info, Warn for 4xx, Error for 5xx.
`logging.FromContext(ctx)` returns the request logger (or `slog.Default()`); `logging.AddAttrs(ctx, attrs...)` adds
attributes later in the request (for example once the user is authenticated), to the logs written afterwards and to
the access log. `logging.Handler(next, opts...)` is the form for plain routes.

Options: `WithLogger` (nil or absent means `slog.Default()`, resolved on every request), `WithFields` (rename
attributes), `WithLevels`, `WithClock`, `WithMessage`, `WithSkip(fn)` (skips the access log), `WithoutAccessLog`,
`WithAccessAttrs(fn)`, `WithHeaders(names...)` with `WithRedactedHeaders(names...)` (Authorization, cookies and API key
headers are redacted by default), `WithRequestIDHeader(name)` (the request's ID, else the same header of the response: an ID an outer wrapper set,
then the one on the response when the record is written; `Record.RequestID` is the resolved one),
`WithRequestIDGenerator(fn)` (creates the ID last, when neither side has one, and sets it on the response; the huma form
cannot see a header an outer middleware set earlier), `WithoutRequestAttrs`, `WithEnrichLogs(fn)` (called once per
request, before the handlers; the returned logger is used for handler logs and the access log; several are applied in
order; a panic in `fn` propagates) and `WithAccessLogFunc(fn)`, which replaces the access log record entirely with
your own function and a `Record`, for applications that must keep an existing log format. `Record.Header` and
`Record.RequestURI` read the request: call them before your function returns.

The enrich function sees what is in the context when the logging middleware runs; put authentication before it, or use
`AddAttrs`. On adapters that stream through a callback (Fiber) the access log is written when the handler returns, before
the stream ends.

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

zstd, brotli and gzip, negotiated from `Accept-Encoding`. `compression.New()` with no option is the recommended
configuration; `compression.WithDefaults()` says so explicitly, and the values can be read.

<!-- example: compression ExampleWithDefaults -->
```go
func ExampleWithDefaults() {
	// compression.New() alone is the recommendation; WithDefaults() says so explicitly.
	mw, err := compression.New(compression.WithDefaults())
	if err != nil {
		panic(err)
	}
	_ = mw

	// The values are readable: Recommended, RecommendedContentTypes and RecommendedAllowList.
	cfg := compression.Recommended()
	fmt.Println(cfg.Preference, *cfg.Levels.Gzip, *cfg.Levels.Brotli, *cfg.Levels.Zstd, cfg.MinSize)
	fmt.Println(compression.RecommendedContentTypes()["+json"].Preference)
	// Output:
	// [br gzip zstd] 6 5 3 1024
	// [zstd br gzip]
}
```

### Recommended configuration

| Setting | Recommended | Why |
| --- | --- | --- |
| Preference | brotli, gzip, zstd | brotli compresses text best (about 20% smaller than gzip on JSON), gzip is understood everywhere, zstd is the last resort for clients that only offer it |
| Preference for JSON (`application/json`, `+json`) | zstd, brotli, gzip | zstd is as small as gzip level 9 at a fraction of the cost, which suits responses built per request |
| gzip level | 6 (the library default) | the same ratio as level 9 at a sixth of the time; level 5 is 4% bigger |
| brotli level | 5 | the knee of the curve: levels 6 to 8 do not compress better and cost up to twice as much, level 4 is 12% bigger, level 11 is 50 times slower |
| zstd level | 3 (zstd's default) | levels 1 and 3 are within 3% of each other; level 9 is 7% smaller for 35% more time and four times the memory |
| Minimum size | 1024 bytes | about one TCP segment: below it a response fits one packet whether compressed or not |
| Content types | text, JSON, XML, JavaScript, ... (`text/*`, `+json`, `+xml`, ...) | what shrinks well. Never images (but SVG), video, audio, archives, PDF, fonts (compressed already), and never `text/event-stream` (a stream must reach the client as it is written; it is compressed only when named exactly) |

The levels come from `BenchmarkLevels` (`task bench`), a 19 KB JSON list of records, median of 5 runs on a 3-core
machine; ratio is compressed size over original, and the times are relative (they vary between machines):

| Encoding / level | ratio | time |
| --- | --- | --- |
| gzip 1 | 0.159 | 55 us |
| gzip 4 | 0.149 | 109 us |
| gzip 5 | 0.133 | 133 us |
| **gzip 6** | **0.128** | 93 us |
| gzip 9 | 0.128 | 636 us |
| brotli 1 | 0.147 | 184 us |
| brotli 4 | 0.117 | 511 us |
| **brotli 5** | **0.105** | 605 us |
| brotli 6 | 0.105 | 716 us |
| brotli 11 | 0.085 | 32.9 ms |
| zstd 1 | 0.120 | 48 us |
| **zstd 3** | **0.117** | 54 us |
| zstd 9 | 0.109 | 75 us |

`Recommended()`, `RecommendedContentTypes()` and `RecommendedAllowList()` return these values, each call a new copy to
change. To start from the recommendation and adjust it, add `WithFallback` (the configuration of the content types
without an entry of their own) or `WithContentType` after `WithDefaults()`; the options compose in any order, and the
recommended JSON entry stays:

<!-- example: compression ExampleRecommended -->
```go
func ExampleRecommended() {
	// Start from the recommendation and change one thing: compress smaller bodies too.
	cfg := compression.Recommended()
	cfg.MinSize = 256
	mw, err := compression.New(compression.WithDefaults(), compression.WithFallback(cfg))
	if err != nil {
		panic(err)
	}

	rec := httptest.NewRecorder()
	serveRec(mw, http.MethodGet, "/", []string{"Accept-Encoding", "gzip, zstd"}, rec,
		reply(http.StatusOK, []string{hdrCT, "application/json"}, strings.Repeat("a", 300)))
	fmt.Println(rec.Header().Get("Content-Encoding")) // JSON keeps preferring zstd
	// Output: zstd
}
```

`WithFallback` was called `WithDefault` in v0.1.0; the old name still works and is deprecated.

### Your own configuration

<!-- example: compression ExampleNew -->
```go
func ExampleNew() {
	// compression.New() alone compresses text and JSON, zstd for JSON, then br, then gzip.
	mw, err := compression.New(
		compression.WithFallback(compression.Config{MinSize: 1}), // compress even tiny bodies here
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
	huma.Get(api, "/items", func(context.Context, *struct{}) (*listOut, error) { return &listOut{}, nil })

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip, br, zstd")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	// Output: zstd Accept-Encoding
}
```

The client's quality values come first (`q=0` excludes, `*` and `identity` are
understood; a header split over several lines is read as one list); the server preference breaks ties.

Configuration is one fallback `Config` (`WithFallback`) plus optional per-content-type configs (`WithContentType`). A
`Config` has `Preference` (order), `Levels` (`compression.Levels{Gzip, Brotli, Zstd}`, each a pointer written with `new(n)`, so that level 0 is selectable: gzip -2 to 9, brotli 0 to 11, zstd 1 to 22; nil keeps the default), `MinSize`
(recommended 1024) and `Disabled`; a content type's config inherits what it leaves at the zero value. The entry for a
response is the most specific match: exact (`application/json`), then suffix (`+json`, the longest), then wildcard
(`text/*`, the longest; `*/*` last), then the default. An entry also makes its type compressible. Encoder pools are keyed
by encoding and level. Invalid configuration is an error from `New`.

Other options: `WithContentTypes(patterns...)` (the allow-list; recommended: text, JSON, XML, JavaScript and similar; no wildcard matches `text/event-stream`),
`WithSkipPathPrefixes`, `WithSkipPathPatterns`, `WithOnlyPathPrefixes`, `WithSkipOperations`, `WithSkip(fn)`. Images
other than SVG, video, audio, archives, PDF and fonts are never compressed.

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

`cleanpath` (outermost, `net/http`) then `tracing`, `logging`, `recovery`, `compression` (huma, outermost first).

- **cleanpath first**: every later layer, including path based access rules, sees the same cleaned path, so
  `/public/../admin` cannot slip past a rule that matches `/admin`. Requests that never reach huma (404, 405, raw
  `net/http` routes) are cleaned too.
- **tracing before logging**: the span exists when logging reads the trace and span IDs, and the span covers the whole
  request.
- **logging before recovery**: the recovered panic is logged with the request attributes, and the access log records the
  500 that recovery wrote.
- **recovery inside tracing and logging, outside the handlers**: it recovers before the panic crosses them. What each
  combination records:
  - recovery inside tracing (above): recovery marks the span (exception `*recovery.PanicError`), writes the 500, and
    tracing and its metrics see the 500 as a normal response;
  - recovery outside tracing: tracing records the panic on the span first (exception `*tracing.PanicError`, status 500,
    the 500 also in the metrics) and raises it again; recovery logs it and answers; the span is not marked twice;
  - recovery outside logging: the panic crosses logging, which writes no access log record for that request.
- **compression innermost**: it wraps the handler's response, and everything outside it sees the final status and the
  bytes actually sent (the access log counts the compressed size).
- **authentication**: before `logging` when the log should carry the user, or call `logging.AddAttrs(ctx, ...)` once the
  user is known.

## Development

Tasks use [Task](https://taskfile.dev): `task` lists them. `task lint`, `task test`, `task coverage` (fails below 100%),
`task bench`, `task fuzz`, `task tidy-check`, `task vulncheck`. The rules for contributors are in
[AGENTS.md](AGENTS.md).

## License

MIT, see [LICENSE](LICENSE).
