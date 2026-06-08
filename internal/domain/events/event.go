package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event is the common envelope for every domain event in the system. The
// envelope is stable across event-type versions; only the payload changes.
type Event struct {
	ID             uuid.UUID       `json:"id"`              // UUIDv7, time-ordered
	AggregateID    uuid.UUID       `json:"aggregate_id"`    // the aggregate this event belongs to
	AggregateType  string          `json:"aggregate_type"`  // e.g. "member", "credential", "access"
	Sequence       int64           `json:"sequence"`        // 1-based, monotonic within aggregate; caller-set
	StreamPosition int64           `json:"stream_position"` // global ordering; assigned by the store on append, zero on Append input
	TenantID       uuid.UUID       `json:"tenant_id"`       // denormalized; caller-set, verified by the events RLS WITH CHECK
	OccurredAt     time.Time       `json:"occurred_at"`     // event timestamp, UTC
	Type           string          `json:"event_type"`      // stable type id with embedded version, e.g. "member.created.v1"
	Payload        json.RawMessage `json:"payload"`         // event-specific data, decoded by the fold handler
	Metadata       EventMetadata   `json:"metadata"`        // correlation, actor, trace context, app version
}

// EventMetadata records the provenance of an event for observability and
// forensics. It does not participate in domain logic.
type EventMetadata struct {
	CorrelationID string    `json:"correlation_id"` // originating command's correlation/request id
	ActorID       uuid.UUID `json:"actor_id"`       // user that triggered the event; zero UUID for system-originated
	ActorType     string    `json:"actor_type"`     // "user" | "system" (vocabulary extends, e.g. "reader", without a struct change)
	TraceID       string    `json:"trace_id"`       // OTel trace id (hex); empty when no valid span
	SpanID        string    `json:"span_id"`        // OTel span id (hex); empty when no valid span
	AppVersion    string    `json:"app_version"`    // application build version that produced the event
}
