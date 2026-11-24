// Package reqlog is the one thing the logging and the accesslog middleware share: the key of the request state that
// logging puts in the request context and the interface accesslog reads it through. Neither middleware imports the
// other, and accesslog costs nothing more than a context lookup when logging is not installed.
package reqlog

import "log/slog"

// Key is the context key of the [Source] of a request.
type Key struct{}

// Source is the request state of the logging middleware, as the access log sees it.
type Source interface {
	// Handler is the handler of the request logger without the request attributes (the access log writes its own
	// method, route and request ID): the root handler with what the enrichers and AddAttrs added.
	Handler() slog.Handler
	// AddIDs adds the trace and span ID attributes of the request to the record, when it has a span, as one inlined
	// group made in buf (which must have room for two attributes): it allocates nothing.
	AddIDs(rec *slog.Record, buf []slog.Attr)
	// SetRequestID tells the request logger the ID that the access log resolved or generated, when the request
	// carried none.
	SetRequestID(id string)
}
