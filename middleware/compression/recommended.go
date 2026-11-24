package compression

import "slices"

// The recommended values. Each one is explained in the package documentation and
// backed by the benchmark of the levels ("BenchmarkLevels", README).
const (
	// recommendedGzipLevel is the library default (6): the ratio of level 9 at a sixth of the time.
	recommendedGzipLevel = 6
	// recommendedBrotliLevel is the knee of the curve: levels 6 to 8 do not compress better, level 4 is 12% bigger.
	recommendedBrotliLevel = 5
	// recommendedZstdLevel is zstd's own default level.
	recommendedZstdLevel = 3
	// recommendedMinSize is about one TCP segment: below it a response fits one packet anyway.
	recommendedMinSize = 1024
)

// Recommended returns the recommended default [Config], every field set
// explicitly: the preference order brotli, gzip, zstd, the levels gzip 6, brotli
// 5 and zstd 3, and a minimum size of 1024 bytes. It is what [New] uses when
// given no options. Every call returns a new value that the caller may change.
func Recommended() Config {
	return Config{
		Levels: Levels{
			Gzip:   new(recommendedGzipLevel),
			Brotli: new(recommendedBrotliLevel),
			Zstd:   new(recommendedZstdLevel),
		},
		Preference: []Encoding{Brotli, Gzip, Zstd},
		MinSize:    recommendedMinSize,
	}
}

// RecommendedContentTypes returns the recommended per-content-type
// configurations, by content type as [WithContentType] takes it: JSON
// ("application/json" and the "+json" suffix) prefers zstd, then brotli, then
// gzip. Every other setting is inherited from [Recommended]. Every call returns
// new values that the caller may change.
func RecommendedContentTypes() map[string]Config {
	json := func() Config { return Config{Preference: []Encoding{Zstd, Brotli, Gzip}} }

	return map[string]Config{"application/json": json(), "+json": json()}
}

// RecommendedAllowList returns the recommended allow-list of compressible
// content types, in the form [WithContentTypes] takes: text, JSON, XML,
// JavaScript and the other text formats that shrink well, including the "+json"
// and "+xml" suffixes (which cover SVG). It leaves out what is compressed
// already (images other than SVG, video, audio, archives, PDF, fonts), which is
// never compressed whatever the list says, and "text/event-stream", which no
// wildcard of the list matches: a stream must reach the client as it is written,
// so it is compressed only when named exactly. Every call returns a new slice.
func RecommendedAllowList() []string {
	return slices.Clone([]string{
		"text/*", "+json", "+xml", "application/json", "application/javascript", "application/x-javascript",
		"application/ecmascript", "application/xml", "application/x-ndjson", "application/yaml",
		"application/x-yaml", "application/graphql", "application/wasm",
	})
}

// WithDefaults applies the recommendation explicitly: [Recommended] as the
// default configuration, [RecommendedContentTypes] as the per-type entries
// (JSON prefers zstd) and [RecommendedAllowList] as the allow-list.
// New(WithDefaults()) is the same as New() with no option.
//
// It is order independent and only fills in what the caller did not set:
// [WithFallback] changes fields of the recommended default config, a
// [WithContentType] entry replaces the recommended one of the same content type,
// and [WithContentTypes] replaces the allow-list, wherever they stand relative
// to WithDefaults. What it adds over giving no option is that the recommended
// JSON entries stay even when [WithFallback] sets a preference: without it, a
// caller that sets the fallback preference gets that preference for JSON too
// (which is how a caller restricts the encodings in use).
func WithDefaults() Option {
	return func(c *config) error {
		c.recommended = true

		return nil
	}
}

// recommendedSettings is [Recommended] as the settings of a response.
func recommendedSettings() *settings {
	return overlay(&settings{}, Recommended())
}
