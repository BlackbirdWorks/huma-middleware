package compression_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/compression"
)

// countingWriter counts the bytes of a response.
type countingWriter struct {
	h http.Header
	n int
}

func (w *countingWriter) Header() http.Header { return w.h }
func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)

	return len(p), nil
}
func (*countingWriter) WriteHeader(int) {}

// apiBody returns a representative API response: a list of records with ids, names, e-mail addresses, timestamps
// and nested tags, about 25 KB of JSON.
func apiBody(b *testing.B) []byte {
	b.Helper()
	type record struct {
		Created string   `json:"created"`
		Name    string   `json:"name"`
		Email   string   `json:"email"`
		Status  string   `json:"status"`
		Tags    []string `json:"tags"`
		ID      int      `json:"id"`
		Score   float64  `json:"score"`
	}
	statuses := []string{"active", "pending", "suspended", "archived"}
	records := make([]record, 0, 120)
	for i := range 120 {
		records = append(records, record{
			ID:     1000 + i*7,
			Name:   "User " + strconv.Itoa(i*37%97) + " Example",
			Email:  "user" + strconv.Itoa(i*13%211) + "@example.com",
			Status: statuses[i%len(statuses)],
			Created: "2026-0" + strconv.Itoa(
				1+i%9,
			) + "-1" + strconv.Itoa(
				i%10,
			) + "T10:" + strconv.Itoa(
				10+i%50,
			) + ":00Z",
			Score: float64(i*29%1000) / 10,
			Tags:  []string{"tag-" + strconv.Itoa(i%11), "group-" + strconv.Itoa(i%5)},
		})
	}
	body, err := json.Marshal(map[string]any{"items": records, "total": len(records)})
	require.NoError(b, err)

	return body
}

// textBody returns about 20 KB of HTML prose: paragraphs built from a vocabulary, with repeated markup.
func textBody() []byte {
	words := strings.Fields(`the quick brown fox jumps over a lazy dog while compression algorithms trade speed
		for size and every response that leaves a server is measured in bytes latency and cost so a sensible default
		matters more than a clever one because most services never tune it and the network is shared by many clients`)
	var sb strings.Builder
	sb.WriteString("<!doctype html><html><head><title>Report</title></head><body>\n")
	for i := range 40 {
		sb.WriteString("<section class=\"entry\"><h2>Entry " + strconv.Itoa(i) + "</h2><p>")
		for j := range 55 {
			sb.WriteString(words[(i*7+j*13+j*j)%len(words)])
			sb.WriteByte(' ')
		}
		sb.WriteString("</p></section>\n")
	}
	sb.WriteString("</body></html>\n")

	return []byte(sb.String())
}

// BenchmarkLevels compares the compression levels on a representative JSON body and on a text (HTML) body:
// ns/op, and the ratio of the compressed size to the original (lower is better) as a custom metric.
func BenchmarkLevels(b *testing.B) {
	bodies := []struct {
		name, ct string
		body     []byte
	}{
		{"json", ctJSON, apiBody(b)},
		{"text", ctHTML, textBody()},
	}
	for _, bd := range bodies {
		b.Run(bd.name, func(b *testing.B) { benchLevels(b, bd.ct, bd.body) })
	}
}

func benchLevels(b *testing.B, ct string, body []byte) {
	b.Helper()
	tests := []struct {
		enc    compression.Encoding
		levels []int
	}{
		{compression.Gzip, []int{1, 2, 4, 5, 6, 9}},
		{compression.Brotli, []int{1, 3, 4, 5, 6, 8, 11}},
		{compression.Zstd, []int{1, 3, 5, 9, 19}},
	}
	for _, tt := range tests {
		for _, level := range tt.levels {
			b.Run(string(tt.enc)+"/"+strconv.Itoa(level), func(b *testing.B) {
				var levels compression.Levels
				switch tt.enc {
				case compression.Brotli:
					levels.Brotli = new(level)
				case compression.Zstd:
					levels.Zstd = new(level)
				default:
					levels.Gzip = new(level)
				}
				mw, err := compression.New(compression.WithDefault(compression.Config{
					Levels: levels, Preference: []compression.Encoding{tt.enc},
				}))
				require.NoError(b, err)
				req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
				req.Header.Set("Accept-Encoding", string(tt.enc))
				op := &huma.Operation{OperationID: "x", Path: "/x"}
				w := &countingWriter{h: http.Header{}}
				next := reply(http.StatusOK, []string{hdrCT, ct}, string(body))
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for b.Loop() {
					clear(w.h)
					w.n = 0
					mw(humatest.NewContext(op, req, w), next)
				}
				b.ReportMetric(float64(w.n)/float64(len(body)), "ratio")
			})
		}
	}
}
