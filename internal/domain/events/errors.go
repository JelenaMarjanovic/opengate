package events

import "errors"

// ErrConcurrencyConflict is returned by EventStore.Append when the caller's
// expectedSequence does not match the aggregate's current sequence in the
// store — another writer advanced the stream first. The caller re-loads the
// aggregate and retries. Matched via errors.Is (System Design §2).
var ErrConcurrencyConflict = errors.New("concurrency conflict")

// ErrEventNotFound is returned by EventStore reads when a requested event or
// aggregate stream has no matching rows. Matched via errors.Is.
var ErrEventNotFound = errors.New("event not found")
