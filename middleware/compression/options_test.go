package compression_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

// TestWithDefault covers the fallback configuration: minimum size, preference, levels, and a disabled fallback.
func TestWithDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "min size", run: runMinSize},
		{name: "preferences", run: runPreferences},
		{name: "levels", run: runLevels},
		{name: "no entry matches", run: runDefaultWhenNoEntryMatches},
		{name: "disabled fallback", run: runDisabledDefaultDisablesJSONToo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runMinSize(t *testing.T) {
	t.Helper()

	tests := []struct {
		name     string
		bodySize int
		minSize  int
		wantComp bool
	}{
		{name: "below default", bodySize: 1023, minSize: -1},
		{name: "at default", bodySize: 1024, minSize: -1, wantComp: true},
		{name: "custom below", bodySize: 99, minSize: 100},
		{name: "custom at", bodySize: 100, minSize: 100, wantComp: true},
		{name: "one compresses small", bodySize: 1, minSize: 1, wantComp: true},
		{name: "empty is never compressed", bodySize: 0, minSize: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []compression.Option
			if tt.minSize >= 0 {
				opts = append(opts, compression.WithDefault(compression.Config{MinSize: tt.minSize}))
			}
			payload := string(bytes.Repeat([]byte("a"), tt.bodySize))
			for _, withLength := range []bool{false, true} {
				hdr := []string{hdrCT, ctText}
				if withLength {
					hdr = append(hdr, hdrCL, strconv.Itoa(tt.bodySize))
				}
				res := serve(t, mustNew(t, opts...), http.MethodGet, []string{hdrAE, encGzip}, reply(200, hdr, payload))
				if tt.wantComp {
					assert.Equal(t, encGzip, res.Header.Get(hdrCE), "length header %v", withLength)
				} else {
					assert.Empty(t, res.Header.Get(hdrCE), "length header %v", withLength)
				}
				assert.Equal(t, payload, res.decoded(t))
			}
		})
	}
}

func runPreferences(t *testing.T) {
	t.Helper()

	tests := append(preferenceCases(), typePreferenceCases()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, tt.opts...),
				http.MethodGet,
				[]string{hdrAE, tt.ae},
				reply(200, []string{hdrCT, tt.ct}, big()),
			)
			assert.Equal(t, tt.want, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

func runLevels(t *testing.T) {
	t.Helper()

	payload := make([]byte, 0, 64*1024)
	for i := range 6000 {
		payload = strconv.AppendInt(payload, int64(i*i), 36)
		payload = append(payload, ' ')
	}
	size := func(t *testing.T, enc compression.Encoding, level int) int {
		t.Helper()
		res := serve(
			t,
			mustNew(
				t,
				levelOpt(enc, level),
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{enc}}),
			),
			http.MethodGet,
			[]string{hdrAE, string(enc)},
			reply(200, []string{hdrCT, ctText}, string(payload)),
		)
		require.Equal(t, string(enc), res.Header.Get(hdrCE))
		assert.Equal(t, string(payload), res.decoded(t))

		return len(res.Raw)
	}
	tests := []struct {
		name       string
		enc        compression.Encoding
		fast, best int
	}{
		{name: encGzip, enc: compression.Gzip, fast: 1, best: 9},
		{name: "gzip huffman only", enc: compression.Gzip, fast: -2, best: 9},
		{name: "gzip level 0 is selectable", enc: compression.Gzip, fast: 0, best: 9},
		{name: "brotli", enc: compression.Brotli, fast: 0, best: 11},
		{name: encZstd, enc: compression.Zstd, fast: 1, best: 22},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Less(t, size(t, tt.enc, tt.best), size(t, tt.enc, tt.fast))
		})
	}
}

func runDefaultWhenNoEntryMatches(t *testing.T) {
	t.Helper()

	tests := []struct {
		name string
		ct   string
		want string
		opts []compression.Option
	}{
		{name: "built-in: json prefers zstd", ct: ctJSON, want: encZstd},
		{name: "built-in: suffix json prefers zstd", ct: "application/vnd+json", want: encZstd},
		{name: "built-in: others prefer brotli", ct: ctText, want: encBrotli},
		{
			name: "default preference applies to unmatched types", ct: ctText, want: encGzip,
			opts: []compression.Option{
				compression.WithDefault(prefer(compression.Gzip)),
				compression.WithContentType(ctHTML, prefer(compression.Brotli)),
			},
		},
		{
			name: "a default preference replaces the built-in json rule", ct: ctJSON, want: encBrotli,
			opts: []compression.Option{compression.WithDefault(prefer(compression.Brotli, compression.Gzip))},
		},
		{
			name: "own json entry replaces the built-in one", ct: ctJSON, want: encGzip,
			opts: []compression.Option{compression.WithContentType(ctJSON, prefer(compression.Gzip))},
		},
		{
			name: "own suffix entry replaces the built-in one", ct: "application/x+json", want: encGzip,
			opts: []compression.Option{compression.WithContentType("+JSON", prefer(compression.Gzip))},
		},
		{
			name: "a later entry for the same type wins", ct: ctHTML, want: encZstd,
			opts: []compression.Option{
				compression.WithContentType(ctHTML, prefer(compression.Gzip)),
				compression.WithContentType(ctHTML, prefer(compression.Zstd)),
			},
		},
		{
			name: "an entry without preference inherits the default's", ct: ctHTML, want: encGzip,
			opts: []compression.Option{
				compression.WithDefault(prefer(compression.Gzip)),
				compression.WithContentType(ctHTML, compression.Config{MinSize: 10}),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, tt.opts...),
				http.MethodGet,
				[]string{hdrAE, allAE},
				reply(200, []string{hdrCT, tt.ct}, big()),
			)
			assert.Equal(t, tt.want, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

func runDisabledDefaultDisablesJSONToo(t *testing.T) {
	t.Helper()

	tests := []struct {
		name    string
		wantEnc string
		opts    []compression.Option
	}{
		{
			name: "the built-in JSON rule does not enable it",
			opts: []compression.Option{compression.WithDefault(compression.Config{Disabled: true})},
		},
		{
			name: "an entry of the caller does, with the default preference", wantEnc: encBrotli,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Disabled: true}),
				compression.WithContentType(ctJSON, compression.Config{}),
			},
		},
		{name: "enabled by default", wantEnc: encZstd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, tt.opts...),
				http.MethodGet,
				[]string{hdrAE, allAE},
				reply(200, []string{hdrCT, ctJSON}, big()),
			)
			assert.Equal(t, tt.wantEnc, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

// TestWithContentType covers the per content type configurations and their precedence.
func TestWithContentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "precedence", run: runContentTypePrecedence},
		{name: "min size and disabled", run: runContentTypeMinSizeAndDisabled},
		{name: "levels", run: runPerTypeLevels},
		{name: "pools per level", run: runPoolsPerLevelInParallel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

// runContentTypePrecedence checks that the most specific entry wins: exact, suffix, wildcard, default.
func runContentTypePrecedence(t *testing.T) {
	t.Helper()

	opts := []compression.Option{
		compression.WithDefault(prefer(compression.Brotli, compression.Gzip, compression.Zstd)),
		compression.WithContentType("*/*", prefer(compression.Zstd)),
		compression.WithContentType("text/*", prefer(compression.Gzip)),
		compression.WithContentType("application/*", prefer(compression.Brotli)),
		compression.WithContentType("+json", prefer(compression.Zstd)),
		compression.WithContentType("+problem+json", prefer(compression.Gzip)),
		compression.WithContentType("text/html", prefer(compression.Brotli)),
	}
	tests := []struct {
		name string
		ct   string
		want string
	}{
		{name: "exact beats wildcard", ct: ctHTML, want: encBrotli},
		{name: "exact with parameters and case", ct: "TEXT/HTML; charset=utf-8", want: encBrotli},
		{name: "wildcard type", ct: ctCSS, want: encGzip},
		{name: "suffix beats wildcard", ct: ctVendor, want: encZstd},
		{name: "longest suffix wins", ct: "application/x+problem+json", want: encGzip},
		{name: "longer wildcard beats the any wildcard", ct: "application/x-ndjson", want: encBrotli},
		{name: "any wildcard", ct: "font/otf", want: encZstd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, opts...),
				http.MethodGet,
				[]string{hdrAE, allAE},
				reply(200, []string{hdrCT, tt.ct}, big()),
			)
			assert.Equal(t, tt.want, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

func runContentTypeMinSizeAndDisabled(t *testing.T) {
	t.Helper()

	small := string(make([]byte, 300))
	tests := []struct {
		name     string
		ct       string
		body     string
		opts     []compression.Option
		wantComp bool
		wantVary bool
	}{
		{name: "default min size", ct: ctHTML, body: small, wantVary: true},
		{
			name: "own lower min size", ct: ctHTML, body: small, wantComp: true, wantVary: true,
			opts: []compression.Option{compression.WithContentType(ctHTML, compression.Config{MinSize: 100})},
		},
		{
			name: "own higher min size", ct: ctHTML, body: big(), wantVary: true,
			opts: []compression.Option{compression.WithContentType(ctHTML, compression.Config{MinSize: 1 << 20})},
		},
		{
			name: "other types keep the default min size", ct: ctText, body: small, wantVary: true,
			opts: []compression.Option{compression.WithContentType(ctHTML, compression.Config{MinSize: 100})},
		},
		{
			name: "disabled type", ct: ctHTML, body: big(),
			opts: []compression.Option{compression.WithContentType(ctHTML, compression.Config{Disabled: true})},
		},
		{
			name: "disabled wildcard keeps the exact entry", ct: ctHTML, body: big(), wantComp: true, wantVary: true,
			opts: []compression.Option{
				compression.WithContentType("text/*", compression.Config{Disabled: true}),
				compression.WithContentType(ctHTML, compression.Config{}),
			},
		},
		{
			name: "disabled json also disables the built-in rule", ct: ctJSON, body: big(),
			opts: []compression.Option{compression.WithContentType("+json", compression.Config{Disabled: true}),
				compression.WithContentType(ctJSON, compression.Config{Disabled: true})},
		},
		{
			name: "default disabled with an enabled type", ct: ctHTML, body: big(), wantComp: true, wantVary: true,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Disabled: true}),
				compression.WithContentType(ctHTML, compression.Config{}),
			},
		},
		{
			name: "default disabled", ct: ctText, body: big(),
			opts: []compression.Option{compression.WithDefault(compression.Config{Disabled: true})},
		},
		{
			name: "an entry makes a type compressible", ct: ctOctet, body: big(), wantComp: true, wantVary: true,
			opts: []compression.Option{compression.WithContentType(ctOctet, compression.Config{})},
		},
		{
			name: "an entry never overrides already compressed formats", ct: ctPNG, body: big(),
			opts: []compression.Option{compression.WithContentType("*/*", compression.Config{})},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, tt.opts...),
				http.MethodGet,
				[]string{hdrAE, encGzip},
				reply(200, []string{hdrCT, tt.ct}, tt.body),
			)
			if tt.wantComp {
				assert.Equal(t, encGzip, res.Header.Get(hdrCE))
			} else {
				assert.Empty(t, res.Header.Get(hdrCE))
			}
			if tt.wantVary {
				assert.Equal(t, []string{varyAE}, res.Header.Values(hdrVary))
			} else {
				assert.Empty(t, res.Header.Values(hdrVary))
			}
			assert.Equal(t, tt.body, res.decoded(t))
		})
	}
}

func runPerTypeLevels(t *testing.T) {
	t.Helper()

	payload := make([]byte, 0, 64*1024)
	for i := range 6000 {
		payload = strconv.AppendInt(payload, int64(i*i), 36)
		payload = append(payload, ' ')
	}
	sizeFor := func(t *testing.T, mw compression.Middleware, ct, enc string) int {
		t.Helper()
		res := serve(t, mw, http.MethodGet, []string{hdrAE, enc}, reply(200, []string{hdrCT, ct}, string(payload)))
		require.Equal(t, enc, res.Header.Get(hdrCE))
		assert.Equal(t, string(payload), res.decoded(t))

		return len(res.Raw)
	}
	mw := mustNew(t,
		compression.WithDefault(compression.Config{Levels: compression.Levels{
			Gzip: new(1), Brotli: new(0), Zstd: new(1),
		}}),
		compression.WithContentType(ctHTML, compression.Config{Levels: compression.Levels{
			Gzip: new(9), Brotli: new(11), Zstd: new(22),
		}}),
	)
	tests := []struct {
		name string
		enc  string
	}{
		{name: "gzip", enc: encGzip},
		{name: "brotli", enc: encBrotli},
		{name: "zstd", enc: encZstd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Less(t, sizeFor(t, mw, ctHTML, tt.enc), sizeFor(t, mw, ctText, tt.enc),
				"the entry's level compresses better than the default's")
		})
	}
}

// runPoolsPerLevelInParallel runs two levels of one encoding side by side.
func runPoolsPerLevelInParallel(t *testing.T) {
	t.Helper()

	mw := mustNew(
		t,
		compression.WithDefault(compression.Config{Levels: compression.Levels{Gzip: new(1)}}),
		compression.WithContentType(
			ctHTML,
			compression.Config{Levels: compression.Levels{Gzip: new(9)}},
		),
	)
	tests := []struct {
		name string
		ct   string
	}{
		{name: "default level", ct: ctText},
		{name: "own level", ct: ctHTML},
		{name: "default level again", ct: ctText},
		{name: "own level again", ct: ctHTML},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for range 20 {
				res := serve(t, mw, http.MethodGet, []string{hdrAE, encGzip}, reply(200, []string{hdrCT, tt.ct}, big()))
				assert.Equal(t, big(), res.decoded(t))
			}
		})
	}
}

// TestWithContentTypes covers the allow-list of compressible content types.
func TestWithContentTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "allow-list", run: runContentTypes},
		{name: "event streams", run: runStreamsAreOnlyMatchedByName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runContentTypes(t *testing.T) {
	t.Helper()

	tests := []struct {
		name     string
		ct       string
		opts     []compression.Option
		wantComp bool
	}{
		{name: "json", ct: ctJSON, wantComp: true},
		{name: "json with params", ct: "Application/JSON; charset=utf-8", wantComp: true},
		{name: "vendor json", ct: ctVendor, wantComp: true},
		{name: "html", ct: ctHTML, wantComp: true},
		{name: "css", ct: ctCSS, wantComp: true},
		{name: "javascript", ct: ctJS, wantComp: true},
		{name: "xml", ct: "application/xml", wantComp: true},
		{name: "svg", ct: "image/svg+xml", wantComp: true},
		{name: "ndjson", ct: "application/x-ndjson", wantComp: true},
		{name: "wasm", ct: "application/wasm", wantComp: true},
		{name: "event stream default", ct: ctSSE},
		{name: "png", ct: ctPNG},
		{name: "jpeg", ct: "image/jpeg"},
		{name: "mp4", ct: "video/mp4"},
		{name: "mp3", ct: "audio/mpeg"},
		{name: "zip", ct: ctZip},
		{name: "gzip type", ct: "application/gzip"},
		{name: "pdf", ct: "application/pdf"},
		{name: "woff2", ct: "font/woff2"},
		{name: "octet stream", ct: ctOctet},
		{
			name:     "bmp is compressible",
			ct:       "image/bmp",
			opts:     []compression.Option{compression.WithContentTypes("image/*")},
			wantComp: true,
		},
		{
			name: "custom list excludes json",
			ct:   ctJSON,
			opts: []compression.Option{compression.WithContentTypes(ctHTML)},
		},
		{
			name:     "custom exact",
			ct:       ctOctet,
			opts:     []compression.Option{compression.WithContentTypes("Application/Octet-Stream")},
			wantComp: true,
		},
		{
			name:     "custom wildcard",
			ct:       ctOctet,
			opts:     []compression.Option{compression.WithContentTypes("*/*")},
			wantComp: true,
		},
		{
			name: "custom wildcard still skips png",
			ct:   ctPNG,
			opts: []compression.Option{compression.WithContentTypes("*/*")},
		},
		{
			name:     "custom suffix",
			ct:       "application/atom+xml",
			opts:     []compression.Option{compression.WithContentTypes("+xml")},
			wantComp: true,
		},
		{
			name:     "event stream opt in",
			ct:       ctSSE,
			opts:     []compression.Option{compression.WithContentTypes(ctSSE)},
			wantComp: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := serve(
				t,
				mustNew(t, tt.opts...),
				http.MethodGet,
				[]string{hdrAE, encGzip},
				reply(200, []string{hdrCT, tt.ct}, big()),
			)
			if tt.wantComp {
				assert.Equal(t, encGzip, res.Header.Get(hdrCE))
			} else {
				assert.Empty(t, res.Header.Get(hdrCE))
				assert.Empty(t, res.Header.Values(hdrVary))
			}
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

// runStreamsAreOnlyMatchedByName checks that no wildcard of an allow-list or of an entry covers server-sent events.
func runStreamsAreOnlyMatchedByName(t *testing.T) {
	t.Helper()

	tests := []struct {
		name     string
		opts     []compression.Option
		wantComp bool
	}{
		{name: "the defaults"},
		{name: "text wildcard", opts: []compression.Option{compression.WithContentTypes("text/*")}},
		{name: "any wildcard", opts: []compression.Option{compression.WithContentTypes("*/*")}},
		{
			name: "a wildcard entry",
			opts: []compression.Option{compression.WithContentType("*/*", compression.Config{})},
		},
		{name: "the exact name", opts: []compression.Option{compression.WithContentTypes(ctSSE)}, wantComp: true},
		{
			name:     "an exact entry",
			opts:     []compression.Option{compression.WithContentType(ctSSE, compression.Config{})},
			wantComp: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := append([]compression.Option{compression.WithDefault(compression.Config{MinSize: 1})}, tt.opts...)
			res := serve(
				t,
				mustNew(t, opts...),
				http.MethodGet,
				[]string{hdrAE, encGzip},
				reply(200, []string{hdrCT, ctSSE}, big()),
			)
			assert.Equal(t, tt.wantComp, res.Header.Get(hdrCE) != "")
		})
	}
}

// TestWithSkip covers the requests that are left uncompressed.
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
		name     string
		target   string
		opts     []compression.Option
		wantSkip bool
	}{
		{name: "default compresses", target: pathAPI},
		{
			name:     "prefix skipped",
			target:   "/metrics/x",
			opts:     []compression.Option{compression.WithSkipPathPrefixes("/metrics")},
			wantSkip: true,
		},
		{
			name:   "prefix other path",
			target: pathAPI,
			opts:   []compression.Option{compression.WithSkipPathPrefixes("/metrics")},
		},
		{
			name:     "pattern skipped",
			target:   "/files/a.txt",
			opts:     []compression.Option{compression.WithSkipPathPatterns("/files/*.txt")},
			wantSkip: true,
		},
		{
			name:   "pattern does not cross slash",
			target: "/files/a/b.txt",
			opts:   []compression.Option{compression.WithSkipPathPatterns("/files/*.txt")},
		},
		{
			name:   "only prefix matches",
			target: pathAPI,
			opts:   []compression.Option{compression.WithOnlyPathPrefixes("/api")},
		},
		{
			name:     "only prefix excludes others",
			target:   "/other",
			opts:     []compression.Option{compression.WithOnlyPathPrefixes("/api")},
			wantSkip: true,
		},
		{
			name:     "operation skipped",
			target:   "/x",
			opts:     []compression.Option{compression.WithSkipOperations(opID)},
			wantSkip: true,
		},
		{name: "other operation", target: "/x", opts: []compression.Option{compression.WithSkipOperations("nope")}},
		{
			name: "skip function true", target: "/x", wantSkip: true,
			opts: []compression.Option{compression.WithSkip(func(huma.Context) bool { return true })},
		},
		{
			name: "skip function false", target: "/x",
			opts: []compression.Option{compression.WithSkip(func(huma.Context) bool { return false })},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			serveRec(mustNew(t, tt.opts...), http.MethodGet, tt.target, []string{hdrAE, encGzip}, rec,
				reply(200, []string{hdrCT, ctJSON}, big()))
			res := resultOf(t, rec)
			if tt.wantSkip {
				assert.Empty(t, res.Header.Get(hdrCE))
				assert.Empty(t, res.Header.Values(hdrVary), "a skipped request is untouched")
			} else {
				assert.Equal(t, encGzip, res.Header.Get(hdrCE))
			}
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

// compressedAt reports whether a JSON response at the path is compressed with the options given.
func compressedAt(t *testing.T, target string, opts ...compression.Option) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	serveRec(mustNew(t, opts...), http.MethodGet, target, []string{hdrAE, encGzip}, rec,
		reply(http.StatusOK, []string{hdrCT, ctJSON}, big()))

	return resultOf(t, rec).Header.Get(hdrCE) != ""
}

// TestWithSkipPathPrefixes covers the path prefixes whose requests are left uncompressed.
func TestWithSkipPathPrefixes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    string
		prefixes  []string
		wantCompr bool
	}{
		{name: "prefix", target: "/metrics/x", prefixes: []string{pathMetrics}},
		{name: "other path", target: "/api/data", prefixes: []string{pathMetrics}, wantCompr: true},
		{name: "second prefix", target: "/debug/x", prefixes: []string{pathMetrics, "/debug"}},
		{name: "no prefix", target: pathMetrics, wantCompr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantCompr, compressedAt(t, tt.target, compression.WithSkipPathPrefixes(tt.prefixes...)))
		})
	}
}

// TestWithSkipPathPatterns covers the path patterns whose requests are left uncompressed.
func TestWithSkipPathPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    string
		patterns  []string
		wantCompr bool
	}{
		{name: "pattern", target: "/files/a.txt", patterns: []string{pathTxtPattern}},
		{
			name:      "star does not cross a slash",
			target:    "/files/a/b.txt",
			patterns:  []string{pathTxtPattern},
			wantCompr: true,
		},
		{name: "other pattern", target: "/files/a.json", patterns: []string{pathTxtPattern}, wantCompr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantCompr, compressedAt(t, tt.target, compression.WithSkipPathPatterns(tt.patterns...)))
		})
	}
}

// TestWithOnlyPathPrefixes covers the path prefixes that are the only ones compressed.
func TestWithOnlyPathPrefixes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    string
		prefixes  []string
		wantCompr bool
	}{
		{name: "inside", target: "/api/data", prefixes: []string{pathAPIPrefix}, wantCompr: true},
		{name: "outside", target: "/other", prefixes: []string{pathAPIPrefix}},
		{name: "second prefix", target: "/v2/data", prefixes: []string{pathAPIPrefix, "/v2"}, wantCompr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantCompr, compressedAt(t, tt.target, compression.WithOnlyPathPrefixes(tt.prefixes...)))
		})
	}
}

// TestWithSkipOperations covers the operation IDs whose requests are left uncompressed.
func TestWithSkipOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ids       []string
		wantCompr bool
	}{
		{name: "operation", ids: []string{opID}},
		{name: "other operation", ids: []string{"nope"}, wantCompr: true},
		{name: "one of several", ids: []string{"nope", opID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.wantCompr, compressedAt(t, "/x", compression.WithSkipOperations(tt.ids...)))
		})
	}
}
