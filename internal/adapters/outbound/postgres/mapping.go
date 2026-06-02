package postgres

import (
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// This file holds the small, shared converters that map between the pgx/sqlc
// generated types (pgtype.*, *netip.Addr, *string) and the plain Go types the
// outbound ports speak (time.Time, time.Duration, netip.Addr, string). Keeping
// them here, unexported, means each adapter maps the generated row into a port
// value type rather than leaking a driver type across the boundary (System
// Design §7, "adapters do not leak driver types").

// durationFromInterval converts a Postgres interval (as decoded by pgx into
// pgtype.Interval) into a time.Duration. session_timeout is stored as e.g.
// '60 minutes', which Postgres normalizes entirely into the Microseconds field;
// Days and Months are 0 for any sub-day timeout. We still sum all three
// defensively so an operator-configured multi-day timeout converts correctly.
// Months are approximated as 30 days because a Postgres month has no exact
// length in microseconds; this branch is not exercised by realistic session
// timeouts and is present only so the conversion is total rather than silently
// dropping a months component.
func durationFromInterval(iv pgtype.Interval) time.Duration {
	return time.Duration(iv.Microseconds)*time.Microsecond +
		time.Duration(iv.Days)*24*time.Hour +
		time.Duration(iv.Months)*30*24*time.Hour
}

// timestamptz wraps a time.Time as a valid pgtype.Timestamptz for use as a query
// parameter. The session timestamps written by this layer are always present, so
// Valid is unconditionally true; a zero time.Time would be written as the zero
// instant, never as SQL NULL.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// nullableAddr maps a netip.Addr to the *netip.Addr the generated inet parameter
// expects: an invalid (zero) Addr becomes nil, which pgx writes as SQL NULL, so
// "no IP recorded" is stored as NULL rather than as a bogus 0.0.0.0.
func nullableAddr(a netip.Addr) *netip.Addr {
	if !a.IsValid() {
		return nil
	}
	return &a
}

// nullableString maps a string to the *string the generated nullable-text
// parameter expects: an empty string becomes nil (SQL NULL), so "no user agent
// recorded" is stored as NULL rather than as an empty string.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
