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
)

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

// TestViewsAreAlwaysGivenBack checks that every view taken is given back once, normally and when the chain panics.
func TestViewsAreAlwaysGivenBack(t *testing.T) {
	t.Parallel()

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

func TestPoolStoreHandsOutViews(t *testing.T) {
	t.Parallel()

	s := newPoolStore([]string{"Traceparent"})
	c := s.get()
	assert.NotNil(t, c)
	assert.NotNil(t, c.hdr)
	s.put(c)
}
