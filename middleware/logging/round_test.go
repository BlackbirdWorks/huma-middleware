package logging_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

func multipartRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	return req
}

// TestHandlerGivesBackWhatItsCopyLearned checks the multipart form (whose temporary files net/http removes through
// the original request) and the routing pattern (read by the wrappers around).
func TestHandlerGivesBackWhatItsCopyLearned(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger(slog.LevelDebug)
	var seen *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen = r
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		assert.NoError(t, r.ParseMultipartForm(1)) //nolint:gosec // G120: the test body is a few bytes
	})
	h := logging.Handler(mux, logging.WithLogger(logger))
	req := multipartRequest(t, "/upload/7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NotNil(t, seen)
	require.NotNil(t, req.MultipartForm, "the original request holds the form, so the server removes its files")
	assert.Same(t, seen.MultipartForm, req.MultipartForm)
	assert.Equal(t, "POST /upload/{id}", req.Pattern)
}

// hijackable is a response writer that can be hijacked.
type hijackable struct {
	*httptest.ResponseRecorder
}

func (hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

func TestHandlerOnlyAdvertisesHijackWhenItCan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		w         http.ResponseWriter
		name      string
		wantAdver bool
	}{
		{name: "a hijacker", w: hijackable{httptest.NewRecorder()}, wantAdver: true},
		{name: "a writer that cannot be hijacked", w: httptest.NewRecorder()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var advertised bool
			var hijackErr error
			h := logging.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				advertised = ok
				if ok {
					_, _, hijackErr = hj.Hijack()
				}
			}), logging.WithLogger(logger))
			h.ServeHTTP(tt.w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))
			assert.Equal(t, tt.wantAdver, advertised)
			assert.NoError(t, hijackErr)
		})
	}
}

// TestCounterIsSafeForStreamingAdapters writes from another goroutine while the access log reads the size.
func TestCounterIsSafeForStreamingAdapters(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger(slog.LevelDebug)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
	var wg sync.WaitGroup
	sink := &syncSink{}
	hctx := &asyncStreamer{ctxBase: base, sink: sink, wg: &wg}
	logging.New(logging.WithLogger(logger))(hctx, func(c huma.Context) {
		bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
		require.True(t, ok)
		bs.StreamBody(func(w io.Writer) {
			for range 200 {
				_, _ = w.Write([]byte("x"))
				runtime.Gosched()
			}
		})
	})
	wg.Wait()
	assert.Equal(t, 200, sink.len())
}

// syncSink is a stream writer.
type syncSink struct {
	mu sync.Mutex // guards buf
	n  int
}

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n += len(p)

	return len(p), nil
}

func (s *syncSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.n
}

// asyncStreamer runs the stream callback on another goroutine, as fasthttp does.
type asyncStreamer struct {
	ctxBase

	sink io.Writer
	wg   *sync.WaitGroup
}

func (a *asyncStreamer) StreamBody(fn func(io.Writer)) {
	a.wg.Go(func() { fn(a.sink) })
}

// deadlineStream is a stream writer with write deadlines.
type deadlineStream struct {
	deadline time.Time
	buf      bytes.Buffer
}

func (d *deadlineStream) Write(p []byte) (int, error) { return d.buf.Write(p) }

func (d *deadlineStream) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

func TestWriteDeadlineThroughTheCounter(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	logger, _ := newLogger(slog.LevelDebug)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
	sink := &deadlineStream{}
	hctx := streamingContext{&bodyContext{ctxBase: base, body: &bytes.Buffer{}, streamed: sink}}
	var got error
	logging.New(logging.WithLogger(logger))(hctx, func(c huma.Context) {
		bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
		require.True(t, ok, "the hook is forwarded")
		bs.StreamBody(func(w io.Writer) {
			dl, isDL := w.(interface{ SetWriteDeadline(time.Time) error })
			require.True(t, isDL)
			got = dl.SetWriteDeadline(deadline)
		})
	})
	require.NoError(t, got)
	assert.Equal(t, deadline, sink.deadline)
}

type idRuleCase struct {
	reqHeaders map[string]string
	outer      string
	innerSet   string
	name       string
	wantRecord string
	wantHeader string
	opts       []logging.Option
}

func idRuleCases() []idRuleCase {
	return []idRuleCase{
		{
			name:  "an ID an outer wrapper set wins over the generator",
			outer: idOuter,
			opts: []logging.Option{
				logging.WithRequestIDGenerator(func() string { return idGenerated }),
			},
			wantRecord: idOuter,
			wantHeader: idOuter,
		},
		{
			name:       "the generator is last",
			opts:       []logging.Option{logging.WithRequestIDGenerator(func() string { return idGenerated })},
			wantRecord: idGenerated,
			wantHeader: idGenerated,
		},
		{
			name: "the record reports what the handler left on the response", innerSet: idInner,
			opts:       []logging.Option{logging.WithRequestIDGenerator(func() string { return idGenerated })},
			wantRecord: idInner, wantHeader: idInner,
		},
		{
			name:       "the request header wins over everything",
			reqHeaders: map[string]string{hdrID: idFromRequest},
			outer:      idOuter,
			innerSet:   idInner,
			opts:       []logging.Option{logging.WithRequestIDGenerator(func() string { return idGenerated })},
			wantRecord: idFromRequest,
			wantHeader: idInner,
		},
	}
}

func TestRequestIDRulesHandler(t *testing.T) {
	t.Parallel()

	tests := idRuleCases()
	for _, tt := range tests {
		t.Run(tt.name+"", func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec logging.Record
			opts := append([]logging.Option{logging.WithLogger(logger)}, tt.opts...)
			opts = append(
				opts,
				logging.WithAccessLogFunc(func(_ context.Context, _ *slog.Logger, r logging.Record) { rec = r }),
			)
			h := logging.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.innerSet != "" {
					w.Header().Set(hdrID, tt.innerSet)
				}
			}), opts...)
			w := httptest.NewRecorder()
			if tt.outer != "" {
				w.Header().Set(hdrID, tt.outer)
			}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(w, req)
			assert.Equal(t, tt.wantRecord, rec.RequestID)
			assert.Equal(t, tt.wantHeader, w.Header().Get(hdrID))
		})
	}
}

func TestRequestIDRulesHuma(t *testing.T) {
	t.Parallel()

	tests := idRuleCases()
	// The huma form cannot see a header an outer middleware set; the other rules are the same.
	for _, tt := range tests {
		if tt.outer != "" {
			continue
		}
		t.Run(tt.name+"/huma", func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec logging.Record
			opts := append([]logging.Option{logging.WithLogger(logger)}, tt.opts...)
			opts = append(
				opts,
				logging.WithAccessLogFunc(func(_ context.Context, _ *slog.Logger, r logging.Record) { rec = r }),
			)
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, w)
			logging.New(opts...)(hctx, func(c huma.Context) {
				if tt.innerSet != "" {
					c.SetHeader(hdrID, tt.innerSet)
				}
			})
			assert.Equal(t, tt.wantRecord, rec.RequestID)
			assert.Equal(t, tt.wantHeader, w.Header().Get(hdrID))
		})
	}
}
