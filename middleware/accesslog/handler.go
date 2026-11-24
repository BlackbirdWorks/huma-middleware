package accesslog

import (
	"bufio"
	"net"
	"net/http"
	"strings"

	"github.com/blackbirdworks/huma-middleware/internal/reqlog"
)

// plain is the state of one request of the net/http form: the response writers handed to the handler (with and
// without Hijack), the log record that is written and the Record. It is pooled.
type plain struct {
	w      respWriter
	hj     hijackWriter
	out    output
	record Record
}

func newPlain() *plain {
	p := &plain{}
	p.hj = hijackWriter{&p.w}

	return p
}

// Handler returns the access log for a plain [http.Handler], for routes that bypass huma. The route is the
// [http.Request] Pattern set by [http.ServeMux] after routing, so it is only known when the mux is called directly
// by the handler this one wraps or through wrappers that give the pattern back (the logging middleware does); the
// operation ID is empty.
//
// The request ID is the request header; failing that the response header (an ID an outer wrapper set), and the
// generator of [WithRequestIDGenerator] last. The record reports the response header as it is when it is written.
func Handler(opts ...Option) func(http.Handler) http.Handler {
	a := newAccessLog(opts)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.servePlain(w, r, next) })
	}
}

func (a *accessLog) servePlain(w http.ResponseWriter, r *http.Request, next http.Handler) {
	cfg := a.cfg
	ctx := r.Context()
	src, _ := ctx.Value(reqlog.Key{}).(reqlog.Source)
	p, _ := a.plain.Get().(*plain)
	p.w = respWriter{ResponseWriter: w}
	reqID, respID := requestIDs(cfg, r, w)
	if respID != "" && src != nil {
		src.SetRequestID(respID)
	}
	start := cfg.clock()
	var out http.ResponseWriter = &p.w
	if _, ok := w.(http.Hijacker); ok {
		out = p.hj
	}

	next.ServeHTTP(out, r)

	if cfg.reqSkip == nil || !cfg.reqSkip(r) {
		if reqID == "" && cfg.requestID != "" {
			reqID = w.Header().Get(cfg.idKey)
		}
		rec := &p.record
		*rec = Record{
			req:        r,
			Method:     r.Method,
			Path:       r.URL.Path,
			Route:      routePart(r.Pattern),
			Host:       r.Host,
			RemoteAddr: r.RemoteAddr,
			UserAgent:  r.UserAgent(),
			RequestID:  reqID,
			Duration:   cfg.clock().Sub(start),
			Size:       p.w.n,
			Status:     statusOf(p.w.status),
		}
		a.write(ctx, src, &p.out, rec)
	}
	p.w = respWriter{}
	p.record = Record{}
	a.plain.Put(p)
}

// requestIDs resolves the request ID of a plain handler: the request header; failing that the response header (an ID
// an outer wrapper set); failing that the generated one, which is set on the response. reqID is the request's, respID
// the one found or made on the response.
func requestIDs(cfg *config, r *http.Request, w http.ResponseWriter) (string, string) {
	if cfg.requestID == "" {
		return "", ""
	}
	if v := r.Header[cfg.idKey]; len(v) > 0 && v[0] != "" {
		return v[0], ""
	}
	respID := w.Header().Get(cfg.idKey)
	if respID == "" && cfg.idGen != nil {
		respID = cfg.idGen()
		w.Header().Set(cfg.idKey, respID)
	}

	return "", respID
}

// routePart strips the optional "METHOD " prefix of a [http.ServeMux] pattern.
func routePart(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}

	return ""
}

// interim reports whether the status is informational (103 Early Hints): not the final one. 101 Switching Protocols
// ends the exchange and is final.
func interim(code int) bool {
	return code >= http.StatusContinue && code < http.StatusOK && code != http.StatusSwitchingProtocols
}

// respWriter records the status and size of a plain handler response.
type respWriter struct {
	http.ResponseWriter

	n      int64
	status int
}

func (w *respWriter) WriteHeader(code int) {
	// Informational statuses (103 Early Hints) are not the final one.
	if w.status == 0 && !interim(code) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)

	return n, err //nolint:wrapcheck // transparent pass-through of the response writer error
}

// Flush keeps streaming responses working.
func (w *respWriter) Flush() { _ = w.FlushError() }

// FlushError flushes the underlying writer and returns what it reports, so [http.ResponseController] sees the real
// result (including "not supported").
func (w *respWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil && w.status == 0 {
		w.status = http.StatusOK // a flush commits the headers with the implicit 200
	}

	return err //nolint:wrapcheck // the writer's own error
}

// hijackWriter is the writer given to handlers when the underlying writer is an [http.Hijacker], so that a direct
// w.(http.Hijacker) assertion succeeds exactly when it would have on the underlying writer.
type hijackWriter struct {
	*respWriter
}

// Hijack lets handlers take over the connection (upgrades).
func (w hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack() //nolint:wrapcheck // the writer's own error
}

// Unwrap exposes the underlying writer to [http.ResponseController].
func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
