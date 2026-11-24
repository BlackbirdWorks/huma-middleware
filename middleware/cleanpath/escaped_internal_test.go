package cleanpath

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEscapedPath checks escapedPath against [url.URL.EscapedPath], which it replaces without the allocation.
const (
	pathAB      = "/a/b"
	pathPercent = "/a%"
	pathSpace   = "/a b"
)

func TestEscapedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		u    url.URL
	}{
		{name: "plain", u: url.URL{Path: pathAB}},
		{name: "needs escaping", u: url.URL{Path: "/a b/é"}},
		{name: "raw matches", u: url.URL{Path: pathAB, RawPath: pathAB}},
		{name: "encoded slash", u: url.URL{Path: pathAB, RawPath: "/a%2Fb"}},
		{name: "raw lower hex", u: url.URL{Path: "/é", RawPath: "/%c3%a9"}},
		{name: "raw does not decode to the path", u: url.URL{Path: "/a", RawPath: "/b"}},
		{name: "raw shorter than the path", u: url.URL{Path: "/ab", RawPath: "/a"}},
		{name: "raw longer than the path", u: url.URL{Path: "/a", RawPath: "/ab"}},
		{name: "raw with an invalid escape", u: url.URL{Path: pathPercent, RawPath: pathPercent}},
		{name: "raw with a short escape", u: url.URL{Path: "/a", RawPath: "/a%2"}},
		{name: "raw with a non hex escape", u: url.URL{Path: "/a", RawPath: "/a%zz"}},
		{name: "raw with an invalid byte", u: url.URL{Path: pathSpace, RawPath: pathSpace}},
		{name: "raw with sub delims", u: url.URL{Path: "/a;b=c", RawPath: "/a;b=c"}},
		{name: "empty path", u: url.URL{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.u.EscapedPath(), escapedPath(&tt.u))
		})
	}
}

func FuzzEscapedPath(f *testing.F) {
	for _, s := range []string{"/", pathAB, "/a%2Fb",
		"/%c3%a9", pathSpace, "/%zz", pathPercent, "/;x",
		"/[x]",
		"/%41%2f%2F"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.ParseRequestURI(raw)
		if err != nil {
			t.Skip()
		}
		assert.Equal(t, u.EscapedPath(), escapedPath(u))
		// A RawPath that is not the one the parser made is checked too.
		v := *u
		v.RawPath = raw
		assert.Equal(t, v.EscapedPath(), escapedPath(&v))
	})
}

// TestRedirect covers the second line of defence of redirect: a target that is not a same-origin absolute path is
// handed to the reject handler. It is internal because the handler cannot produce such a path (it collapses repeated
// slashes and percent-encodes backslashes and control characters), so only a direct call reaches it.
func TestRedirect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cleaned  string
		query    string
		wantCode int
	}{
		{name: "absolute path", cleaned: "/a/b", wantCode: http.StatusMovedPermanently},
		{name: "root", cleaned: "/", wantCode: http.StatusMovedPermanently},
		{name: "double slash", cleaned: "//evil.example", wantCode: http.StatusTeapot},
		{name: "slash backslash", cleaned: "/\\evil.example", wantCode: http.StatusTeapot},
		{name: "tab", cleaned: "/\t/evil.example", wantCode: http.StatusTeapot},
		{name: "newline", cleaned: "/\n/evil.example", wantCode: http.StatusTeapot},
		{name: "delete", cleaned: "/\x7f/a", wantCode: http.StatusTeapot},
		{name: "relative", cleaned: "evil.example", wantCode: http.StatusTeapot},
		{name: "empty", cleaned: "", wantCode: http.StatusTeapot},
		{name: "query after a clean path", cleaned: "/a", query: "x=//evil", wantCode: http.StatusMovedPermanently},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reject := http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) },
			)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			req.URL.RawQuery = tt.query
			rec := httptest.NewRecorder()
			redirect(rec, req, tt.cleaned, reject)
			assert.Equal(t, tt.wantCode, rec.Code)
			if tt.wantCode == http.StatusTeapot {
				assert.Empty(t, rec.Header().Get("Location"))
			}
		})
	}
}
