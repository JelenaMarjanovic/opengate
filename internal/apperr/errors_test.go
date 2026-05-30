package apperr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/JelenaMarjanovic/opengate/internal/apperr"
)

// errOther is a non-matching sentinel used to prove errors.Is discriminates.
var errOther = errors.New("other error")

// TestErrInternalIsTraversable covers AC2: a 3-deep wrapped chain stays
// matchable via errors.Is, and an unrelated sentinel does not match.
func TestErrInternalIsTraversable(t *testing.T) {
	// Wrap ErrInternal three times with %w to build a realistic adapter chain.
	chain := fmt.Errorf("layer 3: %w",
		fmt.Errorf("layer 2: %w",
			fmt.Errorf("layer 1: %w", apperr.ErrInternal)))

	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"matches ErrInternal through 3 wraps", chain, apperr.ErrInternal, true},
		{"does not match unrelated sentinel", chain, errOther, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, tt.target); got != tt.want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", tt.err, tt.target, got, tt.want)
			}
		})
	}
}
