package http

import (
	"net/http"
	"net/netip"
	"testing"
)

// TestClientIP pins the trusted-proxy hop selection. The load-bearing case is
// spoofDefense: behind a single appending proxy the leftmost X-Forwarded-For
// entry is client-controlled, so clientIP MUST return the rightmost (proxy-
// appended) entry. The other cases cover the fallbacks.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		xff        string // X-Forwarded-For header; "" means absent
		remoteAddr string
		want       string // expected netip.Addr.String(); "invalid IP" is the zero Addr
	}{
		{
			// The line between the fix and the bug. RFC 5737 documentation addresses:
			// the leftmost simulates a client spoof, the rightmost is what the single
			// trusted proxy actually observed and appended. We must return the latter.
			name:       "spoofDefense",
			xff:        "203.0.113.9, 198.51.100.7",
			remoteAddr: "198.51.100.7:54321",
			want:       "198.51.100.7",
		},
		{
			// No X-Forwarded-For: the direct peer (the proxy, or the client in tests)
			// is authoritative and comes from RemoteAddr.
			name:       "xffAbsentUsesRemoteAddr",
			xff:        "",
			remoteAddr: "192.0.2.10:443",
			want:       "192.0.2.10",
		},
		{
			// Neither source yields a parseable address: zero Addr, which the use
			// case maps to SQL NULL rather than persisting a bogus value.
			name:       "bothUnparseableYieldsZero",
			xff:        "not-an-ip",
			remoteAddr: "garbage",
			want:       "invalid IP",
		},
		{
			// A single-entry header is the proxy-appended hop itself (LastIndex == -1
			// path), so that entry is returned verbatim.
			name:       "singleEntryHeader",
			xff:        "198.51.100.7",
			remoteAddr: "10.0.0.1:8080",
			want:       "198.51.100.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{
				Header:     http.Header{},
				RemoteAddr: tt.remoteAddr,
			}
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}

			got := clientIP(req)
			if got.String() != tt.want {
				t.Fatalf("clientIP() = %q, want %q", got.String(), tt.want)
			}

			// Guard the headline invariant explicitly: the spoofed leftmost value
			// must never be what we record.
			if tt.name == "spoofDefense" && got == netip.MustParseAddr("203.0.113.9") {
				t.Fatalf("clientIP() returned the spoofed leftmost XFF entry")
			}
		})
	}

	// multiLineSpoofDefense exercises the multi-line bypass that motivated the
	// joined read. A client may send X-Forwarded-For as TWO separate header lines,
	// which Go stores as two slice entries; Header.Get (and the table cases above,
	// which use a single Set) would see only the first. This case needs two Add
	// calls, so it stands apart from the single-string table.
	t.Run("multiLineSpoofDefense", func(t *testing.T) {
		req := &http.Request{
			Header:     http.Header{},
			RemoteAddr: "198.51.100.7:54321",
		}
		// A client sending two separate X-Forwarded-For lines: the first is the
		// spoof, the second simulates the proxy's appended hop. Header.Get would
		// have returned the spoof (203.0.113.9); the joined-rightmost read must
		// return 198.51.100.7.
		req.Header.Add("X-Forwarded-For", "203.0.113.9")
		req.Header.Add("X-Forwarded-For", "198.51.100.7")

		got := clientIP(req)
		if got.String() != "198.51.100.7" {
			t.Fatalf("clientIP() = %q, want %q", got.String(), "198.51.100.7")
		}
		// Explicitly never the spoofed first line.
		if got == netip.MustParseAddr("203.0.113.9") {
			t.Fatalf("clientIP() returned the spoofed first XFF line")
		}
	})
}
