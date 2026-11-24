package bodywriter_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/internal/bodywriter"
)

// flusherOnly is a writer that flushes but is not a response writer.
type flusherOnly struct {
	buf     bytes.Buffer
	flushed int
}

func (f *flusherOnly) Write(p []byte) (int, error) { return f.buf.Write(p) }

func (f *flusherOnly) Flush() { f.flushed++ }

// deadlineOnly is a writer with write deadlines that is not a response writer.
type deadlineOnly struct {
	deadline time.Time
	buf      bytes.Buffer
}

func (d *deadlineOnly) Write(p []byte) (int, error) { return d.buf.Write(p) }

func (d *deadlineOnly) SetWriteDeadline(t time.Time) error {
	d.deadline = t

	return nil
}

// responseOnly is a response writer that cannot flush and has no deadlines.
type responseOnly struct {
	header http.Header
}

func (r *responseOnly) Header() http.Header       { return r.header }
func (*responseOnly) Write(p []byte) (int, error) { return len(p), nil }
func (*responseOnly) WriteHeader(int)             {}

func TestFlush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantErr     error
		w           func() io.Writer
		name        string
		wantFlushed bool
	}{
		{
			name:        "a response writer that flushes",
			w:           func() io.Writer { return httptest.NewRecorder() },
			wantFlushed: true,
		},
		{
			name:    "a response writer that cannot flush",
			w:       func() io.Writer { return &responseOnly{header: http.Header{}} },
			wantErr: http.ErrNotSupported,
		},
		{
			name:        "a flusher that is not a response writer",
			w:           func() io.Writer { return &flusherOnly{} },
			wantFlushed: true,
		},
		{name: "a writer that cannot flush", w: func() io.Writer { return &bytes.Buffer{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := tt.w()
			p := &bodywriter.Passthrough{W: w}
			require.ErrorIs(t, p.FlushError(), tt.wantErr)
			p.Flush()
			switch v := w.(type) {
			case *httptest.ResponseRecorder:
				assert.Equal(t, tt.wantFlushed, v.Flushed)
			case *flusherOnly:
				assert.Equal(t, 2, v.flushed)
			}
		})
	}
}

func TestUnwrap(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	tests := []struct {
		w    io.Writer
		want http.ResponseWriter
		name string
	}{
		{name: "a response writer", w: rec, want: rec},
		{name: "a plain writer", w: &bytes.Buffer{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, (&bodywriter.Passthrough{W: tt.w}).Unwrap())
		})
	}
}

func TestSetWriteDeadline(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	dw := &deadlineOnly{}
	tests := []struct {
		want    time.Time
		w       interface{ Write([]byte) (int, error) }
		wantErr error
		name    string
	}{
		{name: "a writer with deadlines", w: dw, want: deadline},
		{name: "a response writer without deadlines", w: httptest.NewRecorder(), wantErr: http.ErrNotSupported},
		{name: "a plain writer", w: &bytes.Buffer{}, wantErr: http.ErrNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, (&bodywriter.Passthrough{W: tt.w}).SetWriteDeadline(deadline), tt.wantErr)
			if tt.wantErr == nil {
				assert.Equal(t, tt.want, dw.deadline)
			}
		})
	}
}
