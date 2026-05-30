package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

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
