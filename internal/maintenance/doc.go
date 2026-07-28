// Package maintenance holds the periodic retention-enforcement work that bounds
// storage growth (Database Schema §15). Each unit of work is a plain function that
// takes the BYPASSRLS pool, runs one iteration in one transaction under a
// "job.<name>" advisory lock, and returns what it deleted.
//
// The work is deliberately NOT built on the projector framework
// (internal/projection). A projector consumes an event stream and advances a
// watermark; a retention job has neither -- it deletes rows older than a wall-clock
// boundary and keeps no cursor. Reusing the framework would mean carrying a
// projection_progress row that never means anything.
//
// Like the projector framework, this package has no River dependency: the periodic
// scheduling lives in internal/adapters/outbound/queue, so the work itself is
// callable (and testable) directly.
package maintenance
