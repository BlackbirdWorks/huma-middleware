package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewMiddleware checks that every view taken from the store is given back once, normally and when the chain
// panics. It is internal because the store is not reachable from outside.
func TestNewMiddleware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "views given back", run: runViewsAreAlwaysGivenBack},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

// runViewsAreAlwaysGivenBack checks that every view taken is given back once, normally and when the chain panics.
func runViewsAreAlwaysGivenBack(t *testing.T) {
	t.Helper()

	tests := []struct {
		next      func(huma.Context)
		name      string
		wantGets  int
		wantPuts  int
		wantPanic bool
		skip      bool
	}{
		{name: "a normal request", next: func(huma.Context) {}, wantGets: 1, wantPuts: 1},
		{name: "a panic", next: func(huma.Context) { panic("boom") }, wantGets: 1, wantPuts: 1, wantPanic: true},
		{name: "a request that is not traced takes no view", next: func(huma.Context) {}, skip: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := []Option{WithEnabled(true)}
			if tt.skip {
				opts = append(opts, WithSkipPaths("/x"))
			}
			store := &countingStore{}
			mw := newMiddleware(newConfig(opts), nil, store)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
			run := func() { mw(hctx, tt.next) }
			if tt.wantPanic {
				assert.Panics(t, run)
			} else {
				assert.NotPanics(t, run)
			}
			assert.Equal(t, tt.wantGets, store.gets)
			assert.Equal(t, tt.wantPuts, store.puts)
		})
	}
}

// TestPoolStore checks the views the pool hands out.
func TestPoolStore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "hands out views", run: runPoolStoreHandsOutViews},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runPoolStoreHandsOutViews(t *testing.T) {
	t.Helper()

	s := newPoolStore([]string{"Traceparent"})
	c := s.get()
	assert.NotNil(t, c)
	assert.NotNil(t, c.hdr)
	s.put(c)
}

// TestCall checks that a call is a response writer.
func TestCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "response writer", run: runCallIsAResponseWriter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runCallIsAResponseWriter(t *testing.T) {
	t.Helper()

	c := &call{rhdr: http.Header{}}
	c.Header().Set("X", "y")
	assert.Equal(t, "y", c.rhdr.Get("X"))
	n, err := c.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	c.WriteHeader(http.StatusTeapot)
	assert.Equal(t, http.StatusTeapot, c.status)
}

// TestServeCall checks that it leaves foreign requests alone.
func TestServeCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "foreign requests", run: runServeCallIgnoresForeignRequests},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runServeCallIgnoresForeignRequests(t *testing.T) {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	serveCall(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// countingStore counts the views taken and given back.
type countingStore struct {
	mu   sync.Mutex // guards the counters
	gets int
	puts int
}

func (s *countingStore) get() *call {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++

	return newCall(nil)
}

func (s *countingStore) put(*call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
}
