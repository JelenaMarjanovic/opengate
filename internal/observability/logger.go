package observability

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// sensitiveKeys: attribute keys whose values are always redacted so secrets
// never reach the log sink (System Design §8). Matched case-insensitively.
var sensitiveKeys = []string{"password", "token", "api_key", "secret", "signing_key"}

const redactedMarker = "[REDACTED]"

// NewLogger builds the application logger: a JSON handler writing to w at the
// given level with source location, wrapped in contextHandler.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     level,
		AddSource: true, // source location is a standard field (System Design §8).
	})
	return slog.New(&contextHandler{inner: base})
}

// contextHandler enriches every record with trace_id/span_id from the active
// OTel span and tenant_id from the tenant context, and redacts sensitive keys.
// It is a faithful slog.Handler: Enabled/WithAttrs/WithGroup delegate to inner.
type contextHandler struct{ inner slog.Handler }

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)

	// SpanContextFromContext is safe even with no active span; we add IDs only
	// when valid, so this works before the OTel SDK is wired in a later story.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		out.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if id, ok := tenant.IDFromContext(ctx); ok {
		out.AddAttrs(slog.String("tenant_id", id.String()))
	}

	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return &contextHandler{inner: h.inner.WithAttrs(out)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr replaces a's value with the marker if its key is sensitive,
// recursing into group-valued attributes.
func redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, ga := range group {
			out[i] = redactAttr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if slices.Contains(sensitiveKeys, strings.ToLower(a.Key)) {
		return slog.String(a.Key, redactedMarker)
	}
	return a
}
