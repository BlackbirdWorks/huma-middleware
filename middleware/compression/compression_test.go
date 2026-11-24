package compression_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

type headerCase struct {
	next         func(huma.Context)
	name         string
	method       string
	wantEncoding string
	wantCL       string
	wantETag     string
	wantBody     string
	opts         []compression.Option
	reqHeaders   []string
	wantVary     []string
	wantStatus   int
	passedAsIs   bool
}

// jsonHeaders are the response headers of a JSON reply.
func jsonHeaders() []string { return []string{hdrCT, ctJSON} }

func headerCases1() []headerCase {
	return []headerCase{
		{
			name: "json prefers zstd", reqHeaders: []string{hdrAE, allAE}, next: reply(200, jsonHeaders(), big()),
			wantEncoding: encZstd, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "problem json prefers zstd", reqHeaders: []string{hdrAE, allAE},
			next:         reply(404, []string{hdrCT, "application/problem+json"}, big()),
			wantEncoding: encZstd, wantVary: []string{varyAE}, wantStatus: 404, wantBody: big(),
		},
		{
			name:         "json falls back to brotli",
			reqHeaders:   []string{hdrAE, "br, gzip"},
			next:         reply(200, jsonHeaders(), big()),
			wantEncoding: encBrotli,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     big(),
		},
		{
			name:         "json falls back to gzip",
			reqHeaders:   []string{hdrAE, encGzip},
			next:         reply(200, jsonHeaders(), big()),
			wantEncoding: encGzip,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     big(),
		},
		{
			name:         "text prefers brotli",
			reqHeaders:   []string{hdrAE, allAE},
			next:         reply(200, []string{hdrCT, ctText}, big()),
			wantEncoding: encBrotli,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     big(),
		},
		{
			name:         "text uses zstd as a last resort",
			reqHeaders:   []string{hdrAE, encZstd},
			next:         reply(200, []string{hdrCT, ctText}, big()),
			wantEncoding: encZstd,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     big(),
		},
		{
			name:       "client q wins over preference",
			reqHeaders: []string{hdrAE, "zstd;q=0.2, gzip;q=0.8"},
			next: reply(
				200,
				jsonHeaders(),
				big(),
			),
			wantEncoding: encGzip,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     big(),
		},
		{
			name: "no accept-encoding still varies", next: reply(200, jsonHeaders(), big()),
			wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "identity only", reqHeaders: []string{hdrAE, encIdent}, next: reply(200, jsonHeaders(), big()),
			wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name:       "all excluded",
			reqHeaders: []string{hdrAE, "gzip;q=0, br;q=0, zstd;q=0"},
			next:       reply(200, jsonHeaders(), big()),
			wantVary:   []string{varyAE},
			wantStatus: 200,
			wantBody:   big(),
		},
		{
			name:       "small body is not compressed",
			reqHeaders: []string{hdrAE, allAE},
			next:       reply(200, jsonHeaders(), `{"a":1}`),
			wantVary:   []string{varyAE},
			wantStatus: 200,
			wantBody:   `{"a":1}`,
		},
	}
}

func headerCases2() []headerCase {
	return []headerCase{
		{
			name:         "small chunks reach the threshold",
			reqHeaders:   []string{hdrAE, encGzip},
			next:         reply(200, jsonHeaders(), strings.Split(big(), " ")...),
			wantEncoding: encGzip,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     strings.ReplaceAll(big(), " ", ""),
		},
		{
			name: "empty body", reqHeaders: []string{hdrAE, allAE}, next: reply(200, jsonHeaders()),
			wantVary: []string{varyAE}, wantStatus: 200,
		},
		{
			name:         "empty write ignored",
			reqHeaders:   []string{hdrAE, encGzip},
			next:         reply(200, jsonHeaders(), "", big()),
			wantEncoding: encGzip,
			wantVary:     []string{varyAE},
			wantStatus:   200,
			wantBody:     big(),
		},
		{
			name: "small content-length kept", reqHeaders: []string{hdrAE, allAE},
			next: reply(200, []string{hdrCT, ctJSON, hdrCL, "7"}, `{"a":1}`), wantCL: "7",
			wantVary: []string{varyAE}, wantStatus: 200, wantBody: `{"a":1}`,
		},
		{
			name: "content-length dropped when compressing", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrCL, "4800"}, big()),
			wantEncoding: encGzip, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "invalid content-length is ignored", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrCL, "abc"}, big()),
			wantEncoding: encGzip, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "negative content-length is ignored", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrCL, "-4"}, big()),
			wantEncoding: encGzip, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name:       "image never compressed",
			reqHeaders: []string{hdrAE, allAE},
			next:       reply(200, []string{hdrCT, ctPNG}, big()),
			wantStatus: 200,
			wantBody:   big(),
		},
		{
			name: "event stream passes through", reqHeaders: []string{hdrAE, allAE},
			next: reply(200, []string{hdrCT, ctSSE}, big()), wantStatus: 200, wantBody: big(),
		},
		{
			name: noCT, reqHeaders: []string{hdrAE, allAE}, next: reply(200, nil, big()),
			wantStatus: 200, wantBody: big(),
		},
		{
			name: "head is not compressed but varies", method: http.MethodHead, reqHeaders: []string{hdrAE, allAE},
			next: reply(200, []string{hdrCT, ctJSON, hdrCL, "4800"}), wantCL: "4800",
			wantVary: []string{varyAE}, wantStatus: 200,
		},
	}
}

func headerCases3() []headerCase {
	return []headerCase{
		{
			name: "head of an image has no vary", method: http.MethodHead, reqHeaders: []string{hdrAE, allAE},
			next: reply(200, []string{hdrCT, ctPNG}), wantStatus: 200,
		},
		{
			name: "204 untouched", reqHeaders: []string{hdrAE, allAE}, next: reply(204, jsonHeaders()), wantStatus: 204,
		},
		{
			name: "304 varies and keeps the etag", reqHeaders: []string{hdrAE, allAE},
			next: reply(304, []string{hdrETag, strongETag}), wantETag: strongETag,
			wantVary: []string{varyAE}, wantStatus: 304,
		},
		{
			name: "206 untouched", reqHeaders: []string{hdrAE, allAE}, next: reply(206, jsonHeaders(), big()),
			wantStatus: 206, wantBody: big(),
		},
		{
			name: "1xx untouched", reqHeaders: []string{hdrAE, allAE}, next: reply(103, jsonHeaders()),
			wantStatus: 103,
		},
		{
			name: "already encoded", reqHeaders: []string{hdrAE, allAE},
			next:         reply(200, []string{hdrCT, ctJSON, hdrCE, encGzip}, "raw-bytes"),
			wantEncoding: encGzip, wantStatus: 200, wantBody: "raw-bytes", passedAsIs: true,
		},
		{
			name: "identity encoding is compressed", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrCE, "Identity"}, big()),
			wantEncoding: encGzip, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "no-transform", reqHeaders: []string{hdrAE, allAE},
			next:       reply(200, []string{hdrCT, ctJSON, "Cache-Control", "public, No-Transform"}, big()),
			wantStatus: 200, wantBody: big(),
		},
		{
			name: "cache-control appended", reqHeaders: []string{hdrAE, allAE},
			next: func(ctx huma.Context) {
				ctx.AppendHeader("Cache-Control", "public")
				ctx.AppendHeader("Cache-Control", "no-transform")
				reply(200, jsonHeaders(), big())(ctx)
			},
			wantStatus: 200, wantBody: big(),
		},
		{
			name: "cache-control other is fine", reqHeaders: []string{hdrAE, encGzip},
			next: func(ctx huma.Context) {
				ctx.AppendHeader("Cache-Control", "public")
				reply(200, jsonHeaders(), big())(ctx)
			},
			wantEncoding: encGzip, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "vary is appended", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrVary, origin}, big()),
			wantEncoding: encGzip, wantVary: []string{origin, varyAE}, wantStatus: 200, wantBody: big(),
		},
	}
}

func headerCases4() []headerCase {
	return []headerCase{
		{
			name: "vary already lists accept-encoding", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrVary, "origin, accept-encoding"}, big()),
			wantEncoding: encGzip, wantVary: []string{"origin, accept-encoding"}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "vary star", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrVary, "*"}, big()),
			wantEncoding: encGzip, wantVary: []string{"*"}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "vary replaced after being appended", reqHeaders: []string{hdrAE, encGzip},
			next: func(ctx huma.Context) {
				ctx.AppendHeader(hdrVary, varyAE)
				ctx.SetHeader(hdrVary, origin)
				reply(200, jsonHeaders(), big())(ctx)
			},
			wantEncoding: encGzip, wantVary: []string{origin, varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "vary appended twice by the handler", reqHeaders: []string{hdrAE, encGzip},
			next: func(ctx huma.Context) {
				ctx.AppendHeader(hdrVary, varyAE)
				ctx.AppendHeader(hdrVary, origin)
				reply(200, jsonHeaders(), big())(ctx)
			},
			wantEncoding: encGzip, wantVary: []string{varyAE, origin}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "strong etag becomes weak", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrETag, strongETag}, big()),
			wantEncoding: encGzip, wantETag: weakETag, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "weak etag kept", reqHeaders: []string{hdrAE, encGzip},
			next:         reply(200, []string{hdrCT, ctJSON, hdrETag, weakETag}, big()),
			wantEncoding: encGzip, wantETag: weakETag, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
		{
			name: "etag untouched when not compressed", reqHeaders: []string{hdrAE, encGzip},
			next:     reply(200, []string{hdrCT, ctJSON, hdrETag, strongETag}, "{}"),
			wantETag: strongETag, wantVary: []string{varyAE}, wantStatus: 200, wantBody: "{}",
		},
		{
			name: "no status and no body", reqHeaders: []string{hdrAE, allAE}, next: func(huma.Context) {},
			wantStatus: 200,
		},
		{
			name: "body without a status", reqHeaders: []string{hdrAE, encGzip},
			next: func(ctx huma.Context) {
				ctx.SetHeader(hdrCT, ctJSON)
				_, _ = ctx.BodyWriter().Write([]byte(big()))
			},
			wantEncoding: encGzip, wantVary: []string{varyAE}, wantStatus: 200, wantBody: big(),
		},
	}
}

// TestNew covers the middleware: what it compresses and how, the headers, streaming and the pooled objects.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "headers and bodies", run: runHeadersAndBodies},
		{name: "zero config pinned", run: runZeroConfigIsPinned},
		{name: "invalid options", run: runInvalidConfiguration},
		{name: "level bounds", run: runValidLevelBounds},
		{name: "split accept-encoding", run: runAcceptEncodingSplitAcrossLines},
		{name: "real adapters", run: runUnderRealAdapters},
		{name: "adapter unwrap", run: runAdapterUnwrapSeesThroughTheWrapper},
		{name: "early hints", run: runEarlyHintsAreSentAtOnce},
		{name: "streaming", run: runStreaming},
		{name: "no flusher sink", run: runStreamingWithoutFlusherSink},
		{name: "stream body hook", run: runStreamBodyAdapters},
		{name: "write deadline", run: runWriteDeadlineReachesTheWriter},
		{name: "sse write deadline", run: runServerSentEventsKeepTheirWriteDeadline},
		{name: "pool reuse", run: runConcurrentPoolReuse},
		{name: "pooled responses", run: runPooledResponsesAreNeverShared},
		{name: "panic", run: runPanicReturnsResourcesAndWritesNothing},
		{name: "sink errors", run: runSinkErrors},
		{name: "lifecycle", run: runLifecycle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runHeadersAndBodies(t *testing.T) {
	t.Helper()

	tests := slices.Concat(headerCases1(), headerCases2(), headerCases3(), headerCases4())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			res := serve(t, mustNew(t, tt.opts...), method, tt.reqHeaders, tt.next)
			assert.Equal(t, tt.wantStatus, res.StatusCode)
			assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
			assert.Equal(t, tt.wantVary, res.Header.Values(hdrVary))
			assert.Equal(t, tt.wantCL, res.Header.Get(hdrCL))
			assert.Equal(t, tt.wantETag, res.Header.Get(hdrETag))
			if tt.wantStatus == http.StatusNoContent || tt.wantStatus == http.StatusNotModified {
				return
			}
			if tt.passedAsIs {
				assert.Equal(t, tt.wantBody, string(res.Raw))

				return
			}
			assert.Equal(t, tt.wantBody, res.decoded(t))
		})
	}
}

// runZeroConfigIsPinned pins what New() does by content type, Accept-Encoding and body size, so that a change
// of a default fails here and has to be deliberate.
func runZeroConfigIsPinned(t *testing.T) {
	t.Helper()

	tests := append(jsonPinned(), othersPinned()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Repeat("compressible value 0123456789 ", tt.size/30+1)[:tt.size]
			var hdr []string
			if tt.ct != "" {
				hdr = []string{hdrCT, tt.ct}
			}
			res := serve(t, mustNew(t), http.MethodGet, []string{hdrAE, tt.ae}, reply(200, hdr, body))
			assert.Equal(t, tt.wantEnc, res.Header.Get(hdrCE))
			assert.Equal(t, tt.wantVary, len(res.Header.Values(hdrVary)) > 0)
			assert.Equal(t, body, res.decoded(t))
			if tt.wantEnc != "" {
				assert.Equal(
					t,
					reference(t, tt.wantEnc, tt.wantLevel, body),
					res.Raw,
					"compressed at level %d",
					tt.wantLevel,
				)
			}
		})
	}
}

func runInvalidConfiguration(t *testing.T) {
	t.Helper()

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

func runValidLevelBounds(t *testing.T) {
	t.Helper()

	tests := []struct {
		opt  compression.Option
		name string
	}{
		{name: "gzip low", opt: levelOpt(compression.Gzip, -2)},
		{name: "gzip default", opt: levelOpt(compression.Gzip, -1)},
		{name: "gzip high", opt: levelOpt(compression.Gzip, 9)},
		{name: "brotli low", opt: levelOpt(compression.Brotli, 0)},
		{name: "gzip no compression", opt: levelOpt(compression.Gzip, 0)},
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

func runAcceptEncodingSplitAcrossLines(t *testing.T) {
	t.Helper()

	tests := []struct {
		name  string
		want  string
		lines []string
	}{
		{name: "one line", lines: []string{"gzip;q=0.2, br"}, want: encBrotli},
		{name: "a better coding on a later line", lines: []string{"gzip;q=0.2", "br"}, want: encBrotli},
		{name: "three lines", lines: []string{"gzip;q=0.5", "br;q=0.1", "zstd;q=0.4"}, want: encGzip},
		{name: "the first line decides alone when it is the best", lines: []string{"gzip", "br;q=0.5"}, want: encGzip},
		{name: "an excluded coding on a later line", lines: []string{"br, gzip", "br;q=0"}, want: encBrotli},
		{name: "no header", lines: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			for _, l := range tt.lines {
				req.Header.Add(hdrAE, l)
			}
			mustNew(t)(humatest.NewContext(&huma.Operation{OperationID: opID, Path: "/x"}, req, rec),
				reply(200, []string{hdrCT, ctText}, big()))
			res := resultOf(t, rec)
			assert.Equal(t, tt.want, res.Header.Get(hdrCE))
			assert.Equal(t, big(), res.decoded(t))
		})
	}
}

func runUnderRealAdapters(t *testing.T) {
	t.Helper()

	tests := []struct {
		name         string
		path         string
		ae           string
		wantEncoding string
		wantContains string
	}{
		{name: "json zstd", path: pathList, ae: allAE, wantEncoding: encZstd, wantContains: itemValue},
		{name: "json brotli", path: pathList, ae: aeGzipBr, wantEncoding: encBrotli, wantContains: itemValue},
		{name: "json gzip", path: pathList, ae: encGzip, wantEncoding: encGzip, wantContains: itemValue},
		{name: "json identity", path: pathList, ae: "", wantContains: itemValue},
		{name: "flushed stream", path: "/stream", ae: encGzip, wantEncoding: encGzip, wantContains: "first;second"},
	}
	for an, mk := range adapterAPIs() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, api := mk(t)
				api.UseMiddleware(mustNew(t, compression.WithDefault(compression.Config{MinSize: 1})))
				registerRoutes(api)

				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.path, http.NoBody)
				req.Header.Set(hdrAE, tt.ae)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				res := resultOf(t, rec)
				require.Equal(t, http.StatusOK, res.StatusCode)
				assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
				assert.Equal(t, []string{varyAE}, res.Header.Values(hdrVary))
				assert.Contains(t, res.decoded(t), tt.wantContains)
			})
		}
	}
}

func runAdapterUnwrapSeesThroughTheWrapper(t *testing.T) {
	t.Helper()

	tests := []struct {
		build  func() (http.Handler, huma.API)
		unwrap func(huma.Context) *http.Request
		name   string
	}{
		{
			name: "humago",
			build: func() (http.Handler, huma.API) {
				mux := http.NewServeMux()

				return mux, humago.New(mux, huma.DefaultConfig("t", "1"))
			},
			unwrap: unwrapGo,
		},
		{
			name: "humachi",
			build: func() (http.Handler, huma.API) {
				r := chi.NewRouter()

				return r, humachi.New(r, huma.DefaultConfig("t", "1"))
			},
			unwrap: unwrapChi,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := tt.build()
			var got *http.Request
			api.UseMiddleware(mustNew(t), func(ctx huma.Context, next func(huma.Context)) {
				got = tt.unwrap(ctx)
				next(ctx)
			})
			registerRoutes(api)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, pathList, http.NoBody)
			h.ServeHTTP(httptest.NewRecorder(), req)
			require.NotNil(t, got)
			assert.Equal(t, pathList, got.URL.Path)
		})
	}
}

func runEarlyHintsAreSentAtOnce(t *testing.T) {
	t.Helper()

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

func runStreaming(t *testing.T) {
	t.Helper()

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

func runStreamingWithoutFlusherSink(t *testing.T) {
	t.Helper()

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

func runStreamBodyAdapters(t *testing.T) {
	t.Helper()

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
			opts: []compression.Option{compression.WithDefault(compression.Config{MinSize: 1})},
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

func runWriteDeadlineReachesTheWriter(t *testing.T) {
	t.Helper()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	dw := &deadlineWriter{}
	tests := []struct {
		sink    func() io.Writer
		wantErr error
		name    string
	}{
		{name: "a writer with deadlines", sink: func() io.Writer { return dw }},
		{
			name:    "a response writer without deadlines",
			sink:    func() io.Writer { return httptest.NewRecorder() },
			wantErr: http.ErrNotSupported,
		},
		{name: "a plain writer", sink: func() io.Writer { return &bytes.Buffer{} }, wantErr: http.ErrNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			base := newContextWithBody(req, httptest.NewRecorder(), tt.sink())
			var got error
			mustNew(t)(base, func(c huma.Context) {
				dl, ok := c.BodyWriter().(interface{ SetWriteDeadline(time.Time) error })
				require.True(t, ok, "the body writer keeps the capability")
				got = dl.SetWriteDeadline(deadline)
			})
			require.ErrorIs(t, got, tt.wantErr)
			if tt.wantErr == nil {
				assert.Equal(t, deadline, dw.deadline)
			}
		})
	}
}

// runServerSentEventsKeepTheirWriteDeadline runs huma's SSE under a real adapter, through the middleware, to a
// writer that records deadlines: huma sets one before each event.
func runServerSentEventsKeepTheirWriteDeadline(t *testing.T) {
	t.Helper()

	tests := []struct {
		name         string
		wantEncoding string
		opts         []compression.Option
	}{
		{name: "not compressed by default"},
		{
			name: "compressed on request", wantEncoding: encGzip,
			opts: []compression.Option{
				compression.WithContentTypes(ctSSE),
				compression.WithDefault(compression.Config{MinSize: 1}),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("t", "1"))
			api.UseMiddleware(mustNew(t, tt.opts...))
			sse.Register(api, huma.Operation{OperationID: "events", Method: http.MethodGet, Path: "/events"},
				map[string]any{"message": &message{}},
				func(_ context.Context, _ *struct{}, send sse.Sender) {
					assert.NoError(t, send.Data(&message{Text: "one"}))
					assert.NoError(t, send.Data(&message{Text: "two"}))
				})

			w := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events", http.NoBody)
			req.Header.Set(hdrAE, encGzip)
			mux.ServeHTTP(w, req)

			res := resultOf(t, w.ResponseRecorder)
			assert.Equal(t, tt.wantEncoding, res.Header.Get(hdrCE))
			assert.Contains(t, res.decoded(t), "one")
			assert.Contains(t, res.decoded(t), "two")
			assert.GreaterOrEqual(t, w.count(), 2, "a write deadline was set for each event")
		})
	}
}

func runConcurrentPoolReuse(t *testing.T) {
	t.Helper()

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

// runPooledResponsesAreNeverShared runs, through one middleware and from many goroutines, requests that finish
// normally, that panic, and that stream from a goroutine outliving the handler: a pooled response must never be
// used by two requests at once, nor by a stream after it was given back. Run it with -race.
func runPooledResponsesAreNeverShared(t *testing.T) {
	t.Helper()

	mw := mustNew(t, compression.WithDefault(compression.Config{MinSize: 1}))
	scenarios := []struct {
		run  func(t *testing.T, i int)
		name string
	}{
		{name: "normal", run: func(t *testing.T, i int) {
			t.Helper()
			payload := string(bytes.Repeat([]byte(strconv.Itoa(i)+"-"), 100+i))
			rec := httptest.NewRecorder()
			serveRec(
				mw,
				http.MethodGet,
				"/x",
				[]string{hdrAE, encGzip},
				rec,
				reply(200, []string{hdrCT, ctText}, payload),
			)
			assert.Equal(t, payload, decodeNoRequire(encGzip, rec.Body.Bytes()))
		}},
		{name: "panic", run: func(t *testing.T, i int) {
			t.Helper()
			rec := httptest.NewRecorder()
			assert.Panics(t, func() {
				serveRec(mw, http.MethodGet, "/x", []string{hdrAE, encZstd}, rec, func(ctx huma.Context) {
					ctx.SetHeader(hdrCT, ctText)
					_, _ = ctx.BodyWriter().Write([]byte(strconv.Itoa(i)))
					panic("boom")
				})
			})
		}},
		{name: "stream outliving the handler", run: func(t *testing.T, i int) {
			t.Helper()
			payload := string(bytes.Repeat([]byte(strconv.Itoa(i)+"-"), 100+i))
			rec := httptest.NewRecorder()
			var done sync.WaitGroup
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", http.NoBody)
			req.Header.Set(hdrAE, encBrotli)
			base := newContextWithBody(req, rec, rec)
			mw(&asyncStreamer{ctxBase: base, sink: rec, done: &done}, func(ctx huma.Context) {
				ctx.SetHeader(hdrCT, ctText)
				ctx.SetStatus(http.StatusOK)
				bs, ok := ctx.(interface{ StreamBody(func(io.Writer)) })
				assert.True(t, ok, "the hook is forwarded")
				bs.StreamBody(func(w io.Writer) { _, _ = w.Write([]byte(payload)) })
			})
			// Other requests run while the stream is still being written.
			serveRec(
				mw,
				http.MethodGet,
				"/x",
				[]string{hdrAE, encGzip},
				httptest.NewRecorder(),
				reply(200, []string{hdrCT, ctText}, "other"),
			)
			done.Wait()
			assert.Equal(t, payload, decodeNoRequire(encBrotli, rec.Body.Bytes()))
		}},
	}
	for _, sc := range scenarios {
		for g := range 4 {
			t.Run(sc.name+"/"+strconv.Itoa(g), func(t *testing.T) {
				t.Parallel()
				for i := range 40 {
					sc.run(t, g*100+i)
				}
			})
		}
	}
	require.NotNil(t, mw)
}

func runPanicReturnsResourcesAndWritesNothing(t *testing.T) {
	t.Helper()

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

func runSinkErrors(t *testing.T) {
	t.Helper()

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

func runLifecycle(t *testing.T) {
	t.Helper()

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
