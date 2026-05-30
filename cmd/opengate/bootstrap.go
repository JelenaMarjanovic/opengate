package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/application/identity"
	"github.com/JelenaMarjanovic/opengate/internal/config"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// Bootstrap input environment variables (System Design §11). The operator sets
// these immediately before invoking the subcommand and unsets them after.
const (
	envBootstrapTenantName = "OPENGATE_BOOTSTRAP_TENANT_NAME"
	envBootstrapOwnerEmail = "OPENGATE_BOOTSTRAP_OWNER_EMAIL"
	// G101 here is a false positive: this is the NAME of an env var, not a
	// credential value. The secret is read from the environment at runtime.
	envBootstrapOwnerPassword = "OPENGATE_BOOTSTRAP_OWNER_PASSWORD" //nolint:gosec // env var name, not a secret
)

// readBootstrapInputs reads the three required bootstrap inputs via getenv and
// returns an error naming the first one that is missing (AC2). getenv is
// injected (rather than calling os.Getenv directly) so the function is unit
// testable without mutating the process environment.
func readBootstrapInputs(getenv func(string) string) (name, email, password string, err error) {
	name = getenv(envBootstrapTenantName)
	if name == "" {
		return "", "", "", fmt.Errorf("missing required environment variable %s", envBootstrapTenantName)
	}
	email = getenv(envBootstrapOwnerEmail)
	if email == "" {
		return "", "", "", fmt.Errorf("missing required environment variable %s", envBootstrapOwnerEmail)
	}
	password = getenv(envBootstrapOwnerPassword)
	if password == "" {
		return "", "", "", fmt.Errorf("missing required environment variable %s", envBootstrapOwnerPassword)
	}
	return name, email, password, nil
}

// runBootstrap implements `opengate bootstrap`: it creates the first tenant and
// its owner user in one transaction over the BYPASSRLS pool (System Design
// §11). The returned error (if any) is printed by the caller, which exits 1.
func runBootstrap(ctx context.Context, logger *slog.Logger, cfg config.Config, getenv func(string) string) error {
	name, email, password, err := readBootstrapInputs(getenv)
	if err != nil {
		return err
	}

	// BypassRLSURL is optional in config (migrate must not require it), so the
	// subcommand that does need it validates its presence here.
	if cfg.BypassRLSURL == "" {
		return errors.New("bootstrap: BYPASS_RLS_DATABASE_URL is not set")
	}

	pool, err := postgres.NewBypassPool(ctx, cfg.BypassRLSURL)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer pool.Close()

	bootstrapper := identity.NewBootstrapper(postgres.NewIdentityWriter(pool))
	if err := bootstrapper.Run(ctx, name, email, password); err != nil {
		if errors.Is(err, ports.ErrTenantExists) {
			return errors.New("tenant already exists")
		}
		return fmt.Errorf("bootstrap failed: %w", err)
	}

	// The password is never logged; the redacting handler would mask it anyway.
	logger.InfoContext(ctx, "bootstrap completed",
		slog.String("tenant_name", name),
		slog.String("owner_email", email),
	)
	return nil
}
