package logging_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
)

const (
	msgHandler = "handler"
	tenantAcme = "acme"
	hdrTenant  = "X-Tenant"
)

// unwrapGo and unwrapChi reach the request through the adapters' Unwrap helpers.
func unwrapGo(c huma.Context) *http.Request {
	r, _ := humago.Unwrap(c)

	return r
}

func unwrapChi(c huma.Context) *http.Request {
	r, _ := humachi.Unwrap(c)

	return r
}

func newAPI(t *testing.T) (http.Handler, huma.API) {
	t.Helper()

	return builders()["humago"](t)
}

func multipartRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	return req
}
