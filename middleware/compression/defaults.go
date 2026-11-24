package compression

import "slices"

// The built-in defaults, what [New] uses with no option. Each one is explained
// in the package documentation and backed by BenchmarkLevels (README).
const (
	// defaultGzipLevel is the library default (6): the ratio of level 9 at a sixth of the time.
	defaultGzipLevel = 6
	// defaultBrotliLevel is the knee of the curve: levels 6 to 8 do not compress better, level 4 is 12% bigger.
	defaultBrotliLevel = 5
	// defaultZstdLevel is zstd's own default level.
	defaultZstdLevel = 3
	// defaultMinSize is about one TCP segment: below it a response fits one packet anyway.
	defaultMinSize = 1024
)

// DefaultConfig returns the built-in fallback [Config] that [New] uses with no
// option, with every field set explicitly: the preference order brotli, gzip,
// zstd, the levels gzip 6, brotli 5 and zstd 3, and a minimum size of 1024
// bytes. Every call returns a new value that the caller may change; pass it to
// [WithDefault] to start from the defaults and adjust them. The built-in rule
// for JSON (zstd first) stays with it.
func DefaultConfig() Config {
	return Config{
		Levels: Levels{
			Gzip:   new(defaultGzipLevel),
			Brotli: new(defaultBrotliLevel),
			Zstd:   new(defaultZstdLevel),
		},
		Preference: []Encoding{Brotli, Gzip, Zstd},
		MinSize:    defaultMinSize,
	}
}

// defaultAllowList is the allow-list of compressible content types used when
// [WithContentTypes] is not given. No wildcard of it matches text/event-stream.
func defaultAllowList() []string {
	return []string{
		"text/*", "+json", "+xml", "application/json", "application/javascript", "application/x-javascript",
		"application/ecmascript", "application/xml", "application/x-ndjson", "application/yaml",
		"application/x-yaml", "application/graphql", "application/wasm",
	}
}

// defaultSettings is [DefaultConfig] as the settings of a response.
func defaultSettings() *settings {
	return overlay(&settings{}, DefaultConfig())
}

// jsonPreference is the preference order for JSON given the fallback's: zstd
// first when the fallback allows it, the rest in the fallback's order. A
// fallback without zstd is returned as it is.
func jsonPreference(fallback []Encoding) []Encoding {
	if !slices.Contains(fallback, Zstd) {
		return fallback
	}

	return append(
		[]Encoding{Zstd},
		slices.DeleteFunc(slices.Clone(fallback), func(e Encoding) bool { return e == Zstd })...)
}
