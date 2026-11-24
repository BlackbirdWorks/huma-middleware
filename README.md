# huma-middleware

Useful, router-agnostic middleware for [huma v2](https://github.com/danielgtaylor/huma). Each middleware lives in its
own package, so an application links only the dependencies of the ones it uses.

| Package | What | Kind |
| --- | --- | --- |
| `middleware/cleanpath` | Cleans the request path (`//a/../b` to `/b`) before routing | `net/http` middleware, in front of the router |
| `middleware/tracing` | OpenTelemetry server spans and HTTP server metrics, through `otelhttp` | huma middleware |
| `middleware/logging` | Request-scoped `log/slog` logger and an access log | huma middleware |
| `middleware/compression` | zstd, brotli and gzip response compression | huma middleware |

The three huma middlewares are written against the `huma.Context` interface only. They import no router adapter and
behave the same under `humago`, `humachi`, `humagin` and the others (the tests run them under `humatest`, `humago` and
`humachi`). `cleanpath` is not a huma middleware on purpose: huma middleware runs after the router matched a route,
which is too late to change which route matches. It is a plain `http.Handler` wrapper that imports only `net/http`.

## Install

```shell
go get github.com/blackbirdworks/huma-middleware
```

## All four together

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
		compress,
	)

	// ... register operations ...

	// cleanpath goes in front of the router, outside of everything else.
	srv := &http.Server{Addr: ":8080", Handler: cleanpath.Handler()(mux)}
	_ = srv.ListenAndServe()
}
```

### Order

`cleanpath` (outermost, `net/http`) then `tracing`, `logging`, `compression` (huma, outermost first).

- **cleanpath first**: every later layer, including path based access rules, sees the same cleaned path, so
  `/public/../admin` cannot slip past a rule that matches `/admin`. Requests that never reach huma (404, 405, raw
  `net/http` routes) are cleaned too.
- **tracing before logging**: the span exists when logging reads the trace and span IDs, and the span covers the whole
  request including logging and compression.
- **logging before compression**: the access log sees the final status and the bytes actually sent (the compressed
  size), and its duration includes compression.
- **compression innermost**: it wraps the handler's response; everything outside it sees the final status and body.
- **authentication**: put it before `logging` when the log should carry the user, or call `logging.AddAttrs(ctx, ...)`
  once the user is known (see below).

## cleanpath

`cleanpath.Handler(opts...) func(http.Handler) http.Handler`.

- Collapses repeated slashes, resolves `.` and `..` (never above the root), using the `path.Clean` rules that `net/http`
  applies. It works on the escaped path: `%2F` stays inside its segment, `%2e` counts as a dot.
- Modes: `Rewrite` (default; the request is served with the cleaned path, on a shallow copy; `RequestURI` stays as
  received), `Redirect` (301 for GET and HEAD, 308 otherwise, query kept) and `Reject` (404, or `WithRejectHandler`).
- `WithTrailingSlash(KeepSlash | StripSlash | AddSlash)`; `WithSkip(fn)`. `CONNECT` and `OPTIONS *` are untouched.
- An already clean path costs no allocation.

## tracing

`tracing.New(opts...)` for huma and `tracing.Handler(next, opts...)` for plain `net/http` routes.

Options: `WithTracerProvider`, `WithMeterProvider`, `WithPropagators`, `WithSpanNameFormatter(func(method, route string) string)`,
`WithFilter(func(huma.Context) bool)`, `WithRequestFilter` (for `Handler`), `WithSkipOperations(ids...)`,
`WithSkipPaths(routes...)`, `WithPublicEndpoint()`, `WithServerName`, `WithSpanOptions`.

The span is named `METHOD /route/{template}` (low cardinality), has `http.route`, continues the incoming
`traceparent`, and is visible to handlers through the context. 5xx sets the span status to error. The otelhttp HTTP
server metrics carry the same `http.route` label.

How it reuses otelhttp: one `otelhttp` handler is built at construction. Per request the middleware fills a pooled
`http.Request` view of the `huma.Context` (method, URL, host, remote address, protocol, TLS, the propagator headers,
`User-Agent`, `X-Forwarded-For`, and the route as the request `Pattern`, which is how otelhttp learns `http.route`),
the inner handler calls the rest of the chain, and reports `ctx.Status()` back to otelhttp.

Limits compared with otelhttp on a real handler:

- request and response body sizes (span attributes and size metrics) are 0: huma reads and writes the bodies;
- `WithMessageEvents` (read and write span events) is not available;
- only the headers listed above are visible to propagators;
- requests that do not reach huma are not seen (wrap those with `tracing.Handler`);
- a panic is not recorded as an error status.

## logging

`logging.New(opts...)` for huma and `logging.Handler(next, opts...)` for plain routes.
`logging.FromContext(ctx)` returns the request logger (or `slog.Default()` when there is none),
`logging.AddAttrs(ctx, attrs...)` adds attributes to the rest of the request.

The logger in the context carries `method`, `route`, `operation_id`, `request_id` (header `X-Request-ID`) and, when a
span is in the context, `trace_id` and `span_id`. The attributes are added when a record is handled, so a disabled level
costs nothing. After the response, one access log record is written: path (never the query string), status actually
sent, duration, response size, remote address and user agent, at Info, Warn for 4xx and Error for 5xx.

Options: `WithLogger` (nil or absent means `slog.Default()`, resolved on every request), `WithFields` (rename
attributes), `WithLevels`, `WithClock`, `WithMessage`, `WithSkip(fn)` (skips the access log), `WithoutAccessLog`,
`WithAccessAttrs(fn)` (extra attributes), `WithHeaders(names...)` with `WithRedactedHeaders(names...)` (Authorization,
cookies and API key headers are redacted by default), `WithRequestIDHeader`, `WithoutRequestAttrs`,
`WithEnrichLogs(fn)` (called once per request, before the handlers, with the logger to add to; the returned logger is
used for handler logs and for the access log; several are applied in order; a panic in `fn` propagates) and
`WithAccessLogFunc(fn)`, which replaces the access log record entirely with your own function and a `Record` of
everything known about the request, for applications that must keep an existing log format.

```go
logging.New(
	logging.WithLogger(logger),
	logging.WithEnrichLogs(func(ctx huma.Context, l *slog.Logger) *slog.Logger {
		return l.With(slog.String("tenant", ctx.Header("X-Tenant")))
	}),
)
```

The enrich function sees what is in the context when the logging middleware runs. Values that are only known later
(for instance the authenticated user) are added with `logging.AddAttrs(ctx, slog.String("user", id))`: they appear on
the logs written afterwards and on the access log.

## compression

`compression.New(opts...) (Middleware, error)`, and `compression.Negotiate(acceptEncoding, preference)`.

Default preference: zstd for JSON (`application/json`, `+json`) when the client accepts it, otherwise brotli, then gzip;
zstd is the last resort for other types. The client's quality values come first (`q=0` excludes, `*` and `identity`
are understood); the server preference breaks ties.

Options: `WithPreference(encs...)`, `WithContentTypePreference(pattern, encs...)`,
`WithLevel(enc, level)` (gzip -2 to 9, brotli 0 to 11, zstd 1 to 22; invalid levels are an error), `WithMinSize(n)`
(default 1024), `WithContentTypes(patterns...)` (allow-list; default text, JSON, XML, JavaScript and similar),
`WithSkipPathPrefixes`, `WithSkipPathPatterns`, `WithOnlyPathPrefixes`, `WithSkipOperations`, `WithSkip(fn)`.
Images other than SVG, video, audio, archives, PDF and fonts are never compressed.

Headers: `Content-Encoding` is set; `Content-Length` is not sent for a compressed body; `Vary: Accept-Encoding` is
appended to the handler's `Vary` (once) on every response that could be compressed, including those that are not
because the client does not accept anything; a strong `ETag` is made weak (`W/`) when the body is compressed, as nginx
does; no compression for HEAD, 204, 304, 206, an existing `Content-Encoding` or `Cache-Control: no-transform`.

Streaming: `text/event-stream` is not compressed by default and flushes pass straight through. A compressible
response that flushes before reaching the minimum size is compressed from that point and the encoder is flushed with
it, so data is never held back.

Limits: a `huma.Context` cannot read response headers, so only headers set through the context handed down (what
handlers and huma use) are seen, not those set earlier by an outer middleware. HEAD responses are not compressed, so
their `Content-Encoding` differs from the GET.

## Development

Tasks use [Task](https://taskfile.dev): `task` lists them. `task lint`, `task test`, `task coverage` (fails below 100%),
`task bench`, `task fuzz`, `task tidy-check`, `task vulncheck`. The rules for contributors are in
[AGENTS.md](AGENTS.md).

## License

MIT, see [LICENSE](LICENSE).
