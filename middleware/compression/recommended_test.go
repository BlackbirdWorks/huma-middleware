package compression_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func TestRecommendedValues(t *testing.T) {
	t.Parallel()

	cfg := compression.Recommended()
	require.NotNil(t, cfg.Levels.Gzip)
	require.NotNil(t, cfg.Levels.Brotli)
	require.NotNil(t, cfg.Levels.Zstd)
	assert.Equal(t, 6, *cfg.Levels.Gzip)
	assert.Equal(t, 5, *cfg.Levels.Brotli)
	assert.Equal(t, 3, *cfg.Levels.Zstd)
	assert.Equal(t, []compression.Encoding{compression.Brotli, compression.Gzip, compression.Zstd}, cfg.Preference)
	assert.Equal(t, 1024, cfg.MinSize)
	assert.False(t, cfg.Disabled)

	types := compression.RecommendedContentTypes()
	jsonPref := []compression.Encoding{compression.Zstd, compression.Brotli, compression.Gzip}
	assert.Equal(t, map[string]compression.Config{
		ctJSON:  {Preference: jsonPref},
		"+json": {Preference: jsonPref},
	}, types)
}

func TestRecommendedAllowList(t *testing.T) {
	t.Parallel()

	list := compression.RecommendedAllowList()
	for _, want := range []string{"text/*", "+json", "+xml", ctJSON, ctJS} {
		assert.Contains(t, list, want)
	}
	for _, never := range []string{"image/*", "video/*", "audio/*", ctZip, "text/event-stream", "*/*"} {
		assert.NotContains(t, list, never)
	}
}

func TestRecommendationsAreFreshCopies(t *testing.T) {
	t.Parallel()

	cfg := compression.Recommended()
	*cfg.Levels.Gzip = 1
	cfg.Preference[0] = compression.Zstd
	cfg.MinSize = 1
	assert.Equal(t, 1, cfg.MinSize)
	again := compression.Recommended()
	assert.Equal(t, 6, *again.Levels.Gzip)
	assert.Equal(t, compression.Brotli, again.Preference[0])
	assert.Equal(t, 1024, again.MinSize)

	types := compression.RecommendedContentTypes()
	types[ctJSON].Preference[0] = compression.Gzip
	delete(types, "+json")
	assert.Equal(t, compression.Zstd, compression.RecommendedContentTypes()[ctJSON].Preference[0])
	assert.Contains(t, compression.RecommendedContentTypes(), "+json")

	list := compression.RecommendedAllowList()
	list[0] = "changed"
	assert.Equal(t, "text/*", compression.RecommendedAllowList()[0])
}

// recommendedMiddlewares returns the three ways to spell the recommendation.
func recommendedMiddlewares(t *testing.T) map[string]compression.Middleware {
	t.Helper()
	spelled := make([]compression.Option, 0, 2+len(compression.RecommendedContentTypes()))
	spelled = append(spelled, compression.WithFallback(compression.Recommended()))
	for pattern, cfg := range compression.RecommendedContentTypes() {
		spelled = append(spelled, compression.WithContentType(pattern, cfg))
	}
	spelled = append(spelled, compression.WithContentTypes(compression.RecommendedAllowList()...))

	return map[string]compression.Middleware{
		"no option":    mustNew(t),
		"WithDefaults": mustNew(t, compression.WithDefaults()),
		"spelled out":  mustNew(t, spelled...),
		"WithDefaults and the same again": mustNew(
			t,
			compression.WithDefaults(),
			compression.WithFallback(compression.Recommended()),
		),
	}
}

// TestNewIsTheRecommendation checks that every spelling of the recommendation answers alike for a table of content
// types, Accept-Encoding values and body sizes.
func TestNewIsTheRecommendation(t *testing.T) {
	t.Parallel()

	types := []string{
		ctJSON, "application/problem+json", ctText, ctHTML, ctCSS, ctJS, "image/svg+xml",
		"application/xml", "application/atom+xml", ctSSE, ctPNG, ctOctet, ctZip, "",
	}
	encodings := []string{"", encGzip, encBrotli, encZstd, allAE, "br;q=0.1, gzip", "zstd;q=0, *", encIdent}
	sizes := []int{0, 100, 1023, 1024, 20000}
	mws := recommendedMiddlewares(t)
	for _, ct := range types {
		for _, ae := range encodings {
			for _, size := range sizes {
				t.Run(ct+"/"+ae+"/"+strconv.Itoa(size), func(t *testing.T) {
					t.Parallel()
					assertAlike(t, mws, ct, ae, size)
				})
			}
		}
	}
}

// assertAlike serves one request through every middleware and requires the same response.
func assertAlike(t *testing.T, mws map[string]compression.Middleware, ct, ae string, size int) {
	t.Helper()
	body := strings.Repeat("compressible ", size/13+1)[:size]
	var want result
	first := true
	for name, mw := range mws {
		var hdr []string
		if ct != "" {
			hdr = append(hdr, hdrCT, ct)
		}
		res := serve(t, mw, http.MethodGet, []string{hdrAE, ae}, reply(200, hdr, body))
		assert.Equal(t, body, res.decoded(t), name)
		if first {
			want, first = res, false

			continue
		}
		assert.Equal(t, want.Header, res.Header, name)
		assert.Equal(t, want.Raw, res.Raw, name)
	}
}

func TestOptionsCompose(t *testing.T) {
	t.Parallel()

	gzipOnly := compression.Config{Preference: []compression.Encoding{compression.Gzip}}
	tests := []struct {
		name     string
		wantJSON string
		wantHTML string
		opts     []compression.Option
	}{
		{name: "no option", wantJSON: encZstd, wantHTML: encBrotli},
		{
			name:     "defaults",
			opts:     []compression.Option{compression.WithDefaults()},
			wantJSON: encZstd,
			wantHTML: encBrotli,
		},
		{
			name:     "defaults, then an entry overrides one type",
			opts:     []compression.Option{compression.WithDefaults(), compression.WithContentType(ctJSON, gzipOnly)},
			wantJSON: encGzip, wantHTML: encBrotli,
		},
		{
			name:     "an entry, then defaults: the entry is kept",
			opts:     []compression.Option{compression.WithContentType(ctJSON, gzipOnly), compression.WithDefaults()},
			wantJSON: encGzip, wantHTML: encBrotli,
		},
		{
			name:     "defaults, then a fallback replaces only the fallback: JSON keeps its entry",
			opts:     []compression.Option{compression.WithDefaults(), compression.WithFallback(gzipOnly)},
			wantJSON: encZstd, wantHTML: encGzip,
		},
		{
			name:     "a fallback, then defaults: the same",
			opts:     []compression.Option{compression.WithFallback(gzipOnly), compression.WithDefaults()},
			wantJSON: encZstd, wantHTML: encGzip,
		},
		{
			name:     "a fallback preference alone restricts JSON too",
			opts:     []compression.Option{compression.WithFallback(gzipOnly)},
			wantJSON: encGzip, wantHTML: encGzip,
		},
		{
			name: "start from the recommendation and change one field",
			opts: []compression.Option{
				compression.WithDefaults(),
				compression.WithFallback(compression.Config{MinSize: 5000}),
			},
			wantJSON: encZstd,
			wantHTML: encBrotli,
		},
		{
			name:     "defaults keep a custom allow-list of the caller",
			opts:     []compression.Option{compression.WithDefaults(), compression.WithContentTypes(ctHTML)},
			wantJSON: "", wantHTML: encBrotli,
		},
		{
			name: "the former name still works",
			opts: []compression.Option{
				compression.WithDefault(gzipOnly),
			},
			wantJSON: encGzip,
			wantHTML: encGzip,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mw := mustNew(t, tt.opts...)
			for ct, want := range map[string]string{ctJSON: tt.wantJSON, ctHTML: tt.wantHTML} {
				res := serve(t, mw, http.MethodGet, []string{hdrAE, allAE}, reply(200, []string{hdrCT, ct}, big()))
				assert.Equal(t, want, res.Header.Get(hdrCE), ct)
				assert.Equal(t, big(), res.decoded(t))
			}
		})
	}
}

// TestMinSizeFromTheRecommendation checks that changing one field of the recommendation keeps the others.
func TestMinSizeFromTheRecommendation(t *testing.T) {
	t.Parallel()

	cfg := compression.Recommended()
	cfg.MinSize = 100
	mw := mustNew(t, compression.WithDefaults(), compression.WithFallback(cfg))
	body := strings.Repeat("a", 300)
	res := serve(t, mw, http.MethodGet, []string{hdrAE, allAE}, reply(200, []string{hdrCT, ctJSON}, body))
	assert.Equal(
		t,
		encZstd,
		res.Header.Get(hdrCE),
		"the JSON entry of the recommendation is kept and the lower minimum applies",
	)
	assert.Equal(t, body, res.decoded(t))
}

// TestStreamsAreOnlyMatchedByName checks that no wildcard of an allow-list or of an entry covers server-sent events.
func TestStreamsAreOnlyMatchedByName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     []compression.Option
		wantComp bool
	}{
		{
			name: "the recommended list",
			opts: []compression.Option{compression.WithContentTypes(compression.RecommendedAllowList()...)},
		},
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
			opts := append([]compression.Option{compression.WithFallback(compression.Config{MinSize: 1})}, tt.opts...)
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
