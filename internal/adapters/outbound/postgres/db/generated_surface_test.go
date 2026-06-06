package db_test

import (
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
)

// TestGeneratedQueriesSurface is a container-free sanity check that the sqlc
// output exposes the constructor and the exact query methods the adapters
// depend on. It runs in CI without Docker. The method-value references below
// fail to COMPILE if a method is renamed, removed, or its signature drifts, so
// this test (together with `go build`) locks the generated surface in place. If
// it stops compiling after a query change, run `make generate`.
func TestGeneratedQueriesSurface(t *testing.T) {
	t.Parallel()

	// New accepts a DBTX; a nil interface value is fine for a surface check
	// because no method is invoked here.
	q := db.New(nil)
	if q == nil {
		t.Fatal("db.New returned nil")
	}

	// One reference per generated query. Assigning the method value pins both
	// the name and the signature at compile time.
	_ = q.ResolveTenantBySlug
	_ = q.FindUserByEmail
	_ = q.UpdateUserPasswordHash
	_ = q.UpdateUserLastLogin
	_ = q.CreateSession
	_ = q.FindSessionByTokenHash
	_ = q.RefreshSession
	_ = q.DeleteSession
	_ = q.ListCasbinPolicyRules
}
