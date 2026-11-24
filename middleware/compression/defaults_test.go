package compression_test

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

// reference compresses body with the library directly, as the middleware should at the given level.
func reference(t *testing.T, enc string, level int, body string) []byte {
	t.Helper()
	var out bytes.Buffer
	switch enc {
	case encGzip:
		w, err := gzip.NewWriterLevel(&out, level)
		require.NoError(t, err)
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
		require.NoError(t, w.Close())
	case encBrotli:
		w := brotli.NewWriterLevel(&out, level)
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
		require.NoError(t, w.Close())
	default:
		w, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
			zstd.WithEncoderConcurrency(1), zstd.WithZeroFrames(true))
		require.NoError(t, err)
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}

	return out.Bytes()
}

const (
	belowMin = 1023
	atMin    = 1024
	big20k   = 20000
)

type pinnedCase struct {
	name      string
	ct        string
	ae        string
	wantEnc   string
	wantLevel int
	size      int
	wantVary  bool
}

// jsonPinned are the pinned cases of JSON.
func jsonPinned() []pinnedCase {
	return []pinnedCase{
		{
			name:      "json prefers zstd",
			ct:        ctJSON,
			ae:        allAE,
			size:      big20k,
			wantEnc:   encZstd,
			wantLevel: 3,
			wantVary:  true,
		},
		{
			name:      "json without zstd",
			ct:        ctJSON,
			ae:        aeGzipBr,
			size:      big20k,
			wantEnc:   encBrotli,
			wantLevel: 5,
			wantVary:  true,
		},
		{name: "json gzip only", ct: ctJSON, ae: encGzip, size: big20k, wantEnc: encGzip, wantLevel: 6, wantVary: true},
		{
			name:      "json with charset",
			ct:        ctJSON + "; charset=utf-8",
			ae:        allAE,
			size:      big20k,
			wantEnc:   encZstd,
			wantLevel: 3,
			wantVary:  true,
		},
		{
			name:      "problem json",
			ct:        "application/problem+json",
			ae:        allAE,
			size:      big20k,
			wantEnc:   encZstd,
			wantLevel: 3,
			wantVary:  true,
		},
		{
			name:      "vendor json",
			ct:        ctVendor,
			ae:        allAE,
			size:      big20k,
			wantEnc:   encZstd,
			wantLevel: 3,
			wantVary:  true,
		},
		{
			name:      "json q values",
			ct:        ctJSON,
			ae:        "zstd;q=0.2, br;q=0.9",
			size:      big20k,
			wantEnc:   encBrotli,
			wantLevel: 5,
			wantVary:  true,
		},
		{name: "json not accepted", ct: ctJSON, ae: "", size: big20k, wantVary: true},
		{name: "json identity", ct: ctJSON, ae: encIdent, size: big20k, wantVary: true},
	}
}

// othersPinned are the pinned cases of the other content types, sizes and encodings.
func othersPinned() []pinnedCase {
	return []pinnedCase{
		{
			name:      "html prefers brotli",
			ct:        ctHTML,
			ae:        allAE,
			size:      big20k,
			wantEnc:   encBrotli,
			wantLevel: 5,
			wantVary:  true,
		},
		{name: "html gzip", ct: ctHTML, ae: encGzip, size: big20k, wantEnc: encGzip, wantLevel: 6, wantVary: true},
		{name: "html zstd only", ct: ctHTML, ae: encZstd, size: big20k, wantEnc: encZstd, wantLevel: 3, wantVary: true},
		{
			name:      "html q values",
			ct:        ctHTML,
			ae:        "br;q=0.1, gzip",
			size:      big20k,
			wantEnc:   encGzip,
			wantLevel: 6,
			wantVary:  true,
		},
		{name: "plain text", ct: ctText, ae: allAE, size: big20k, wantEnc: encBrotli, wantLevel: 5, wantVary: true},
		{name: "css", ct: ctCSS, ae: allAE, size: big20k, wantEnc: encBrotli, wantLevel: 5, wantVary: true},
		{name: "javascript", ct: ctJS, ae: allAE, size: big20k, wantEnc: encBrotli, wantLevel: 5, wantVary: true},
		{name: "xml", ct: "application/xml", ae: allAE, size: big20k, wantEnc: encBrotli, wantLevel: 5, wantVary: true},
		{name: "svg", ct: "image/svg+xml", ae: allAE, size: big20k, wantEnc: encBrotli, wantLevel: 5, wantVary: true},
		{name: "one byte below the minimum", ct: ctJSON, ae: allAE, size: belowMin, wantVary: true},
		{
			name:      "exactly the minimum",
			ct:        ctJSON,
			ae:        allAE,
			size:      atMin,
			wantEnc:   encZstd,
			wantLevel: 3,
			wantVary:  true,
		},
		{name: "empty", ct: ctJSON, ae: allAE, size: 0, wantVary: true},
		{name: "png", ct: ctPNG, ae: allAE, size: big20k},
		{name: "zip", ct: ctZip, ae: allAE, size: big20k},
		{name: "octet stream", ct: ctOctet, ae: allAE, size: big20k},
		{name: "event stream", ct: ctSSE, ae: allAE, size: big20k},
		{name: noCT, ct: "", ae: allAE, size: big20k},
	}
}

// TestDefaultConfig covers the defaults: their values, that each call is a copy, and starting from them.
func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "values", run: runDefaultConfigValues},
		{name: "fresh copy", run: runDefaultConfigIsAFreshCopy},
		{name: "start from the defaults", run: runStartFromTheDefaults},
		{name: "zero config equals defaults", run: runZeroConfigEqualsTheDefaultConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runDefaultConfigValues(t *testing.T) {
	t.Helper()

	cfg := compression.DefaultConfig()
	require.NotNil(t, cfg.Levels.Gzip)
	require.NotNil(t, cfg.Levels.Brotli)
	require.NotNil(t, cfg.Levels.Zstd)
	assert.Equal(t, 6, *cfg.Levels.Gzip)
	assert.Equal(t, 5, *cfg.Levels.Brotli)
	assert.Equal(t, 3, *cfg.Levels.Zstd)
	assert.Equal(t, []compression.Encoding{compression.Brotli, compression.Gzip, compression.Zstd}, cfg.Preference)
	assert.Equal(t, 1024, cfg.MinSize)
	assert.False(t, cfg.Disabled)
}

func runDefaultConfigIsAFreshCopy(t *testing.T) {
	t.Helper()

	cfg := compression.DefaultConfig()
	*cfg.Levels.Gzip = 1
	cfg.Preference[0] = compression.Zstd
	cfg.MinSize = 1
	assert.Equal(t, 1, cfg.MinSize)
	again := compression.DefaultConfig()
	assert.Equal(t, 6, *again.Levels.Gzip)
	assert.Equal(t, compression.Brotli, again.Preference[0])
	assert.Equal(t, 1024, again.MinSize)
}

// runStartFromTheDefaults checks that the defaults, changed in one field and passed to WithDefault, keep the others,
// including the built-in rule for JSON.
func runStartFromTheDefaults(t *testing.T) {
	t.Helper()

	tests := []struct {
		change  func(*compression.Config)
		name    string
		ct      string
		wantEnc string
		size    int
	}{
		{name: "unchanged", change: func(*compression.Config) {}, ct: ctJSON, size: 2000, wantEnc: encZstd},
		{
			name:    "lower minimum, JSON",
			change:  func(c *compression.Config) { c.MinSize = 256 },
			ct:      ctJSON,
			size:    300,
			wantEnc: encZstd,
		},
		{
			name:    "lower minimum, html",
			change:  func(c *compression.Config) { c.MinSize = 256 },
			ct:      ctHTML,
			size:    300,
			wantEnc: encBrotli,
		},
		{
			name:   "default minimum stays for the rest",
			change: func(c *compression.Config) { c.MinSize = 256 },
			ct:     ctHTML,
			size:   100,
		},
		{
			name:    "another level",
			change:  func(c *compression.Config) { c.Levels.Brotli = new(8) },
			ct:      ctHTML,
			size:    2000,
			wantEnc: encBrotli,
		},
		{
			name: "without zstd the JSON rule has nothing to put first",
			change: func(c *compression.Config) {
				c.Preference = []compression.Encoding{compression.Gzip, compression.Brotli}
			},
			ct: ctJSON, size: 2000, wantEnc: encGzip,
		},
		{
			name:    "with zstd in the fallback, JSON still starts with it",
			change:  func(c *compression.Config) { c.Preference = []compression.Encoding{compression.Gzip, compression.Zstd} },
			ct:      ctJSON,
			size:    2000,
			wantEnc: encZstd,
		},
		{
			name:    "the same fallback serves the other types in its own order",
			change:  func(c *compression.Config) { c.Preference = []compression.Encoding{compression.Gzip, compression.Zstd} },
			ct:      ctHTML,
			size:    2000,
			wantEnc: encGzip,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := compression.DefaultConfig()
			tt.change(&cfg)
			body := strings.Repeat("a", tt.size)
			res := serve(t, mustNew(t, compression.WithDefault(cfg)), http.MethodGet, []string{hdrAE, allAE},
				reply(200, []string{hdrCT, tt.ct}, body))
			assert.Equal(t, tt.wantEnc, res.Header.Get(hdrCE))
			assert.Equal(t, body, res.decoded(t))
		})
	}
}

// runZeroConfigEqualsTheDefaultConfig checks that spelling the defaults out changes nothing.
func runZeroConfigEqualsTheDefaultConfig(t *testing.T) {
	t.Helper()

	zero := mustNew(t)
	spelled := mustNew(t, compression.WithDefault(compression.DefaultConfig()))
	for _, ct := range []string{ctJSON, ctHTML, ctText, ctPNG, ctSSE} {
		for _, ae := range []string{"", encGzip, encBrotli, encZstd, allAE} {
			for _, size := range []int{0, 500, 5000} {
				t.Run(ct+"/"+ae, func(t *testing.T) {
					t.Parallel()
					body := strings.Repeat("compressible ", size/13+1)[:size]
					hdr := []string{hdrCT, ct}
					a := serve(t, zero, http.MethodGet, []string{hdrAE, ae}, reply(200, hdr, body))
					b := serve(t, spelled, http.MethodGet, []string{hdrAE, ae}, reply(200, hdr, body))
					assert.Equal(t, a.Header, b.Header)
					assert.Equal(t, a.Raw, b.Raw)
				})
			}
		}
	}
}
