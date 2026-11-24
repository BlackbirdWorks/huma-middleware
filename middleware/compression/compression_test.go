package compression_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"

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
			name: "no content type", reqHeaders: []string{hdrAE, allAE}, next: reply(200, nil, big()),
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

func TestHeadersAndBodies(t *testing.T) {
	t.Parallel()

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
