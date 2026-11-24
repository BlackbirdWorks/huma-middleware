package compression_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func prefer(encs ...compression.Encoding) compression.Config {
	return compression.Config{Preference: encs}
}

// TestContentTypePrecedence checks that the most specific entry wins: exact, suffix, wildcard, default.
func TestContentTypePrecedence(t *testing.T) {
	t.Parallel()

	opts := []compression.Option{
		compression.WithFallback(prefer(compression.Brotli, compression.Gzip, compression.Zstd)),
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
		{name: "suffix beats wildcard", ct: "application/vnd.api+json", want: encZstd},
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

func TestDefaultWhenNoEntryMatches(t *testing.T) {
	t.Parallel()

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
				compression.WithFallback(prefer(compression.Gzip)),
				compression.WithContentType(ctHTML, prefer(compression.Brotli)),
			},
		},
		{
			name: "a default preference replaces the built-in json rule", ct: ctJSON, want: encBrotli,
			opts: []compression.Option{compression.WithFallback(prefer(compression.Brotli, compression.Gzip))},
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
				compression.WithFallback(prefer(compression.Gzip)),
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

func TestContentTypeMinSizeAndDisabled(t *testing.T) {
	t.Parallel()

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
				compression.WithFallback(compression.Config{Disabled: true}),
				compression.WithContentType(ctHTML, compression.Config{}),
			},
		},
		{
			name: "default disabled", ct: ctText, body: big(),
			opts: []compression.Option{compression.WithFallback(compression.Config{Disabled: true})},
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

func TestPerTypeLevels(t *testing.T) {
	t.Parallel()

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
		compression.WithFallback(compression.Config{Levels: compression.Levels{
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

// TestPoolsPerLevelInParallel runs two levels of one encoding side by side.
func TestPoolsPerLevelInParallel(t *testing.T) {
	t.Parallel()

	mw := mustNew(
		t,
		compression.WithFallback(compression.Config{Levels: compression.Levels{Gzip: new(1)}}),
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

// failFlusher is a response writer that records informational statuses.
type statusRecorder struct {
	header http.Header
	codes  []int
	body   []byte
}

func (s *statusRecorder) Header() http.Header { return s.header }

func (s *statusRecorder) WriteHeader(code int) { s.codes = append(s.codes, code) }

func (s *statusRecorder) Write(p []byte) (int, error) {
	s.body = append(s.body, p...)

	return len(p), nil
}

func TestEarlyHintsAreSentAtOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		statuses  []int
		wantCodes []int
	}{
		{name: "103 then 200", statuses: []int{103, 200}, wantCodes: []int{103, 200}},
		{name: "100 then 201", statuses: []int{100, 201}, wantCodes: []int{100, 201}},
		{name: "101 is a final status", statuses: []int{101}, wantCodes: []int{101}},
		{name: "plain 200", statuses: []int{200}, wantCodes: []int{200}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := &statusRecorder{header: http.Header{}}
			serveRec(mustNew(t), http.MethodGet, "/x", []string{hdrAE, encGzip}, w, func(ctx huma.Context) {
				ctx.SetHeader(hdrCT, ctJSON)
				ctx.SetHeader("Link", "</style.css>; rel=preload")
				for _, s := range tt.statuses {
					ctx.SetStatus(s)
				}
				_, _ = ctx.BodyWriter().Write([]byte(big()))
			})
			assert.Equal(t, tt.wantCodes, w.codes)
			assert.NotEmpty(t, w.header.Get("Link"))
		})
	}
}

// streamerContext is a context of an adapter that streams the body through a callback, like Fiber.
type streamerContext struct {
	ctxBase

	sink     io.Writer
	headers  func() http.Header
	onStream func(headers http.Header)
}

func (s *streamerContext) StreamBody(fn func(io.Writer)) {
	s.onStream(s.headers())
	fn(s.sink)
}

func TestStreamBodyAdapters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ct           string
		ae           string
		wantEncoding string
		opts         []compression.Option
		streamer     bool
	}{
		{
			name:         "streaming adapter compresses a flushed stream",
			ct:           ctText,
			ae:           encGzip,
			wantEncoding: encGzip,
			streamer:     true,
		},
		{name: "streaming adapter passes events through", ct: ctSSE, ae: encGzip, streamer: true},
		{
			name: "adapter without a stream callback", ct: ctText, ae: encGzip, wantEncoding: encGzip,
			opts: []compression.Option{compression.WithFallback(compression.Config{MinSize: 1})},
		},
		{name: "adapter without a stream callback, events", ct: ctSSE, ae: encGzip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			var headersAtStream http.Header
			next := func(ctx huma.Context) {
				ctx.SetHeader(hdrCT, tt.ct)
				ctx.SetStatus(http.StatusOK)
				bs, ok := ctx.(interface{ StreamBody(func(io.Writer)) })
				require.True(t, ok, "the context forwards the stream hook")
				bs.StreamBody(func(w io.Writer) {
					_, _ = w.Write([]byte("data: one\n\n"))
					_, _ = w.Write([]byte("data: two\n\n"))
				})
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			req.Header.Set(hdrAE, tt.ae)
			base := newContextWithBody(req, rec, rec)
			hctx := base
			if tt.streamer {
				hctx = &streamerContext{
					ctxBase: base, sink: rec, headers: func() http.Header { return rec.Header().Clone() },
					onStream: func(h http.Header) { headersAtStream = h },
				}
			}
			mustNew(t, tt.opts...)(hctx, next)
			res := resultOf(t, rec)
			assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
			assert.Equal(t, "data: one\n\ndata: two\n\n", res.decoded(t))
			if tt.streamer {
				assert.Equal(
					t,
					tt.wantEncoding,
					headersAtStream.Get(hdrCE),
					"the headers are sent before the stream starts",
				)
			}
		})
	}
}
