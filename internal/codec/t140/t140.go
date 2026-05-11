// Package t140 implements ITU-T T.140 real-time text packetization with the
// RFC 4103 RTP payload format and optional RFC 2198 redundancy (text/red).
package t140

const (
	// ClockRate is the RTP clock rate for T.140 streams (RFC 4103 §4).
	ClockRate = 1000

	// DefaultBufferMs is the recommended T.140 transmission interval
	// (RFC 4103 §5.2).
	DefaultBufferMs = 300

	// ReplacementChar is the T.140 missing-text marker (U+FFFD) inserted on
	// detected packet loss not covered by RED redundancy.
	ReplacementChar = "�"

	// DefaultT140PT is the conventional dynamic payload type for text/t140.
	DefaultT140PT uint8 = 99

	// DefaultREDPT is the conventional dynamic payload type for text/red.
	DefaultREDPT uint8 = 98
)
