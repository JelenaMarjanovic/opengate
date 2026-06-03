package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	httpadapter "github.com/JelenaMarjanovic/opengate/internal/adapters/inbound/http"
	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	appauth "github.com/JelenaMarjanovic/opengate/internal/application/auth"
	"github.com/JelenaMarjanovic/opengate/internal/auth"
	"github.com/JelenaMarjanovic/opengate/internal/observability"
	"github.com/JelenaMarjanovic/opengate/internal/testsupport"
)

// sessionCookieName is the cookie name the api issues when CookieSecure is false
// (the integration mode). It mirrors the inbound adapter's unexported
// sessionCookieBaseName; the adapter owns the constant, this is the wire-contract
// value the test client looks for.
const sessionCookieName = "opengate_session"

// The shared fixture: one tenant (known slug) and one active owner whose password
// is hashed with the real Argon2id hasher. must_change_password is true so AC-1
// can assert the flag round-trips from the DB through the login response.
const (
	fixtureSlug     = "acme-climbing"
	fixtureEmail    = "owner@acme.test"
	fixturePassword = "correct horse battery staple"
)

// authEnv bundles the running test server, the pools, and the seeded fixture so
// every subtest shares ONE container (testcontainers startup dominates wall time).
type authEnv struct {
	t        *testing.T
	ctx      context.Context
	server   *httptest.Server
	bypass   *pgxpool.Pool // pre-auth pool: seeding + direct assertions
	super    *pgxpool.Pool // bypass-capable: out-of-band tenant suspension
	tenantID uuid.UUID
	userID   uuid.UUID
}

// TestAuthAPIIntegration is the first end-to-end HTTP test of the auth chain: a
// real router wired with the real Authenticator over BOTH pools against a
// testcontainers Postgres, exercising every US-02.03 acceptance criterion and the
// middleware edge cases. The cookie is configured non-Secure so the plain-HTTP
// httptest client round-trips it.
func TestAuthAPIIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in short mode")
	}

	env := setupAuthEnv(t)

	t.Run("AC-1 login success sets cookie and persists a session", env.testLoginSuccess)
	t.Run("login failure is uniform (wrong password, unknown email, unknown slug)", env.testLoginUniformFailure)
	t.Run("login validation rejects missing fields with 422", env.testLoginValidation)
	t.Run("AC-2 expired session is 401 and does not touch last_seen_at", env.testExpiredSession)
	t.Run("AC-3 valid session is 200 and slides the window", env.testValidSession)
	t.Run("AC-4 logout is 204, deletes the row, and re-rejects the stale cookie", env.testLogout)
	t.Run("middleware rejects a missing cookie with 401", env.testNoCookie)
	t.Run("middleware rejects a malformed cookie with 401", env.testMalformedCookie)
	t.Run("middleware rejects a suspended tenant's session with 403", env.testSuspendedTenant)
}

// --- AC-1 -------------------------------------------------------------------

func (e *authEnv) testLoginSuccess(t *testing.T) {
	resp, body := e.postJSON(t, e.loginURL(fixtureSlug),
		fmt.Sprintf(`{"email":%q,"password":%q}`, fixtureEmail, fixturePassword))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	// Set-Cookie attributes: HttpOnly and SameSite=Lax (System Design §9).
	cookie := sessionCookieFrom(t, resp)
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	// Session cookie: no Max-Age / Expires (the server owns expiry).
	if cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Errorf("session cookie carries a lifetime (MaxAge=%d Expires=%v); want a session cookie", cookie.MaxAge, cookie.Expires)
	}

	// The stored token_hash equals sha256 of the decoded cookie token.
	wantHash := tokenHashFromCookie(t, cookie.Value)
	var count int
	if err := e.bypass.QueryRow(e.ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND token_hash = $2`, e.userID, wantHash,
	).Scan(&count); err != nil {
		t.Fatalf("query session row: %v", err)
	}
	if count != 1 {
		t.Errorf("sessions rows for (user, token_hash) = %d, want 1", count)
	}

	// The body conveys must_change_password, sourced from the seeded user (true).
	var lr struct {
		MustChangePassword bool      `json:"must_change_password"`
		ExpiresAt          time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &lr); err != nil {
		t.Fatalf("login body is not JSON: %v\nbody: %s", err, body)
	}
	if !lr.MustChangePassword {
		t.Error("login body must_change_password = false, want true (seeded user)")
	}
	if lr.ExpiresAt.IsZero() {
		t.Error("login body expires_at is zero, want a future instant")
	}
}

// --- Login failure: uniform -------------------------------------------------

func (e *authEnv) testLoginUniformFailure(t *testing.T) {
	cases := []struct {
		name, url, body string
	}{
		{"wrong password", e.loginURL(fixtureSlug),
			fmt.Sprintf(`{"email":%q,"password":"wrong-password"}`, fixtureEmail)},
		{"unknown email", e.loginURL(fixtureSlug),
			`{"email":"ghost@acme.test","password":"any-password"}`},
		{"unknown slug", e.loginURL("no-such-tenant"),
			fmt.Sprintf(`{"email":%q,"password":%q}`, fixtureEmail, fixturePassword)},
	}

	var titles, details []string
	for _, tc := range cases {
		resp, body := e.postJSON(t, tc.url, tc.body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401; body: %s", tc.name, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: Content-Type = %q, want application/problem+json", tc.name, ct)
		}
		pd := decodeProblem(t, body)
		titles = append(titles, pd.Title)
		details = append(details, pd.Detail)
	}

	// The enumeration defense reaches HTTP: all three are byte-identical.
	for i := 1; i < len(titles); i++ {
		if titles[i] != titles[0] || details[i] != details[0] {
			t.Errorf("non-uniform failure: case %d {%q,%q} != case 0 {%q,%q}",
				i, titles[i], details[i], titles[0], details[0])
		}
	}
}

// --- Login validation -------------------------------------------------------

func (e *authEnv) testLoginValidation(t *testing.T) {
	cases := map[string]struct {
		body        string
		wantPointer string
	}{
		"missing email":    {`{"password":"x"}`, "/email"},
		"missing password": {fmt.Sprintf(`{"email":%q}`, fixtureEmail), "/password"},
		"empty email":      {`{"email":"","password":"x"}`, "/email"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp, body := e.postJSON(t, e.loginURL(fixtureSlug), tc.body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", resp.StatusCode, body)
			}
			pd := decodeProblem(t, body)
			if len(pd.Errors) == 0 {
				t.Fatalf("422 body carries no errors array: %s", body)
			}
			found := false
			for _, fe := range pd.Errors {
				if fe.Pointer == tc.wantPointer && fe.Code == "required" {
					found = true
				}
			}
			if !found {
				t.Errorf("errors array %+v does not contain required %q", pd.Errors, tc.wantPointer)
			}
		})
	}
}

// --- AC-2: expired session --------------------------------------------------

func (e *authEnv) testExpiredSession(t *testing.T) {
	_, cookieValue, tokenHash := deriveToken("ac2-expired-session")
	sessionID := uuid.New()
	past := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Hour)
	lastSeen := past // capture: the middleware must NOT advance this.
	e.insertSession(t, sessionID, e.tenantID, e.userID, tokenHash, lastSeen, past)

	resp, body := e.getWithCookie(t, e.whoamiURL(), cookieValue)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired whoami status = %d, want 401; body: %s", resp.StatusCode, body)
	}

	var got time.Time
	if err := e.bypass.QueryRow(e.ctx,
		`SELECT last_seen_at FROM sessions WHERE id = $1`, sessionID,
	).Scan(&got); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	if !got.Equal(lastSeen) {
		t.Errorf("last_seen_at = %s, want unchanged %s (middleware must not refresh an expired session)", got, lastSeen)
	}
}

// --- AC-3: valid session ----------------------------------------------------

func (e *authEnv) testValidSession(t *testing.T) {
	client := e.jarClient(t)

	// A real login is the cleanest source of a valid session.
	resp, body := e.postJSONClient(t, client, e.loginURL(fixtureSlug),
		fmt.Sprintf(`{"email":%q,"password":%q}`, fixtureEmail, fixturePassword))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	cookie := sessionCookieFrom(t, resp)
	tokenHash := tokenHashFromCookie(t, cookie.Value)

	before := e.readSession(t, tokenHash)

	// whoami over the same jar (the cookie is resent automatically).
	wResp, wBody := e.getClient(t, client, e.whoamiURL())
	if wResp.StatusCode != http.StatusOK {
		t.Fatalf("whoami status = %d, want 200; body: %s", wResp.StatusCode, wBody)
	}
	var who struct {
		UserID   string `json:"user_id"`
		Role     string `json:"role"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal([]byte(wBody), &who); err != nil {
		t.Fatalf("whoami body is not JSON: %v\nbody: %s", err, wBody)
	}
	if who.UserID != e.userID.String() || who.TenantID != e.tenantID.String() || who.Role != "owner" {
		t.Errorf("whoami identity = %+v, want user=%s tenant=%s role=owner", who, e.userID, e.tenantID)
	}

	after := e.readSession(t, tokenHash)
	if !after.lastSeen.After(before.lastSeen) {
		t.Errorf("last_seen_at did not advance: before=%s after=%s", before.lastSeen, after.lastSeen)
	}
	if !after.expiresAt.After(before.expiresAt) {
		t.Errorf("expires_at did not slide forward: before=%s after=%s", before.expiresAt, after.expiresAt)
	}
}

// --- AC-4: logout -----------------------------------------------------------

func (e *authEnv) testLogout(t *testing.T) {
	client := e.jarClient(t)

	resp, body := e.postJSONClient(t, client, e.loginURL(fixtureSlug),
		fmt.Sprintf(`{"email":%q,"password":%q}`, fixtureEmail, fixturePassword))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	loginCookie := sessionCookieFrom(t, resp)
	tokenHash := tokenHashFromCookie(t, loginCookie.Value)

	// Logout over the jar (cookie resent automatically).
	lResp, lBody := e.postJSONClient(t, client, e.logoutURL(), "")
	if lResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204; body: %s", lResp.StatusCode, lBody)
	}
	// The clearing Set-Cookie expires the cookie (Max-Age <= 0).
	cleared := sessionCookieFrom(t, lResp)
	if cleared.MaxAge >= 0 && cleared.Value != "" {
		t.Errorf("logout Set-Cookie did not clear the cookie: MaxAge=%d value=%q", cleared.MaxAge, cleared.Value)
	}

	// The session row is gone.
	var count int
	if err := e.bypass.QueryRow(e.ctx,
		`SELECT count(*) FROM sessions WHERE token_hash = $1`, tokenHash,
	).Scan(&count); err != nil {
		t.Fatalf("count session after logout: %v", err)
	}
	if count != 0 {
		t.Errorf("session row count after logout = %d, want 0", count)
	}

	// A second logout with the now-stale cookie is rejected by the middleware
	// (the session no longer exists) BEFORE the handler runs: 401.
	sResp, sBody := e.getWithCookieMethod(t, http.MethodPost, e.logoutURL(), loginCookie.Value)
	if sResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("second logout with stale cookie = %d, want 401; body: %s", sResp.StatusCode, sBody)
	}
}

// --- Middleware edges -------------------------------------------------------

func (e *authEnv) testNoCookie(t *testing.T) {
	resp, body := e.getClient(t, e.server.Client(), e.whoamiURL())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami without a cookie = %d, want 401; body: %s", resp.StatusCode, body)
	}
}

func (e *authEnv) testMalformedCookie(t *testing.T) {
	// A non-base64, wrong-length value. Authenticate rejects it before the store
	// is queried (proven at the use-case seam by TestAuthenticateMalformedToken);
	// here we assert the HTTP outcome.
	resp, body := e.getWithCookie(t, e.whoamiURL(), "!!! not a valid token !!!")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("whoami with a malformed cookie = %d, want 401; body: %s", resp.StatusCode, body)
	}
}

func (e *authEnv) testSuspendedTenant(t *testing.T) {
	// A dedicated tenant/user/session so suspending it cannot disturb the shared
	// fixture the other subtests rely on.
	susTenant := uuid.New()
	e.seedTenant(t, susTenant, "Suspended Gym", "suspended-gym")
	susUser := uuid.New()
	e.seedUser(t, susUser, susTenant, "sus@suspended.test", mustHash(t, "pw"), false)

	_, cookieValue, tokenHash := deriveToken("suspended-tenant-session")
	sessionID := uuid.New()
	future := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	e.insertSession(t, sessionID, susTenant, susUser, tokenHash, future.Add(-time.Hour), future)

	// Suspend the tenant out-of-band via the superuser (opengate_bypass holds no
	// UPDATE on tenants — status is an operator action).
	if _, err := e.super.Exec(e.ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, susTenant); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	resp, body := e.getWithCookie(t, e.whoamiURL(), cookieValue)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("suspended-tenant whoami = %d, want 403; body: %s", resp.StatusCode, body)
	}
	pd := decodeProblem(t, body)
	if pd.Status != http.StatusForbidden {
		t.Errorf("problem status = %d, want 403", pd.Status)
	}
	if !strings.Contains(pd.Type, "tenant-suspended") {
		t.Errorf("problem type = %q, want a tenant-suspended type", pd.Type)
	}
}

// --- Setup ------------------------------------------------------------------

func setupAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	ctx := context.Background()
	container := startMigratedPostgres(ctx, t)

	bypass := openPool(ctx, t, roleDSN(ctx, t, container, "opengate_bypass"))
	super := openPool(ctx, t, superConnString(ctx, t, container))
	// The regular RLS-bound pool with the real tenant-binding hooks. A discard
	// logger at error level keeps the post-auth (tenant-present) path quiet.
	regular, err := postgres.NewPool(ctx, roleDSN(ctx, t, container, "opengate_app"),
		observability.NewLogger(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("open regular pool: %v", err)
	}
	t.Cleanup(regular.Close)

	authenticator := appauth.NewAuthenticator(
		postgres.NewTenantResolver(bypass),
		postgres.NewUserReader(bypass),
		postgres.NewUserWriter(bypass),
		postgres.NewSessionStore(bypass, regular),
		appauth.VerifierFunc(auth.VerifyPassword),
		appauth.HasherFunc(auth.HashPassword),
		time.Now,
		appauth.CryptoRandToken,
		auth.MustDummyHash(),
		observability.NewLogger(io.Discard, slog.LevelError),
	)

	router := httpadapter.NewRouter(httpadapter.Config{
		Pinger:        bypass,
		Authenticator: authenticator,
		CookieSecure:  false, // plain-HTTP httptest client must receive the cookie
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	env := &authEnv{
		t: t, ctx: ctx, server: server, bypass: bypass, super: super,
		tenantID: uuid.New(), userID: uuid.New(),
	}
	env.seedTenant(t, env.tenantID, "Acme Climbing", fixtureSlug)
	env.seedUser(t, env.userID, env.tenantID, fixtureEmail, mustHash(t, fixturePassword), true)
	return env
}

// --- URL helpers ------------------------------------------------------------

func (e *authEnv) loginURL(slug string) string {
	return e.server.URL + "/api/v1/tenants/" + slug + "/auth/login"
}
func (e *authEnv) logoutURL() string { return e.server.URL + "/api/v1/auth/logout" }
func (e *authEnv) whoamiURL() string { return e.server.URL + "/api/v1/auth/whoami" }

// --- HTTP helpers -----------------------------------------------------------

// jarClient returns an http.Client with a fresh cookie jar so a login's
// Set-Cookie is resent on subsequent requests to the same host.
func (e *authEnv) jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	c := e.server.Client()
	c2 := *c
	c2.Jar = jar
	c2.Timeout = 10 * time.Second
	return &c2
}

func (e *authEnv) postJSON(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	return e.postJSONClient(t, e.server.Client(), url, body)
}

func (e *authEnv) postJSONClient(t *testing.T, client *http.Client, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp, readClose(t, resp)
}

func (e *authEnv) getClient(t *testing.T, client *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp, readClose(t, resp)
}

// getWithCookie issues a GET carrying exactly one manually-attached session
// cookie (no jar), so the test controls precisely what reaches the server.
func (e *authEnv) getWithCookie(t *testing.T, url, cookieValue string) (*http.Response, string) {
	t.Helper()
	return e.getWithCookieMethod(t, http.MethodGet, url, cookieValue)
}

func (e *authEnv) getWithCookieMethod(t *testing.T, method, url, cookieValue string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp, readClose(t, resp)
}

func readClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// sessionCookieFrom returns the session cookie from a response's Set-Cookie
// headers, failing if absent.
func sessionCookieFrom(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("no %q cookie in response Set-Cookie headers", sessionCookieName)
	return nil
}

// --- Problem-body decoding --------------------------------------------------

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Pointer string `json:"pointer"`
		Code    string `json:"code"`
		Detail  string `json:"detail"`
	} `json:"errors"`
}

func decodeProblem(t *testing.T, body string) problemBody {
	t.Helper()
	var pd problemBody
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem body is not JSON: %v\nbody: %s", err, body)
	}
	return pd
}

// --- Token helpers ----------------------------------------------------------

// deriveToken builds a deterministic 32-byte token from a label, returning the
// raw bytes, the raw-url base64 cookie value, and the SHA-256 token_hash. Distinct
// labels yield distinct hashes, so direct session inserts never collide on the
// sessions_token_hash_unique constraint.
func deriveToken(label string) (raw []byte, cookieValue string, tokenHash []byte) {
	sum := sha256.Sum256([]byte(label))
	raw = sum[:] // exactly 32 bytes, the length Authenticate requires
	cookieValue = base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256(raw)
	tokenHash = h[:]
	return raw, cookieValue, tokenHash
}

// tokenHashFromCookie decodes a real cookie value and returns sha256(raw), the
// stored token_hash, mirroring how the use case derives it at issue time.
func tokenHashFromCookie(t *testing.T, cookieValue string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cookieValue)
	if err != nil {
		t.Fatalf("cookie value is not raw-url base64: %v", err)
	}
	sum := sha256.Sum256(raw)
	return sum[:]
}

// --- DB helpers -------------------------------------------------------------

type sessionTimes struct {
	lastSeen  time.Time
	expiresAt time.Time
}

func (e *authEnv) readSession(t *testing.T, tokenHash []byte) sessionTimes {
	t.Helper()
	var st sessionTimes
	if err := e.bypass.QueryRow(e.ctx,
		`SELECT last_seen_at, expires_at FROM sessions WHERE token_hash = $1`, tokenHash,
	).Scan(&st.lastSeen, &st.expiresAt); err != nil {
		t.Fatalf("read session times: %v", err)
	}
	return st
}

func (e *authEnv) seedTenant(t *testing.T, id uuid.UUID, name, slug string) {
	t.Helper()
	if _, err := e.bypass.Exec(e.ctx,
		`INSERT INTO tenants (id, name, slug, status, session_timeout)
		 VALUES ($1, $2, $3, 'active', make_interval(mins => 60))`,
		id, name, slug); err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
}

func (e *authEnv) seedUser(t *testing.T, id, tenantID uuid.UUID, email, passwordHash string, mustChange bool) {
	t.Helper()
	if _, err := e.bypass.Exec(e.ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, status, must_change_password)
		 VALUES ($1, $2, $3, $4, 'owner', 'active', $5)`,
		id, tenantID, email, passwordHash, mustChange); err != nil {
		t.Fatalf("seed user %q: %v", email, err)
	}
}

func (e *authEnv) insertSession(t *testing.T, id, tenantID, userID uuid.UUID, tokenHash []byte, lastSeen, expiresAt time.Time) {
	t.Helper()
	if _, err := e.bypass.Exec(e.ctx,
		`INSERT INTO sessions (id, tenant_id, user_id, token_hash, role, issued_at, last_seen_at, expires_at)
		 VALUES ($1, $2, $3, $4, 'owner', $5, $6, $7)`,
		id, tenantID, userID, tokenHash, lastSeen, lastSeen, expiresAt); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// mustHash hashes a plaintext with the real Argon2id hasher, the same one login
// verifies against, so a seeded user can actually log in.
func mustHash(t *testing.T, plaintext string) string {
	t.Helper()
	h, err := auth.HashPassword(plaintext)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return h
}

// --- Container + pool helpers -----------------------------------------------

func startMigratedPostgres(ctx context.Context, t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	container := testsupport.StartPostgres(ctx, t)
	applyMigrations(ctx, t, superConnString(ctx, t, container))
	return container
}

// applyMigrations runs every embedded migration up as the superuser (needed for
// the CREATE ROLE in create_app_roles).
func applyMigrations(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	sub, err := fs.Sub(postgres.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

func openPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// roleDSN builds a connection string for one of the application roles created by
// create_app_roles (password 'placeholder'), against the container's host/port.
func roleDSN(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer, role string) string {
	t.Helper()
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return fmt.Sprintf("postgres://%s:placeholder@%s:%s/opengate_test?sslmode=disable",
		role, host, port.Port())
}

func superConnString(ctx context.Context, t *testing.T, c *tcpostgres.PostgresContainer) string {
	t.Helper()
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("superuser connection string: %v", err)
	}
	return dsn
}
