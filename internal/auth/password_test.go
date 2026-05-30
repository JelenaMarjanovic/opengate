package auth_test

import (
	"strings"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/auth"
)

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
