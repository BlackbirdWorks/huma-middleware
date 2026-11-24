package compression_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

const (
	ctJSON     = "application/json"
	ctText     = "text/plain; charset=utf-8"
	hdrAE      = "Accept-Encoding"
	hdrCE      = "Content-Encoding"
	hdrCL      = "Content-Length"
	hdrVary    = "Vary"
	hdrETag    = "ETag"
	hdrCT      = "Content-Type"
	encGzip    = "gzip"
	encBrotli  = "br"
	encZstd    = "zstd"
	varyAE     = "Accept-Encoding"
	strongETag = `"abc"`
	weakETag   = `W/"abc"`
	allAE      = "gzip, br, zstd"

	ctHTML   = "text/html"
	ctSSE    = "text/event-stream"
	ctPNG    = "image/png"
	ctOctet  = "application/octet-stream"
	pathList = "/list"
	pathAPI  = "/api/data"
	origin   = "Origin"
	ctCSS    = "text/css"
	encFake  = "deflate"
	opID     = "op-x"
)

// big is a compressible body above the default minimum size.
func big() string { return strings.Repeat("hello compressible world ", 200) }

func mustNew(t *testing.T, opts ...compression.Option) compression.Middleware {
	t.Helper()
	mw, err := compression.New(opts...)
	require.NoError(t, err)

	return mw
}

// reply sets the headers (name, value pairs), the status and writes the chunks.
func reply(status int, headers []string, chunks ...string) func(huma.Context) {
	return func(ctx huma.Context) {
		for i := 0; i+1 < len(headers); i += 2 {
			ctx.SetHeader(headers[i], headers[i+1])
		}
		ctx.SetStatus(status)
		for _, c := range chunks {
			_, _ = ctx.BodyWriter().Write([]byte(c))
		}
	}
}

// result is a finished response: headers, status and the body as sent.
type result struct {
	Header     http.Header
	Raw        []byte
	StatusCode int
}

// resultOf reads a recorded response.
func resultOf(t *testing.T, rec *httptest.ResponseRecorder) result {
	t.Helper()
	res := rec.Result()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	return result{Header: res.Header, Raw: raw, StatusCode: res.StatusCode}
}

// decoded returns the body decoded according to Content-Encoding.
func (r result) decoded(t *testing.T) string {
	t.Helper()

	return decode(t, r.Header.Get(hdrCE), r.Raw)
}

// serve runs the middleware around next on a humatest context.
func serve(
	t *testing.T, mw compression.Middleware, method string, reqHeaders []string, next func(huma.Context),
) result {
	t.Helper()
	rec := httptest.NewRecorder()
	serveRec(mw, method, "/x", reqHeaders, rec, next)

	return resultOf(t, rec)
}

func serveRec(
	mw compression.Middleware, method, target string, reqHeaders []string, w http.ResponseWriter,
	next func(huma.Context),
) {
	req := httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody)
	for i := 0; i+1 < len(reqHeaders); i += 2 {
		req.Header.Set(reqHeaders[i], reqHeaders[i+1])
	}
	op := &huma.Operation{OperationID: opID, Path: target}
	mw(humatest.NewContext(op, req, w), next)
}

func decode(t *testing.T, enc string, raw []byte) string {
	t.Helper()
	var r io.Reader
	switch enc {
	case "":
		return string(raw)
	case encGzip:
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		require.NoError(t, err)
		r = gr
	case encBrotli:
		r = brotli.NewReader(bytes.NewReader(raw))
	case encZstd:
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		require.NoError(t, err)
		defer zr.Close()
		r = zr
	default:
		require.Failf(t, "unknown encoding", "%q", enc)
	}
	out, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(out)
}

// decodeNoRequire is decode for use off the test goroutine; it returns an empty string on failure.
func decodeNoRequire(enc string, raw []byte) string {
	var r io.Reader
	switch enc {
	case encGzip:
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return ""
		}
		r = gr
	case encBrotli:
		r = brotli.NewReader(bytes.NewReader(raw))
	default:
		zr, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return ""
		}
		defer zr.Close()
		r = zr
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return ""
	}

	return string(out)
}

// levelOpt sets the default compression level of one encoding.
func levelOpt(enc compression.Encoding, level int) compression.Option {
	var levels compression.Levels
	switch enc {
	case compression.Brotli:
		levels.Brotli = new(level)
	case compression.Zstd:
		levels.Zstd = new(level)
	default:
		levels.Gzip = new(level)
	}

	return compression.WithDefault(compression.Config{Levels: levels})
}
