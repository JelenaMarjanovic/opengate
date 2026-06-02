package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// spyPinger is a fake Pinger recording how many times Ping was called and
// returning a fixed error. The call count lets liveness prove it never touches
// the DB, and readiness prove it pings exactly once.
type spyPinger struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *spyPinger) Ping(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *spyPinger) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestLivenessDoesNotTouchDB asserts /livez returns 200 with the tiny body and
// NEVER pings the database — even when the pinger is wired to fail. A DB outage
// must not fail liveness.
func TestLivenessDoesNotTouchDB(t *testing.T) {
	pinger := &spyPinger{err: errors.New("liveness must not ping the database")}
	router := NewRouter(Config{Pinger: pinger})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, LivenessPath, nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ok"}`)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if n := pinger.callCount(); n != 0 {
		t.Errorf("liveness pinged the database %d time(s); want 0", n)
	}
}

// TestReadinessReachable asserts /readyz returns 200 when the ping succeeds, and
// that it pings exactly once.
func TestReadinessReachable(t *testing.T) {
	pinger := &spyPinger{err: nil}
	router := NewRouter(Config{Pinger: pinger})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, ReadinessPath, nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ok"}`)
	}
	if n := pinger.callCount(); n != 1 {
		t.Errorf("readiness pinged the database %d time(s); want 1", n)
	}
}

// TestReadinessUnreachable asserts /readyz returns a Problem Details 503 when the
// ping fails, with the RFC 9457 content type and the static title — and that the
// underlying ping error does NOT leak into the response body.
func TestReadinessUnreachable(t *testing.T) {
	const pingErrText = "connection refused to host=secret-db"
	pinger := &spyPinger{err: errors.New(pingErrText)}
	router := NewRouter(Config{Pinger: pinger})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, ReadinessPath, nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeProblemJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeProblemJSON)
	}

	raw := rec.Body.String()
	var pd ProblemDetails
	if err := json.Unmarshal([]byte(raw), &pd); err != nil {
		t.Fatalf("body is not JSON: %v\nbody: %s", err, raw)
	}
	if pd.Status != http.StatusServiceUnavailable {
		t.Errorf("body status = %d, want 503", pd.Status)
	}
	if pd.Title != "Service unavailable" {
		t.Errorf("body title = %q, want %q", pd.Title, "Service unavailable")
	}
	if strings.Contains(raw, "secret-db") {
		t.Errorf("response body leaked the ping error detail:\n%s", raw)
	}
}
