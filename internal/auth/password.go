package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"golang.org/x/crypto/argon2"
)

// OWASP-recommended Argon2id parameters (System Design §9): 64 MiB memory, 3 iterations,
// 4-way parallelism, 16-byte salt, 32-byte output.
const (
	argonMemoryKiB = 64 * 1024 // 65536 KiB = 64 MiB
	argonTime      = 3
	argonThreads   = 4
	argonSaltLen   = 16
	argonKeyLen    = 32
	argonVersion   = argon2.Version // 19 (0x13)
)

// HashPassword returns a PHC string of the form
// $argon2id$v=19$m=65536,t=3,p=4$<b64-salt>$<b64-hash>.
func HashPassword(plaintext string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	// PHC uses base64 without padding.
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemoryKiB, argonTime, argonThreads, b64(salt), b64(hash)), nil
}

// ErrPasswordMismatch is returned when the supplied plaintext does not match the
// stored hash. It is distinct from a malformed-hash error so a caller can map a
// wrong password (401) separately from corrupt stored data (500).
var ErrPasswordMismatch = errors.New("password does not match")

// phcParams holds the parameters parsed from a PHC-formatted Argon2id string.
type phcParams struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	keyLen    uint32 // derived from the stored hash length, bounds-checked in parsePHCBody
	salt      []byte
	hash      []byte
}

// VerifyPassword checks plaintext against a PHC-formatted Argon2id hash, recomputing
// the candidate with the parameters stored IN the string (not the current defaults)
// so older-parameter hashes still verify. Returns:
//
//	rehash=true,  err=nil               match, but stored params are weaker than current
//	rehash=false, err=nil               match at current params
//	err=ErrPasswordMismatch             wrong password
//	err wrapping apperr.ErrInternal     malformed/unsupported stored hash
func VerifyPassword(plaintext, phc string) (rehash bool, err error) {
	p, err := parsePHC(phc)
	if err != nil {
		return false, err // already wraps apperr.ErrInternal
	}
	// keyLen is taken from the stored hash length so a future output-length change
	// verifies without code changes; the empty-hash guard in parsePHC prevents the
	// degenerate keyLen=0 case (which would otherwise compare-equal to an empty hash).
	candidate := argon2.IDKey([]byte(plaintext), p.salt, p.time, p.memoryKiB, p.threads, p.keyLen)
	if subtle.ConstantTimeCompare(candidate, p.hash) != 1 {
		return false, ErrPasswordMismatch
	}
	rehash = p.memoryKiB != argonMemoryKiB || p.time != argonTime || p.threads != argonThreads
	return rehash, nil
}

// parsePHC parses $argon2id$v=19$m=..,t=..,p=..$salt$hash. Any deviation — wrong
// algorithm, unsupported version, bad/degenerate params, bad base64, empty salt or
// hash — yields an error wrapping apperr.ErrInternal. A malformed stored hash is
// corrupt data, NEVER a password mismatch, and an empty/degenerate hash must not be
// allowed to verify as a match (auth-bypass guard).
func parsePHC(phc string) (phcParams, error) {
	parts := strings.Split(phc, "$") // ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[0] != "" {
		return failPHC("unexpected format")
	}
	if parts[1] != "argon2id" {
		return failPHC("unsupported algorithm")
	}
	var version int
	if n, e := fmt.Sscanf(parts[2], "v=%d", &version); e != nil || n != 1 || version != argon2.Version {
		return failPHC("unsupported version")
	}
	// Params and salt/hash decoding are split out to keep each function's
	// cyclomatic complexity within the linter budget.
	return parsePHCBody(parts[3], parts[4], parts[5])
}

// parsePHCBody parses the parameter segment and the base64 salt/hash segments of a
// PHC string. Same failure semantics as parsePHC: any deviation wraps apperr.ErrInternal.
func parsePHCBody(paramSegment, saltB64, hashB64 string) (phcParams, error) {
	var p phcParams
	if n, e := fmt.Sscanf(paramSegment, "m=%d,t=%d,p=%d", &p.memoryKiB, &p.time, &p.threads); e != nil || n != 3 {
		return failPHC("bad parameters")
	}
	if p.memoryKiB < 1 || p.time < 1 || p.threads < 1 {
		return failPHC("degenerate parameters") // would crash or weaken argon2.IDKey
	}
	salt, e := base64.RawStdEncoding.DecodeString(saltB64)
	if e != nil {
		return failPHC("bad salt encoding")
	}
	hash, e := base64.RawStdEncoding.DecodeString(hashB64)
	if e != nil {
		return failPHC("bad hash encoding")
	}
	if len(salt) == 0 || len(hash) == 0 {
		return failPHC("empty salt or hash") // auth-bypass guard
	}
	if len(hash) > math.MaxUint32 {
		return failPHC("hash too long") // bounds the keyLen conversion below
	}
	// len(hash) is non-negative and bounded above by the guard, so the conversion
	// cannot overflow; gosec cannot follow the bound through the guard (G115).
	keyLen := uint32(len(hash)) //nolint:gosec // G115: bounded by the guard above
	p.salt, p.hash, p.keyLen = salt, hash, keyLen
	return p, nil
}

// failPHC wraps a parse-failure reason in apperr.ErrInternal so callers route a
// corrupt stored hash to a 500, never to ErrPasswordMismatch.
func failPHC(reason string) (phcParams, error) {
	return phcParams{}, fmt.Errorf("parse password hash: %s: %w", reason, apperr.ErrInternal)
}
