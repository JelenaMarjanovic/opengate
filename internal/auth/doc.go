// Package auth hashes administrative-user passwords. HashPassword produces a
// PHC-formatted Argon2id hash embedding its parameters (System Design §9) so
// future parameter changes coexist with existing hashes. VerifyPassword is
// added in US-02.02 to this same package.
//
// Import constraint: this package depends only on the Go standard library and
// golang.org/x/crypto. It holds no state and performs no I/O, so any layer
// (domain, application, adapters) may import it.
package auth
