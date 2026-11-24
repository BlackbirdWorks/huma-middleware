package logging

import (
	"context"
	"log/slog"
	"slices"
)

const redacted = "[REDACTED]"

// access writes the access log record for a finished request.
func (c *config) access(ctx context.Context, st *state, rec Record) {
	rec.Extra = st.lateAttrs()
	if c.emit != nil {
		c.emit(ctx, quietLogger(st.logger), rec)

		return
	}
	c.defaultRecord(ctx, st.logger, rec)
}

func (c *config) defaultRecord(ctx context.Context, l *slog.Logger, r Record) {
	level := c.levels.of(r.Status)
	if !l.Enabled(ctx, level) {
		return
	}
	var buf [8]slog.Attr
	attrs := append(buf[:0],
		slog.String(c.fields.Path, r.Path),
		slog.Int(c.fields.Status, r.Status),
		slog.Duration(c.fields.Duration, r.Duration),
		slog.Int64(c.fields.Size, r.Size),
		slog.String(c.fields.RemoteAddr, r.RemoteAddr),
		slog.String(c.fields.UserAgent, r.UserAgent),
	)
	if len(c.headers) > 0 {
		attrs = append(attrs, c.headerGroup(r))
	}
	if c.extra != nil {
		attrs = append(attrs, c.extra(r)...)
	}
	l.LogAttrs(ctx, level, c.message, attrs...)
}

func (c *config) headerGroup(r Record) slog.Attr {
	hs := make([]any, 0, len(c.headers))
	for _, name := range c.headers {
		v := r.Header(name)
		if v == "" {
			continue
		}
		if slices.Contains(c.redacted, name) {
			v = redacted
		}
		hs = append(hs, slog.String(name, v))
	}

	return slog.Group(c.fields.Headers, hs...)
}
