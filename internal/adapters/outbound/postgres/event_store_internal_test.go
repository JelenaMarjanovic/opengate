package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
)

// TestMapAppendError is the white-box proof that the 23505 mapping is
// constraint-specific (US-03.03, "do not collapse the mapping"). It is the cheap,
// container-free complement to the contract suite's AC2 conflict path: only a
// unique violation on events_aggregate_sequence_unique is a concurrency conflict;
// any other 23505 (a stream_position or id collision) is an internal defect and
// must NOT be masked as one.
func TestMapAppendError(t *testing.T) {
	pgUnique := func(constraint string) error {
		return &pgconn.PgError{Code: "23505", ConstraintName: constraint}
	}

	tests := []struct {
		name       string
		err        error
		wantConfl  bool // errors.Is ErrConcurrencyConflict
		wantIntern bool // errors.Is apperr.ErrInternal
	}{
		{
			name:      "aggregate_sequence 23505 -> concurrency conflict",
			err:       pgUnique("events_aggregate_sequence_unique"),
			wantConfl: true,
		},
		{
			name:       "stream_position 23505 -> internal, NOT conflict",
			err:        pgUnique("events_stream_position_unique"),
			wantIntern: true,
		},
		{
			name:       "primary-key 23505 -> internal, NOT conflict",
			err:        pgUnique("events_pkey"),
			wantIntern: true,
		},
		{
			name:       "non-pg error -> internal, NOT conflict",
			err:        errors.New("connection reset"),
			wantIntern: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapAppendError(tt.err)
			if isConfl := errors.Is(got, events.ErrConcurrencyConflict); isConfl != tt.wantConfl {
				t.Errorf("errors.Is(ErrConcurrencyConflict) = %v, want %v (err: %v)", isConfl, tt.wantConfl, got)
			}
			if isIntern := errors.Is(got, apperr.ErrInternal); isIntern != tt.wantIntern {
				t.Errorf("errors.Is(ErrInternal) = %v, want %v (err: %v)", isIntern, tt.wantIntern, got)
			}
		})
	}
}
