package accesslog_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/middleware/accesslog"
)

// TestWithLogger covers the root logger, and the default logger when there is none.
func TestWithLogger(t *testing.T) { //nolint:paralleltest // it sets the process wide default logger
	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "default logger", run: runDefaultLoggerFallback},
	}
	for _, tt := range tests { //nolint:paralleltest // it sets the process wide default logger
		t.Run(tt.name, tt.run)
	}
}

// runDefaultLoggerFallback sets the global default logger, so it must not run
// in parallel with other tests that read it.
func runDefaultLoggerFallback(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	first, firstBuf := newLogger(slog.LevelDebug)
	slog.SetDefault(first)

	for _, withNil := range []bool{false, true} {
		h, api := humatest.New(t)
		if withNil {
			api.UseMiddleware(accesslog.New(accesslog.WithLogger(nil)))
		} else {
			api.UseMiddleware(accesslog.New())
		}
		register(api)

		// A default set after construction is honoured.
		second, secondBuf := newLogger(slog.LevelDebug)
		serve(h, "/users/1")
		assert.NotNil(t, find(firstBuf.lines(t), msgRequest))
		slog.SetDefault(second)
		serve(h, "/users/2")
		assert.NotNil(t, find(secondBuf.lines(t), msgRequest))
		slog.SetDefault(first)
	}
}

// TestWithLevels covers the levels by status class.
func TestWithLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "by class", run: runLevelsByClass},
		{name: "disabled level", run: runLevelsDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runLevelsByClass(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	status := http.StatusFound
	h := accesslog.Handler(
		accesslog.WithLogger(l),
		accesslog.WithLevels(
			accesslog.Levels{Redirect: slog.LevelDebug, ClientError: slog.LevelError},
		),
	)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }),
	)
	serve(h, "/x")
	assert.Equal(t, "DEBUG", find(buf.lines(t), msgRequest)["level"])
}

func runLevelsDisabled(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelError)
	h, api := humatest.New(t)
	api.UseMiddleware(accesslog.New(accesslog.WithLogger(l)))
	register(api)
	serve(h, "/users/1")
	assert.Empty(t, buf.lines(t))
	serve(h, "/users/boom")
	assert.Len(t, buf.lines(t), 1)
}

// TestWithRecordFunc covers the replaced access log record.
func TestWithRecordFunc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "custom record", run: runCustomAccessLogFunc},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runCustomAccessLogFunc(t *testing.T) {
	t.Helper()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			var got accesslog.Record
			h, buf := setup(
				t,
				b,
				accesslog.WithRecordFunc(func(ctx context.Context, l *slog.Logger, r *accesslog.Record) {
					got = *r
					l.LogAttrs(ctx, slog.LevelInfo, "custom", slog.Int("status", r.Status), slog.Int64("size", r.Size))
				}),
			)
			serve(h, "/users/1?q=1")

			line := find(buf.lines(t), "custom")
			require.NotNil(t, line)
			assert.NotContains(t, line, "method", "built-in attributes are not forced on a custom record")
			assert.Nil(t, find(buf.lines(t), msgRequest))
			assert.Equal(t, http.StatusOK, got.Status)
			assert.Equal(t, "/users/1?q=1", got.RequestURI())
			assert.Equal(t, routeUser, got.Route)
			assert.Equal(t, "get-user", got.OperationID)
			assert.Equal(t, wantAgent, got.UserAgent)
			assert.Equal(t, wantAgent, got.Header("User-Agent"))
			assert.Equal(t, remoteAddr, got.RemoteAddr)
			assert.NotEmpty(t, got.Host)
		})
	}
}

// TestWithRequestIDHeader covers where the request ID comes from.
func TestWithRequestIDHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "huma", run: runRequestIDHuma},
		{name: "huma default record", run: runRequestIDHumaDefaultRecord},
		{name: msgHandler, run: runRequestIDHandler},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runRequestIDHuma(t *testing.T) {
	t.Helper()

	for _, tt := range idCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec accesslog.Record
			opts := append([]accesslog.Option{accesslog.WithLogger(logger)}, tt.opts...)
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, w)
			accesslog.New(
				append(opts, accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) {
					rec = *r
				}))...)(
				hctx,
				func(c huma.Context) {
					if tt.respSet != nil {
						tt.respSet(c.SetHeader)
					}
					_ = c
				},
			)
			assert.Equal(t, tt.wantRecord, rec.RequestID)
			if tt.wantRespHdr != "" {
				assert.Equal(t, tt.wantRespHdr, w.Header().Get(hdrID))
			}
		})
	}
}

func runRequestIDHumaDefaultRecord(t *testing.T) {
	t.Helper()

	tests := []struct {
		set  func(c huma.Context)
		name string
	}{
		{name: "set", set: func(c huma.Context) { c.SetHeader(hdrID, "late") }},
		{name: "appended", set: func(c huma.Context) { c.AppendHeader(hdrID, "late") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger(slog.LevelDebug)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, httptest.NewRecorder())
			accesslog.New(accesslog.WithLogger(logger))(hctx, tt.set)
			rec := find(buf.lines(t), msgRequest)
			require.NotNil(t, rec)
			assert.Equal(t, "late", rec["request_id"])
		})
	}
}

func runRequestIDHandler(t *testing.T) {
	t.Helper()

	for _, tt := range idCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec accesslog.Record
			opts := append([]accesslog.Option{accesslog.WithLogger(logger)}, tt.opts...)
			opts = append(
				opts,
				accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { rec = *r }),
			)
			h := accesslog.Handler(opts...)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.respSet != nil {
					tt.respSet(w.Header().Set)
				}
			}))
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(w, req)
			assert.Equal(t, tt.wantRecord, rec.RequestID)
			if tt.wantRespHdr != "" {
				assert.Equal(t, tt.wantRespHdr, w.Header().Get(hdrID))
			}
		})
	}
}

// TestWithRequestIDGenerator covers the order: request, response, generated.
func TestWithRequestIDGenerator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: msgHandler, run: runRequestIDRulesHandler},
		{name: "huma", run: runRequestIDRulesHuma},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runRequestIDRulesHandler(t *testing.T) {
	t.Helper()

	tests := idRuleCases()
	for _, tt := range tests {
		t.Run(tt.name+"", func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec accesslog.Record
			opts := append([]accesslog.Option{accesslog.WithLogger(logger)}, tt.opts...)
			opts = append(
				opts,
				accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { rec = *r }),
			)
			h := accesslog.Handler(opts...)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.innerSet != "" {
					w.Header().Set(hdrID, tt.innerSet)
				}
			}))
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

func runRequestIDRulesHuma(t *testing.T) {
	t.Helper()

	tests := idRuleCases()
	// The huma form cannot see a header an outer middleware set; the other rules are the same.
	for _, tt := range tests {
		if tt.outer != "" {
			continue
		}
		t.Run(tt.name+"/huma", func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec accesslog.Record
			opts := append([]accesslog.Option{accesslog.WithLogger(logger)}, tt.opts...)
			opts = append(
				opts,
				accesslog.WithRecordFunc(func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { rec = *r }),
			)
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, w)
			accesslog.New(opts...)(hctx, func(c huma.Context) {
				if tt.innerSet != "" {
					c.SetHeader(hdrID, tt.innerSet)
				}
			})
			assert.Equal(t, tt.wantRecord, rec.RequestID)
			assert.Equal(t, tt.wantHeader, w.Header().Get(hdrID))
		})
	}
}

// optionCase is one row of an option test: the options, and what the lines logged for one request must show.
type optionCase struct {
	check func(t *testing.T, lines []map[string]any)
	name  string
	opts  []accesslog.Option
}

// runOption serves one request through every adapter with the options and checks the lines logged.
func runOption(t *testing.T, tt optionCase) {
	t.Helper()

	for an, b := range builders() {
		t.Run(an, func(t *testing.T) {
			t.Parallel()
			h, buf := setup(t, b, tt.opts...)
			serve(h, "/users/1?q=1", "Authorization", "Bearer s3cr3t", hdrTenant, tenantAcme,
				"X-Custom-Secret", "x", "Cookie", "a=b")
			lines := buf.lines(t)
			raw, err := json.Marshal(lines)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "s3cr3t")
			tt.check(t, lines)
		})
	}
}

// requestLine is the access log record of the request.
func requestLine(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	rec := find(lines, msgRequest)
	require.NotNil(t, rec)

	return rec
}

// TestWithFields covers the renamed attributes.
func TestWithFields(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "fields renamed",
			opts: []accesslog.Option{
				accesslog.WithFields(accesslog.Fields{Status: "code", Route: "r", Method: "verb"}),
			},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				rec := requestLine(t, lines)
				assert.InDelta(t, 200, rec["code"], 0)
				assert.Equal(t, routeUser, rec["r"])
				assert.Equal(t, "GET", rec["verb"])
				assert.Contains(t, rec, "path", "unset names keep their default")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithMessage covers the message of the access log record.
func TestWithMessage(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "message",
			opts: []accesslog.Option{accesslog.WithMessage("access")},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.NotNil(t, find(lines, "access"))
				assert.Nil(t, find(lines, msgRequest))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithSkip covers the requests that are not logged.
func TestWithSkip(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: nameSkipped,
			opts: []accesslog.Option{accesslog.WithSkip(func(huma.Context) bool { return true })},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Nil(t, find(lines, msgRequest))
			},
		},
		{
			name: "not skipped",
			opts: []accesslog.Option{accesslog.WithSkip(func(huma.Context) bool { return false })},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				requestLine(t, lines)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// headersCheck is the check of the header options: the access log shows the headers named, redacted or not.
func headersCheck(want map[string]any) func(t *testing.T, lines []map[string]any) {
	return func(t *testing.T, lines []map[string]any) {
		t.Helper()
		hs, ok := requestLine(t, lines)["headers"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, want, hs)
	}
}

// TestWithHeaders covers the headers in the access log.
func TestWithHeaders(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name:  "named headers",
			opts:  []accesslog.Option{accesslog.WithHeaders(hdrTenant, "X-Absent")},
			check: headersCheck(map[string]any{hdrTenant: tenantAcme}),
		},
		{
			name: "sensitive headers are redacted",
			opts: []accesslog.Option{accesslog.WithHeaders("authorization", "Cookie")},
			check: headersCheck(map[string]any{
				"Authorization": redactedValue, "Cookie": redactedValue,
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithRedactedHeaders covers the headers whose value is replaced.
func TestWithRedactedHeaders(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "added to the defaults",
			opts: []accesslog.Option{
				accesslog.WithHeaders(hdrTenant, "x-custom-secret"),
				accesslog.WithRedactedHeaders("X-Custom-Secret"),
			},
			check: headersCheck(map[string]any{hdrTenant: tenantAcme, "X-Custom-Secret": redactedValue}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithAttrs covers the extra attributes of the access log record.
func TestWithAttrs(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "extra access attrs",
			opts: []accesslog.Option{accesslog.WithAttrs(func(r *accesslog.Record) []slog.Attr {
				return []slog.Attr{slog.String("tenant", r.Header(hdrTenant)), slog.String("uri", r.RequestURI())}
			})},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				rec := requestLine(t, lines)
				assert.Equal(t, tenantAcme, rec["tenant"])
				assert.Equal(t, "/users/1?q=1", rec["uri"])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestRecord_Header covers the request header of a record, in the huma and the net/http form.
func TestRecord_Header(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
		hdr  string
	}{
		{name: "present", hdr: hdrTenant, want: tenantAcme},
		{name: "absent", hdr: "X-Absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var fromHuma, fromRequest string
			l, _ := newLogger(slog.LevelDebug)
			h, api := newAPI(t)
			api.UseMiddleware(accesslog.New(accesslog.WithLogger(l), accesslog.WithRecordFunc(
				func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { fromHuma = r.Header(tt.hdr) })))
			register(api)
			serve(h, "/users/1", hdrTenant, tenantAcme)
			raw := accesslog.Handler(accesslog.WithLogger(l), accesslog.WithRecordFunc(
				func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { fromRequest = r.Header(tt.hdr) },
			))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			serve(raw, "/raw", hdrTenant, tenantAcme)
			assert.Equal(t, tt.want, fromHuma)
			assert.Equal(t, tt.want, fromRequest)
		})
	}
}

// TestRecord_RequestURI covers the request target of a record, in the huma and the net/http form.
func TestRecord_RequestURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
	}{
		{name: "path", target: "/users/1"},
		{name: "query", target: "/users/1?q=1&r=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var fromHuma, fromRequest string
			l, _ := newLogger(slog.LevelDebug)
			h, api := newAPI(t)
			api.UseMiddleware(accesslog.New(accesslog.WithLogger(l), accesslog.WithRecordFunc(
				func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { fromHuma = r.RequestURI() })))
			register(api)
			serve(h, tt.target)
			raw := accesslog.Handler(accesslog.WithLogger(l), accesslog.WithRecordFunc(
				func(_ context.Context, _ *slog.Logger, r *accesslog.Record) { fromRequest = r.RequestURI() },
			))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			serve(raw, tt.target)
			assert.Equal(t, tt.target, fromHuma)
			assert.Equal(t, tt.target, fromRequest)
		})
	}
}

// TestWithClock covers the clock of the durations.
func TestWithClock(t *testing.T) {
	t.Parallel()

	tests := []optionCase{
		{
			name: "nil clock ignored",
			opts: []accesslog.Option{accesslog.WithClock(nil)},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				requestLine(t, lines)
			},
		},
		{
			name: "stepping clock",
			opts: []accesslog.Option{accesslog.WithClock(stepClock(time.Second))},
			check: func(t *testing.T, lines []map[string]any) {
				t.Helper()
				assert.Positive(t, requestLine(t, lines)["duration"])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runOption(t, tt)
		})
	}
}

// TestWithRequestSkip covers the requests that are not logged in the net/http form.
func TestWithRequestSkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		skip       bool
		wantAccess bool
	}{
		{name: nameSkipped, skip: true},
		{name: "not skipped", wantAccess: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(slog.LevelDebug)
			h := accesslog.Handler(
				accesslog.WithLogger(l),
				accesslog.WithRequestSkip(func(*http.Request) bool { return tt.skip }),
			)(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
			)
			serve(h, "/raw")
			assert.Equal(t, tt.wantAccess, find(buf.lines(t), msgRequest) != nil)
		})
	}
}

// TestDefaultFields covers the default attribute names.
func TestDefaultFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  func(accesslog.Fields) string
		want string
	}{
		{name: "method", got: func(f accesslog.Fields) string { return f.Method }, want: "method"},
		{name: "route", got: func(f accesslog.Fields) string { return f.Route }, want: "route"},
		{name: "status", got: func(f accesslog.Fields) string { return f.Status }, want: "status"},
		{name: "size", got: func(f accesslog.Fields) string { return f.Size }, want: "response_size"},
		{name: "request id", got: func(f accesslog.Fields) string { return f.RequestID }, want: "request_id"},
		{name: "headers", got: func(f accesslog.Fields) string { return f.Headers }, want: "headers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.got(accesslog.DefaultFields()))
		})
	}
}

// TestDefaultLevels covers the default level of each status class.
func TestDefaultLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		got  func(accesslog.Levels) slog.Level
		name string
		want slog.Level
	}{
		{name: "success", got: func(l accesslog.Levels) slog.Level { return l.Success }, want: slog.LevelInfo},
		{name: "redirect", got: func(l accesslog.Levels) slog.Level { return l.Redirect }, want: slog.LevelInfo},
		{name: "client error", got: func(l accesslog.Levels) slog.Level { return l.ClientError }, want: slog.LevelWarn},
		{
			name: "server error",
			got:  func(l accesslog.Levels) slog.Level { return l.ServerError },
			want: slog.LevelError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.got(accesslog.DefaultLevels()))
		})
	}
}

// keepingHandler keeps every record it is given, as an asynchronous handler does.
type keepingHandler struct {
	records *[]slog.Record
}

func (keepingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h keepingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)

	return nil
}

func (h keepingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h keepingHandler) WithGroup(string) slog.Handler      { return h }

// flatten lists the attributes of a record with the groups that have an empty key inlined.
func flatten(r slog.Record) map[string]string {
	out := map[string]string{}
	var add func(a slog.Attr)
	add = func(a slog.Attr) {
		if a.Value.Kind() == slog.KindGroup {
			for _, g := range a.Value.Group() {
				add(g)
			}

			return
		}
		out[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		add(a)

		return true
	})

	return out
}

// TestWithRetainedRecords covers the handlers that keep the record: what they kept is not changed by later requests.
func TestWithRetainedRecords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		retain bool
	}{
		{name: "retained", retain: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var kept []slog.Record
			opts := []accesslog.Option{accesslog.WithLogger(slog.New(keepingHandler{&kept}))}
			if tt.retain {
				opts = append(opts, accesslog.WithRetainedRecords())
			}
			h := accesslog.Handler(opts...)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			for _, path := range []string{"/first", "/second", "/third"} {
				serve(h, path)
			}
			require.Len(t, kept, 3)
			for i, path := range []string{"/first", "/second", "/third"} {
				assert.Equal(t, path, flatten(kept[i])["path"])
				assert.Equal(t, wantAgent, flatten(kept[i])["user_agent"])
			}
		})
	}
}
