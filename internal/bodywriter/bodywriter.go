// Package bodywriter holds what the response body writers of the middleware
// packages have in common: they wrap an [io.Writer] and must keep its optional
// capabilities (flush, flush errors, unwrapping to the [http.ResponseWriter],
// write deadlines) reachable for the code that looks for them, such as huma's
// server-sent events.
package bodywriter

import (
	"io"
	"net/http"
	"time"
)

// Passthrough forwards the optional capabilities of the wrapped writer. Embed
// it in a writer type that defines Write.
type Passthrough struct {
	// W is the wrapped writer.
	W io.Writer
}

// Flush flushes the wrapped writer when it can be flushed.
func (p *Passthrough) Flush() { _ = p.FlushError() }

// FlushError flushes the wrapped writer and returns what it reports, for
// [http.ResponseController]: [http.ErrNotSupported] for a response writer that
// cannot flush. A writer that is not a response writer but has Flush is flushed;
// one that has neither is not an error.
func (p *Passthrough) FlushError() error {
	switch w := p.W.(type) {
	case http.ResponseWriter:
		return http.NewResponseController(w).Flush() //nolint:wrapcheck // the writer's own error
	case http.Flusher:
		w.Flush()
	}

	return nil
}

// Unwrap exposes the wrapped writer to code that follows Unwrap chains
// ([http.ResponseController], huma's SSE support).
func (p *Passthrough) Unwrap() http.ResponseWriter {
	rw, _ := p.W.(http.ResponseWriter)

	return rw
}

// SetWriteDeadline forwards to the wrapped writer, directly or through its
// [http.ResponseWriter] chain; [http.ErrNotSupported] when it has no deadlines.
func (p *Passthrough) SetWriteDeadline(t time.Time) error {
	if d, ok := p.W.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t) //nolint:wrapcheck // the writer's own error
	}
	if rw, ok := p.W.(http.ResponseWriter); ok {
		return http.NewResponseController(rw).SetWriteDeadline(t) //nolint:wrapcheck // the writer's own error
	}

	return http.ErrNotSupported
}
