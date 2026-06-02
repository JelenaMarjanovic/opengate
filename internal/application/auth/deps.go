package auth

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
)

// TokenBytes is the length, in bytes, of a raw session token: 256 bits of
// entropy. The token is hashed (SHA-256) into the stored token_hash and base64
// (raw-url) encoded into the cookie value; Authenticate rejects any decoded
// token that is not exactly this length before it can reach the database.
const TokenBytes = 32

// PasswordVerifier verifies a plaintext against a stored PHC hash. The use case
// depends on this narrow interface rather than calling auth.VerifyPassword
// directly so a test can inject a spy that counts calls and records arguments —
// the single-verification-per-attempt invariant is the testable heart of the
// enumeration defense. auth.VerifyPassword satisfies it via VerifierFunc.
type PasswordVerifier interface {
	// Verify reports whether plaintext matches phc. rehash is true when the
	// stored hash used weaker-than-current parameters and should be upgraded; err
	// is non-nil on mismatch or a malformed stored hash.
	Verify(plaintext, phc string) (rehash bool, err error)
}

// PasswordHasher produces a fresh PHC hash at the current parameters, used only
// on the best-effort rehash-on-login path. Injected for the same testability
// reason as PasswordVerifier; auth.HashPassword satisfies it via HasherFunc.
type PasswordHasher interface {
	// Hash returns a PHC-formatted hash of plaintext at the current parameters.
	Hash(plaintext string) (string, error)
}

// Clock returns the current time. Every "now" in this package — expiry checks,
// expires_at, last_login_at, last_seen_at — comes from the injected Clock, never
// time.Now directly, so expiry and the sliding window are deterministic in tests.
type Clock func() time.Time

// TokenSource produces the raw bytes of a new session token. Injected so a test
// can supply known bytes and assert the stored hash; production uses
// CryptoRandToken. The use case must never read randomness inline.
type TokenSource func() (token []byte, err error)

// VerifierFunc adapts a plain function to PasswordVerifier so the composition
// root can pass auth.VerifyPassword without declaring a wrapper type.
type VerifierFunc func(plaintext, phc string) (rehash bool, err error)

// Verify calls the wrapped function.
func (f VerifierFunc) Verify(plaintext, phc string) (bool, error) { return f(plaintext, phc) }

// HasherFunc adapts a plain function to PasswordHasher so the composition root
// can pass auth.HashPassword without declaring a wrapper type.
type HasherFunc func(plaintext string) (string, error)

// Hash calls the wrapped function.
func (f HasherFunc) Hash(plaintext string) (string, error) { return f(plaintext) }

// CryptoRandToken is the production TokenSource: TokenBytes of cryptographically
// secure randomness from crypto/rand. A read failure is an internal fault, not a
// credential problem, so it wraps apperr.ErrInternal.
func CryptoRandToken() ([]byte, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random session token: %w: %w", apperr.ErrInternal, err)
	}
	return b, nil
}
