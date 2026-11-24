package recovery_test

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/recovery"
)

// panicRecord serves the panicking route behind the middleware and returns the one record it logged.
func panicRecord(t *testing.T, opts ...recovery.Option) (int, map[string]any) {
	t.Helper()

	h, api := humatest.New(t)
	logger, buf := newLogger()
	api.UseMiddleware(recovery.New(api, append([]recovery.Option{recovery.WithLogger(logger)}, opts...)...))
	registerRoutes(api)
	rec, p := get(h, pathBoom)
	require.Nil(t, p)
	records := buf.records(t)
	require.Len(t, records, 1)

	return rec.Code, records[0]
}

// TestWithLogger covers the logger that gets the recovered panics.
func TestWithLogger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "given logger", run: func(t *testing.T) {
			t.Helper()
			code, r := panicRecord(t)
			assert.Equal(t, http.StatusInternalServerError, code)
			assert.Equal(t, msgPanic, r["msg"])
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

// TestWithStack covers the stack in the log record.
func TestWithStack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      []recovery.Option
		wantStack bool
	}{
		{name: "no stack", opts: []recovery.Option{recovery.WithStack(false)}},
		{name: "default stack", wantStack: true},
		{name: "stack on", opts: []recovery.Option{recovery.WithStack(true)}, wantStack: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, r := panicRecord(t, tt.opts...)
			assert.Equal(t, tt.wantStack, r["stack"] != nil)
			assert.Equal(t, "domain failure", r["panic"])
		})
	}
}

// TestWithStackSize covers the size of the stack buffer.
func TestWithStackSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		size    int
		wantLen int
	}{
		{name: "stack size", size: 64, wantLen: 64},
		{name: "at least one", size: -5, wantLen: 1},
		{name: "zero is one", size: 0, wantLen: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, r := panicRecord(t, recovery.WithStackSize(tt.size))
			assert.Len(t, r["stack"], tt.wantLen)
		})
	}
}

// TestWithHandler covers the handler that writes the response of a recovered panic.
func TestWithHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		wantStatus int
	}{
		{name: "teapot", status: http.StatusTeapot, wantStatus: http.StatusTeapot},
		{name: "bad gateway", status: http.StatusBadGateway, wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, _ := panicRecord(t, recovery.WithHandler(func(ctx huma.Context, recovered any) {
				assert.Equal(t, errDomain, recovered)
				ctx.SetStatus(tt.status)
			}))
			assert.Equal(t, tt.wantStatus, code)
		})
	}
}

// TestWithSkip covers the requests whose panics pass through.
func TestWithSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		skip       bool
		wantPanic  bool
		wantStatus int
	}{
		{name: "skipped requests keep their panic", path: pathBoom, skip: true, wantPanic: true},
		{name: "skipped requests without a panic", path: pathOK, skip: true, wantStatus: http.StatusNoContent},
		{name: "other requests are recovered", path: pathBoom, wantStatus: http.StatusInternalServerError},
		{name: "requests without a panic", path: pathOK, wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, api := humatest.New(t)
			logger, buf := newLogger()
			api.UseMiddleware(recovery.New(api, recovery.WithLogger(logger),
				recovery.WithSkip(func(huma.Context) bool { return tt.skip })))
			registerRoutes(api)
			rec, p := get(h, tt.path)
			assert.Equal(t, tt.wantPanic, p != nil)
			if !tt.wantPanic {
				assert.Equal(t, tt.wantStatus, rec.Code)
			}
			assert.Equal(t, tt.wantStatus == http.StatusInternalServerError, len(buf.records(t)) == 1)
		})
	}
}

// requestPanic serves a request that panics through Handler with the options given.
func requestPanic(t *testing.T, opts ...recovery.Option) (int, any) {
	t.Helper()

	logger, _ := newLogger()
	h := recovery.Handler(append([]recovery.Option{recovery.WithLogger(logger)}, opts...)...)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec, p := get(h, pathBoom)

	return rec.Code, p
}

// TestWithRequestHandler covers the handler that writes the response in the net/http form.
func TestWithRequestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		wantStatus int
	}{
		{name: "teapot", status: http.StatusTeapot, wantStatus: http.StatusTeapot},
		{name: "bad gateway", status: http.StatusBadGateway, wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, p := requestPanic(
				t,
				recovery.WithRequestHandler(func(w http.ResponseWriter, _ *http.Request, recovered any) {
					assert.Equal(t, "boom", recovered)
					w.WriteHeader(tt.status)
				}),
			)
			assert.Nil(t, p)
			assert.Equal(t, tt.wantStatus, code)
		})
	}
}

// TestWithRequestSkip covers the requests whose panics pass through in the net/http form.
func TestWithRequestSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		skip       bool
		wantPanic  bool
		wantStatus int
	}{
		{name: "skipped", skip: true, wantPanic: true},
		{name: "not skipped", wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, p := requestPanic(t, recovery.WithRequestSkip(func(*http.Request) bool { return tt.skip }))
			assert.Equal(t, tt.wantPanic, p != nil)
			if !tt.wantPanic {
				assert.Equal(t, tt.wantStatus, code)
			}
		})
	}
}
