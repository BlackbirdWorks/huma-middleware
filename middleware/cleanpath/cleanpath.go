// Package cleanpath cleans the request URL path before routing. It is a plain
// [net/http] middleware, not a huma middleware: huma middleware runs after the
// router has matched a route, which is too late to change which route matches.
// Put it in front of any router (gin, chi, [http.ServeMux], a huma adapter's
// mux), as the outermost layer, so that every later layer (logging, tracing,
// authentication, path based access rules) sees the same cleaned path.
//
// # Rules
//
// The path is cleaned with the [path.Clean] rules that net/http applies in its
// own cleaning: repeated slashes collapse, "." segments disappear and ".."
// segments remove the segment before them, never going above the root. Two
// differences from net/http's cleanPath: the trailing slash is governed by
// [TrailingSlash] (the default, [KeepSlash], matches net/http), and a path that
// ends in a "." or ".." segment ("/a/b/..") has no trailing slash, as with
// [path.Clean].
//
// Cleaning works on the escaped path ([url.URL.EscapedPath]), never on the
// decoded one, so an encoded slash ("%2F") is data and stays part of its
// segment: it never becomes a separator. Encoded dots ("%2e", "%2E") are
// treated as dots: "/a/%2e%2e/b" is "/b". The reason is security: a path-based
// access rule that sees "/a/%2e%2e/admin" as a harmless literal name, while the
// router or a decoding proxy resolves it to "/admin", is bypassed by exactly
// that difference. Resolving encoded dots here makes every layer agree.
//
// # Modes
//
// [Rewrite] (the default, also used for an unknown mode) serves the request with the cleaned path. The
// caller's [http.Request] is not modified: the middleware passes a shallow
// copy with its own [url.URL]. Once the handler returned, the parsed multipart form (whose temporary
// files net/http removes through the original request) and the routing pattern of the copy are
// set on the original, so the server and the wrappers around see them. URL.Path and URL.RawPath are kept consistent
// (RawPath is set only when the default encoding of Path differs from the
// cleaned escaped path) and Request.RequestURI is rewritten to the cleaned
// escaped path plus the original query, so a handler or access log that reads
// it never sees the unclean form. A request without RequestURI (one built by a
// client) keeps it empty.
//
// The trailing slash policy must match how the router registers its routes:
// with [StripSlash] in front of an [http.ServeMux] that has only a subtree
// pattern such as "/docs/", the request "/docs/" is cleaned to "/docs", which
// the mux redirects back to "/docs/": a redirect loop. Use [KeepSlash] (the
// default) unless every route is registered in the form the policy produces.
//
// [Redirect] answers 301 (GET, HEAD) or 308 (other methods) with the cleaned
// path and the original query string in Location. [Reject] answers 404 or runs
// the handler given with [WithRejectHandler].
//
// An already clean path costs nothing: no allocation, the request goes to the
// next handler untouched. CONNECT requests, the "OPTIONS *" request and URLs without a
// path (opaque ones) are never changed, and [WithSkip] can exclude more.
//
// # Security
//
// Rewrite before anything that decides on the path. A rule such as "paths under
// /admin need a role" is bypassed by "/public/../admin" at a layer that matches
// the raw string while the router resolves the dots; if this middleware is
// outermost, every layer sees "/admin".
package cleanpath

import (
	"net/http"
	"net/url"
	"strings"
)

// Mode is what the middleware does with a request whose path is not clean.
type Mode int

// The modes.
const (
	// Rewrite serves the request with the cleaned path (the default).
	Rewrite Mode = iota
	// Redirect answers with a redirect to the cleaned path.
	Redirect
	// Reject answers 404, or runs the handler given with [WithRejectHandler].
	Reject
)

// TrailingSlash is the policy for the trailing slash of a path. The root "/"
// always keeps its slash.
type TrailingSlash int

// The trailing slash policies.
const (
	// KeepSlash leaves a trailing slash as the client sent it (the default).
	KeepSlash TrailingSlash = iota
	// StripSlash removes the trailing slash.
	StripSlash
	// AddSlash adds a trailing slash.
	AddSlash
)

// Option configures [Handler].
type Option func(*config)

type config struct {
	reject   http.Handler
	skip     func(*http.Request) bool
	mode     Mode
	trailing TrailingSlash
}

// WithMode sets what happens to a request with an unclean path.
func WithMode(m Mode) Option { return func(c *config) { c.mode = m } }

// WithTrailingSlash sets the trailing slash policy.
func WithTrailingSlash(t TrailingSlash) Option { return func(c *config) { c.trailing = t } }

// WithRejectHandler sets the handler used by [Reject] instead of a plain 404.
func WithRejectHandler(h http.Handler) Option { return func(c *config) { c.reject = h } }

// WithSkip leaves the requests for which fn returns true untouched.
func WithSkip(fn func(*http.Request) bool) Option { return func(c *config) { c.skip = fn } }

// Handler returns the middleware. Use it as the outermost layer:
//
//	srv := &http.Server{Handler: cleanpath.Handler()(router)}
func Handler(opts ...Option) func(http.Handler) http.Handler {
	cfg := &config{reject: http.NotFoundHandler()}
	for _, o := range opts {
		o(cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect || r.URL.Path == "*" || r.URL.Opaque != "" ||
				(cfg.skip != nil && cfg.skip(r)) {
				next.ServeHTTP(w, r)

				return
			}
			escaped := r.URL.EscapedPath()
			if isClean(escaped, cfg.trailing) {
				next.ServeHTTP(w, r)

				return
			}
			cleaned := clean(escaped, cfg.trailing)
			switch cfg.mode {
			case Redirect:
				redirect(w, r, cleaned)
			case Reject:
				cfg.reject.ServeHTTP(w, r)
			default: // Rewrite
				serveRewritten(next, w, r, cleaned)
			}
		})
	}
}

// serveRewritten serves the request with the cleaned path. net/http removes the temporary files of an
// upload through the original request, and wrappers around this one read the routing pattern from it:
// the multipart form and the pattern of the copy are given back to it.
func serveRewritten(next http.Handler, w http.ResponseWriter, r *http.Request, cleaned string) {
	r2 := rewrite(r, cleaned)
	defer func() {
		if r2.MultipartForm != nil {
			r.MultipartForm = r2.MultipartForm
		}
		if r2.Pattern != "" {
			r.Pattern = r2.Pattern
		}
	}()
	next.ServeHTTP(w, r2)
}

func redirect(w http.ResponseWriter, r *http.Request, cleaned string) {
	target := cleaned
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		target += "?" + r.URL.RawQuery
	}
	code := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		code = http.StatusMovedPermanently
	}
	// The target starts with exactly one slash (repeated slashes are collapsed and a backslash is always
	// percent-encoded in an escaped path), so it cannot name another host.
	http.Redirect(w, r, target, code) //nolint:gosec // G710: see above, the target is a cleaned absolute path
}

// rewrite returns a shallow copy of r whose URL carries the cleaned path.
func rewrite(r *http.Request, cleaned string) *http.Request {
	u := *r.URL
	// The cleaned string only rearranges the well-formed escapes of
	// EscapedPath, so it always unescapes; the error cannot happen.
	u.Path, _ = url.PathUnescape(cleaned)
	u.RawPath = ""
	if u.EscapedPath() != cleaned {
		u.RawPath = cleaned
	}
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = &u
	if r.RequestURI != "" {
		// Keep it consistent with the URL: the cleaned escaped path and the original query.
		r2.RequestURI = u.RequestURI()
	}

	return r2
}

// isDot reports whether the escaped segment is "." (also "%2e").
func isDot(seg string) bool {
	return seg == "." || strings.EqualFold(seg, "%2e")
}

// isDotDot reports whether the escaped segment is ".." (also with "%2e").
func isDotDot(seg string) bool {
	switch len(seg) {
	case 2: //nolint:mnd // ".."
		return seg == ".."
	case 4: //nolint:mnd // ".%2e" or "%2e."
		return strings.EqualFold(seg, ".%2e") || strings.EqualFold(seg, "%2e.")
	case 6: //nolint:mnd // "%2e%2e"
		return strings.EqualFold(seg, "%2e%2e")
	default:
		return false
	}
}

// isClean reports whether the escaped path needs no change, without allocating.
func isClean(p string, ts TrailingSlash) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	if p == "/" {
		return true
	}
	trailing := p[len(p)-1] == '/'
	if (ts == StripSlash && trailing) || (ts == AddSlash && !trailing) {
		return false
	}
	for rest := p[1:]; rest != ""; {
		seg, after, _ := strings.Cut(rest, "/")
		if seg == "" || isDot(seg) || isDotDot(seg) {
			return false
		}
		rest = after
	}

	return true
}

// clean returns the cleaned escaped path.
func clean(p string, ts TrailingSlash) string {
	trailing := strings.HasSuffix(p, "/")
	segs := make([]string, 0, strings.Count(p, "/")+1)
	for rest := p; rest != ""; {
		var seg string
		seg, rest, _ = strings.Cut(rest, "/")
		switch {
		case seg == "" || isDot(seg):
		case isDotDot(seg):
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return "/"
	}
	var b strings.Builder
	b.Grow(len(p) + 1)
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(s)
	}
	if ts == AddSlash || (ts == KeepSlash && trailing) {
		b.WriteByte('/')
	}

	return b.String()
}
