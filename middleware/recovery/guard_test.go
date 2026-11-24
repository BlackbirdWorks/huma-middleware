package recovery_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
)

func registerStreams(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "nostatus", Method: http.MethodGet, Path: "/nostatus"},
		func(context.Context, *empty) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(ctx huma.Context) {
				ctx.SetHeader("Content-Type", "text/plain")
				_, _ = ctx.BodyWriter().Write([]byte(bodyPartial))
				panic("after an implicit 200")
			}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "flushed", Method: http.MethodGet, Path: "/flushed"},
		func(context.Context, *empty) (*huma.StreamResponse, error) {
			return &huma.StreamResponse{Body: func(ctx huma.Context) {
				if f, ok := ctx.BodyWriter().(http.Flusher); ok {
					f.Flush()
				}
				panic("after a flush")
			}}, nil
		})
}

// TestStartedStreamsAreLeftAlone checks that a body written without a status, or a flush, counts as a started response.
func TestStartedStreamsAreLeftAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		wantBody string
	}{
		{name: "a write without a status", path: "/nostatus", wantBody: bodyPartial},
		{name: "a flush", path: "/flushed"},
	}
	for an, mk := range builders() {
		for _, tt := range tests {
			t.Run(an+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				h, api := mk(t)
				logger, buf := newLogger()
				api.UseMiddleware(recovery.New(api, recovery.WithLogger(logger)))
				registerStreams(api)
				rec, p := get(h, tt.path)
				require.Nil(t, p)
				assert.Equal(t, http.StatusOK, rec.Code)
				assert.Equal(t, tt.wantBody, rec.Body.String(), "no error body is appended to the stream")
				assert.Len(t, buf.records(t), 1, "the panic is still logged")
			})
		}
	}
}

func TestEarlyHintsDoNotStartTheResponse(t *testing.T) {
	t.Parallel()

	logger, _ := newLogger()
	rec := &statusRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
	hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec)
	var codes []int
	recovery.New(nil, recovery.WithLogger(logger))(hctx, func(c huma.Context) {
		c.SetStatus(http.StatusEarlyHints)
		codes = append(codes, rec.last)
		panic("boom")
	})
	assert.Equal(t, []int{http.StatusEarlyHints}, codes)
	assert.Equal(t, http.StatusInternalServerError, rec.last, "the 500 follows the early hints")
}

func TestAdapterUnwrapSeesThroughTheGuard(t *testing.T) {
	t.Parallel()

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
			unwrap: func(c huma.Context) *http.Request {
				r, _ := humago.Unwrap(c)

				return r
			},
		},
		{
			name: "humachi",
			build: func() (http.Handler, huma.API) {
				r := chi.NewRouter()

				return r, humachi.New(r, huma.DefaultConfig("t", "1"))
			},
			unwrap: func(c huma.Context) *http.Request {
				r, _ := humachi.Unwrap(c)

				return r
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := tt.build()
			logger, _ := newLogger()
			var got *http.Request
			api.UseMiddleware(
				recovery.New(api, recovery.WithLogger(logger)),
				func(ctx huma.Context, next func(huma.Context)) {
					got = tt.unwrap(ctx)
					next(ctx)
				},
			)
			registerRoutes(api)
			rec, p := get(h, pathOK)
			require.Nil(t, p)
			assert.Equal(t, http.StatusNoContent, rec.Code)
			require.NotNil(t, got)
			assert.Equal(t, pathOK, got.URL.Path)
		})
	}
}

// deadlineSink is a stream writer with write deadlines, like the one of Fiber's callback.
type deadlineSink struct {
	deadline time.Time
	body     bytes.Buffer
}

func (d *deadlineSink) Write(p []byte) (int, error) { return d.body.Write(p) }

func (d *deadlineSink) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

// ctxBase lets the test contexts embed a context.
type ctxBase huma.Context

// streamerCtx is the context of an adapter that streams through a callback.
type streamerCtx struct {
	ctxBase

	sink io.Writer
}

func (s streamerCtx) StreamBody(fn func(io.Writer)) { fn(s.sink) }

func TestStreamBodyHook(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name      string
		streaming bool
	}{
		{name: "an adapter with a stream callback", streaming: true},
		{name: "an adapter without one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			rec := httptest.NewRecorder()
			sink := &deadlineSink{}
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, rec)
			if tt.streaming {
				hctx = streamerCtx{ctxBase: hctx, sink: sink}
			}
			var deadlineErr error
			recovery.New(nil, recovery.WithLogger(logger))(hctx, func(c huma.Context) {
				bs, ok := c.(interface{ StreamBody(func(io.Writer)) })
				require.True(t, ok, "the hook is forwarded")
				bs.StreamBody(func(w io.Writer) {
					_, _ = w.Write([]byte("data"))
					dl, isDL := w.(interface{ SetWriteDeadline(time.Time) error })
					require.True(t, isDL)
					deadlineErr = dl.SetWriteDeadline(deadline)
				})
			})
			if tt.streaming {
				assert.Equal(t, "data", sink.body.String())
				require.NoError(t, deadlineErr)
				assert.Equal(t, deadline, sink.deadline)
			} else {
				assert.Equal(t, "data", rec.Body.String(), "the stream goes to the body writer; no error body follows")
				require.ErrorIs(t, deadlineErr, http.ErrNotSupported)
			}
		})
	}
}
