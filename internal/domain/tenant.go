package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TenantStatus enumerates the lifecycle states of a tenant. The values match
// the tenants_status_check constraint (DB §5.1).
type TenantStatus string

const (
	// StatusActive marks a tenant that can serve requests.
	StatusActive TenantStatus = "active"
	// StatusSuspended marks a tenant whose requests are rejected (operator action).
	StatusSuspended TenantStatus = "suspended"
)

// Defaults applied at tenant creation, mirroring the DB §5.1 column defaults so
// the row is well-formed even before the owner configures the tenant.
const (
	defaultTimezone       = "UTC"
	defaultSessionTimeout = 60 * time.Minute
)

// ErrTenantNameRequired is returned by NewTenant when the name is empty or only
// whitespace. A tenant must be nameable; the bootstrap operator supplies it.
var ErrTenantNameRequired = errors.New("tenant name must not be empty")

// Tenant is the root aggregate of tenant scoping: one row per gym using the
// deployment (DB §5.1). It is state-stored (direct INSERT), not event-sourced.
type Tenant struct {
	ID             uuid.UUID
	Name           string
	Slug           string
	ContactEmail   string
	Timezone       string
	SessionTimeout time.Duration
	Status         TenantStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewTenant builds an active Tenant with the given ID, name, and slug, applying
// the DB §5.1 defaults (UTC timezone, 60-minute session timeout, active status).
// The name is trimmed and must be non-empty; the slug must satisfy ValidateSlug
// (the same grammar as the tenants_slug_format_check constraint), so a Tenant can
// never carry a slug the DB would reject. The ID is passed in so the constructor
// stays pure (no clock, no randomness); the caller mints the UUID and resolves
// the slug (explicit or derived from the name).
func NewTenant(id uuid.UUID, name, slug string) (Tenant, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Tenant{}, ErrTenantNameRequired
	}
	if err := ValidateSlug(slug); err != nil {
		return Tenant{}, err
	}
	return Tenant{
		ID:             id,
		Name:           name,
		Slug:           slug,
		Timezone:       defaultTimezone,
		SessionTimeout: defaultSessionTimeout,
		Status:         StatusActive,
	}, nil
}
