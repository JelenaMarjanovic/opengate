package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/application/auth"
)

// problemTypeBase is the URI prefix for the type member of a Problem Details
// document. Per RFC 9457 the type is a URI identifying the problem class;
// dereferencing it should (eventually) yield human-readable documentation. We
// use a configured documentation base rather than about:blank so each problem
// class has a stable, linkable identity, matching the System Design §22 example.
// It is a compile-time constant here; if a deployment needs to relocate the docs
// host, promote it to config.
const problemTypeBase = "https://docs.opengate.example/errors/"

// contentTypeProblemJSON is the RFC 9457 media type for Problem Details. It is
// deliberately NOT application/json: clients and proxies distinguish a problem
// document from a normal JSON payload by this type.
const contentTypeProblemJSON = "application/problem+json"

// ProblemDetails is the RFC 9457 response body. The status is duplicated in the
// body (alongside the HTTP status line) for clients that cannot inspect headers.
// Field values are sourced from the STATIC mapping table, never from the
// underlying error, so an error's message can never leak to the client.
type ProblemDetails struct {
	// Type is a URI identifying the problem class (the "what kind of error").
	Type string `json:"type"`
	// Title is a short, static, human-readable summary of the problem class.
	Title string `json:"title"`
	// Status is the HTTP status code, duplicated here per RFC 9457 §3.1.
	Status int `json:"status"`
	// Detail is a static, human-readable explanation of this problem class. It is
	// generic by design and reveals nothing operational about the failure.
	Detail string `json:"detail"`
	// Instance is a URI identifying this specific occurrence, for log/trace
	// correlation. Here it is the request path (see §22's example).
	Instance string `json:"instance"`
	// Errors carries field-level validation failures (RFC 9457 extension). The
	// shape is defined now; the validator that populates it lands in Step 5b, so
	// it is omitempty and absent on every response this step produces.
	Errors []FieldError `json:"errors,omitempty"`
	// TraceID is the OpenTelemetry trace id of the failing request (extension),
	// for cross-reference with traces. It is best-effort: present only when a
	// valid span is in context, otherwise omitted. With the OTel SDK absent there
	// is no valid span yet, so it is omitted today and appears automatically once
	// the SDK and an HTTP tracing middleware land.
	TraceID string `json:"trace_id,omitempty"`
}

// FieldError is one entry in the Problem Details errors extension: a single
// field-level validation failure. Defined now (Step 5a) for a stable response
// contract; the wiring that produces these lives in Step 5b's request validator.
type FieldError struct {
	// Pointer is a JSON Pointer (RFC 6901) to the failing field.
	Pointer string `json:"pointer"`
	// Code is a stable, machine-readable validation code (e.g. "required").
	Code string `json:"code"`
	// Detail is a human-readable explanation of this field's failure.
	Detail string `json:"detail"`
}

// problemMapping is a single static row of the error-mapping table: the safe,
// generic {type, title, status, detail} a given error class renders as. All
// strings are operator-authored and safe to expose; none are derived from the
// runtime error value.
type problemMapping struct {
	typeSlug string // appended to problemTypeBase to form the type URI.
	title    string
	status   int
	detail   string
}

// typeURI returns the fully-qualified type URI for this mapping.
func (m problemMapping) typeURI() string { return problemTypeBase + m.typeSlug }

// errNotReady is the transport-level sentinel the readiness probe returns when
// the database is unreachable. It is unexported because nothing outside this
// adapter raises it; it lives in the mapping table so the readiness 503 renders
// through the same WriteProblem path as every other error (one rendering seam).
var errNotReady = errors.New("service not ready")

// defaultProblem is the fail-safe mapping for any error not found in the table:
// a generic 500. An unmapped error must never render as anything other than a
// generic 500, so a forgotten mapping row fails safe (no detail leaks, no wrong
// status). It is identical to the apperr.ErrInternal row.
var defaultProblem = problemMapping{
	typeSlug: "internal",
	title:    "Internal server error",
	status:   http.StatusInternalServerError,
	detail:   "An internal error occurred.",
}

// validationProblem is the static mapping for a request-body validation failure
// (422). Unlike the rows in problemTable it is keyed to no domain sentinel — a
// validation failure is detected in the handler, not surfaced as a domain error
// — so it is rendered directly via WriteValidationProblem with the field-level
// errors extension. Its detail is generic; the per-field specifics travel in the
// errors array, which (per Step 5a) is safe to expose: "email is required" is
// not enumeration-sensitive the way a credential outcome is.
var validationProblem = problemMapping{
	typeSlug: "validation-failed",
	title:    "Validation failed",
	status:   http.StatusUnprocessableEntity,
	detail:   "The request body failed validation. See errors for details.",
}

// problemTable is the error-mapping table: the single place domain (and the
// readiness) errors are translated to HTTP semantics (System Design §22).
// Resolution is by errors.Is, so a wrapped sentinel still matches; the first
// matching row wins. Adding a new error class is two changes: define the
// sentinel and add a row here.
//
// The 401 invalid-credentials detail is deliberately UNIFORM — it does not
// reveal which part of the login failed — consistent with the enumeration
// defense behind auth.ErrInvalidCredentials.
var problemTable = []struct {
	sentinel error
	mapping  problemMapping
}{
	{auth.ErrInvalidCredentials, problemMapping{
		typeSlug: "invalid-credentials",
		title:    "Invalid credentials",
		status:   http.StatusUnauthorized,
		detail:   "The credentials provided are invalid.",
	}},
	{auth.ErrSessionInvalid, problemMapping{
		typeSlug: "session-invalid",
		title:    "Session invalid or expired",
		status:   http.StatusUnauthorized,
		detail:   "Your session is invalid or has expired. Please sign in again.",
	}},
	{auth.ErrTenantSuspended, problemMapping{
		typeSlug: "tenant-suspended",
		title:    "Tenant suspended",
		status:   http.StatusForbidden,
		detail:   "This tenant is suspended. Contact your administrator.",
	}},
	{errNotReady, problemMapping{
		typeSlug: "service-unavailable",
		title:    "Service unavailable",
		status:   http.StatusServiceUnavailable,
		detail:   "The service is temporarily unable to handle the request. Please retry shortly.",
	}},
	{apperr.ErrInternal, defaultProblem},
}

// mappingFor resolves err to its static mapping via errors.Is, falling through
// to defaultProblem (a generic 500) when nothing matches. It returns ONLY the
// static row; err itself is never read for response content.
func mappingFor(err error) problemMapping {
	for _, row := range problemTable {
		if errors.Is(err, row.sentinel) {
			return row.mapping
		}
	}
	return defaultProblem
}

// WriteProblem writes err as an RFC 9457 Problem Details response. It is the
// single entry point inbound handlers use to render a domain error. The process
// default logger (set as slog.Default at the composition root, where it is the
// trace/tenant-enriching, secret-redacting handler) receives the server-side log.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	writeProblem(w, r, err, slog.Default())
}

// writeProblem is the testable core of WriteProblem with the logger injected so
// a test can capture and assert the server-side record. It resolves err to its
// static mapping (defaulting to a 500) and renders it; the log line carries
// err.Error() — which for a pgconn.PgError omits its Detail field — and NOT any
// pgx-specific detail, request body, headers, or cookies.
func writeProblem(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	renderProblem(w, r, mappingFor(err), nil, err.Error(), logger)
}

// WriteValidationProblem writes a 422 Problem Details for a request-body
// validation failure, carrying the field-level errors extension (RFC 9457). It
// is the second handler-facing entry point alongside WriteProblem: validation
// failures are not domain errors, so they have no sentinel and are rendered from
// the static validationProblem mapping. Detailed field errors are intentionally
// exposed here — a missing/empty field is not enumeration-sensitive, whereas a
// credential outcome stays uniform via WriteProblem(ErrInvalidCredentials).
func WriteValidationProblem(w http.ResponseWriter, r *http.Request, fieldErrors []FieldError) {
	writeValidationProblem(w, r, fieldErrors, slog.Default())
}

// writeValidationProblem is the testable core of WriteValidationProblem.
func writeValidationProblem(w http.ResponseWriter, r *http.Request, fieldErrors []FieldError, logger *slog.Logger) {
	renderProblem(w, r, validationProblem, fieldErrors, "request validation failed", logger)
}

// renderProblem is the shared rendering core behind both writeProblem and
// writeValidationProblem. The flow is:
//
//  1. Build the body from the STATIC mapping — never from a runtime error — so no
//     error message (which for a wrapped pgx error could carry parameter values)
//     reaches the client. Any field errors are attached verbatim (the caller is
//     responsible for keeping them non-sensitive).
//  2. Compute trace_id best-effort: include it only when the request carries a
//     valid span context; otherwise omit the field.
//  3. Log server-side at warn (4xx) or error (5xx), carrying the problem type and
//     the caller-supplied logErr. trace_id/tenant_id are attached by the context
//     handler from r.Context(), so the log correlates with the response body.
//  4. Write the status and JSON body with the problem+json content type.
func renderProblem(w http.ResponseWriter, r *http.Request, mapping problemMapping, fieldErrors []FieldError, logErr string, logger *slog.Logger) {
	pd := ProblemDetails{
		Type:     mapping.typeURI(),
		Title:    mapping.title,
		Status:   mapping.status,
		Detail:   mapping.detail,
		Instance: r.URL.Path,
		Errors:   fieldErrors,
	}

	// Best-effort trace id: only attach a valid one. With no OTel SDK wired there
	// is no valid span context, so this is omitted today (do not fabricate one).
	if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
		pd.TraceID = sc.TraceID().String()
	}

	// Marshal before writing the header so a (practically impossible) marshal
	// failure can still fall back to a bare 500 rather than a half-written body.
	body, mErr := json.Marshal(pd)
	if mErr != nil {
		logger.ErrorContext(r.Context(), "http: marshal problem details failed",
			slog.String("error", mErr.Error()))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Server-side log per §22: warn for client mistakes (4xx), error for server
	// failures (5xx). The redacting context handler adds trace_id/tenant_id; we
	// pass only the caller's logErr, keeping any pgx Detail out of the logged string.
	level := slog.LevelWarn
	if mapping.status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	logger.LogAttrs(r.Context(), level, "http problem response",
		slog.Int("status", mapping.status),
		slog.String("type", pd.Type),
		slog.String("error", logErr),
	)

	w.Header().Set("Content-Type", contentTypeProblemJSON)
	w.WriteHeader(mapping.status)
	if _, werr := w.Write(body); werr != nil {
		// The client likely disconnected; the status/headers are already sent, so
		// there is nothing to do but record it. Best-effort.
		logger.WarnContext(r.Context(), "http: writing problem details body failed",
			slog.String("error", werr.Error()))
	}
}
