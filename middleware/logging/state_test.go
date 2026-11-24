package logging_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/internal/reqlog"
	"github.com/blackbirdworks/huma-middleware/middleware/logging"
)

// TestFromContext covers the request logger of a context.
func TestFromContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		run  func(t *testing.T)
		name string
	}{
		{name: "context logger", run: runFromContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func runFromContext(t *testing.T) {
	t.Helper()

	l, buf := newLogger(slog.LevelDebug)
	ctx := logging.NewContext(context.Background(), l)
	assert.Same(t, l, logging.FromContext(ctx))
	logging.AddAttrs(ctx, slog.String("k", "v"))
	logging.AddAttrs(context.Background(), slog.String("ignored", "x"))
	assert.Same(t, slog.Default(), logging.FromContext(context.Background()))
	logging.FromContext(ctx).Info("m")
	assert.Equal(t, "m", buf.lines(t)[0]["msg"])
}

// TestNewContext covers the context that carries a logger.
func TestNewContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		parent context.Context //nolint:containedctx // the table holds the parent context of each row
		name   string
	}{
		{name: "background", parent: context.Background()},
		{name: "with a value", parent: context.WithValue(context.Background(), ctxKey{}, "kept")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, _ := newLogger(slog.LevelDebug)
			ctx := logging.NewContext(tt.parent, l)
			assert.Same(t, l, logging.FromContext(ctx))
			src, ok := ctx.Value(reqlog.Key{}).(reqlog.Source)
			require.True(t, ok)
			assert.Equal(t, l.Handler(), src.Handler(), "a foreign logger is the destination as it is")
			assert.Equal(t, tt.parent.Value(ctxKey{}), ctx.Value(ctxKey{}))
		})
	}
}

// TestAddAttrs covers the attributes added to the request after the middleware ran.
func TestAddAttrs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		inReq   bool
		wantKey bool
	}{
		{name: "in a request", inReq: true, wantKey: true},
		{name: "outside a request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(slog.LevelDebug)
			if !tt.inReq {
				logging.AddAttrs(logging.NewContext(context.Background(), l), slog.String("k", "v"))
				logging.FromContext(logging.NewContext(context.Background(), l)).Info("m")
				assert.NotContains(t, buf.lines(t)[0], "k")

				return
			}
			h, api := newAPI(t)
			api.UseMiddleware(logging.New(logging.WithLogger(l)))
			huma.Get(api, "/a", func(ctx context.Context, _ *struct{}) (*struct{}, error) {
				logging.AddAttrs(ctx, slog.String("k", "v"))
				logging.FromContext(ctx).InfoContext(ctx, "m")

				return &struct{}{}, nil
			})
			serve(h, "/a")
			assert.Equal(t, "v", find(buf.lines(t), "m")["k"])
		})
	}
}
