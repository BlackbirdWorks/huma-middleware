// Package middleware is the root of the huma-middleware tree; it has no code.
//
// The middlewares live in their own packages so that an application links only
// the dependencies of the ones it uses:
//
//   - cleanpath: a net/http middleware, in front of the router, that cleans the
//     request path (not a huma middleware: it must run before routing).
//   - tracing: OpenTelemetry server spans and metrics through otelhttp.
//   - logging: a request-scoped log/slog logger and an access log.
//   - recovery: recovers panics, logs them, marks the span and answers 500.
//   - compression: zstd, brotli and gzip response compression.
//
// Tracing, logging, recovery and compression are huma middleware, written
// against the huma.Context interface only. Register them with api.UseMiddleware
// in this order, the first one being the outermost: tracing, logging, recovery,
// compression.
package middleware
