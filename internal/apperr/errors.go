package apperr

import "errors"

// ErrInternal is returned for unexpected, non-domain failures. Adapters wrap the
// original cause with %w; callers match on ErrInternal via errors.Is.
var ErrInternal = errors.New("internal error")

// ErrForbidden is the authorization-denial sentinel: an authenticated principal
// lacks permission for the requested action. It is distinct from authentication
// failures (auth.ErrSessionInvalid/ErrInvalidCredentials) — the caller IS who
// they claim, they simply may not do this. The inbound HTTP adapter maps it to a
// generic 403 whose body never names the specific (resource, action) denied, so
// the authorization model is not disclosed to the caller. Callers return it
// directly; it carries no cause to wrap.
var ErrForbidden = errors.New("forbidden")
