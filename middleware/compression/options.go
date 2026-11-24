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
// (9), -1 being [gzip.DefaultCompression]. Brotli takes 1 to 11. Zstd takes the
// levels of the zstd command line tool, 1 to 22, mapped by the library to the
// closest of its four implementations.
const (
	minGzipLevel   = gzip.HuffmanOnly
	maxGzipLevel   = gzip.BestCompression
	minBrotliLevel = 1
	maxBrotliLevel = 11
	minZstdLevel   = 1
	maxZstdLevel   = 22

	defaultBrotliLevel = 5
	defaultZstdLevel   = 3
	defaultMinSize     = 1024
)

// Levels are compression levels by encoding; zero keeps the default level.
// Ranges: gzip -2 to 9 (-1 is the library default; 0, "no compression", is not
// available), brotli 1 to 11, zstd 1 to 22. The defaults are gzip -1, brotli 5
// and zstd 3.
type Levels struct {
	Gzip   int
	Brotli int
	Zstd   int
}

// of returns the level of an encoding.
func (l Levels) of(e Encoding) int {
	switch e {
	case Brotli:
		return l.Brotli
	case Zstd:
		return l.Zstd
	default:
		return l.Gzip
	}
}

// overlay returns l with the non-zero levels of o applied.
func (l Levels) overlay(o Levels) Levels {
	if o.Gzip != 0 {
		l.Gzip = o.Gzip
	}
	if o.Brotli != 0 {
		l.Brotli = o.Brotli
	}
	if o.Zstd != 0 {
		l.Zstd = o.Zstd
	}

	return l
}

// Config is the compression configuration of a set of responses: the default
// one ([WithDefault]) or that of a content type ([WithContentType]). The zero
// value of each field means "keep the default's" for a content type.
type Config struct {
	// Preference is the server preference order. The client's q values decide
	// first, the order breaks ties. Only the listed encodings are used. The
	// default is brotli, gzip, zstd; for JSON ("application/json", "+json")
	// zstd, brotli, gzip, unless the default config sets a preference.
	Preference []Encoding

	// Levels are the compression levels by encoding; see [Levels].
	Levels Levels

	// MinSize is the smallest body, in bytes, that is compressed. A response with
	// a Content-Length below it, or that ends before reaching it, is sent as is.
	// Empty bodies are never compressed. Zero keeps the default's (1024 for the
	// default config).
	MinSize int

	// Disabled turns compression off for the content type.
	Disabled bool
}

// Option configures [New].
type Option func(*config) error

type config struct {
	skip         func(huma.Context) bool
	types        []string
	skipPrefixes []string
	onlyPrefixes []string
	skipPatterns []string
	skipOps      []string
	entries      []typeEntry
	def          Config
}

type typeEntry struct {
	pattern string
	cfg     Config
	builtin bool
}

func newConfig(opts []Option) (*config, error) {
	c := &config{}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if len(c.def.Preference) == 0 {
		// JSON: zstd first when the client accepts it, unless the caller has an entry for the same pattern.
		json := Config{Preference: []Encoding{Zstd, Brotli, Gzip}}
		for _, p := range []string{"application/json", "+json"} {
			if !slices.ContainsFunc(c.entries, func(e typeEntry) bool { return e.pattern == p }) {
				c.entries = append(c.entries, typeEntry{p, json, true})
			}
		}
	}

	return c, nil
}

func validEncodings(encs []Encoding) error {
	for _, e := range encs {
		if e != Zstd && e != Brotli && e != Gzip {
			return fmt.Errorf("%w: %q", ErrUnknownEncoding, e)
		}
	}

	return nil
}

func validConfig(cfg Config, isDefault bool) error {
	if err := validEncodings(cfg.Preference); err != nil {
		return err
	}
	if isDefault && len(cfg.Preference) == 0 && cfg.Preference != nil {
		return ErrEmptyPreference
	}
	for _, l := range []struct {
		enc    Encoding
		level  int
		lo, hi int
	}{
		{Gzip, cfg.Levels.Gzip, minGzipLevel, maxGzipLevel},
		{Brotli, cfg.Levels.Brotli, minBrotliLevel, maxBrotliLevel},
		{Zstd, cfg.Levels.Zstd, minZstdLevel, maxZstdLevel},
	} {
		if l.level != 0 && (l.level < l.lo || l.level > l.hi) {
			return fmt.Errorf("%w: %s level %d is outside %d..%d", ErrInvalidLevel, l.enc, l.level, l.lo, l.hi)
		}
	}
	if cfg.MinSize < 0 {
		return fmt.Errorf("%w: %d", ErrInvalidMinSize, cfg.MinSize)
	}

	return nil
}

// WithDefault sets the default configuration, used for every content type
// without an entry of its own. Fields left at their zero value keep the
// built-in defaults. Several calls are merged, later fields winning.
func WithDefault(cfg Config) Option {
	return func(c *config) error {
		if err := validConfig(cfg, true); err != nil {
			return err
		}
		c.def = mergeConfig(c.def, cfg)

		return nil
	}
}

// WithContentType sets the configuration of the responses of one content type;
// fields left at their zero value keep the default's. contentType is "type/subtype"
// (exact), "+suffix" (a structured syntax suffix such as "+json") or "type/*"
// ("*/*" for all). When several entries match a response, the most specific wins:
// exact, then suffix (the longest), then wildcard (the longest), then the default.
// An entry also makes its content type compressible when it is not on the allow-list.
// A later entry for the same contentType replaces an earlier one.
func WithContentType(contentType string, cfg Config) Option {
	return func(c *config) error {
		if err := validType(contentType); err != nil {
			return err
		}
		if err := validConfig(cfg, false); err != nil {
			return err
		}
		pattern := strings.ToLower(contentType)
		c.entries = slices.DeleteFunc(c.entries, func(e typeEntry) bool { return e.pattern == pattern })
		c.entries = append(c.entries, typeEntry{pattern, cloneConfig(cfg), false})

		return nil
	}
}

// mergeConfig applies the non-zero fields of over to base.
func mergeConfig(base, over Config) Config {
	over = cloneConfig(over)
	if over.Preference != nil {
		base.Preference = over.Preference
	}
	base.Levels = base.Levels.overlay(over.Levels)
	if over.MinSize > 0 {
		base.MinSize = over.MinSize
	}
	base.Disabled = over.Disabled

	return base
}

func cloneConfig(cfg Config) Config {
	cfg.Preference = slices.Clone(cfg.Preference)

	return cfg
}

func validType(pattern string) error {
	if pattern == "" || pattern == "+" || (!strings.Contains(pattern, "/") && !strings.HasPrefix(pattern, "+")) {
		return fmt.Errorf("%w: content type %q", ErrInvalidPattern, pattern)
	}

	return nil
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
