package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/JelenaMarjanovic/opengate/internal/adapters/outbound/postgres"
	"github.com/JelenaMarjanovic/opengate/internal/application/identity"
	"github.com/JelenaMarjanovic/opengate/internal/config"
	"github.com/JelenaMarjanovic/opengate/internal/domain"
	ports "github.com/JelenaMarjanovic/opengate/internal/ports/outbound"
)

// Bootstrap input environment variables (System Design §11). The operator sets
// these immediately before invoking the subcommand and unsets them after.
const (
	envBootstrapTenantName = "OPENGATE_BOOTSTRAP_TENANT_NAME"
	// envBootstrapTenantSlug is optional: when unset, the slug is derived from
	// the tenant name using the same normalization as the migration backfill.
	envBootstrapTenantSlug = "OPENGATE_BOOTSTRAP_TENANT_SLUG"
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

// resolveTenantSlug determines the tenant slug for bootstrap and validates it
// before any resource is acquired (argument validation before resource
// acquisition, per the standing discipline).
//
//   - An explicit slug (OPENGATE_BOOTSTRAP_TENANT_SLUG) is validated against the
//     same grammar as the tenants_slug_format_check DB constraint.
//   - When absent, the slug is derived from the tenant name with the same
//     normalization rule as the add_tenant_slug migration backfill. An
//     underivable (empty) result is a fail-fast error telling the operator to set
//     the slug explicitly, rather than letting a NULL/empty slug reach the DB.
func resolveTenantSlug(explicit, name string) (string, error) {
	if explicit != "" {
		if err := domain.ValidateSlug(explicit); err != nil {
			return "", fmt.Errorf("invalid %s: %w", envBootstrapTenantSlug, err)
		}
		return explicit, nil
	}

	derived := domain.NormalizeSlug(name)
	if derived == "" {
		return "", fmt.Errorf(
			"cannot derive a slug from %s=%q (no alphanumeric characters); set %s explicitly",
			envBootstrapTenantName, name, envBootstrapTenantSlug)
	}
	// Defense in depth: a derived slug is normally valid, but validate so a future
	// change to NormalizeSlug (or an overlong name) cannot emit an out-of-grammar
	// slug that the DB would reject with an opaque error.
	if err := domain.ValidateSlug(derived); err != nil {
		return "", fmt.Errorf("slug %q derived from %s is invalid: %w", derived, envBootstrapTenantName, err)
	}
	return derived, nil
}

// runBootstrap implements `opengate bootstrap`: it creates the first tenant and
// its owner user in one transaction over the BYPASSRLS pool (System Design
// §11). The returned error (if any) is printed by the caller, which exits 1.
func runBootstrap(ctx context.Context, logger *slog.Logger, cfg config.Config, getenv func(string) string) error {
	name, email, password, err := readBootstrapInputs(getenv)
	if err != nil {
		return err
	}

	// Resolve and validate the slug before acquiring any resource: a bad explicit
	// slug or an underivable name should fail fast, not after opening a pool.
	slug, err := resolveTenantSlug(getenv(envBootstrapTenantSlug), name)
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
	if err := bootstrapper.Run(ctx, name, slug, email, password); err != nil {
		if errors.Is(err, ports.ErrTenantExists) {
			return errors.New("tenant already exists")
		}
		return fmt.Errorf("bootstrap failed: %w", err)
	}

	// The password is never logged; the redacting handler would mask it anyway.
	logger.InfoContext(ctx, "bootstrap completed",
		slog.String("tenant_name", name),
		slog.String("tenant_slug", slug),
		slog.String("owner_email", email),
	)
	return nil
}
