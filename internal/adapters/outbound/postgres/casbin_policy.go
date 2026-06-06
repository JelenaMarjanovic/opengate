package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres/db"
	"github.com/JelenaMarjanovic/opengate/internal/apperr"
)

// CasbinPolicyLoader reads the global Casbin policy rules and adapts them into
// the [][]string shape the authorizer wants ([sub, obj, act] per inner slice).
//
// It runs on the BYPASSRLS pool: casbin_rules is a global table with no
// tenant_id (the role -> (resource, action) mapping is identical for every
// tenant) and the load is a system-level operation with no tenant context, so
// the regular RLS-bound pool would trip its missing-tenant warning. The loader
// exposes Load as a plain method whose signature matches authz.PolicyLoaderFunc,
// so the composition root injects `loader.Load` without this package importing
// the authz package.
type CasbinPolicyLoader struct {
	bypass *db.Queries
	logger *slog.Logger
}

// NewCasbinPolicyLoader wires the loader to the BYPASSRLS pool. The logger is
// used to warn about malformed rows; a nil logger falls back to slog.Default.
func NewCasbinPolicyLoader(bypassPool *pgxpool.Pool, logger *slog.Logger) *CasbinPolicyLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return &CasbinPolicyLoader{
		bypass: db.New(bypassPool),
		logger: logger,
	}
}

// Load returns the `p` policy rules as [][]string, each inner slice exactly
// [sub, obj, act]. v0..v2 are nullable in the schema, so a rule with a null
// sub, obj, or act is MALFORMED — it is skipped with a warning rather than fed
// to Casbin as an empty string, which would silently create a bogus policy
// (e.g. an all-empty rule, or a rule matching the empty subject). A query
// failure is wrapped and returned; the authorizer decides whether that is
// fail-fast (initial load) or keep-last-good (refresh).
func (l *CasbinPolicyLoader) Load(ctx context.Context) ([][]string, error) {
	rows, err := l.bypass.ListCasbinPolicyRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("list casbin policy rules: %w: %w", apperr.ErrInternal, err)
	}

	rules := make([][]string, 0, len(rows))
	for i, row := range rows {
		if row.V0 == nil || row.V1 == nil || row.V2 == nil {
			l.logger.LogAttrs(ctx, slog.LevelWarn,
				"postgres: skipping malformed casbin policy rule with a null field",
				slog.Int("row_index", i))
			continue
		}
		rules = append(rules, []string{*row.V0, *row.V1, *row.V2})
	}
	return rules, nil
}
