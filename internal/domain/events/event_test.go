package events_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JelenaMarjanovic/opengate/internal/domain/events"
)

// fullyPopulatedEvent builds an Event with every envelope and metadata field
// set to a non-zero value, so the JSON key-set and round-trip assertions
// exercise the complete stable shape.
//
// Two deliberate choices keep the round-trip exact:
//   - OccurredAt uses time.Date(..., time.UTC), not time.Now(): JSON drops the
//     monotonic clock reading, so a wall-clock value avoids a DeepEqual mismatch.
//   - Payload is a compact json.RawMessage, so the raw bytes survive Marshal then
//     Unmarshal byte-for-byte.
func fullyPopulatedEvent() events.Event {
	return events.Event{
		ID:             uuid.MustParse("018f3a2b-0000-7000-8000-000000000001"),
		AggregateID:    uuid.MustParse("018f3a2b-0000-7000-8000-000000000002"),
		AggregateType:  "member",
		Sequence:       1,
		StreamPosition: 42,
		TenantID:       uuid.MustParse("018f3a2b-0000-7000-8000-000000000003"),
		OccurredAt:     time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC),
		Type:           "member.created.v1",
		Payload:        json.RawMessage(`{"name":"Ada"}`),
		Metadata: events.EventMetadata{
			CorrelationID: "req-abc-123",
			ActorID:       uuid.MustParse("018f3a2b-0000-7000-8000-000000000004"),
			ActorType:     "user",
			TraceID:       "0af7651916cd43dd8448eb211c80319c",
			SpanID:        "b7ad6b7169203331",
			AppVersion:    "v1.2.3",
		},
	}
}

// TestEventJSONKeySet (AC2) asserts json.Marshal emits exactly the ten envelope
// keys and, under "metadata", exactly the six metadata keys — no more, no less.
// It compares key sets, not substrings, so a renamed or dropped tag fails.
func TestEventJSONKeySet(t *testing.T) {
	raw, err := json.Marshal(fullyPopulatedEvent())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("Unmarshal envelope: %v", err)
	}

	wantEnvelope := []string{
		"id", "aggregate_id", "aggregate_type", "sequence", "stream_position",
		"tenant_id", "occurred_at", "event_type", "payload", "metadata",
	}
	assertKeySet(t, "envelope", envelope, wantEnvelope)

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(envelope["metadata"], &metadata); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}

	wantMetadata := []string{
		"correlation_id", "actor_id", "actor_type", "trace_id", "span_id", "app_version",
	}
	assertKeySet(t, "metadata", metadata, wantMetadata)
}

// assertKeySet fails the test unless got's keys are exactly want (set equality).
func assertKeySet(t *testing.T, label string, got map[string]json.RawMessage, want []string) {
	t.Helper()

	gotKeys := make(map[string]bool, len(got))
	for k := range got {
		gotKeys[k] = true
	}

	wantKeys := make(map[string]bool, len(want))
	for _, k := range want {
		wantKeys[k] = true
		if !gotKeys[k] {
			t.Errorf("%s: missing key %q", label, k)
		}
	}
	for k := range gotKeys {
		if !wantKeys[k] {
			t.Errorf("%s: unexpected key %q", label, k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: got %d keys, want %d", label, len(got), len(want))
	}
}

// TestEventJSONRoundTrip (AC2) proves Marshal then Unmarshal reconstructs the
// original Event exactly (reflect.DeepEqual), guarding against any lossy tag or
// type. See fullyPopulatedEvent for why OccurredAt and Payload are built the way
// they are.
func TestEventJSONRoundTrip(t *testing.T) {
	original := fullyPopulatedEvent()

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got events.Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(original, got) {
		t.Errorf("round-trip mismatch:\n original = %#v\n got      = %#v", original, got)
	}
}

// TestSentinelsErrorsIs (AC3) proves each sentinel survives %w wrapping under
// errors.Is and that the two sentinels are distinct from each other.
func TestSentinelsErrorsIs(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrConcurrencyConflict", events.ErrConcurrencyConflict},
		{"ErrEventNotFound", events.ErrEventNotFound},
	}

	for _, s := range sentinels {
		wrapped := fmt.Errorf("append: %w", s.err)
		if !errors.Is(wrapped, s.err) {
			t.Errorf("errors.Is(wrapped, %s) = false, want true", s.name)
		}
	}

	if errors.Is(events.ErrConcurrencyConflict, events.ErrEventNotFound) {
		t.Error("ErrConcurrencyConflict must not match ErrEventNotFound")
	}
	if errors.Is(events.ErrEventNotFound, events.ErrConcurrencyConflict) {
		t.Error("ErrEventNotFound must not match ErrConcurrencyConflict")
	}
}
