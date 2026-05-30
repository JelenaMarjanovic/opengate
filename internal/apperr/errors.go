package apperr

import "errors"

// ErrInternal is returned for unexpected, non-domain failures. Adapters wrap the
// original cause with %w; callers match on ErrInternal via errors.Is.
var ErrInternal = errors.New("internal error")
