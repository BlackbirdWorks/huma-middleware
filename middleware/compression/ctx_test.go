package compression_test

import (
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
)

// ctxBase lets bodyOverride embed a context and still define a Context method.
type ctxBase huma.Context

// bodyOverride is a huma.Context with another body writer.
type bodyOverride struct {
	ctxBase

	w io.Writer
}

func (b bodyOverride) BodyWriter() io.Writer { return b.w }

func newContextWithBody(req *http.Request, rec *httptest.ResponseRecorder, w io.Writer) huma.Context {
	op := &huma.Operation{OperationID: opID, Path: "/x"}

	return bodyOverride{ctxBase: humatest.NewContext(op, req, rec), w: w}
}

// newContextNoOp is a context without an operation.
func newContextNoOp(req *http.Request, rec *httptest.ResponseRecorder) huma.Context {
	return humatest.NewContext(nil, req, rec)
}
