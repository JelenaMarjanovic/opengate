package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	appauth "github.com/JelenaMarjanovic/opengate/internal/application/auth"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
	"github.com/JelenaMarjanovic/opengate/internal/tenant"
)

// --- Fakes for the outbound ports -------------------------------------------

// fakeResolver returns a configurable TenantRef/error from ResolveBySlug.
type fakeResolver struct {
	ref     ports.TenantRef
	err     error
	gotSlug string
}

func (f *fakeResolver) ResolveBySlug(_ context.Context, slug string) (ports.TenantRef, error) {
	f.gotSlug = slug
	return f.ref, f.err
}

// fakeUserReader returns a configurable AuthUser/error from FindByEmail and
// records the (tenantID, email) it was asked for.
type fakeUserReader struct {
	user        ports.AuthUser
	err         error
	gotTenantID uuid.UUID
	gotEmail    string
}

func (f *fakeUserReader) FindByEmail(_ context.Context, tenantID uuid.UUID, email string) (ports.AuthUser, error) {
	f.gotTenantID, f.gotEmail = tenantID, email
	return f.user, f.err
}

// fakeUserWriter records the rehash and last-login mutations and returns
// configurable errors so the best-effort paths are exercisable.
type fakeUserWriter struct {
	updateErr    error
	recordErr    error
	gotPHC       string
	updateCalls  int
	recordCalls  int
	gotLastLogin time.Time
}

func (f *fakeUserWriter) UpdatePasswordHash(_ context.Context, _, _ uuid.UUID, phc string) error {
	f.updateCalls++
	f.gotPHC = phc
	return f.updateErr
}

func (f *fakeUserWriter) RecordLastLogin(_ context.Context, _, _ uuid.UUID, at time.Time) error {
	f.recordCalls++
	f.gotLastLogin = at
	return f.recordErr
}

// fakeSessionStore records every call and returns configurable results. For the
// post-authentication mutations it records the tenant carried in the context, so
// a test can prove the middleware-set context reached the (adapter) boundary.
type fakeSessionStore struct {
	created       ports.NewSession
	createErr     error
	createCalls   int
	record        ports.SessionRecord
	findErr       error
	findCalls     int
	gotFindHash   []byte
	refreshErr    error
	refreshCalls  int
	gotRefreshID  uuid.UUID
	gotLastSeen   time.Time
	gotExpiresAt  time.Time
	refreshTenant tenant.ID
	deleteErr     error
	deleteCalls   int
	gotDeleteID   uuid.UUID
}

func (f *fakeSessionStore) Create(_ context.Context, s ports.NewSession) error {
	f.createCalls++
	f.created = s
	return f.createErr
}

func (f *fakeSessionStore) FindByTokenHash(_ context.Context, tokenHash []byte) (ports.SessionRecord, error) {
	f.findCalls++
	f.gotFindHash = tokenHash
	return f.record, f.findErr
}

func (f *fakeSessionStore) Refresh(ctx context.Context, id uuid.UUID, lastSeenAt, expiresAt time.Time) error {
	f.refreshCalls++
	f.gotRefreshID, f.gotLastSeen, f.gotExpiresAt = id, lastSeenAt, expiresAt
	if tid, ok := tenant.IDFromContext(ctx); ok {
		f.refreshTenant = tid
	}
	return f.refreshErr
}

func (f *fakeSessionStore) Delete(_ context.Context, id uuid.UUID) error {
	f.deleteCalls++
	f.gotDeleteID = id
	return f.deleteErr
}

// --- Spies for the injected verifier/hasher ---------------------------------

type verifyCall struct{ plaintext, phc string }

// spyVerifier records every Verify call so a test can assert the count and the
// exact (plaintext, phc) arguments — the core of the enumeration-defense proof.
type spyVerifier struct {
	calls  []verifyCall
	rehash bool
	err    error
}

func (s *spyVerifier) Verify(plaintext, phc string) (bool, error) {
	s.calls = append(s.calls, verifyCall{plaintext, phc})
	return s.rehash, s.err
}

// spyHasher records each Hash call and returns a fixed new hash unless overridden.
type spyHasher struct {
	calls []string
	err   error
}

func (s *spyHasher) Hash(plaintext string) (string, error) {
	s.calls = append(s.calls, plaintext)
	return newHash, s.err
}

// --- Shared fixtures ---------------------------------------------------------

const (
	dummyHash = "$argon2id$dummy$hash" // injected dummy hash; deny paths must use this.
	realHash  = "$argon2id$real$hash"  // the stored user hash; the real path must use this.
	newHash   = "$argon2id$rehashed$hash"
	password  = "correct horse battery staple"
	timeout   = 30 * time.Minute
)

var (
	fixedNow  = time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	tenantID  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	userID    = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	sessionID = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	rawToken  = bytes.Repeat([]byte{0xAB}, appauth.TokenBytes)
)

// harness bundles the fakes, the authenticator, and the captured log buffer.
// newHarness wires everything for a SUCCESSFUL login by default; each test
// mutates the relevant fake before acting.
type harness struct {
	resolver *fakeResolver
	users    *fakeUserReader
	writer   *fakeUserWriter
	sessions *fakeSessionStore
	verifier *spyVerifier
	hasher   *spyHasher
	logbuf   *bytes.Buffer
	auth     *appauth.Authenticator
}

func newHarness() *harness {
	h := &harness{
		resolver: &fakeResolver{ref: ports.TenantRef{ID: tenantID, Status: domain.StatusActive, SessionTimeout: timeout}},
		users: &fakeUserReader{user: ports.AuthUser{
			ID: userID, TenantID: tenantID, Email: "owner@acme.test",
			PasswordHash: realHash, Role: domain.RoleManager, Status: domain.UserStatusActive,
		}},
		writer:   &fakeUserWriter{},
		sessions: &fakeSessionStore{},
		verifier: &spyVerifier{},
		hasher:   &spyHasher{},
		logbuf:   &bytes.Buffer{},
	}
	// LevelDebug so warn (best-effort failures) and debug (idempotent logout)
	// records are captured for assertion.
	logger := observability.NewLogger(h.logbuf, slog.LevelDebug)
	h.auth = appauth.NewAuthenticator(
		h.resolver, h.users, h.writer, h.sessions,
		h.verifier, h.hasher,
		func() time.Time { return fixedNow },
		func() ([]byte, error) { return rawToken, nil },
		dummyHash, logger,
	)
	return h
}

// logLines decodes the captured JSON log buffer into one map per line.
func (h *harness) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(h.logbuf.Bytes()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\nline: %q", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// hasLogMessage reports whether any captured record has the given level and a
// message containing msgSubstr.
func (h *harness) hasLogMessage(t *testing.T, level, msgSubstr string) bool {
	t.Helper()
	for _, rec := range h.logLines(t) {
		lvl, _ := rec[slog.LevelKey].(string)
		msg, _ := rec[slog.MessageKey].(string)
		if lvl == level && strings.Contains(msg, msgSubstr) {
			return true
		}
	}
	return false
}

func defaultParams() appauth.LoginParams {
	return appauth.LoginParams{
		Slug:      "acme",
		Email:     "owner@acme.test",
		Password:  password,
		IP:        netip.MustParseAddr("203.0.113.7"),
		UserAgent: "Mozilla/5.0 (test)",
	}
}

// --- Login: success ----------------------------------------------------------

// TestLoginSuccess proves a valid credential mints a session: the returned token
// decodes to the token-source bytes, MustChangePassword is propagated, ExpiresAt
// is now+timeout, and Create receives the snapshotted role, the SHA-256 of the
// RAW token bytes, the computed expiry, and the forensic IP/user-agent.
func TestLoginSuccess(t *testing.T) {
	h := newHarness()
	h.users.user.MustChangePassword = true
	params := defaultParams()

	res, err := h.auth.Login(context.Background(), params)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if res.Token == "" {
		t.Fatal("returned token is empty")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(res.Token)
	if err != nil {
		t.Fatalf("returned token is not raw-url base64: %v", err)
	}
	if !bytes.Equal(decoded, rawToken) {
		t.Errorf("decoded token = %x, want %x", decoded, rawToken)
	}
	if !res.MustChangePassword {
		t.Error("MustChangePassword not propagated from user")
	}
	if want := fixedNow.Add(timeout); !res.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", res.ExpiresAt, want)
	}

	if h.sessions.createCalls != 1 {
		t.Fatalf("Create called %d times, want 1", h.sessions.createCalls)
	}
	got := h.sessions.created
	if got.Role != domain.RoleManager {
		t.Errorf("session role = %q, want manager (snapshot)", got.Role)
	}
	wantHash := sha256.Sum256(rawToken)
	if !bytes.Equal(got.TokenHash, wantHash[:]) {
		t.Errorf("stored token_hash = %x, want sha256(raw) %x", got.TokenHash, wantHash[:])
	}
	if want := fixedNow.Add(timeout); !got.ExpiresAt.Equal(want) {
		t.Errorf("session expires_at = %v, want %v", got.ExpiresAt, want)
	}
	if !got.IssuedAt.Equal(fixedNow) || !got.LastSeenAt.Equal(fixedNow) {
		t.Errorf("issued_at/last_seen_at = %v/%v, want %v", got.IssuedAt, got.LastSeenAt, fixedNow)
	}
	if got.IssuedFromIP != params.IP {
		t.Errorf("issued_from_ip = %v, want %v", got.IssuedFromIP, params.IP)
	}
	if got.UserAgent != params.UserAgent {
		t.Errorf("user_agent = %q, want %q", got.UserAgent, params.UserAgent)
	}
	if got.TenantID != tenantID || got.UserID != userID {
		t.Errorf("tenant/user = %v/%v, want %v/%v", got.TenantID, got.UserID, tenantID, userID)
	}

	// The success path is intentionally silent (rehash and last-login log only on
	// failure; minting logs nothing), so there is no log to assert against here.
	// Secret-absence is covered on the post-mint failure path by
	// TestLoginDoesNotLogSessionSecret.
	if h.logbuf.Len() != 0 {
		t.Errorf("success path emitted unexpected logs:\n%s", h.logbuf.String())
	}
}

// --- Login: enumeration defense (the central test) ---------------------------

// TestLoginEnumerationDefense proves the two security properties at once: every
// failure path returns the SAME ErrInvalidCredentials (no information leak), and
// EXACTLY ONE Verify happens per attempt (uniform timing). The not-found paths
// must verify against the dummy hash; the real paths against the user's hash.
func TestLoginEnumerationDefense(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(h *harness)
		wantPHC   string
		wantError bool // true => expect ErrInvalidCredentials; false => success.
	}{
		{
			name:      "unknown tenant",
			setup:     func(h *harness) { h.resolver.err = ports.ErrTenantNotFound },
			wantPHC:   dummyHash,
			wantError: true,
		},
		{
			name:      "suspended tenant",
			setup:     func(h *harness) { h.resolver.ref.Status = domain.StatusSuspended },
			wantPHC:   dummyHash,
			wantError: true,
		},
		{
			name:      "unknown email",
			setup:     func(h *harness) { h.users.err = ports.ErrUserNotFound },
			wantPHC:   dummyHash,
			wantError: true,
		},
		{
			name:      "deactivated user",
			setup:     func(h *harness) { h.users.user.Status = domain.UserStatusDeactivated },
			wantPHC:   dummyHash,
			wantError: true,
		},
		{
			name:      "wrong password",
			setup:     func(h *harness) { h.verifier.err = errors.New("mismatch") },
			wantPHC:   realHash,
			wantError: true,
		},
		{
			name:      "success",
			setup:     func(_ *harness) {},
			wantPHC:   realHash,
			wantError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			tc.setup(h)

			_, err := h.auth.Login(context.Background(), defaultParams())

			// Exactly one Verify per attempt — never zero, never two.
			if len(h.verifier.calls) != 1 {
				t.Fatalf("Verify called %d times, want exactly 1", len(h.verifier.calls))
			}
			if got := h.verifier.calls[0].phc; got != tc.wantPHC {
				t.Errorf("Verify phc = %q, want %q", got, tc.wantPHC)
			}
			if got := h.verifier.calls[0].plaintext; got != password {
				t.Errorf("Verify plaintext = %q, want the supplied password", got)
			}

			if tc.wantError {
				if !errors.Is(err, appauth.ErrInvalidCredentials) {
					t.Errorf("err = %v, want ErrInvalidCredentials", err)
				}
				return
			}
			if err != nil {
				t.Errorf("success case returned error: %v", err)
			}
		})
	}
}

// --- Login: best-effort rehash ----------------------------------------------

// TestLoginRehashUpgradesHash proves a rehash=true verify triggers a fresh hash
// and an UpdatePasswordHash with the new value.
func TestLoginRehashUpgradesHash(t *testing.T) {
	h := newHarness()
	h.verifier.rehash = true

	if _, err := h.auth.Login(context.Background(), defaultParams()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(h.hasher.calls) != 1 {
		t.Fatalf("Hash called %d times, want 1", len(h.hasher.calls))
	}
	if h.writer.updateCalls != 1 {
		t.Fatalf("UpdatePasswordHash called %d times, want 1", h.writer.updateCalls)
	}
	if h.writer.gotPHC != newHash {
		t.Errorf("UpdatePasswordHash phc = %q, want freshly hashed %q", h.writer.gotPHC, newHash)
	}
}

// TestLoginRehashPersistFailureStillSucceeds proves an ErrUserNotFound (race) on
// the rehash write does not fail the login and is logged at warn — without the
// hash appearing in the log.
func TestLoginRehashPersistFailureStillSucceeds(t *testing.T) {
	h := newHarness()
	h.verifier.rehash = true
	h.writer.updateErr = ports.ErrUserNotFound

	res, err := h.auth.Login(context.Background(), defaultParams())
	if err != nil {
		t.Fatalf("Login should still succeed on best-effort rehash failure: %v", err)
	}
	if res.Token == "" {
		t.Error("expected a session token despite rehash failure")
	}
	if !h.hasLogMessage(t, "WARN", "rehash on login") {
		t.Error("expected a warn log for the rehash failure")
	}
	if strings.Contains(h.logbuf.String(), newHash) {
		t.Error("the password hash leaked into the log")
	}
}

// TestLoginLastLoginFailureStillSucceeds proves an ErrUserNotFound on the
// last-login stamp does not fail an otherwise-valid login.
func TestLoginLastLoginFailureStillSucceeds(t *testing.T) {
	h := newHarness()
	h.writer.recordErr = ports.ErrUserNotFound

	res, err := h.auth.Login(context.Background(), defaultParams())
	if err != nil {
		t.Fatalf("Login should still succeed on best-effort last-login failure: %v", err)
	}
	if res.Token == "" {
		t.Error("expected a session token despite last-login failure")
	}
	if h.writer.recordCalls != 1 {
		t.Errorf("RecordLastLogin called %d times, want 1", h.writer.recordCalls)
	}
	if !h.writer.gotLastLogin.Equal(fixedNow) {
		t.Errorf("last_login_at = %v, want injected now %v", h.writer.gotLastLogin, fixedNow)
	}
	if !h.hasLogMessage(t, "WARN", "record last login") {
		t.Error("expected a warn log for the last-login failure")
	}
}

// --- Login: session-create failure ------------------------------------------

// TestLoginSessionCreateFailure proves a Create error is a wrapped internal
// fault, not a credential problem and not a raw error.
func TestLoginSessionCreateFailure(t *testing.T) {
	h := newHarness()
	h.sessions.createErr = errors.New("db is down")

	_, err := h.auth.Login(context.Background(), defaultParams())
	if err == nil {
		t.Fatal("expected an error when Create fails")
	}
	if !errors.Is(err, apperr.ErrInternal) {
		t.Errorf("err = %v, want wrapped apperr.ErrInternal", err)
	}
	if errors.Is(err, appauth.ErrInvalidCredentials) {
		t.Error("a creation fault must not surface as ErrInvalidCredentials")
	}
}

// TestLoginDoesNotLogSessionSecret proves the session token and its token_hash
// never reach the logs on the post-mint path. The Create-failure path is the
// most dangerous one — it has the freshly-minted NewSession (carrying TokenHash)
// in scope while it builds the error — so we drive Login into it and assert that
// none of the secret's plausible encodings appears in the captured buffer: the
// raw-url cookie value, and the SHA-256 hash in hex and in both std-base64
// variants (a stray "%+v" of the struct or of the byte slice would surface one
// of these).
func TestLoginDoesNotLogSessionSecret(t *testing.T) {
	h := newHarness()
	h.sessions.createErr = errors.New("db is down") // reach minting, then fail at Create.

	if _, err := h.auth.Login(context.Background(), defaultParams()); err == nil {
		t.Fatal("expected an error when Create fails")
	}

	sum := sha256.Sum256(rawToken)
	secrets := map[string]string{
		"cookie token (raw-url base64)": base64.RawURLEncoding.EncodeToString(rawToken),
		"token_hash (hex)":              hex.EncodeToString(sum[:]),
		"token_hash (std base64)":       base64.StdEncoding.EncodeToString(sum[:]),
		"token_hash (raw-std base64)":   base64.RawStdEncoding.EncodeToString(sum[:]),
	}

	logged := h.logbuf.String()
	for name, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Errorf("session secret leaked into logs: %s = %q\nlog buffer:\n%s", name, secret, logged)
		}
	}
}

// --- Authenticate ------------------------------------------------------------

// validToken is the cookie value the token-source bytes encode to.
func validToken() string { return base64.RawURLEncoding.EncodeToString(rawToken) }

// activeRecord is a valid, unexpired session of an active tenant.
func activeRecord() ports.SessionRecord {
	return ports.SessionRecord{
		ID: sessionID, TenantID: tenantID, UserID: userID, Role: domain.RoleManager,
		IssuedAt: fixedNow.Add(-time.Minute), LastSeenAt: fixedNow.Add(-time.Minute),
		ExpiresAt: fixedNow.Add(timeout), SessionTimeout: timeout, TenantStatus: domain.StatusActive,
	}
}

// TestAuthenticateSuccess proves a valid token yields the right principal, with
// the role taken from the session snapshot.
func TestAuthenticateSuccess(t *testing.T) {
	h := newHarness()
	h.sessions.record = activeRecord()

	p, err := h.auth.Authenticate(context.Background(), validToken())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.TenantID != tenantID || p.UserID != userID || p.SessionID != sessionID {
		t.Errorf("principal ids = %v/%v/%v, want %v/%v/%v", p.TenantID, p.UserID, p.SessionID, tenantID, userID, sessionID)
	}
	if p.Role != domain.RoleManager {
		t.Errorf("principal role = %q, want manager (snapshot)", p.Role)
	}
	if !p.ExpiresAt.Equal(fixedNow.Add(timeout)) {
		t.Errorf("principal ExpiresAt = %v, want %v", p.ExpiresAt, fixedNow.Add(timeout))
	}
	// The lookup hash must be the SHA-256 of the raw bytes.
	wantHash := sha256.Sum256(rawToken)
	if !bytes.Equal(h.sessions.gotFindHash, wantHash[:]) {
		t.Errorf("lookup hash = %x, want %x", h.sessions.gotFindHash, wantHash[:])
	}
}

// TestAuthenticateMalformedToken proves a non-base64 token and a correctly
// encoded but wrong-length token are both rejected BEFORE the store is queried.
func TestAuthenticateMalformedToken(t *testing.T) {
	cases := map[string]string{
		"not base64":   "!!! not base64 !!!",
		"wrong length": base64.RawURLEncoding.EncodeToString([]byte("only-16-bytes!!!")),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			_, err := h.auth.Authenticate(context.Background(), token)
			if !errors.Is(err, appauth.ErrSessionInvalid) {
				t.Errorf("err = %v, want ErrSessionInvalid", err)
			}
			if h.sessions.findCalls != 0 {
				t.Errorf("FindByTokenHash called %d times, want 0 (rejected before DB)", h.sessions.findCalls)
			}
		})
	}
}

// TestAuthenticateUnknownSession proves an ErrSessionNotFound maps to
// ErrSessionInvalid.
func TestAuthenticateUnknownSession(t *testing.T) {
	h := newHarness()
	h.sessions.findErr = ports.ErrSessionNotFound

	_, err := h.auth.Authenticate(context.Background(), validToken())
	if !errors.Is(err, appauth.ErrSessionInvalid) {
		t.Errorf("err = %v, want ErrSessionInvalid", err)
	}
}

// TestAuthenticateExpiredSession proves an expired session is invalid and that
// no delete is attempted on the read-only path.
func TestAuthenticateExpiredSession(t *testing.T) {
	h := newHarness()
	rec := activeRecord()
	rec.ExpiresAt = fixedNow.Add(-time.Second) // expired one second ago.
	h.sessions.record = rec

	_, err := h.auth.Authenticate(context.Background(), validToken())
	if !errors.Is(err, appauth.ErrSessionInvalid) {
		t.Errorf("err = %v, want ErrSessionInvalid", err)
	}
	if h.sessions.deleteCalls != 0 {
		t.Errorf("Delete called %d times on expiry, want 0 (read-only path)", h.sessions.deleteCalls)
	}
}

// TestAuthenticateSuspendedTenant proves a live-suspended tenant yields the
// distinct ErrTenantSuspended (decision 3), not ErrSessionInvalid.
func TestAuthenticateSuspendedTenant(t *testing.T) {
	h := newHarness()
	rec := activeRecord()
	rec.TenantStatus = domain.StatusSuspended
	h.sessions.record = rec

	_, err := h.auth.Authenticate(context.Background(), validToken())
	if !errors.Is(err, appauth.ErrTenantSuspended) {
		t.Errorf("err = %v, want ErrTenantSuspended", err)
	}
	if errors.Is(err, appauth.ErrSessionInvalid) {
		t.Error("suspended tenant must not collapse into ErrSessionInvalid")
	}
}

// --- Refresh -----------------------------------------------------------------

// TestRefreshSlidesWindow proves Refresh writes last_seen=now and
// expires=now+timeout for the session id, carrying the tenant from context.
func TestRefreshSlidesWindow(t *testing.T) {
	h := newHarness()
	ctx := tenant.NewContext(context.Background(), tenant.ID(tenantID))

	if err := h.auth.Refresh(ctx, sessionID, timeout); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if h.sessions.refreshCalls != 1 {
		t.Fatalf("Refresh called %d times, want 1", h.sessions.refreshCalls)
	}
	if h.sessions.gotRefreshID != sessionID {
		t.Errorf("refresh id = %v, want %v", h.sessions.gotRefreshID, sessionID)
	}
	if !h.sessions.gotLastSeen.Equal(fixedNow) {
		t.Errorf("last_seen_at = %v, want %v", h.sessions.gotLastSeen, fixedNow)
	}
	if want := fixedNow.Add(timeout); !h.sessions.gotExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", h.sessions.gotExpiresAt, want)
	}
	if h.sessions.refreshTenant != tenant.ID(tenantID) {
		t.Errorf("tenant from context = %v, want %v", h.sessions.refreshTenant, tenant.ID(tenantID))
	}
}

// TestRefreshVanishedSession proves a zero-rows result propagates as
// ErrSessionNotFound.
func TestRefreshVanishedSession(t *testing.T) {
	h := newHarness()
	h.sessions.refreshErr = ports.ErrSessionNotFound
	ctx := tenant.NewContext(context.Background(), tenant.ID(tenantID))

	err := h.auth.Refresh(ctx, sessionID, timeout)
	if !errors.Is(err, ports.ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

// --- Logout ------------------------------------------------------------------

// TestLogoutSuccess proves a deleted session returns nil.
func TestLogoutSuccess(t *testing.T) {
	h := newHarness()
	ctx := tenant.NewContext(context.Background(), tenant.ID(tenantID))

	if err := h.auth.Logout(ctx, sessionID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if h.sessions.deleteCalls != 1 || h.sessions.gotDeleteID != sessionID {
		t.Errorf("Delete calls=%d id=%v, want 1 and %v", h.sessions.deleteCalls, h.sessions.gotDeleteID, sessionID)
	}
}

// TestLogoutIdempotent proves logging out an already-gone session is success.
func TestLogoutIdempotent(t *testing.T) {
	h := newHarness()
	h.sessions.deleteErr = ports.ErrSessionNotFound
	ctx := tenant.NewContext(context.Background(), tenant.ID(tenantID))

	if err := h.auth.Logout(ctx, sessionID); err != nil {
		t.Errorf("Logout of absent session should be nil (idempotent), got %v", err)
	}
}
