package logging_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

// ctxBase lets the test contexts embed a context and override its methods.
type ctxBase huma.Context

// bodyContext is a context with another body writer, optionally with a stream callback like Fiber's.
type bodyContext struct {
	ctxBase

	body     io.Writer
	streamed io.Writer
}

func (c *bodyContext) BodyWriter() io.Writer { return c.body }

// streamingContext adds the StreamBody hook of adapters that stream through a callback.
type streamingContext struct {
	*bodyContext
}

func (c streamingContext) StreamBody(fn func(io.Writer)) { fn(c.streamed) }

// flushOnly is a writer that is an [http.Flusher] but not an [http.ResponseWriter].
type flushOnly struct {
	bytes.Buffer

	flushed int
}

func (f *flushOnly) Flush() { f.flushed++ }

// plainWriter is neither.
type plainWriter struct{ bytes.Buffer }

// responseOnly is an [http.ResponseWriter] that cannot flush.
type responseOnly struct {
	header http.Header
	body   bytes.Buffer
}

func (r *responseOnly) Header() http.Header         { return r.header }
func (r *responseOnly) Write(p []byte) (int, error) { return r.body.Write(p) }
func (*responseOnly) WriteHeader(int)               {}

func TestFlushErrorsAreReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr   error
		body      func() io.Writer
		name      string
		wantFlush bool
	}{
		{name: "a flushing response writer", body: func() io.Writer { return httptest.NewRecorder() }, wantFlush: true},
		{
			name:    "a response writer that cannot flush",
			body:    func() io.Writer { return &responseOnly{header: http.Header{}} },
			wantErr: http.ErrNotSupported,
		},
		{
			name:      "a flusher that is not a response writer",
			body:      func() io.Writer { return &flushOnly{} },
			wantFlush: true,
		},
		{name: "a writer that cannot flush", body: func() io.Writer { return &plainWriter{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
			hctx := &bodyContext{ctxBase: base, body: tt.body()}
			var flushErr error
			var flusher http.Flusher
			logging.New(logging.WithLogger(logger))(hctx, func(c huma.Context) {
				fe, ok := c.BodyWriter().(interface{ FlushError() error })
				require.True(t, ok)
				flushErr = fe.FlushError()
				flusher, _ = c.BodyWriter().(http.Flusher)
				flusher.Flush()
			})
			require.ErrorIs(t, flushErr, tt.wantErr)
			if fo, ok := hctx.body.(*flushOnly); ok {
				assert.Equal(t, 2, fo.flushed, "FlushError and Flush both reached the writer")
			}
		})
	}
}

func TestHandlerFlushErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr error
		w       func() http.ResponseWriter
		name    string
	}{
		{name: "flushes", w: func() http.ResponseWriter { return httptest.NewRecorder() }},
		{
			name:    "reports a writer that cannot flush",
			w:       func() http.ResponseWriter { return &responseOnly{header: http.Header{}} },
			wantErr: http.ErrNotSupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var got error
			h := logging.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				got = http.NewResponseController(w).Flush()
				f, ok := w.(http.Flusher)
				assert.True(t, ok, "plain flusher assertions keep working")
				f.Flush()
			}), logging.WithLogger(logger))
			h.ServeHTTP(tt.w(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody))
			require.ErrorIs(t, got, tt.wantErr)
		})
	}
}

func TestStreamBodyHook(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		wantBody  string
		wantSize  int
		streaming bool
	}{
		{name: "adapter with a stream callback", streaming: true, wantSize: 11},
		{name: "adapter without one", wantSize: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger(slog.LevelDebug)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			base := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
			body, streamed := &plainWriter{}, &plainWriter{}
			var hctx huma.Context = &bodyContext{ctxBase: base, body: body}
			if tt.streaming {
				hctx = streamingContext{&bodyContext{ctxBase: base, body: body, streamed: streamed}}
			}
			logging.New(logging.WithLogger(logger))(hctx, func(c huma.Context) {
				bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
				require.True(t, ok, "the hook is forwarded")
				bs.StreamBody(func(w io.Writer) { _, _ = w.Write([]byte("data: one\n\n")[:11]) })
			})
			if tt.streaming {
				assert.Equal(t, "data: one\n\n", streamed.String())
				assert.Empty(t, body.String())
			} else {
				assert.Equal(t, "data: one\n\n", body.String())
			}
			rec := find(buf.lines(t), msgRequest)
			require.NotNil(t, rec)
			assert.InDelta(t, tt.wantSize, rec["response_size"], 0, "the streamed bytes are counted")
		})
	}
}
