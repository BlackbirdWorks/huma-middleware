package compression_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"

	"github.com/blackbirdworks/huma-middleware/internal/racedetect"
	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

// TestAllocs pins what the middleware allocates on top of the handler.
func TestAllocs(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics in parallel tests
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "added allocations", run: runAddedAllocations},
	}
	for _, tt := range tests { //nolint:paralleltest // AllocsPerRun panics in parallel tests
		t.Run(tt.name, tt.run)
	}
}

// runAddedAllocations pins what the middleware allocates on top of the handler, per kind of response: nothing
// when it has no reason to touch the response, and only what the response itself needs (the Vary header, the
// Content-Encoding header) otherwise. It is sequential: [testing.AllocsPerRun] panics in parallel tests.
func runAddedAllocations(t *testing.T) {
	t.Helper()
	if racedetect.Enabled {
		t.Skip("allocation counts mean nothing under the race detector; see task allocs")
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	req.Header.Set(hdrAE, "gzip, br, zstd")
	noAE := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	op := &huma.Operation{OperationID: opID, Path: "/x"}
	big, tiny := strings.Repeat("compressible ", 2000), "tiny"

	tests := []struct {
		req   *http.Request
		name  string
		ct    string
		body  string
		opts  []compression.Option
		added float64
	}{
		{
			name: "skipped by path",
			req:  req,
			opts: []compression.Option{compression.WithSkipPathPrefixes("/x")},
			ct:   ctJSON,
			body: big,
		},
		{name: "not compressible", req: req, ct: ctPNG, body: big},
		{name: noCT, req: req, ct: "", body: big},
		{
			name: "disabled type",
			req:  req,
			opts: []compression.Option{compression.WithContentType(ctJSON, compression.Config{Disabled: true})},
			ct:   ctJSON,
			body: big,
		},
		{name: "event stream", req: req, ct: ctSSE, body: big},
		{name: "client accepts nothing: Vary only", req: noAE, ct: ctJSON, body: big, added: 1},
		{name: "below the minimum: Vary only", req: req, ct: ctJSON, body: tiny, added: 1},
		{name: "compressed: Vary and Content-Encoding", req: req, ct: ctJSON, body: big, added: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := mustNew(t, tt.opts...)
			var hdr []string
			if tt.ct != "" {
				hdr = []string{hdrCT, tt.ct}
			}
			next := reply(200, hdr, tt.body)
			bare := testing.AllocsPerRun(50, func() {
				next(humatest.NewContext(op, tt.req, &discardWriter{h: http.Header{}}))
			})
			wrapped := testing.AllocsPerRun(50, func() {
				mw(humatest.NewContext(op, tt.req, &discardWriter{h: http.Header{}}), next)
			})
			assert.InDelta(t, tt.added, wrapped-bare, 0)
		})
	}
}
