package logging_test

import (
	"context"
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

const (
	hdrID          = "X-Request-ID"
	idGenerated    = "generated-1"
	idFromRequest  = "from-request"
	idFromResponse = "from-response"
	idLower        = "lower"
	idTrace        = "t-1"
	idOuter        = "outer-1"
	idInner        = "inner-1"
)

type idCase struct {
	reqHeaders  map[string]string
	respSet     func(h func(name, value string))
	name        string
	wantAccess  string
	wantInCtx   string
	wantRespHdr string
	wantRecord  string
	opts        []logging.Option
}

func idCases() []idCase {
	gen := logging.WithRequestIDGenerator(func() string { return idGenerated })

	return []idCase{
		{
			name:        "request header wins",
			reqHeaders:  map[string]string{hdrID: idFromRequest},
			respSet:     func(h func(string, string)) { h(hdrID, idFromResponse) },
			wantAccess:  idFromRequest,
			wantInCtx:   idFromRequest,
			wantRecord:  idFromRequest,
			wantRespHdr: idFromResponse,
		},
		{
			name:        "response header is the fallback",
			respSet:     func(h func(string, string)) { h(hdrID, idFromResponse) },
			wantAccess:  idFromResponse,
			wantInCtx:   idFromResponse,
			wantRecord:  idFromResponse,
			wantRespHdr: idFromResponse,
		},
		{
			name:       "response header is matched case-insensitively",
			respSet:    func(h func(string, string)) { h("x-request-id", idLower) },
			wantAccess: idLower, wantInCtx: idLower, wantRecord: idLower, wantRespHdr: idLower,
		},
		{name: "neither"},
		{
			name: "another header", opts: []logging.Option{logging.WithRequestIDHeader("X-Trace")},
			respSet:    func(h func(string, string)) { h("X-Trace", idTrace); h(hdrID, "ignored") },
			wantAccess: idTrace, wantInCtx: idTrace, wantRecord: idTrace, wantRespHdr: "ignored",
		},
		{
			name: "generator when neither has one", opts: []logging.Option{gen},
			wantAccess: idGenerated, wantInCtx: idGenerated, wantRecord: idGenerated, wantRespHdr: idGenerated,
		},
		{
			name: "generator is not used when the request has an ID", opts: []logging.Option{gen},
			reqHeaders: map[string]string{hdrID: idFromRequest},
			wantAccess: idFromRequest, wantInCtx: idFromRequest, wantRecord: idFromRequest,
		},
		{
			name: "generator without a header name does nothing",
			opts: []logging.Option{logging.WithRequestIDHeader(""), gen},
		},
	}
}

func TestRequestIDHuma(t *testing.T) {
	t.Parallel()

	for _, tt := range idCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newLogger(slog.LevelDebug)
			var rec logging.Record
			opts := append([]logging.Option{logging.WithLogger(logger)}, tt.opts...)
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", http.NoBody)
			for k, v := range tt.reqHeaders {
				req.Header.Set(k, v)
			}
			hctx := humatest.NewContext(&huma.Operation{OperationID: "x", Path: "/x"}, req, w)
			logging.New(
				append(opts, logging.WithAccessLogFunc(func(_ context.Context, _ *slog.Logger, r logging.Record) {
					rec = r
				}))...)(
				hctx,
				func(c huma.Context) {
					if tt.respSet != nil {
						tt.respSet(c.SetHeader)
					}
					logging.FromContext(c.Context()).InfoContext(c.Context(), "in handler")
				},
			)
			handler := find(buf.lines(t), "in handler")
			require.NotNil(t, handler)
			if tt.wantInCtx == "" {
				assert.NotContains(t, handler, "request_id")
			} else {
				assert.Equal(t, tt.wantInCtx, handler["request_id"])
			}
			assert.Equal(t, tt.wantRecord, rec.RequestID)
			if tt.wantRespHdr != "" {
				assert.Equal(t, tt.wantRespHdr, w.Header().Get(hdrID))
			}
		})
	}
}

func TestRequestIDHumaDefaultRecord(t *testing.T) {
	t.Parallel()

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
			logging.New(logging.WithLogger(logger))(hctx, tt.set)
			rec := find(buf.lines(t), msgRequest)
			require.NotNil(t, rec)
			assert.Equal(t, "late", rec["request_id"])
		})
	}
}

func TestRequestIDHandler(t *testing.T) {
	t.Parallel()

	for _, tt := range idCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newLogger(slog.LevelDebug)
			var rec logging.Record
			opts := append([]logging.Option{logging.WithLogger(logger)}, tt.opts...)
			opts = append(
				opts,
				logging.WithAccessLogFunc(func(_ context.Context, _ *slog.Logger, r logging.Record) { rec = r }),
			)
			h := logging.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.respSet != nil {
					tt.respSet(w.Header().Set)
				}
			}), opts...)
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
