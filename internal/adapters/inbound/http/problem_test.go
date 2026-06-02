package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
)

// TestWriteProblemMappings is the table-driven proof that every mapped sentinel
// (and a wrapped one) renders the static {status, title, type} from the table,
// with the RFC 9457 content type. No server is needed: WriteProblem is exercised
// against an httptest recorder directly.
func TestWriteProblemMappings(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  int
		wantTitle string
		wantType  string
	}{
		{"invalid credentials", auth.ErrInvalidCredentials, http.StatusUnauthorized,
			"Invalid credentials", problemTypeBase + "invalid-credentials"},
		{"session invalid", auth.ErrSessionInvalid, http.StatusUnauthorized,
			"Session invalid or expired", problemTypeBase + "session-invalid"},
		{"tenant suspended", auth.ErrTenantSuspended, http.StatusForbidden,
			"Tenant suspended", problemTypeBase + "tenant-suspended"},
		{"not ready", errNotReady, http.StatusServiceUnavailable,
			"Service unavailable", problemTypeBase + "service-unavailable"},
		{"internal", apperr.ErrInternal, http.StatusInternalServerError,
			"Internal server error", problemTypeBase + "internal"},
		// errors.Is must see through wrapping: a wrapped sentinel maps identically.
		{"wrapped sentinel", fmt.Errorf("resolve tenant: %w", auth.ErrInvalidCredentials),
			http.StatusUnauthorized, "Invalid credentials", problemTypeBase + "invalid-credentials"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, pd, _ := doWriteProblem(t, tc.err)

			if rec.Code != tc.wantCode {
				t.Errorf("status code = %d, want %d", rec.Code, tc.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != contentTypeProblemJSON {
				t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblemJSON)
			}
			if pd.Status != tc.wantCode {
				t.Errorf("body status = %d, want %d", pd.Status, tc.wantCode)
			}
			if pd.Title != tc.wantTitle {
				t.Errorf("body title = %q, want %q", pd.Title, tc.wantTitle)
			}
			if pd.Type != tc.wantType {
				t.Errorf("body type = %q, want %q", pd.Type, tc.wantType)
			}
			// instance is the request path (see the helper's request).
			if pd.Instance != "/some/path" {
				t.Errorf("body instance = %q, want %q", pd.Instance, "/some/path")
			}
		})
	}
}

// TestWriteProblemUnmappedFailsSafe proves an error matching NO table row renders
// as the generic 500 — never as anything else — so a forgotten mapping fails safe.
func TestWriteProblemUnmappedFailsSafe(t *testing.T) {
	rec, pd, _ := doWriteProblem(t, errors.New("some entirely unmapped failure"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want 500", rec.Code)
	}
	if pd.Title != "Internal server error" {
		t.Errorf("title = %q, want %q", pd.Title, "Internal server error")
	}
	if pd.Detail != "An internal error occurred." {
		t.Errorf("detail = %q, want the generic internal detail", pd.Detail)
	}
}

// TestWriteProblemSanitizesBody is the security crux for the RESPONSE: for both
// apperr.ErrInternal and an error wrapping a sensitive-looking string, the body
// carries only the static generic detail and the sensitive substring is absent.
// (The body is built from the static table, never from err.Error().)
func TestWriteProblemSanitizesBody(t *testing.T) {
	const sensitive = "DEADBEEF"

	cases := map[string]error{
		"bare internal": apperr.ErrInternal,
		// The prompt's inline case: a wrapped error whose message itself contains a
		// pgx-detail-style secret. The body must still be generic.
		"wrapped sensitive": fmt.Errorf("boom: %w: %w", apperr.ErrInternal,
			errors.New(`Key (token_hash)=(\xDEADBEEF) already exists`)),
	}

	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			rec, pd, raw := doWriteProblem(t, err)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status code = %d, want 500", rec.Code)
			}
			if pd.Detail != "An internal error occurred." {
				t.Errorf("detail = %q, want the generic internal detail", pd.Detail)
			}
			if strings.Contains(raw, sensitive) {
				t.Errorf("response body leaked the sensitive substring %q:\n%s", sensitive, raw)
			}
		})
	}
}

// TestWriteProblemDoesNotLogPgErrorDetail is the security crux for the LOG. A
// real pgconn.PgError carries the secret in its Detail field; PgError.Error()
// omits Detail (it renders only Severity/Message/SQLSTATE). Because the helper
// logs err.Error() and does NO Detail extraction, the secret reaches neither the
// response body nor the server-side log — while the safe SQLSTATE still does,
// proving the standard error string WAS logged.
//
// This is the assertion variant chosen over "DEADBEEF absent from log" for an
// inline-constructed error: with a real PgError the secret genuinely lives in a
// field Error() skips, so the absence is a property of the helper's design, not
// of how the test happened to spell the error.
func TestWriteProblemDoesNotLogPgErrorDetail(t *testing.T) {
	const sensitive = "DEADBEEF"

	pgErr := &pgconn.PgError{
		Severity: "ERROR",
		Code:     "23505",
		Message:  `duplicate key value violates unique constraint "sessions_token_hash_key"`,
		Detail:   `Key (token_hash)=(\xDEADBEEF) already exists.`,
	}
	// Sanity-check the premise: the secret is in Detail, and Error() omits it.
	if !strings.Contains(pgErr.Detail, sensitive) {
		t.Fatalf("test premise broken: Detail does not contain %q", sensitive)
	}
	if strings.Contains(pgErr.Error(), sensitive) {
		t.Fatalf("test premise broken: PgError.Error() unexpectedly contains the Detail secret")
	}

	err := fmt.Errorf("create session: %w: %w", apperr.ErrInternal, pgErr)

	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	writeProblem(rec, req, err, logger)

	body := rec.Body.String()
	logged := buf.String()

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status code = %d, want 500", rec.Code)
	}
	if strings.Contains(body, sensitive) {
		t.Errorf("response body leaked the pgx Detail secret %q:\n%s", sensitive, body)
	}
	if strings.Contains(logged, sensitive) {
		t.Errorf("server log leaked the pgx Detail secret %q:\n%s", sensitive, logged)
	}
	// The safe SQLSTATE from err.Error() IS present, proving the helper logged the
	// standard error string (and merely did not reach into the Detail field).
	if !strings.Contains(logged, "SQLSTATE 23505") {
		t.Errorf("server log does not contain the expected err.Error() text; got:\n%s", logged)
	}
}

// TestWriteProblemLogLevels asserts §22's level policy: 4xx (client mistakes) log
// at warn, 5xx (server failures) log at error.
func TestWriteProblemLogLevels(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantLevel string
	}{
		{"4xx warns", auth.ErrInvalidCredentials, "WARN"},
		{"5xx errors", apperr.ErrInternal, "ERROR"},
		{"503 errors", errNotReady, "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := observability.NewLogger(&buf, slog.LevelDebug)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			writeProblem(rec, req, tc.err, logger)

			logRec := firstLogRecord(t, buf.Bytes())
			if logRec[slog.LevelKey] != tc.wantLevel {
				t.Errorf("log level = %v, want %s", logRec[slog.LevelKey], tc.wantLevel)
			}
		})
	}
}

// TestWriteProblemTraceID covers the best-effort trace_id field: omitted with no
// valid span (the only case today, with the OTel SDK absent), and present and
// correct when a valid span context is faked.
func TestWriteProblemTraceID(t *testing.T) {
	t.Run("omitted without a valid span", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		WriteProblem(rec, req, apperr.ErrInternal)

		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if _, ok := m["trace_id"]; ok {
			t.Errorf("trace_id present without a valid span: %v", m["trace_id"])
		}
	})

	t.Run("included with a valid span", func(t *testing.T) {
		const wantTrace = "0123456789abcdef0123456789abcdef"
		tid, err := trace.TraceIDFromHex(wantTrace)
		if err != nil {
			t.Fatalf("trace id: %v", err)
		}
		sid, err := trace.SpanIDFromHex("0123456789abcdef")
		if err != nil {
			t.Fatalf("span id: %v", err)
		}
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    tid,
			SpanID:     sid,
			TraceFlags: trace.FlagsSampled,
		})
		if !sc.IsValid() {
			t.Fatal("constructed span context is not valid")
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(trace.ContextWithSpanContext(context.Background(), sc))
		WriteProblem(rec, req, apperr.ErrInternal)

		var pd ProblemDetails
		if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if pd.TraceID != wantTrace {
			t.Errorf("trace_id = %q, want %q", pd.TraceID, wantTrace)
		}
	})
}

// doWriteProblem runs WriteProblem against a recorder for a fixed request path
// and returns the recorder, the decoded body, and the raw body string.
func doWriteProblem(t *testing.T, err error) (*httptest.ResponseRecorder, ProblemDetails, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	WriteProblem(rec, req, err)

	raw := rec.Body.String()
	var pd ProblemDetails
	if uerr := json.Unmarshal([]byte(raw), &pd); uerr != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", uerr, raw)
	}
	return rec, pd, raw
}

// firstLogRecord returns the first JSON log record in raw, failing if none.
func firstLogRecord(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\nline: %s", err, line)
		}
		return rec
	}
	t.Fatalf("no log line found in output: %s", raw)
	return nil
}
