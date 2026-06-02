package auth

import "errors"

// ErrInvalidCredentials is the single, uniform error every PRE-authentication
// Login failure returns — unknown tenant, suspended tenant, unknown email,
// deactivated user, or wrong password all map to it. Pre-authentication the
// caller is untrusted, so the error reveals nothing that would let an attacker
// distinguish those cases (user/tenant enumeration defense, decision 3). Step 5
// maps it to 401.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrSessionInvalid is the POST-authentication error for a session that cannot be
// trusted: a malformed token, an unknown or deleted session, or an expired one.
// Step 5 maps it to 401. It is deliberately distinct from ErrTenantSuspended so
// the middleware can tell "re-authenticate" from "your tenant is suspended".
var ErrSessionInvalid = errors.New("session invalid")

// ErrTenantSuspended is returned by Authenticate when the session itself is valid
// and unexpired but its tenant is no longer active. Because the principal is
// already trusted at that point, revealing the operational reason is appropriate
// (decision 3); Step 5 maps it to 403. Kept distinct from ErrSessionInvalid — do
// not collapse the two.
var ErrTenantSuspended = errors.New("tenant suspended")
