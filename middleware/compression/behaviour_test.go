package compression_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

func TestSkips(t *testing.T) {
	t.Parallel()

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

func TestContentTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ct       string
		opts     []compression.Option
		wantComp bool
	}{
		{name: "json", ct: ctJSON, wantComp: true},
		{name: "json with params", ct: "Application/JSON; charset=utf-8", wantComp: true},
		{name: "vendor json", ct: "application/vnd.api+json", wantComp: true},
		{name: "html", ct: ctHTML, wantComp: true},
		{name: "css", ct: ctCSS, wantComp: true},
		{name: "javascript", ct: "application/javascript", wantComp: true},
		{name: "xml", ct: "application/xml", wantComp: true},
		{name: "svg", ct: "image/svg+xml", wantComp: true},
		{name: "ndjson", ct: "application/x-ndjson", wantComp: true},
		{name: "wasm", ct: "application/wasm", wantComp: true},
		{name: "event stream default", ct: ctSSE},
		{name: "png", ct: ctPNG},
		{name: "jpeg", ct: "image/jpeg"},
		{name: "mp4", ct: "video/mp4"},
		{name: "mp3", ct: "audio/mpeg"},
		{name: "zip", ct: "application/zip"},
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

func TestMinSize(t *testing.T) {
	t.Parallel()

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

type preferenceCase struct {
	name string
	ct   string
	ae   string
	want string
	opts []compression.Option
}

func preferenceCases() []preferenceCase {
	return []preferenceCase{
		{
			name: "global gzip only",
			ct:   ctJSON,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Gzip}}),
			},
			want: encGzip,
		},
		{
			name: "global order",
			ct:   ctText,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithDefault(
					compression.Config{Preference: []compression.Encoding{compression.Zstd, compression.Gzip}},
				),
			},
			want: encZstd,
		},
		{
			name: "global replaces the json rule",
			ct:   ctJSON,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Brotli}}),
			},
			want: encBrotli,
		},
	}
}

// typePreferenceCases are the cases of preferences set per content type.
func typePreferenceCases() []preferenceCase {
	return []preferenceCase{
		{
			name: "per type",
			ct:   ctHTML,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					ctHTML,
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type enables an encoding outside the global list", ct: ctHTML, ae: allAE,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Gzip}}),
				compression.WithContentType(
					ctHTML,
					compression.Config{Preference: []compression.Encoding{compression.Zstd}},
				),
			},
			want: encZstd,
		},
		{
			name: "per type wildcard",
			ct:   ctCSS,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"text/*",
					compression.Config{Preference: []compression.Encoding{compression.Gzip, compression.Brotli}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type wins over the json rule",
			ct:   ctJSON,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"application/json",
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type suffix",
			ct:   "application/x+xml",
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"+xml",
					compression.Config{Preference: []compression.Encoding{compression.Zstd}},
				),
			},
			want: encZstd,
		},
		{
			name: "per type with params",
			ct:   "TEXT/HTML; charset=utf-8",
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					"Text/Html",
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encGzip,
		},
		{
			name: "per type other type unaffected",
			ct:   ctText,
			ae:   allAE,
			opts: []compression.Option{
				compression.WithContentType(
					ctHTML,
					compression.Config{Preference: []compression.Encoding{compression.Gzip}},
				),
			},
			want: encBrotli,
		},
		{
			name: "client accepts only a disabled encoding",
			ct:   ctText,
			ae:   encZstd,
			opts: []compression.Option{
				compression.WithDefault(compression.Config{Preference: []compression.Encoding{compression.Gzip}}),
			},
		},
	}
}

func TestPreferences(t *testing.T) {
	t.Parallel()

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

func TestLevels(t *testing.T) {
	t.Parallel()

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
		{name: "brotli", enc: compression.Brotli, fast: 1, best: 11},
		{name: encZstd, enc: compression.Zstd, fast: 1, best: 22},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Less(t, size(t, tt.enc, tt.best), size(t, tt.enc, tt.fast))
		})
	}
}

type invalidCase struct {
	wantErr error
	opt     compression.Option
	name    string
}

func invalidCases() []invalidCase {
	return []invalidCase{
		{
			name:    "gzip level too low",
			opt:     levelOpt(compression.Gzip, -3),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "gzip level too high",
			opt:     levelOpt(compression.Gzip, 10),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "brotli level too low",
			opt:     levelOpt(compression.Brotli, -1),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "brotli level too high",
			opt:     levelOpt(compression.Brotli, 12),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "zstd level negative",
			opt:     levelOpt(compression.Zstd, -1),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "zstd level too high",
			opt:     levelOpt(compression.Zstd, 23),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "unknown encoding preference",
			opt:     compression.WithDefault(compression.Config{Preference: []compression.Encoding{encFake}}),
			wantErr: compression.ErrUnknownEncoding,
		},
		{
			name:    "empty preference",
			opt:     compression.WithDefault(compression.Config{Preference: []compression.Encoding{}}),
			wantErr: compression.ErrEmptyPreference,
		},
		{
			name: "unknown encoding in a type preference",
			opt: compression.WithContentType(
				"text/*",
				compression.Config{Preference: []compression.Encoding{encFake}},
			),
			wantErr: compression.ErrUnknownEncoding,
		},
		{
			name: "type level out of range",
			opt: compression.WithContentType(
				"text/*",
				compression.Config{Levels: compression.Levels{Zstd: 99}},
			),
			wantErr: compression.ErrInvalidLevel,
		},
		{
			name:    "type negative min size",
			opt:     compression.WithContentType("text/*", compression.Config{MinSize: -1}),
			wantErr: compression.ErrInvalidMinSize,
		},
		{
			name: "bad type preference pattern",
			opt: compression.WithContentType(
				"html",
				compression.Config{Preference: []compression.Encoding{compression.Gzip}},
			),
			wantErr: compression.ErrInvalidPattern,
		},
		{name: "empty content type", opt: compression.WithContentTypes(""), wantErr: compression.ErrInvalidPattern},
		{name: "bare plus", opt: compression.WithContentTypes("+"), wantErr: compression.ErrInvalidPattern},
		{
			name:    "negative min size",
			opt:     compression.WithDefault(compression.Config{MinSize: -1}),
			wantErr: compression.ErrInvalidMinSize,
		},
		{
			name:    "bad path pattern",
			opt:     compression.WithSkipPathPatterns("[a-"),
			wantErr: compression.ErrInvalidPattern,
		},
	}
}

func TestInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := invalidCases()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mw, err := compression.New(tt.opt)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Nil(t, mw)
		})
	}
}

func TestValidLevelBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		opt  compression.Option
		name string
	}{
		{name: "gzip low", opt: levelOpt(compression.Gzip, -2)},
		{name: "gzip default", opt: levelOpt(compression.Gzip, -1)},
		{name: "gzip high", opt: levelOpt(compression.Gzip, 9)},
		{name: "brotli low", opt: levelOpt(compression.Brotli, 1)},
		{name: "zero keeps the default", opt: levelOpt(compression.Gzip, 0)},
		{name: "brotli high", opt: levelOpt(compression.Brotli, 11)},
		{name: "zstd low", opt: levelOpt(compression.Zstd, 1)},
		{name: "zstd high", opt: levelOpt(compression.Zstd, 22)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := compression.New(tt.opt)
			require.NoError(t, err)
		})
	}
}

// flushRecorder records the body at the time of each flush.
type flushRecorder struct {
	*httptest.ResponseRecorder

	atFlush []int
}

func (f *flushRecorder) Flush() {
	f.atFlush = append(f.atFlush, f.Body.Len())
	f.ResponseRecorder.Flush()
}

func TestStreaming(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		ct           string
		wantEncoding string
		opts         []compression.Option
	}{
		{name: "event stream passes through", ct: ctSSE, wantEncoding: ""},
		{name: "compressed stream flushes through the encoder", ct: ctText, wantEncoding: encGzip},
		{
			name: "event stream opt in", ct: ctSSE, wantEncoding: encGzip,
			opts: []compression.Option{compression.WithContentTypes(ctSSE)},
		},
		{name: "not compressible type", ct: ctPNG, wantEncoding: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
			serveRec(
				mustNew(t, tt.opts...),
				http.MethodGet,
				"/x",
				[]string{hdrAE, encGzip},
				rec,
				func(ctx huma.Context) {
					ctx.SetHeader(hdrCT, tt.ct)
					ctx.SetStatus(http.StatusOK)
					w := ctx.BodyWriter()
					_, err := w.Write([]byte("data: one\n\n"))
					require.NoError(t, err)
					f, ok := w.(http.Flusher)
					require.True(t, ok)
					f.Flush()
					f.Flush()
					_, err = w.Write([]byte("data: two\n\n"))
					require.NoError(t, err)
				},
			)
			require.NotEmpty(t, rec.atFlush)
			assert.Positive(t, rec.atFlush[0], "data reached the client at the first flush, not at the end")
			res := resultOf(t, rec.ResponseRecorder)
			assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
			assert.Equal(t, "data: one\n\ndata: two\n\n", res.decoded(t))
		})
	}
}

func TestStreamingWithoutFlusherSink(t *testing.T) {
	t.Parallel()

	w := &plainWriter{header: http.Header{}}
	serveRec(
		mustNew(t, compression.WithDefault(compression.Config{MinSize: 0})),
		http.MethodGet,
		"/x",
		[]string{hdrAE, encGzip},
		w,
		func(ctx huma.Context) {
			ctx.SetHeader(hdrCT, ctText)
			ctx.SetStatus(http.StatusOK)
			f, ok := ctx.BodyWriter().(http.Flusher)
			require.True(t, ok)
			_, _ = ctx.BodyWriter().Write([]byte("hello"))
			f.Flush()
		},
	)
	assert.Equal(t, encGzip, w.header.Get(hdrCE))
	assert.Equal(t, "hello", decode(t, encGzip, w.body.Bytes()))
}

// plainWriter is an [http.ResponseWriter] that is not an [http.Flusher].
type plainWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (*plainWriter) WriteHeader(int)               {}

func TestConcurrentPoolReuse(t *testing.T) {
	t.Parallel()

	mw := mustNew(t, compression.WithDefault(compression.Config{MinSize: 1}))
	encodings := []string{encGzip, encBrotli, encZstd}
	var wg sync.WaitGroup
	for g := range 12 {
		wg.Go(func() {
			for i := range 25 {
				enc := encodings[(g+i)%len(encodings)]
				payload := bytes.Repeat([]byte(strconv.Itoa(g*1000+i)+"-"), 200+i)
				rec := httptest.NewRecorder()
				serveRec(
					mw,
					http.MethodGet,
					"/x",
					[]string{hdrAE, enc},
					rec,
					reply(200, []string{hdrCT, ctText}, string(payload)),
				)
				assert.Equal(t, enc, rec.Header().Get(hdrCE))
				assert.Equal(t, string(payload), decodeNoRequire(enc, rec.Body.Bytes()))
			}
		})
	}
	wg.Wait()
}

func TestPanicReturnsResourcesAndWritesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		writeBefore string
	}{
		{name: "before any write"},
		{name: "after a small buffered write", writeBefore: "{}"},
		{name: "after the stream started", writeBefore: big()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mw := mustNew(t)
			rec := httptest.NewRecorder()
			assert.PanicsWithValue(t, "boom", func() {
				serveRec(mw, http.MethodGet, "/x", []string{hdrAE, encGzip}, rec, func(ctx huma.Context) {
					ctx.SetHeader(hdrCT, ctJSON)
					ctx.SetStatus(http.StatusOK)
					_, _ = ctx.BodyWriter().Write([]byte(tt.writeBefore))
					panic("boom")
				})
			})
			// The pools still hand out working encoders.
			for _, enc := range []string{encGzip, encBrotli, encZstd} {
				res := serve(t, mw, http.MethodGet, []string{hdrAE, enc}, reply(200, []string{hdrCT, ctText}, big()))
				assert.Equal(t, enc, res.Header.Get(hdrCE))
				assert.Equal(t, big(), res.decoded(t))
			}
		})
	}
}

var errSink = errors.New("sink failed")

// failingWriter is a body writer that always fails.
type failingWriter struct{}

func (*failingWriter) Write([]byte) (int, error) { return 0, errSink }

func TestSinkErrors(t *testing.T) {
	t.Parallel()

	incompressible := make([]byte, 1<<20)
	_, err := rand.Read(incompressible)
	require.NoError(t, err)
	small := []byte("payload one payload two")
	const chunk = 32 << 10
	tests := []struct {
		name        string
		ae          string
		payload     []byte
		wantWriteEr bool
	}{
		{name: "passthrough write fails", ae: "", payload: small, wantWriteEr: true},
		{name: "gzip header write fails", ae: encGzip, payload: small, wantWriteEr: true},
		{name: "brotli reports a sink error only at close", ae: encBrotli, payload: incompressible},
		{name: "zstd stream fails", ae: encZstd, payload: incompressible, wantWriteEr: true},
		{name: "brotli close fails silently", ae: encBrotli, payload: small},
		{name: "zstd close fails silently", ae: encZstd, payload: small},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			req.Header.Set(hdrAE, tt.ae)
			ctx := newContextWithBody(req, httptest.NewRecorder(), &failingWriter{})
			var writeErr error
			mustNew(t, compression.WithDefault(compression.Config{MinSize: 1}))(ctx, func(c huma.Context) {
				c.SetHeader(hdrCT, ctText)
				c.SetStatus(http.StatusOK)
				for off := 0; off < len(tt.payload) && writeErr == nil; off += chunk {
					_, writeErr = c.BodyWriter().Write(tt.payload[off:min(off+chunk, len(tt.payload))])
				}
			})
			if tt.wantWriteEr {
				require.ErrorIs(t, writeErr, errSink)
			} else {
				assert.NoError(t, writeErr)
			}
		})
	}
}

func TestLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T, mw compression.Middleware)
		name string
		opts []compression.Option
	}{
		{
			name: "status is zero until set",
			run: func(t *testing.T, mw compression.Middleware) {
				t.Helper()
				var before, after int
				serve(t, mw, http.MethodGet, nil, func(ctx huma.Context) {
					before = ctx.Status()
					ctx.SetStatus(http.StatusCreated)
					after = ctx.Status()
				})
				assert.Equal(t, 0, before)
				assert.Equal(t, http.StatusCreated, after)
			},
		},
		{
			name: "writing after the response ended fails",
			run: func(t *testing.T, mw compression.Middleware) {
				t.Helper()
				var w io.Writer
				serve(t, mw, http.MethodGet, []string{hdrAE, encGzip}, func(ctx huma.Context) {
					w = ctx.BodyWriter()
					reply(200, []string{hdrCT, ctJSON}, big())(ctx)
				})
				n, err := w.Write([]byte("late"))
				assert.Equal(t, 0, n)
				require.ErrorIs(t, err, io.ErrClosedPipe)
			},
		},
		{
			name: "a failing sink does not panic when the buffered body is released",
			run: func(t *testing.T, mw compression.Middleware) {
				t.Helper()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
				req.Header.Set(hdrAE, encGzip)
				ctx := newContextWithBody(req, httptest.NewRecorder(), &failingWriter{})
				assert.NotPanics(t, func() { mw(ctx, reply(200, []string{hdrCT, ctText}, "tiny")) })
			},
		},
		{
			name: "an operation-less context is never skipped by operation",
			opts: []compression.Option{compression.WithSkipOperations(opID)},
			run: func(t *testing.T, mw compression.Middleware) {
				t.Helper()
				rec := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
				req.Header.Set(hdrAE, encGzip)
				mw(newContextNoOp(req, rec), reply(200, []string{hdrCT, ctJSON}, big()))
				assert.Equal(t, encGzip, resultOf(t, rec).Header.Get(hdrCE))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, mustNew(t, tt.opts...))
		})
	}
}
