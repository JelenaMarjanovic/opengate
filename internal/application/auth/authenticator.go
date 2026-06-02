package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// Authenticator orchestrates dashboard authentication over the outbound ports.
// It depends only on ports and injected collaborators (verifier, hasher, clock,
// token source, dummy hash, logger) — never on concrete adapters — so it is
// fully testable with fakes (System Design §7).
type Authenticator struct {
	resolver   ports.TenantResolver
	users      ports.UserReader
	userWriter ports.UserWriter
	sessions   ports.SessionStore
	verifier   PasswordVerifier
	hasher     PasswordHasher
	now        Clock
	tokens     TokenSource
	// dummyHash is a fixed Argon2id hash carrying the current parameters, used
	// only by the Login enumeration defense to equalize timing on the not-found
	// paths. Produced once in the composition root via auth.MustDummyHash.
	dummyHash string
	logger    *slog.Logger
}

// NewAuthenticator wires the use case. The pre/post-authentication pool split
// lives in the adapters behind the ports, so the caller passes ports, not pools.
// dummyHash must be a real PHC hash at current parameters (auth.MustDummyHash);
// now, tokens, verifier, and hasher are injected for deterministic tests.
func NewAuthenticator(
	resolver ports.TenantResolver,
	users ports.UserReader,
	userWriter ports.UserWriter,
	sessions ports.SessionStore,
	verifier PasswordVerifier,
	hasher PasswordHasher,
	now Clock,
	tokens TokenSource,
	dummyHash string,
	logger *slog.Logger,
) *Authenticator {
	return &Authenticator{
		resolver:   resolver,
		users:      users,
		userWriter: userWriter,
		sessions:   sessions,
		verifier:   verifier,
		hasher:     hasher,
		now:        now,
		tokens:     tokens,
		dummyHash:  dummyHash,
		logger:     logger,
	}
}

// LoginParams is the input to Login: the tenant slug and credentials plus the
// forensic IP/user-agent recorded on the minted session.
type LoginParams struct {
	Slug      string
	Email     string
	Password  string
	IP        netip.Addr
	UserAgent string
}

// LoginResult is what a successful Login returns to the caller (Step 5 sets the
// cookie from Token and reacts to MustChangePassword). It carries no secret
// beyond the opaque session token itself.
type LoginResult struct {
	// Token is the base64 (raw-url) cookie value the client presents on
	// subsequent requests. The stored token_hash is the SHA-256 of the raw bytes,
	// not of this string.
	Token              string
	MustChangePassword bool
	ExpiresAt          time.Time
}

// Principal is the validated identity Authenticate returns: enough for the
// middleware to set the tenant context and authorize the request, with the role
// taken from the session snapshot (decision 2).
type Principal struct {
	TenantID  uuid.UUID
	UserID    uuid.UUID
	Role      domain.Role
	SessionID uuid.UUID
	ExpiresAt time.Time
	// SessionTimeout is the owning tenant's configured idle window, carried out
	// of the by-token lookup so the middleware can slide the window via Refresh
	// (which takes the timeout as an argument) WITHOUT a second tenant query. It
	// is identity-adjacent metadata, not a secret.
	SessionTimeout time.Duration
}

// Login verifies a credential and, on success, mints a session. The control flow
// is security-critical: exactly one password Verify happens per invocation (the
// real one when the user exists, a dummy one on every not-found path), every
// failure before success returns the uniform ErrInvalidCredentials, and the
// timing of the not-found paths matches the user-exists path.
func (a *Authenticator) Login(ctx context.Context, params LoginParams) (LoginResult, error) {
	// 1. Resolve the tenant by slug. A genuinely unknown slug is treated exactly
	// like a wrong password (dummy verify + uniform error) so an attacker cannot
	// enumerate tenant slugs by latency or error. A non-not-found error is a real
	// fault and surfaces as an internal error, not a credential outcome.
	tenantRef, err := a.resolver.ResolveBySlug(ctx, params.Slug)
	if err != nil {
		if errors.Is(err, ports.ErrTenantNotFound) {
			return a.denyLogin(params.Password)
		}
		return LoginResult{}, fmt.Errorf("resolve tenant: %w", err)
	}
	// A suspended (or otherwise non-active) tenant is also indistinguishable from
	// bad credentials pre-authentication: same dummy verify, same uniform error.
	if tenantRef.Status != domain.StatusActive {
		return a.denyLogin(params.Password)
	}

	// 2. Look up the user. tenantRef.ID is passed EXPLICITLY — this runs on the
	// bypass pool and must not depend on a tenant context. An unknown email and a
	// deactivated account are, like a wrong password, indistinguishable to the
	// caller: dummy verify + uniform error.
	user, err := a.users.FindByEmail(ctx, tenantRef.ID, params.Email)
	if err != nil {
		if errors.Is(err, ports.ErrUserNotFound) {
			return a.denyLogin(params.Password)
		}
		return LoginResult{}, fmt.Errorf("find user: %w", err)
	}
	if user.Status != domain.UserStatusActive {
		return a.denyLogin(params.Password)
	}

	// 3. The single real verification for the user-exists path. Any verify error
	// — a mismatch OR a corrupt stored hash — collapses to the same uniform
	// credential error; we do NOT verify a second time here, preserving the
	// one-Verify-per-attempt invariant. Across all paths: exactly one Verify.
	rehash, err := a.verifier.Verify(params.Password, user.PasswordHash)
	if err != nil {
		return LoginResult{}, ErrInvalidCredentials
	}

	// 4. Success. Post-success work is best-effort except minting the session.
	now := a.now()
	a.maybeRehash(ctx, user, params.Password, rehash)
	a.recordLastLogin(ctx, user, now)
	return a.mintSession(ctx, tenantRef, user, params, now)
}

// denyLogin performs the single dummy verification that equalizes timing with
// the user-exists path, then returns the uniform credential error. The return of
// the dummy Verify is discarded entirely (rehash and err alike): it exists only
// to burn the same CPU, not to influence the outcome.
func (a *Authenticator) denyLogin(password string) (LoginResult, error) {
	_, _ = a.verifier.Verify(password, a.dummyHash)
	return LoginResult{}, ErrInvalidCredentials
}

// maybeRehash upgrades the stored hash when Verify reported weaker-than-current
// parameters. It is best-effort: a hashing or write failure (including the
// ErrUserNotFound race where the user vanished) is logged at warn and swallowed —
// the old hash still works and the next login retries. The hash itself is never
// logged.
func (a *Authenticator) maybeRehash(ctx context.Context, user ports.AuthUser, password string, rehash bool) {
	if !rehash {
		return
	}
	newHash, err := a.hasher.Hash(password)
	if err != nil {
		a.logger.WarnContext(ctx, "rehash on login: hashing failed, keeping old hash",
			slog.String("user_id", user.ID.String()), slog.String("error", err.Error()))
		return
	}
	if err := a.userWriter.UpdatePasswordHash(ctx, user.TenantID, user.ID, newHash); err != nil {
		a.logger.WarnContext(ctx, "rehash on login: persist failed, keeping old hash",
			slog.String("user_id", user.ID.String()), slog.String("error", err.Error()))
	}
}

// recordLastLogin stamps last_login_at. Best-effort: recording the timestamp is
// not worth failing an otherwise-valid login over, so any error (including the
// ErrUserNotFound race) is logged at warn and swallowed.
func (a *Authenticator) recordLastLogin(ctx context.Context, user ports.AuthUser, now time.Time) {
	if err := a.userWriter.RecordLastLogin(ctx, user.TenantID, user.ID, now); err != nil {
		a.logger.WarnContext(ctx, "record last login failed, continuing",
			slog.String("user_id", user.ID.String()), slog.String("error", err.Error()))
	}
}

// mintSession generates the token, derives the stored hash and cookie value, and
// persists the session row. A creation failure is a real internal fault, not a
// credential problem, so it wraps apperr.ErrInternal — never ErrInvalidCredentials
// and never a raw pgx error.
func (a *Authenticator) mintSession(
	ctx context.Context,
	tenantRef ports.TenantRef,
	user ports.AuthUser,
	params LoginParams,
	now time.Time,
) (LoginResult, error) {
	raw, err := a.tokens()
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate session token: %w", err)
	}
	// Hash the RAW bytes, not the base64 string, so the stored hash is
	// independent of transport encoding and a malformed cookie is rejected at
	// decode time on the read path.
	sum := sha256.Sum256(raw)
	cookieValue := base64.RawURLEncoding.EncodeToString(raw)

	sessionID, err := uuid.NewV7()
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate session id: %w: %w", apperr.ErrInternal, err)
	}
	expiresAt := now.Add(tenantRef.SessionTimeout)

	newSession := ports.NewSession{
		ID:        sessionID,
		TenantID:  tenantRef.ID,
		UserID:    user.ID,
		TokenHash: sum[:],
		// Role is snapshotted at issue time (decision 2): the session carries its
		// own role so per-request validation needs no users JOIN.
		Role:         user.Role,
		IssuedAt:     now,
		LastSeenAt:   now,
		ExpiresAt:    expiresAt,
		IssuedFromIP: params.IP,
		UserAgent:    params.UserAgent,
	}
	if err := a.sessions.Create(ctx, newSession); err != nil {
		return LoginResult{}, fmt.Errorf("create session: %w: %w", apperr.ErrInternal, err)
	}

	return LoginResult{
		Token:              cookieValue,
		MustChangePassword: user.MustChangePassword,
		ExpiresAt:          expiresAt,
	}, nil
}

// Authenticate validates an opaque session token on every request. It is
// read-only and runs pre-authentication on the bypass pool (the token's tenant
// is the lookup's output, not an input), so it never deletes and never writes.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	// 1. Decode and length-check before touching the DB: a malformed cookie can
	// never be a valid session, and rejecting it here keeps a bad value out of
	// the store entirely.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != TokenBytes {
		return Principal{}, ErrSessionInvalid
	}

	// 2. Look up by the SHA-256 of the raw bytes (matches the issue-time hash).
	sum := sha256.Sum256(raw)
	record, err := a.sessions.FindByTokenHash(ctx, sum[:])
	if err != nil {
		if errors.Is(err, ports.ErrSessionNotFound) {
			return Principal{}, ErrSessionInvalid // unknown or already-deleted session.
		}
		return Principal{}, fmt.Errorf("find session: %w", err)
	}

	// 3. Expiry. We do NOT delete the expired row here: this path is read-only
	// and has no tenant context for a write; the cleanup.sessions job (Step 5)
	// reaps expired rows.
	if a.now().After(record.ExpiresAt) {
		return Principal{}, ErrSessionInvalid
	}

	// 4. Tenant status is read LIVE from the JOINed row. A suspended tenant on an
	// otherwise-valid session returns the distinct ErrTenantSuspended (403) — the
	// principal is already trusted, so revealing the operational reason is fine
	// (decision 3).
	if record.TenantStatus != domain.StatusActive {
		return Principal{}, ErrTenantSuspended
	}

	// 5. Valid. Role is the session snapshot (decision 2). Refreshing the window
	// is a separate post-authentication write (Refresh), sequenced by the
	// middleware after it sets the tenant context. SessionTimeout is carried out
	// of the JOINed record so the middleware can pass it to Refresh without a
	// re-query.
	return Principal{
		TenantID:       record.TenantID,
		UserID:         record.UserID,
		Role:           record.Role,
		SessionID:      record.ID,
		ExpiresAt:      record.ExpiresAt,
		SessionTimeout: record.SessionTimeout,
	}, nil
}

// Refresh slides the inactivity window for an already-validated session. It is a
// POST-authentication write on the regular pool; the adapter reads the tenant
// from context, so the middleware must set the tenant context (from the
// Authenticate principal) before calling. sessionTimeout is passed as an argument
// because the middleware already has it from Authenticate (SessionRecord carries
// it) — avoiding a redundant tenant re-query. A zero-rows result means the
// session vanished between validation and refresh; ErrSessionNotFound propagates
// so Step 5 can treat the request as unauthenticated.
func (a *Authenticator) Refresh(ctx context.Context, sessionID uuid.UUID, sessionTimeout time.Duration) error {
	now := a.now()
	if err := a.sessions.Refresh(ctx, sessionID, now, now.Add(sessionTimeout)); err != nil {
		if errors.Is(err, ports.ErrSessionNotFound) {
			return ports.ErrSessionNotFound
		}
		return fmt.Errorf("refresh session: %w", err)
	}
	return nil
}

// Logout deletes a session. It is POST-authentication on the regular pool (the
// adapter reads the tenant from context). Deleting an already-gone session is not
// an error — logout is idempotent — so ErrSessionNotFound returns nil; any other
// error is wrapped and returned.
func (a *Authenticator) Logout(ctx context.Context, sessionID uuid.UUID) error {
	if err := a.sessions.Delete(ctx, sessionID); err != nil {
		if errors.Is(err, ports.ErrSessionNotFound) {
			a.logger.DebugContext(ctx, "logout of already-absent session, treating as success",
				slog.String("session_id", sessionID.String()))
			return nil
		}
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
