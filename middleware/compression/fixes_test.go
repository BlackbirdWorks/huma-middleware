package compression_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unwrapGo(c huma.Context) *http.Request {
	r, _ := humago.Unwrap(c)

	return r
}

func unwrapChi(c huma.Context) *http.Request {
	r, _ := humachi.Unwrap(c)

	return r
}

func TestAdapterUnwrapSeesThroughTheWrapper(t *testing.T) {
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

// deadlineWriter is a body writer that records the write deadline it is given.
type deadlineWriter struct {
	deadline time.Time
	body     bytes.Buffer
}

func (d *deadlineWriter) Write(p []byte) (int, error) { return d.body.Write(p) }

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

func TestWriteDeadlineReachesTheWriter(t *testing.T) {
	t.Parallel()

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
