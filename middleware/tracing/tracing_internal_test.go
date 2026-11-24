package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallIsAResponseWriter(t *testing.T) {
	t.Parallel()

	c := &call{rhdr: http.Header{}}
	c.Header().Set("X", "y")
	assert.Equal(t, "y", c.rhdr.Get("X"))
	n, err := c.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	c.WriteHeader(http.StatusTeapot)
	assert.Equal(t, http.StatusTeapot, c.status)
}

func TestServeCallIgnoresForeignRequests(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	serveCall(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
}
