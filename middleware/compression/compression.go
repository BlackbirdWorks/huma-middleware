// Package compression is response compression for huma v2: zstd, brotli and
// gzip, negotiated from Accept-Encoding with its quality values. It only uses
// the huma.Context interface.
//
// # Negotiation
//
// The coding with the highest q wins; ties go to the server preference. By
// default JSON ("application/json", "+json") prefers zstd, then brotli, then
// gzip, and everything else brotli, then gzip, then zstd. Both orders can be
// replaced for all responses ([WithDefault]) and per content type
// ([WithContentType]). [Negotiate] is the pure function behind it.
// A request without Accept-Encoding, or accepting nothing usable, gets the
// response as is; the middleware never answers 406.
//
// # Configuration
//
// A [Config] holds a preference order, compression levels by encoding, a minimum
// size and a disabled flag. [WithDefault] sets the default one; [WithContentType]
// sets one for a content type, which inherits what it leaves at the zero value.
// The entry that matches a response is the most specific one: an exact type
// ("application/json"), then a suffix ("+json", the longest), then a wildcard
// ("text/*", the longest; "*/*" last), then the default. Encoders are pooled per
// encoding and level.
//
// # What is compressed
//
// Only content types on the allow-list ([WithContentTypes]; text, JSON, XML,
// JavaScript and similar by default) and never formats that are compressed
// already. Bodies below [Config.MinSize] (1024 bytes by default) are sent as is:
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
	"compress/gzip"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/blackbirdworks/huma-middleware/internal/bodywriter"
)

// Middleware is the huma middleware signature.
type Middleware = func(ctx huma.Context, next func(huma.Context))

// settings is the effective configuration of a set of responses: the default
// with the content type's overrides applied.
type settings struct {
	levels   Levels
	pref     []Encoding
	minSize  int
	disabled bool
	// explicit is set for entries the caller wrote: they make their content type compressible.
	explicit bool
}

// patternSettings is the settings of a suffix or wildcard entry.
type patternSettings struct {
	s       *settings
	pattern string // "+json", or the prefix "text/" of "text/*" ("" for "*/*")
}

type poolKey struct {
	enc   Encoding
	level int
}

type compressor struct {
	bufPool   sync.Pool
	cfg       *config
	def       *settings
	exact     map[string]*settings
	pools     map[poolKey]*encoderPool
	suffixes  []patternSettings
	wildcards []patternSettings
}

// overlay returns base with the non-zero fields of cfg applied.
func overlay(base *settings, cfg Config) *settings {
	s := &settings{
		levels:   base.levels.overlay(cfg.Levels),
		pref:     base.pref,
		minSize:  base.minSize,
		disabled: cfg.Disabled,
	}
	if len(cfg.Preference) > 0 {
		s.pref = cfg.Preference
	}
	if cfg.MinSize > 0 {
		s.minSize = cfg.MinSize
	}

	return s
}

// New returns the compression middleware. It returns an error for an invalid
// option; see the Err variables.
func New(opts ...Option) (Middleware, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	builtin := &settings{
		levels: Levels{
			Gzip:   new(gzip.DefaultCompression),
			Brotli: new(defaultBrotliLevel),
			Zstd:   new(defaultZstdLevel),
		},
		pref:    []Encoding{Brotli, Gzip, Zstd},
		minSize: defaultMinSize,
	}
	c := &compressor{
		cfg:   cfg,
		def:   overlay(builtin, cfg.def),
		exact: map[string]*settings{},
		pools: map[poolKey]*encoderPool{},
	}
	all := []*settings{c.def}
	for _, e := range cfg.entries {
		s := overlay(c.def, e.cfg)
		s.explicit = !e.builtin
		if e.builtin {
			// The built-in JSON entry only changes the preference: a disabled default stays disabled.
			s.disabled = c.def.disabled
		}
		all = append(all, s)
		switch {
		case strings.HasPrefix(e.pattern, "+"):
			c.suffixes = append(c.suffixes, patternSettings{s, e.pattern})
		case e.pattern == "*/*":
			c.wildcards = append(c.wildcards, patternSettings{s, ""})
		case strings.HasSuffix(e.pattern, "/*"):
			c.wildcards = append(c.wildcards, patternSettings{s, strings.TrimSuffix(e.pattern, "*")})
		default:
			c.exact[e.pattern] = s
		}
	}
	longestFirst := func(a, b patternSettings) int { return len(b.pattern) - len(a.pattern) }
	slices.SortFunc(c.suffixes, longestFirst)
	slices.SortFunc(c.wildcards, longestFirst)
	maxMin := 0
	for _, s := range all {
		maxMin = max(maxMin, s.minSize)
		for _, e := range s.pref {
			key := poolKey{e, s.levels.of(e)}
			if c.pools[key] == nil {
				c.pools[key] = newEncoderPool(e, key.level)
			}
		}
	}
	c.bufPool.New = func() any {
		b := make([]byte, 0, maxMin)

		return &b
	}

	return c.handle, nil
}

// settingsFor returns the settings of a media type.
func (c *compressor) settingsFor(mt string) *settings {
	if s, ok := c.exact[mt]; ok {
		return s
	}
	for _, e := range c.suffixes {
		if strings.HasSuffix(mt, e.pattern) {
			return e.s
		}
	}
	for _, e := range c.wildcards {
		if strings.HasPrefix(mt, e.pattern) {
			return e.s
		}
	}

	return c.def
}

// allowed reports whether a response of the content type may be compressed, and with which settings.
func (c *compressor) allowed(ct string) (*settings, bool) {
	mt := mediaType(ct)
	s := c.settingsFor(mt)
	if mt == "" || alreadyCompressed(mt) || s.disabled {
		return s, false
	}
	if s.explicit || (len(c.cfg.types) == 0 && isDefaultAllowed(mt)) {
		return s, true
	}

	return s, slices.ContainsFunc(c.cfg.types, func(p string) bool { return matchType(p, mt) })
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

// acceptEncoding returns the Accept-Encoding of the request with all its field
// lines joined: they are one comma-separated list, but Header returns the first.
func acceptEncoding(ctx huma.Context) string {
	first := ctx.Header("Accept-Encoding")
	if first == "" {
		return ""
	}
	joined := first
	count := 0
	ctx.EachHeader(func(name, value string) {
		if strings.EqualFold(name, "Accept-Encoding") {
			count++
			if count > 1 {
				joined += ", " + value
			}
		}
	})

	return joined
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
		accept: acceptEncoding(ctx),
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
	if !r.streaming {
		r.finish()
	}
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

	set             *settings
	contentType     string
	contentEncoding string
	cacheControl    string
	etag            string
	contentLength   string

	status     int
	state      state
	statusSet  bool
	hasLength  bool
	streaming  bool
	allowed    bool
	hasETag    bool
	varyListed bool
}

// SetStatus records the status; it is sent when the body starts.
//
// Informational statuses such as 103 Early Hints are sent at once with the
// headers set so far: they are not the final status, and the final one is
// still to come.
func (r *response) SetStatus(code int) {
	if code >= http.StatusContinue && code < http.StatusOK && code != http.StatusSwitchingProtocols {
		r.base.SetStatus(code)

		return
	}
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

// bodyStreamer is implemented by the contexts of adapters that stream the body
// through a callback (Fiber), which huma's SSE support looks for.
type bodyStreamer interface {
	StreamBody(func(io.Writer))
}

// StreamBody keeps streaming through a callback working on the adapters that
// need it. A stream is not buffered: the decision is taken, and the status and
// headers are sent, before the stream starts, as they must be on those adapters.
func (r *response) StreamBody(fn func(io.Writer)) {
	bs, ok := r.base.(bodyStreamer)
	if !ok {
		fn(r)

		return
	}
	if r.state == undecided {
		r.decide(max(r.resolve().minSize, 1))
	}
	r.streaming = true
	bs.StreamBody(func(sink io.Writer) {
		r.sink = sink
		if r.state == compressing {
			r.enc.Reset(sink)
		}
		fn(r)
		r.finish()
	})
}

// Unwrap returns the context this one wraps, which adapters' Unwrap helpers
// (humago.Unwrap, humachi.Unwrap) follow to reach the request and the writer.
func (r *response) Unwrap() huma.Context { return r.base }

// SetWriteDeadline forwards to the writer under the response, so huma's SSE
// support can still apply its per-send write timeout through the compressed body writer.
func (r *response) SetWriteDeadline(t time.Time) error {
	return (&bodywriter.Passthrough{W: r.sink}).SetWriteDeadline(t) //nolint:wrapcheck // the writer's own error
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
		if size < r.resolve().minSize {
			r.buffer(p)

			return len(p), nil
		}
	}
	r.decide(size)

	return r.Write(p)
}

// resolve returns the settings for the response's content type, once.
func (r *response) resolve() *settings {
	if r.set == nil {
		r.set, r.allowed = r.c.allowed(r.contentType)
	}

	return r.set
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
		r.decide(max(r.resolve().minSize, 1))
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
		r.pool = r.c.pools[poolKey{enc, r.set.levels.of(enc)}]
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
	set := r.resolve()
	allowed := r.allowed
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
	case size == 0 || size < set.minSize:
		return "", false, true
	}
	enc, ok := Negotiate(r.accept, set.pref)

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
