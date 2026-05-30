package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// decodeLine unmarshals a single JSON log line into a generic map.
func decodeLine(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, b)
	}
	return rec
}

// TestInfoRecordIsJSON covers AC1: an info record is JSON carrying the standard
// time/level/msg fields.
func TestInfoRecordIsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo)

	logger.Info("hello")

	rec := decodeLine(t, buf.Bytes())
	if _, ok := rec[slog.TimeKey]; !ok {
		t.Errorf("missing %q key", slog.TimeKey)
	}
	if got := rec[slog.LevelKey]; got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
	if got := rec[slog.MessageKey]; got != "hello" {
		t.Errorf("msg = %v, want hello", got)
	}
}

// TestContextEnrichment covers AC3: tenant_id and trace_id/span_id appear when
// a tenant and an active span are in context.
func TestContextEnrichment(t *testing.T) {
	tid, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("build trace id: %v", err)
	}
	sid, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("build span id: %v", err)
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = tenant.NewContext(ctx, "acme")

	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo)
	logger.InfoContext(ctx, "x")

	rec := decodeLine(t, buf.Bytes())
	if got := rec["tenant_id"]; got != "acme" {
		t.Errorf("tenant_id = %v, want acme", got)
	}
	if got := rec["trace_id"]; got != tid.String() {
		t.Errorf("trace_id = %v, want %v", got, tid.String())
	}
	if got := rec["span_id"]; got != sid.String() {
		t.Errorf("span_id = %v, want %v", got, sid.String())
	}
}

// TestNoEnrichmentWithoutContext proves the handler stays silent on enrichment
// fields when neither a span nor a tenant is present (the pre-SDK default).
func TestNoEnrichmentWithoutContext(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo)
	logger.InfoContext(context.Background(), "x")

	rec := decodeLine(t, buf.Bytes())
	for _, k := range []string{"trace_id", "span_id", "tenant_id"} {
		if _, ok := rec[k]; ok {
			t.Errorf("unexpected %q in record without context", k)
		}
	}
}

// TestRedaction covers System Design §8: sensitive keys are redacted, including
// when nested in a group, while non-sensitive keys pass through untouched.
func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo)

	logger.Info("auth",
		slog.String("password", "hunter2"),
		slog.String("username", "alice"),
		slog.Group("creds",
			slog.String("api_key", "sk-secret"),
			slog.String("scope", "read"),
		),
	)

	rec := decodeLine(t, buf.Bytes())

	if got := rec["password"]; got != "[REDACTED]" {
		t.Errorf("password = %v, want [REDACTED]", got)
	}
	if got := rec["username"]; got != "alice" {
		t.Errorf("username = %v, want alice (untouched)", got)
	}

	group, ok := rec["creds"].(map[string]any)
	if !ok {
		t.Fatalf("creds group missing or wrong type: %T", rec["creds"])
	}
	if got := group["api_key"]; got != "[REDACTED]" {
		t.Errorf("creds.api_key = %v, want [REDACTED]", got)
	}
	if got := group["scope"]; got != "read" {
		t.Errorf("creds.scope = %v, want read (untouched)", got)
	}
}

// TestRedactionCaseInsensitive proves matching ignores key casing.
func TestRedactionCaseInsensitive(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo)

	logger.Info("auth", slog.String("Token", "abc"), slog.String("SIGNING_KEY", "xyz"))

	rec := decodeLine(t, buf.Bytes())
	if got := rec["Token"]; got != "[REDACTED]" {
		t.Errorf("Token = %v, want [REDACTED]", got)
	}
	if got := rec["SIGNING_KEY"]; got != "[REDACTED]" {
		t.Errorf("SIGNING_KEY = %v, want [REDACTED]", got)
	}
}

// TestWithAttrsRedaction proves redaction also applies to attributes attached
// via With (the WithAttrs path), not only per-call attrs.
func TestWithAttrsRedaction(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelInfo).With(slog.String("secret", "s3cr3t"))

	logger.Info("x")

	rec := decodeLine(t, buf.Bytes())
	if got := rec["secret"]; got != "[REDACTED]" {
		t.Errorf("secret = %v, want [REDACTED]", got)
	}
}
