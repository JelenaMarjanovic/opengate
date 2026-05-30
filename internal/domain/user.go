package domain

import (
	"errors"
	"fmt"
	"net/mail"
	"time"

	"github.com/google/uuid"
)

// Role enumerates the administrative roles a user may hold. The values match
// the users_role_check constraint (DB §5.2).
type Role string

const (
	// RoleOwner is the tenant owner: full administrative control.
	RoleOwner Role = "owner"
	// RoleManager administers members, credentials, and policies.
	RoleManager Role = "manager"
	// RoleAuditor has read-only access to the audit log and reports.
	RoleAuditor Role = "auditor"
)

// UserStatus enumerates the lifecycle states of a user. The values match the
// users_status_check constraint (DB §5.2).
type UserStatus string

const (
	// UserStatusActive marks a user who may authenticate.
	UserStatusActive UserStatus = "active"
	// UserStatusDeactivated marks a user who may no longer authenticate.
	UserStatusDeactivated UserStatus = "deactivated"
)

// ErrInvalidEmail is returned by user constructors when the email does not
// parse as a valid RFC 5322 address.
var ErrInvalidEmail = errors.New("invalid email address")

// ErrPasswordHashRequired is returned by user constructors when the password
// hash is empty. The domain stores hashes only; hashing happens upstream.
var ErrPasswordHashRequired = errors.New("password hash must not be empty")

// User is an administrative user belonging to exactly one tenant (DB §5.2). It
// is state-stored (direct INSERT), not event-sourced.
type User struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	Email              string
	PasswordHash       string
	Role               Role
	Status             UserStatus
	MustChangePassword bool
	LastLoginAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// NewOwnerUser builds the first owner-role user of a tenant: active, with the
// owner role and no forced password change (the operator chose the password at
// bootstrap). The email is validated via net/mail.ParseAddress and stored in
// its normalized address form; the password hash must be non-empty. IDs are
// passed in so the constructor stays pure.
func NewOwnerUser(id, tenantID uuid.UUID, email, passwordHash string) (User, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return User{}, fmt.Errorf("%w: %s", ErrInvalidEmail, email)
	}
	if passwordHash == "" {
		return User{}, ErrPasswordHashRequired
	}
	return User{
		ID:                 id,
		TenantID:           tenantID,
		Email:              addr.Address,
		PasswordHash:       passwordHash,
		Role:               RoleOwner,
		Status:             UserStatusActive,
		MustChangePassword: false,
	}, nil
}
