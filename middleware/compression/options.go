package compression

import (
	"compress/gzip"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Errors returned by [New] for an invalid configuration.
var (
	// ErrUnknownEncoding is returned for an encoding that is not Zstd, Brotli or Gzip.
	ErrUnknownEncoding = errors.New("compression: unknown encoding")
	// ErrInvalidLevel is returned for a compression level outside the range of its encoding.
	ErrInvalidLevel = errors.New("compression: invalid level")
	// ErrInvalidMinSize is returned for a negative minimum size.
	ErrInvalidMinSize = errors.New("compression: invalid minimum size")
	// ErrEmptyPreference is returned for an empty list of encodings.
	ErrEmptyPreference = errors.New("compression: empty encoding preference")
	// ErrInvalidPattern is returned for an empty or malformed content type or path pattern.
	ErrInvalidPattern = errors.New("compression: invalid pattern")
)

// Level ranges. Gzip takes [gzip.HuffmanOnly] (-2) to [gzip.BestCompression]
// (9), -1 being [gzip.DefaultCompression]. Brotli takes 0 to 11. Zstd takes the
// levels of the zstd command line tool, 1 to 22, mapped by the library to the
// closest of its four implementations.
const (
	minGzipLevel   = gzip.HuffmanOnly
	maxGzipLevel   = gzip.BestCompression
	minBrotliLevel = 0
	maxBrotliLevel = 11
	minZstdLevel   = 1
	maxZstdLevel   = 22

	defaultBrotliLevel = 5
	defaultZstdLevel   = 3
	defaultMinSize     = 1024
)

// Option configures [New].
type Option func(*config) error

type typeRule struct {
	pattern string
	prefs   []Encoding
}

type config struct {
	skip         func(huma.Context) bool
	levels       map[Encoding]int
	preference   []Encoding
	rules        []typeRule
	types        []string
	skipPrefixes []string
	onlyPrefixes []string
	skipPatterns []string
	skipOps      []string
	minSize      int
	hasPref      bool
}

func newConfig(opts []Option) (*config, error) {
	c := &config{
		levels: map[Encoding]int{Gzip: gzip.DefaultCompression, Brotli: defaultBrotliLevel, Zstd: defaultZstdLevel},
		// Brotli compresses text best, gzip is the universal fallback and zstd the last resort.
		preference: []Encoding{Brotli, Gzip, Zstd},
		minSize:    defaultMinSize,
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if !c.hasPref {
		// JSON: zstd first when the client accepts it. Added after the caller's rules, which therefore win.
		c.rules = append(c.rules,
			typeRule{"application/json", []Encoding{Zstd, Brotli, Gzip}},
			typeRule{"+json", []Encoding{Zstd, Brotli, Gzip}})
	}

	return c, nil
}

func validEncodings(encs []Encoding) error {
	if len(encs) == 0 {
		return ErrEmptyPreference
	}
	for _, e := range encs {
		if e != Zstd && e != Brotli && e != Gzip {
			return fmt.Errorf("%w: %q", ErrUnknownEncoding, e)
		}
	}

	return nil
}

// WithPreference sets the server preference order for every content type,
// replacing the default (brotli, gzip, zstd; and zstd, brotli, gzip for JSON).
// Only the listed encodings are used, except for those named by
// [WithContentTypePreference]. The client's q values still decide first.
func WithPreference(encs ...Encoding) Option {
	return func(c *config) error {
		if err := validEncodings(encs); err != nil {
			return err
		}
		c.preference, c.hasPref = slices.Clone(encs), true

		return nil
	}
}

// WithContentTypePreference sets the preference order for the content types
// matching pattern: "type/subtype", "type/*", "+suffix" or "*/*". Rules are
// tried in the order given, before the built-in JSON rule.
func WithContentTypePreference(pattern string, encs ...Encoding) Option {
	return func(c *config) error {
		if err := validEncodings(encs); err != nil {
			return err
		}
		if err := validType(pattern); err != nil {
			return err
		}
		c.rules = append(c.rules, typeRule{strings.ToLower(pattern), slices.Clone(encs)})

		return nil
	}
}

func validType(pattern string) error {
	if pattern == "" || pattern == "+" || (!strings.Contains(pattern, "/") && !strings.HasPrefix(pattern, "+")) {
		return fmt.Errorf("%w: content type %q", ErrInvalidPattern, pattern)
	}

	return nil
}

// WithLevel sets the compression level of one encoding. Ranges: gzip -2 to 9
// (-1 is the default), brotli 0 to 11, zstd 1 to 22. The defaults are gzip -1,
// brotli 5 and zstd 3.
func WithLevel(enc Encoding, level int) Option {
	return func(c *config) error {
		lo, hi := minGzipLevel, maxGzipLevel
		switch enc {
		case Gzip:
		case Brotli:
			lo, hi = minBrotliLevel, maxBrotliLevel
		case Zstd:
			lo, hi = minZstdLevel, maxZstdLevel
		default:
			return fmt.Errorf("%w: %q", ErrUnknownEncoding, enc)
		}
		if level < lo || level > hi {
			return fmt.Errorf("%w: %s level %d is outside %d..%d", ErrInvalidLevel, enc, level, lo, hi)
		}
		c.levels[enc] = level

		return nil
	}
}

// WithMinSize sets the smallest body, in bytes, that is compressed (default 1024).
// A response with a Content-Length below it, or that ends before reaching it,
// is sent as is. 0 compresses every non-empty body.
func WithMinSize(n int) Option {
	return func(c *config) error {
		if n < 0 {
			return fmt.Errorf("%w: %d", ErrInvalidMinSize, n)
		}
		c.minSize = n

		return nil
	}
}

// WithContentTypes replaces the default allow-list of compressible content
// types, which is "text/*" (without text/event-stream), JSON, XML, JavaScript
// and a few more text formats. Patterns are "type/subtype", "type/*", "+suffix"
// or "*/*". Formats that are compressed already (images other than SVG, video,
// audio, archives, PDF, fonts) are never compressed, whatever the list says.
// Listing "text/event-stream" compresses Server-Sent Events, flushing through
// the encoder on every flush.
func WithContentTypes(patterns ...string) Option {
	return func(c *config) error {
		for _, p := range patterns {
			if err := validType(p); err != nil {
				return err
			}
			c.types = append(c.types, strings.ToLower(p))
		}

		return nil
	}
}

// WithSkipPathPrefixes leaves the requests whose URL path starts with one of
// the prefixes uncompressed.
func WithSkipPathPrefixes(prefixes ...string) Option {
	return func(c *config) error {
		c.skipPrefixes = append(c.skipPrefixes, prefixes...)

		return nil
	}
}

// WithSkipPathPatterns leaves the requests whose URL path matches one of the
// [path.Match] patterns uncompressed ("*" does not cross a "/").
func WithSkipPathPatterns(patterns ...string) Option {
	return func(c *config) error {
		for _, p := range patterns {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("%w: %q: %w", ErrInvalidPattern, p, err)
			}
		}
		c.skipPatterns = append(c.skipPatterns, patterns...)

		return nil
	}
}

// WithOnlyPathPrefixes compresses only the requests whose URL path starts with
// one of the prefixes.
func WithOnlyPathPrefixes(prefixes ...string) Option {
	return func(c *config) error {
		c.onlyPrefixes = append(c.onlyPrefixes, prefixes...)

		return nil
	}
}

// WithSkipOperations leaves the operations with the given IDs uncompressed.
func WithSkipOperations(ids ...string) Option {
	return func(c *config) error {
		c.skipOps = append(c.skipOps, ids...)

		return nil
	}
}

// WithSkip leaves the requests for which fn returns true uncompressed.
func WithSkip(fn func(huma.Context) bool) Option {
	return func(c *config) error {
		c.skip = fn

		return nil
	}
}
