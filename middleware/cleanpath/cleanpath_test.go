package cleanpath_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/internal/racedetect"
	"github.com/blackbirdworks/huma-middleware/middleware/cleanpath"
)

const (
	pathDotInside = "/a/./b"
	pathAB        = "/a/b"
	pathEncLead   = "/%2Fa"
	pathDouble    = "/a//b"
	pathABC       = "/a/b/c"
	pathDotDot    = "/a/../b"
	pathATrail    = "/a/"
	pathABTrail   = "/a/b/"
	pathAdmin     = "/admin"
	pathEncMid    = "/a%2Fb/c"
	pathEncDots   = "/a%2F..%2Fb"
)

// seen is what the wrapped handler observed.
type seen struct {
	path, raw, escaped, requestURI string
	called                         bool
}

func run(
	t *testing.T,
	target string,
	method string,
	opts ...cleanpath.Option,
) (*httptest.ResponseRecorder, seen, *http.Request) {
	t.Helper()
	var got seen
	h := cleanpath.Handler(opts...)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = seen{r.URL.Path, r.URL.RawPath, r.URL.EscapedPath(), r.RequestURI, true}
	}))
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec, got, req
}

func FuzzClean(f *testing.F) {
	for _, s := range []string{
		"/", "//", "/a//b", "/a/../b", "/..", "/a/%2e%2e/b", "/a%2F..%2Fb", "/a/.", "/a/b/",
		"/%2e", "/.%2E/x", "/a b", "/caf%C3%A9",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.ParseRequestURI(raw)
		if err != nil || u.Path == "*" || u.Opaque != "" {
			t.Skip()
		}
		for _, ts := range []cleanpath.TrailingSlash{cleanpath.KeepSlash, cleanpath.StripSlash, cleanpath.AddSlash} {
			clean := func(in *url.URL) (*url.URL, string) {
				var out *url.URL
				var requestURI string
				h := cleanpath.Handler(
					cleanpath.WithTrailingSlash(ts),
				)(
					http.HandlerFunc(
						func(_ http.ResponseWriter, r *http.Request) { out, requestURI = r.URL, r.RequestURI },
					),
				)
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
				c := *in
				req.URL = &c
				req.RequestURI = in.RequestURI()
				h.ServeHTTP(httptest.NewRecorder(), req)

				return out, requestURI
			}
			once, onceURI := clean(u)
			twice, _ := clean(once)
			e := once.EscapedPath()
			assert.Equal(t, e, twice.EscapedPath(), "idempotent")
			assert.Equal(t, once.Path, twice.Path)
			pathPart, _, _ := strings.Cut(onceURI, "?")
			assert.Equal(t, once.EscapedPath(), pathPart, "RequestURI is consistent with the cleaned path")
			assert.True(t, strings.HasPrefix(e, "/"))
			assert.NotContains(t, e, "//")
			lower := strings.ToLower(e)
			for seg := range strings.SplitSeq(lower, "/") {
				assert.NotContains(t, []string{".", "..", "%2e", ".%2e", "%2e.", "%2e%2e"}, seg)
			}
			assert.NotContains(t, e+"/", "/./")
			assert.NotContains(t, e+"/", "/../")
		}
	})
}

// TestHandler covers cleaning, the real routers behind it and what the rewritten copy gives back.
func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "rewrite", run: runRewriteKeepSlash},
		{name: "empty and relative", run: runEmptyAndRelativePaths},
		{name: "real routers", run: runInFrontOfRealRouters},
		{name: "multipart and pattern given back", run: runRewriteGivesBackWhatItsCopyLearned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runRewriteKeepSlash(t *testing.T) {
	t.Helper()

	tests := []struct {
		name        string
		target      string
		wantEscaped string
		wantPath    string
		wantRaw     string
	}{
		{name: "clean", target: pathAB, wantEscaped: pathAB, wantPath: pathAB},
		{name: "root", target: "/", wantEscaped: "/", wantPath: "/"},
		{
			name:        "leading double slash",
			target:      pathEncLead,
			wantEscaped: pathEncLead,
			wantPath:    "//a",
			wantRaw:     pathEncLead,
		},
		{name: "double slash", target: pathDouble, wantEscaped: pathAB, wantPath: pathAB},
		{name: "many slashes", target: "/a////b///c", wantEscaped: pathABC, wantPath: pathABC},
		{name: "dot", target: "/./a", wantEscaped: "/a", wantPath: "/a"},
		{name: "dot middle", target: pathDotInside, wantEscaped: pathAB, wantPath: pathAB},
		{name: "dotdot", target: pathDotDot, wantEscaped: "/b", wantPath: "/b"},
		{name: "dotdot above root", target: "/..", wantEscaped: "/", wantPath: "/"},
		{name: "dotdot many above root", target: "/../../a", wantEscaped: "/a", wantPath: "/a"},
		{name: "trailing slash kept", target: pathATrail, wantEscaped: pathATrail, wantPath: pathATrail},
		{name: "trailing slash after clean", target: "/a//b/", wantEscaped: pathABTrail, wantPath: pathABTrail},
		{name: "trailing dotdot has no slash", target: "/a/b/..", wantEscaped: "/a", wantPath: "/a"},
		{name: "trailing dot has no slash", target: "/a/.", wantEscaped: "/a", wantPath: "/a"},
		{name: "dotdot then slash", target: "/a/b/../", wantEscaped: pathATrail, wantPath: pathATrail},
		{name: "admin bypass", target: "/public/../admin", wantEscaped: pathAdmin, wantPath: pathAdmin},
		{name: "encoded dots", target: "/a/%2e%2e/b", wantEscaped: "/b", wantPath: "/b"},
		{name: "encoded dots upper", target: "/a/%2E%2E/b", wantEscaped: "/b", wantPath: "/b"},
		{name: "encoded half dots", target: "/a/.%2e/b/%2e./c", wantEscaped: "/c", wantPath: "/c"},
		{name: "encoded dot", target: "/a/%2e/b", wantEscaped: pathAB, wantPath: pathAB},
		{
			name:        "encoded slash stays",
			target:      "/a%2Fb//c",
			wantEscaped: pathEncMid,
			wantPath:    pathABC,
			wantRaw:     pathEncMid,
		},
		{
			name:        "encoded slash dotdot not separator",
			target:      pathEncDots,
			wantEscaped: pathEncDots,
			wantPath:    pathDotDot,
			wantRaw:     pathEncDots,
		},
		{name: "encoded chars kept", target: "/caf%C3%A9//x", wantEscaped: "/caf%C3%A9/x", wantPath: "/café/x"},
		{name: "query preserved", target: "/a//b?x=1&y=%2F", wantEscaped: pathAB, wantPath: pathAB},
		{
			name:        "long",
			target:      "/" + strings.Repeat("a//", 2000),
			wantEscaped: "/" + strings.Repeat("a/", 1999) + "a/",
			wantPath:    "/" + strings.Repeat("a/", 1999) + "a/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, got, req := run(t, tt.target, http.MethodGet)
			require.True(t, got.called)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tt.wantEscaped, got.escaped)
			assert.Equal(t, tt.wantPath, got.path)
			assert.Equal(t, tt.wantRaw, got.raw)
			wantURI := tt.wantEscaped
			if _, query, found := strings.Cut(tt.target, "?"); found {
				wantURI += "?" + query
			}
			assert.Equal(t, wantURI, got.requestURI, "RequestURI is the cleaned escaped path and the original query")
			assert.Equal(t, tt.target, req.RequestURI, "the caller's RequestURI is not mutated")
			orig, err := url.ParseRequestURI(tt.target)
			require.NoError(t, err)
			assert.Equal(t, orig.Path, req.URL.Path, "the caller's request is not mutated")
			assert.Equal(t, orig.RawPath, req.URL.RawPath)
		})
	}
}

func runEmptyAndRelativePaths(t *testing.T) {
	t.Helper()

	tests := []struct {
		name string
		want string
		url  url.URL
	}{
		{name: "empty", url: url.URL{}, want: "/"},
		{name: "relative", url: url.URL{Path: "a/b"}, want: pathAB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			h := cleanpath.Handler()(
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.URL.Path }),
			)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
			u := tt.url
			req.URL = &u
			h.ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tt.want, got)
		})
	}
}

func runInFrontOfRealRouters(t *testing.T) {
	t.Helper()

	type out struct {
		Body struct {
			Who string `json:"who"`
		}
	}
	build := map[string]func() http.Handler{
		"servemux+huma": func() http.Handler {
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("t", "1"))
			huma.Get(api, "/public", func(context.Context, *struct{}) (*out, error) { return &out{}, nil })
			huma.Get(api, pathAdmin, func(context.Context, *struct{}) (*out, error) {
				o := &out{}
				o.Body.Who = "admin"

				return o, nil
			})

			return mux
		},
		"chi": func() http.Handler {
			r := chi.NewRouter()
			r.Get(pathAdmin, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"who":"admin"}`)) })

			return r
		},
	}
	for name, mk := range build {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := cleanpath.Handler()(mk())
			for _, target := range []string{pathAdmin, "/public/../admin", "//admin", "/./admin", "/x/%2e%2e/admin"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(
					rec,
					httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody),
				)
				assert.Equal(t, http.StatusOK, rec.Code, target)
				assert.Contains(t, rec.Body.String(), "admin", target)
			}
		})
	}
}

// runRewriteGivesBackWhatItsCopyLearned checks the multipart form (whose temporary files net/http removes
// through the original request) and the routing pattern (read by the wrappers around).
func runRewriteGivesBackWhatItsCopyLearned(t *testing.T) {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	var seen *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen = r
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		assert.NoError(t, r.ParseMultipartForm(1)) //nolint:gosec // G120: the test body is a few bytes
	})
	h := cleanpath.Handler()(mux)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "//upload//7", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(httptest.NewRecorder(), req)

	require.NotNil(t, seen)
	require.NotNil(t, req.MultipartForm, "the original request holds the form, so the server removes its files")
	assert.Same(t, seen.MultipartForm, req.MultipartForm)
	assert.Equal(t, "POST /upload/{id}", req.Pattern)
	assert.Equal(t, "//upload//7", req.RequestURI, "the original request is otherwise untouched")
}

// TestWithMode covers the redirect and reject modes.
func TestWithMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "redirect", run: runRedirect},
		{name: "reject", run: runReject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runRedirect(t *testing.T) {
	t.Helper()

	tests := []struct {
		name         string
		method       string
		target       string
		wantLocation string
		wantCode     int
		wantCalled   bool
	}{
		{
			name:         "get",
			method:       http.MethodGet,
			target:       pathDouble,
			wantCode:     http.StatusMovedPermanently,
			wantLocation: pathAB,
		},
		{
			name:         "head",
			method:       http.MethodHead,
			target:       pathDotDot,
			wantCode:     http.StatusMovedPermanently,
			wantLocation: "/b",
		},
		{
			name:         "post",
			method:       http.MethodPost,
			target:       pathDouble,
			wantCode:     http.StatusPermanentRedirect,
			wantLocation: pathAB,
		},
		{
			name:         "delete",
			method:       http.MethodDelete,
			target:       pathDotInside,
			wantCode:     http.StatusPermanentRedirect,
			wantLocation: pathAB,
		},
		{
			name:         "query kept",
			method:       http.MethodGet,
			target:       "/a//b?x=1&y=%2F",
			wantCode:     http.StatusMovedPermanently,
			wantLocation: "/a/b?x=1&y=%2F",
		},
		{
			name:         "empty query kept",
			method:       http.MethodGet,
			target:       "/a//b?",
			wantCode:     http.StatusMovedPermanently,
			wantLocation: "/a/b?",
		},
		{
			name:         "encoded slash kept",
			method:       http.MethodGet,
			target:       "/a%2Fb//c",
			wantCode:     http.StatusMovedPermanently,
			wantLocation: pathEncMid,
		},
		{
			name:         "no open redirect",
			method:       http.MethodGet,
			target:       "//evil.com//x",
			wantCode:     http.StatusMovedPermanently,
			wantLocation: "/evil.com/x",
		},
		{
			name:         "backslash stays encoded",
			method:       http.MethodGet,
			target:       "/%5Cevil.com//x",
			wantCode:     http.StatusMovedPermanently,
			wantLocation: "/%5Cevil.com/x",
		},
		{name: "clean passes", method: http.MethodGet, target: pathAB, wantCode: http.StatusOK, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, got, _ := run(t, tt.target, tt.method, cleanpath.WithMode(cleanpath.Redirect))
			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Equal(t, tt.wantLocation, rec.Header().Get("Location"))
			assert.Equal(t, tt.wantCalled, got.called)
		})
	}
}

func runReject(t *testing.T) {
	t.Helper()

	custom := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	tests := []struct {
		name       string
		target     string
		opts       []cleanpath.Option
		wantCode   int
		wantCalled bool
	}{
		{
			name:     "default 404",
			opts:     []cleanpath.Option{cleanpath.WithMode(cleanpath.Reject)},
			target:   pathDouble,
			wantCode: http.StatusNotFound,
		},
		{
			name:     "custom handler",
			opts:     []cleanpath.Option{cleanpath.WithMode(cleanpath.Reject), cleanpath.WithRejectHandler(custom)},
			target:   pathDotDot,
			wantCode: http.StatusBadRequest,
		},
		{
			name: "clean passes", opts: []cleanpath.Option{cleanpath.WithMode(cleanpath.Reject)},
			target: pathAB, wantCode: http.StatusOK, wantCalled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, got, _ := run(t, tt.target, http.MethodGet, tt.opts...)
			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Equal(t, tt.wantCalled, got.called)
		})
	}
}

// TestWithTrailingSlash covers the trailing slash policies.
func TestWithTrailingSlash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "policies", run: runTrailingSlashPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runTrailingSlashPolicy(t *testing.T) {
	t.Helper()

	tests := []struct {
		name   string
		target string
		want   string
		policy cleanpath.TrailingSlash
	}{
		{name: "keep with", policy: cleanpath.KeepSlash, target: pathATrail, want: pathATrail},
		{name: "keep without", policy: cleanpath.KeepSlash, target: "/a", want: "/a"},
		{name: "strip with", policy: cleanpath.StripSlash, target: pathATrail, want: "/a"},
		{name: "strip without", policy: cleanpath.StripSlash, target: "/a", want: "/a"},
		{name: "strip nested", policy: cleanpath.StripSlash, target: "/a//b/", want: pathAB},
		{name: "strip root", policy: cleanpath.StripSlash, target: "/", want: "/"},
		{name: "strip root doubled", policy: cleanpath.StripSlash, target: "//", want: "/"},
		{name: "add without", policy: cleanpath.AddSlash, target: "/a", want: pathATrail},
		{name: "add with", policy: cleanpath.AddSlash, target: pathATrail, want: pathATrail},
		{name: "add root", policy: cleanpath.AddSlash, target: "/", want: "/"},
		{name: "add after dotdot", policy: cleanpath.AddSlash, target: "/a/b/..", want: pathATrail},
		{name: "add to root dotdot", policy: cleanpath.AddSlash, target: "/a/..", want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got, _ := run(t, tt.target, http.MethodGet, cleanpath.WithTrailingSlash(tt.policy))
			assert.Equal(t, tt.want, got.escaped)
		})
	}
}

// TestWithSkip covers the requests that are left alone.
func TestWithSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "skips", run: runSkips},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runSkips(t *testing.T) {
	t.Helper()

	tests := []struct {
		name   string
		method string
		target string
		opaque string
		want   string
		opts   []cleanpath.Option
	}{
		{name: "connect", method: http.MethodConnect, target: pathDouble, want: pathDouble},
		{name: "options star", method: http.MethodOptions, target: "*", want: "*"},
		{name: "opaque url", method: http.MethodGet, target: "", opaque: "0", want: ""},
		{
			name:   "predicate",
			method: http.MethodGet,
			target: "/skip//me",
			opts: []cleanpath.Option{
				cleanpath.WithSkip(func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/skip") }),
			},
			want: "/skip//me",
		},
		{
			name: "predicate false", method: http.MethodGet, target: "/other//me",
			opts: []cleanpath.Option{cleanpath.WithSkip(func(*http.Request) bool { return false })},
			want: "/other/me",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			h := cleanpath.Handler(
				tt.opts...)(
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.URL.Path }),
			)
			req := httptest.NewRequestWithContext(context.Background(), tt.method, "/", http.NoBody)
			req.URL = &url.URL{Path: tt.target, Opaque: tt.opaque}
			h.ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAllocs pins what the middleware allocates.
func TestAllocs(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in parallel tests
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "clean paths", run: runNoAllocationsOnCleanPaths},
		{name: "rewrite", run: runRewriteAllocations},
	}
	for _, tt := range tests { //nolint:paralleltest // AllocsPerRun panics in parallel tests
		t.Run(tt.name, tt.run)
	}
}

// runNoAllocationsOnCleanPaths is sequential on purpose: [testing.AllocsPerRun] panics in parallel tests and
// would count the allocations of other goroutines.
func runNoAllocationsOnCleanPaths(t *testing.T) {
	t.Helper()
	if racedetect.Enabled {
		t.Skip("allocation counts mean nothing under the race detector; see task allocs")
	}
	tests := []struct {
		name       string
		target     string
		opts       []cleanpath.Option
		wantAllocs float64
	}{
		{name: "plain", target: pathABC},
		{name: "root", target: "/"},
		{name: "trailing", target: pathABTrail},
		{name: "encoded", target: "/caf%C3%A9/a%2Fb"},
		{name: "strip", target: pathAB, opts: []cleanpath.Option{cleanpath.WithTrailingSlash(cleanpath.StripSlash)}},
		{name: "add", target: pathABTrail, opts: []cleanpath.Option{cleanpath.WithTrailingSlash(cleanpath.AddSlash)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := cleanpath.Handler(tt.opts...)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.target, http.NoBody)
			rec := httptest.NewRecorder()
			assert.InDelta(t, tt.wantAllocs, testing.AllocsPerRun(100, func() { h.ServeHTTP(rec, req) }), 0)
		})
	}
}

// runRewriteAllocations pins the cost of a rewrite: the request with its URL (one object) and the cleaned path.
// It is sequential: [testing.AllocsPerRun] panics in parallel tests.
func runRewriteAllocations(t *testing.T) {
	t.Helper()
	if racedetect.Enabled {
		t.Skip("allocation counts mean nothing under the race detector; see task allocs")
	}
	tests := []struct {
		name   string
		target string
		want   float64
	}{
		{name: "double slash", target: "//a/b", want: 2},
		{name: "dot segment", target: pathDotInside, want: 2},
		{name: "dotdot", target: "/a/../b", want: 2},
		{name: "with a query", target: "//a/b?x=1", want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := cleanpath.Handler()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.target, http.NoBody)
			rec := httptest.NewRecorder()
			assert.InDelta(t, tt.want, testing.AllocsPerRun(100, func() { h.ServeHTTP(rec, req) }), 0)
		})
	}
}

// TestWithRejectHandler covers the handler that answers a rejected request.
func TestWithRejectHandler(t *testing.T) {
	t.Parallel()

	teapot := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	rejecting := []cleanpath.Option{cleanpath.WithMode(cleanpath.Reject), cleanpath.WithRejectHandler(teapot)}
	tests := []struct {
		name       string
		target     string
		opts       []cleanpath.Option
		wantStatus int
	}{
		{name: "custom handler", opts: rejecting, target: pathDouble, wantStatus: http.StatusTeapot},
		{name: "clean path passes", opts: rejecting, target: "/a/b", wantStatus: http.StatusOK},
		{
			name: "unused when rewriting", opts: []cleanpath.Option{cleanpath.WithRejectHandler(teapot)},
			target: pathDouble, wantStatus: http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, _, _ := run(t, tt.target, http.MethodGet, tt.opts...)
			assert.Equal(t, tt.wantStatus, rec.Code)
		})
	}
}
