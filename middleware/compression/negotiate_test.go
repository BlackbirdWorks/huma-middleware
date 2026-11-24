package compression_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func TestNegotiate(t *testing.T) {
	t.Parallel()

	all := []compression.Encoding{compression.Zstd, compression.Brotli, compression.Gzip}
	brFirst := []compression.Encoding{compression.Brotli, compression.Gzip, compression.Zstd}
	tests := []struct {
		name   string
		header string
		want   compression.Encoding
		prefs  []compression.Encoding
		wantOK bool
	}{
		{name: "empty header", header: "", prefs: all},
		{name: "identity only", header: "identity", prefs: all},
		{name: "unknown only", header: "deflate, compress", prefs: all},
		{name: "single gzip", header: encGzip, prefs: all, want: compression.Gzip, wantOK: true},
		{name: "single br", header: encBrotli, prefs: all, want: compression.Brotli, wantOK: true},
		{name: "single zstd", header: encZstd, prefs: all, want: compression.Zstd, wantOK: true},
		{
			name:   "server preference on equal q",
			header: allAE,
			prefs:  all,
			want:   compression.Zstd,
			wantOK: true,
		},
		{
			name:   "server preference br first",
			header: allAE,
			prefs:  brFirst,
			want:   compression.Brotli,
			wantOK: true,
		},
		{
			name:   "q beats preference",
			header: "zstd;q=0.5, gzip;q=0.9",
			prefs:  all,
			want:   compression.Gzip,
			wantOK: true,
		},
		{name: "q zero excludes", header: "zstd;q=0, br;q=0, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "all excluded", header: "zstd;q=0, br;q=0.000, gzip;q=0", prefs: all},
		{name: "star", header: "*", prefs: all, want: compression.Zstd, wantOK: true},
		{name: "star with exclusion", header: "*, zstd;q=0", prefs: all, want: compression.Brotli, wantOK: true},
		{name: "star q zero", header: "*;q=0, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "star lower than explicit", header: "gzip, *;q=0.1", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "star only q zero", header: "*;q=0", prefs: all},
		{name: "case insensitive", header: "GZIP, Br", prefs: brFirst, want: compression.Brotli, wantOK: true},
		{name: "x-gzip alias", header: "x-gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "spaces", header: "  gzip ; q = 0.4 ,  br ; q=0.6 ", prefs: all, want: compression.Brotli, wantOK: true},
		{name: "q uppercase", header: "gzip;Q=0.2, br;Q=0.1", prefs: all, want: compression.Gzip, wantOK: true},
		{
			name:   "other params ignored",
			header: "gzip;level=1;q=0.3, br;q=0.2",
			prefs:  all,
			want:   compression.Gzip,
			wantOK: true,
		},
		{name: "param without equals", header: "gzip;foo, br;q=0.2", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "malformed q ignored", header: "zstd;q=abc, br", prefs: all, want: compression.Brotli, wantOK: true},
		{name: "q above 1 ignored", header: "zstd;q=1.5, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q too many decimals", header: "zstd;q=0.5555, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q negative", header: "zstd;q=-1, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q empty", header: "zstd;q=, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q bad decimal", header: "zstd;q=0.x, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q no dot", header: "zstd;q=05, gzip", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q 1.000", header: "gzip;q=1.000, br;q=0.999", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "q 1.", header: "gzip;q=1., br;q=0.9", prefs: all, want: compression.Gzip, wantOK: true},
		{
			name:   "q 1.001 rejected",
			header: "gzip;q=1.001, br;q=0.9",
			prefs:  all,
			want:   compression.Brotli,
			wantOK: true,
		},
		{name: "first occurrence wins", header: "gzip;q=0, gzip", prefs: all},
		{name: "empty items", header: ",,gzip,,", prefs: all, want: compression.Gzip, wantOK: true},
		{name: "not in preference", header: encZstd, prefs: []compression.Encoding{compression.Gzip}},
		{
			name:   "unknown preference entry",
			header: "*",
			prefs:  []compression.Encoding{"deflate"},
			want:   "deflate",
			wantOK: true,
		},
		{name: "no preference", header: encGzip, prefs: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compression.Negotiate(tt.header, tt.prefs)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func FuzzNegotiate(f *testing.F) {
	for _, s := range []string{
		"", encGzip, "br;q=0.5, gzip", "*;q=0", "zstd;q=0.001, *", "gzip;q=", ";;;", "gzip;q=1.0000", "br, , ,gzip;q=0",
		"identity;q=0, *;q=0.3", "\x00;q=1", "gzip;q=0.5;q=1",
	} {
		f.Add(s)
	}
	prefs := []compression.Encoding{compression.Zstd, compression.Brotli, compression.Gzip}
	f.Fuzz(func(t *testing.T, header string) {
		got, ok := compression.Negotiate(header, prefs)
		if ok {
			assert.Contains(t, prefs, got)
		} else {
			assert.Empty(t, got)
		}
		again, okAgain := compression.Negotiate(header, prefs)
		assert.Equal(t, got, again, "deterministic")
		assert.Equal(t, ok, okAgain)
		// Zero on every coding first (the first occurrence wins) always disables.
		_, disabled := compression.Negotiate("zstd;q=0, br;q=0, gzip;q=0, *;q=0, "+header, prefs)
		assert.False(t, disabled)
	})
}
