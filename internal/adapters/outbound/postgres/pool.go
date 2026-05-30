package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewBypassPool opens a pgx connection pool for the BYPASSRLS operator path
// (System Design §10): the bootstrap CLI and, later, the data export job.
//
// Unlike the regular request pool (US-02.03), it installs no AfterAcquire /
// BeforeRelease tenant-binding hooks. Operator paths run outside any single
// tenant's RLS scope, so there is no app.current_tenant_id to bind.
func NewBypassPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create bypass pool: %w", err)
	}
	return pool, nil
}
