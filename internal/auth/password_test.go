package auth_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/auth"
	"golang.org/x/crypto/argon2"
)

// formatPHC builds a PHC-formatted Argon2id string with arbitrary parameters so
// tests can construct stored hashes with deliberately outdated params (AC3) or
// otherwise exercise the verifier independently of HashPassword's fixed defaults.
func formatPHC(plaintext string, memoryKiB, time uint32, threads uint8) string {
	salt := []byte("0123456789abcdef") // 16 bytes; deterministic so the test is stable
	hash := argon2.IDKey([]byte(plaintext), salt, time, memoryKiB, threads, 32)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, time, threads, b64(salt), b64(hash))
}

// TestVerifyPasswordMatch covers AC2: a hash produced by HashPassword at the
// current default parameters verifies as (rehash=false, err=nil).
func TestVerifyPasswordMatch(t *testing.T) {
	const pw = "correct horse"
	phc, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	rehash, err := auth.VerifyPassword(pw, phc)
	if err != nil {
		t.Fatalf("VerifyPassword: unexpected err %v", err)
	}
	if rehash {
		t.Errorf("rehash = true, want false for current-default params")
	}
}

// TestVerifyPasswordRehash covers AC3: a hash whose stored parameters are weaker
// than the current defaults still verifies, but flags rehash=true.
func TestVerifyPasswordRehash(t *testing.T) {
	const pw = "correct horse"
	// Deliberately lower than the §9 defaults (m=65536, t=3).
	oldPHC := formatPHC(pw, 32768, 2, 4)
	rehash, err := auth.VerifyPassword(pw, oldPHC)
	if err != nil {
		t.Fatalf("VerifyPassword: unexpected err %v", err)
	}
	if !rehash {
		t.Errorf("rehash = false, want true for outdated params")
	}
}

// TestVerifyPasswordWrong covers AC4: a wrong plaintext returns ErrPasswordMismatch
// (not ErrInternal) with rehash=false.
func TestVerifyPasswordWrong(t *testing.T) {
	phc, err := auth.HashPassword("correct horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	rehash, err := auth.VerifyPassword("wrong", phc)
	if !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Errorf("err = %v, want ErrPasswordMismatch", err)
	}
	if errors.Is(err, apperr.ErrInternal) {
		t.Errorf("wrong password must not route to ErrInternal: %v", err)
	}
	if rehash {
		t.Errorf("rehash = true, want false on mismatch")
	}
}

// TestVerifyPasswordMalformed covers D-A: every malformed/unsupported/empty stored
// hash routes to apperr.ErrInternal and NEVER to ErrPasswordMismatch. The empty-hash
// case specifically proves the auth-bypass guard (an empty hash must not verify).
func TestVerifyPasswordMalformed(t *testing.T) {
	cases := []struct {
		name string
		phc  string
	}{
		{"not a phc", "not-a-phc"},
		{"wrong algorithm", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2g"},
		{"unsupported version", "$argon2id$v=18$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2g"},
		{"degenerate memory", "$argon2id$v=19$m=0,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2g"},
		{"bad base64 salt", "$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaGhhc2g"},
		{"empty hash", "$argon2id$v=19$m=65536,t=3,p=4$$"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rehash, err := auth.VerifyPassword("whatever", tc.phc)
			if !errors.Is(err, apperr.ErrInternal) {
				t.Errorf("err = %v, want wrapping apperr.ErrInternal", err)
			}
			if errors.Is(err, auth.ErrPasswordMismatch) {
				t.Errorf("malformed hash must not route to ErrPasswordMismatch: %v", err)
			}
			if rehash {
				t.Errorf("rehash = true, want false on malformed hash")
			}
		})
	}
}

// TestHashPasswordPHCFormat proves the hash is a PHC-formatted Argon2id string
// that embeds the System Design §9 parameters, so a future verifier can read
// the parameters back from the stored hash.
func TestHashPasswordPHCFormat(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	const wantPrefix = "$argon2id$v=19$m=65536,t=3,p=4$"
	if !strings.HasPrefix(hash, wantPrefix) {
		t.Fatalf("hash = %q, want prefix %q", hash, wantPrefix)
	}

	// PHC layout: ["", "argon2id", "v=19", "m=65536,t=3,p=4", salt, hash].
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Fatalf("hash has %d $-segments, want 6: %q", len(parts), hash)
	}
	if parts[4] == "" || parts[5] == "" {
		t.Errorf("salt or hash segment empty: salt=%q hash=%q", parts[4], parts[5])
	}
}

// TestHashPasswordUniqueSalt proves each call uses a fresh random salt, so the
// same plaintext never produces the same stored hash.
func TestHashPasswordUniqueSalt(t *testing.T) {
	const pw = "same-password"
	first, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("first HashPassword: %v", err)
	}
	second, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("second HashPassword: %v", err)
	}
	if first == second {
		t.Errorf("identical hashes for the same plaintext; salt is not random:\n%q", first)
	}
}
