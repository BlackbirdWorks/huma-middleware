// Package compression is response compression for huma v2: zstd, brotli and
// gzip, negotiated from Accept-Encoding with its quality values. It only uses
// the huma.Context interface.
//
// # Negotiation
//
// The coding with the highest q wins; ties go to the server preference. By
// default JSON ("application/json", "+json") prefers zstd, then brotli, then
// gzip, and everything else brotli, then gzip, then zstd. Both orders can be
// replaced globally ([WithPreference]) and per content type
// ([WithContentTypePreference]). [Negotiate] is the pure function behind it.
// A request without Accept-Encoding, or accepting nothing usable, gets the
// response as is; the middleware never answers 406.
//
// # What is compressed
//
// Only content types on the allow-list ([WithContentTypes]; text, JSON, XML,
// JavaScript and similar by default) and never formats that are compressed
// already. Bodies below [WithMinSize] (1024 bytes by default) are sent as is:
// a response with a known Content-Length is decided at once, otherwise up to
// that many bytes are buffered. Responses are not compressed for HEAD, 204, 304,
// 206 and 1xx statuses, when Content-Encoding is already set, or when
// Cache-Control says no-transform. Requests can be excluded by path prefix, path
// pattern, operation or function; [WithOnlyPathPrefixes] inverts the path rule.
//
// # Headers
//
//   - Content-Encoding is set to the coding used.
//   - Content-Length is not sent for a compressed response (the length is not
//     known before the end). It is sent unchanged otherwise.
//   - Vary: Accept-Encoding is appended to any Vary the handler set, once, on every
//     response that is eligible for compression (compressible type, large
//     enough), including those that end up uncompressed because the client
//     does not accept anything, and on HEAD and 304 responses. A response that
//     never varies (excluded path, already encoded, an image) has no Vary.
//   - A strong ETag is made weak ("W/" prefix) when the body is compressed,
//     as nginx does. The compressed bytes are a different representation, so a
//     strong validator would be wrong (RFC 9110, section 8.8.3), while a weak
//     one stays valid for If-None-Match, whose comparison is weak. A weak ETag
//     is kept. An uncompressed response keeps its ETag.
//
// # Streaming
//
// text/event-stream is not compressed by default and is passed through with its
// flushes intact. When a compressible response calls Flush before the minimum
// size is reached, the decision is taken then: it is compressed and the encoder
// is flushed, so data is never held back indefinitely.
//
// # Limits
//
// A huma.Context cannot read response headers. The middleware sees the headers
// set through the context it passes down (Content-Type, Content-Encoding,
// Content-Length, ETag, Cache-Control, Vary), which is how handlers and huma
// set them, but not headers that an outer middleware or the router set
// earlier. HEAD responses are not compressed (they have no body), so their
// Content-Encoding differs from the matching GET.
//
// Encoders are pooled per encoding. A write error to the client is returned
// to the handler by Write; the middleware has nobody to report a failure of
// the final flush to.
package compression

import (
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

type compressor struct {
	cfg     *config
	pools   map[Encoding]*encoderPool
	bufPool sync.Pool
}

// New returns the compression middleware. It returns an error for an invalid
// option; see the Err variables.
func New(opts ...Option) (Middleware, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	c := &compressor{cfg: cfg, pools: make(map[Encoding]*encoderPool)}
	for _, e := range c.enabled() {
		c.pools[e] = newEncoderPool(e, cfg.levels[e])
	}
	c.bufPool.New = func() any {
		b := make([]byte, 0, cfg.minSize)

		return &b
	}

	return c.handle, nil
}

// enabled returns every encoding that some preference list names.
func (c *compressor) enabled() []Encoding {
	var out []Encoding
	add := func(es []Encoding) {
		for _, e := range es {
			if !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	add(c.cfg.preference)
	for _, r := range c.cfg.rules {
		add(r.prefs)
	}

	return out
}

// prefsFor returns the preference list for a media type.
func (c *compressor) prefsFor(mt string) []Encoding {
	for _, r := range c.cfg.rules {
		if matchType(r.pattern, mt) {
			return r.prefs
		}
	}

	return c.cfg.preference
}

func (c *compressor) allowed(ct string) bool {
	mt := mediaType(ct)
	if mt == "" || alreadyCompressed(mt) {
		return false
	}
	if len(c.cfg.types) == 0 {
		return isDefaultAllowed(mt)
	}

	return slices.ContainsFunc(c.cfg.types, func(p string) bool { return matchType(p, mt) })
}

// skipped reports whether the request is excluded before the response is known.
func (c *compressor) skipped(ctx huma.Context) bool {
	cfg := c.cfg
	if cfg.skip != nil && cfg.skip(ctx) {
		return true
	}
	if len(cfg.skipOps) > 0 {
		if op := ctx.Operation(); op != nil && slices.Contains(cfg.skipOps, op.OperationID) {
			return true
		}
	}
	if len(cfg.skipPrefixes)+len(cfg.skipPatterns)+len(cfg.onlyPrefixes) == 0 {
		return false
	}
	u := ctx.URL()
	p := u.Path
	if slices.ContainsFunc(cfg.skipPrefixes, func(s string) bool { return strings.HasPrefix(p, s) }) {
		return true
	}
	if slices.ContainsFunc(cfg.skipPatterns, func(s string) bool { return globMatch(s, p) }) {
		return true
	}

	return len(cfg.onlyPrefixes) > 0 &&
		!slices.ContainsFunc(cfg.onlyPrefixes, func(s string) bool { return strings.HasPrefix(p, s) })
}

// globMatch reports whether name matches the (validated) [path.Match] pattern.
func globMatch(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)

	return ok
}

func (c *compressor) handle(ctx huma.Context, next func(huma.Context)) {
	if c.skipped(ctx) {
		next(ctx)

		return
	}
	r := &response{
		base:   ctx,
		c:      c,
		sink:   ctx.BodyWriter(),
		accept: ctx.Header("Accept-Encoding"),
		method: ctx.Method(),
	}
	done := false
	defer func() {
		if !done {
			r.abort()
		}
	}()
	next(r)
	done = true
	r.finish()
}

type state int

const (
	undecided state = iota
	passthrough
	compressing
	finished
)

// base lets response embed the context while overriding some of its methods.
type base huma.Context

// response is the huma.Context given to the rest of the chain. It holds the
// status and the headers that decide compression back until the first byte of
// the body, so the decision can still change them.
type response struct {
	base

	c      *compressor
	sink   io.Writer
	enc    encoder
	pool   *encoderPool
	bufp   *[]byte
	method string
	accept string

	contentType     string
	contentEncoding string
	cacheControl    string
	etag            string
	contentLength   string

	status     int
	state      state
	statusSet  bool
	hasLength  bool
	hasETag    bool
	varyListed bool
}

// SetStatus records the status; it is sent when the body starts.
func (r *response) SetStatus(code int) {
	r.status, r.statusSet = code, true
}

// Status returns the status set through this context.
func (r *response) Status() int {
	if r.statusSet {
		return r.status
	}

	return r.base.Status()
}

// SetHeader sets a header, watching the ones that matter here.
func (r *response) SetHeader(name, value string) {
	r.track(name, value, true)
}

// AppendHeader appends to a header, watching the ones that matter here.
func (r *response) AppendHeader(name, value string) {
	r.track(name, value, false)
}

func (r *response) track(name, value string, replace bool) {
	switch {
	case strings.EqualFold(name, "Content-Length"):
		r.contentLength, r.hasLength = value, true

		return
	case strings.EqualFold(name, "ETag"):
		r.etag, r.hasETag = value, true

		return
	case strings.EqualFold(name, "Content-Type"):
		r.contentType = value
	case strings.EqualFold(name, "Content-Encoding"):
		r.contentEncoding = value
	case strings.EqualFold(name, "Cache-Control"):
		if replace || r.cacheControl == "" {
			r.cacheControl = value
		} else {
			r.cacheControl += "," + value
		}
	case strings.EqualFold(name, "Vary"):
		listed := varyListsAcceptEncoding(value)
		r.varyListed = listed || (!replace && r.varyListed)
	}
	if replace {
		r.base.SetHeader(name, value)
	} else {
		r.base.AppendHeader(name, value)
	}
}

func varyListsAcceptEncoding(v string) bool {
	for t := range strings.SplitSeq(v, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.EqualFold(t, "Accept-Encoding") {
			return true
		}
	}

	return false
}

// BodyWriter returns the writer the handler writes the body to.
func (r *response) BodyWriter() io.Writer { return r }

// Write decides, on the first call that can, whether to compress, and writes.
func (r *response) Write(p []byte) (int, error) {
	switch r.state {
	case compressing:
		return r.enc.Write(p)
	case passthrough:
		return r.sink.Write(p)
	case finished:
		return 0, io.ErrClosedPipe
	case undecided:
	}
	if len(p) == 0 {
		return 0, nil
	}
	size, known := r.knownSize()
	if !known {
		size = r.buffered() + len(p)
		if size < r.c.cfg.minSize {
			r.buffer(p)

			return len(p), nil
		}
	}
	r.decide(size)

	return r.Write(p)
}

func (r *response) buffered() int {
	if r.bufp == nil {
		return 0
	}

	return len(*r.bufp)
}

func (r *response) buffer(p []byte) {
	if r.bufp == nil {
		r.bufp, _ = r.c.bufPool.Get().(*[]byte)
	}
	*r.bufp = append(*r.bufp, p...)
}

// knownSize returns the body size from a Content-Length the handler set.
func (r *response) knownSize() (int, bool) {
	if !r.hasLength {
		return 0, false
	}
	n, err := strconv.Atoi(r.contentLength)

	return n, err == nil && n >= 0
}

// Flush flushes the body to the client, deciding first when still undecided:
// a flush means a stream, which is compressed without waiting for more data.
func (r *response) Flush() {
	if r.state == undecided {
		r.decide(max(r.c.cfg.minSize, 1))
	}
	if r.state == compressing {
		_ = r.enc.Flush()
	}
	if f, ok := r.sink.(http.Flusher); ok {
		f.Flush()
	}
}

// decide fixes the mode for a body of the given size, sends the status and the
// headers, then writes what was buffered.
func (r *response) decide(size int) {
	enc, compress, vary := r.plan(size)
	if vary && !r.varyListed {
		r.base.AppendHeader("Vary", "Accept-Encoding")
	}
	if r.hasETag {
		etag := r.etag
		if compress && !strings.HasPrefix(etag, "W/") {
			etag = "W/" + etag
		}
		r.base.SetHeader("ETag", etag)
	}
	if compress {
		r.base.SetHeader("Content-Encoding", string(enc))
	} else if r.hasLength {
		r.base.SetHeader("Content-Length", r.contentLength)
	}
	if r.statusSet {
		r.base.SetStatus(r.status)
	}
	r.state = passthrough
	if compress {
		r.pool = r.c.pools[enc]
		r.enc = r.pool.get(r.sink)
		r.state = compressing
	}
	if r.bufp != nil {
		// Errors reach the handler on its next Write to the same writer.
		_, _ = r.Write(*r.bufp)
		r.releaseBuffer()
	}
}

// plan applies the rules: the encoding, whether to compress, whether to add Vary.
func (r *response) plan(size int) (Encoding, bool, bool) {
	status := r.status
	if !r.statusSet {
		status = http.StatusOK
	}
	allowed := r.c.allowed(r.contentType)
	switch {
	case r.method == http.MethodHead:
		return "", false, allowed
	case status == http.StatusNotModified:
		return "", false, true
	case status < http.StatusOK || status == http.StatusNoContent || status == http.StatusPartialContent:
		return "", false, false
	case r.contentEncoding != "" && !strings.EqualFold(r.contentEncoding, "identity"):
		return "", false, false
	case !allowed || strings.Contains(strings.ToLower(r.cacheControl), "no-transform"):
		return "", false, false
	case size == 0 || size < r.c.cfg.minSize:
		return "", false, true
	}
	enc, ok := Negotiate(r.accept, r.c.prefsFor(mediaType(r.contentType)))

	return enc, ok, true
}

func (r *response) releaseBuffer() {
	if r.bufp != nil {
		*r.bufp = (*r.bufp)[:0]
		r.c.bufPool.Put(r.bufp)
		r.bufp = nil
	}
}

// finish ends the response: it decides on what was buffered and closes the encoder.
func (r *response) finish() {
	if r.state == undecided {
		r.decide(r.buffered())
	}
	if r.state == compressing {
		_ = r.enc.Close()
		r.pool.put(r.enc)
		r.enc = nil
	}
	r.state = finished
}

// abort is finish for a handler that panicked: nothing more is written, and the
// encoder and the buffer go back to their pools.
func (r *response) abort() {
	if r.state == compressing {
		r.pool.put(r.enc)
		r.enc = nil
	}
	r.releaseBuffer()
	r.state = finished
}
