package sip

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/emiago/sipgo/sip"
)

const (
	// DefaultMinSE is the minimum session interval we accept (RFC 4028 §4).
	DefaultMinSE = 90

	// DefaultSessionExpires is the default session interval when the remote
	// requests timers but doesn't specify an interval.
	DefaultSessionExpires = 1800
)

// SessionTimerParams holds parsed Session-Expires / Min-SE values from a SIP
// request or response.
type SessionTimerParams struct {
	Interval  uint32 // Session-Expires delta-seconds
	Refresher string // "uac" or "uas"
	MinSE     uint32 // Min-SE value (0 = not present)
}

// ParseSessionExpires parses a Session-Expires header value, e.g.
// "1800;refresher=uac" → interval=1800, refresher="uac".
func ParseSessionExpires(value string) (interval uint32, refresher string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, ""
	}

	parts := strings.SplitN(value, ";", 2)

	n, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 32)
	if err != nil || n == 0 {
		return 0, ""
	}
	interval = uint32(n)

	if len(parts) > 1 {
		for _, param := range strings.Split(parts[1], ";") {
			param = strings.TrimSpace(param)
			if strings.HasPrefix(strings.ToLower(param), "refresher=") {
				refresher = strings.ToLower(strings.TrimPrefix(param, "refresher="))
				refresher = strings.TrimPrefix(refresher, "Refresher=") // already lowered
			}
		}
	}

	return interval, refresher
}

// ParseMinSE parses a Min-SE header value, e.g. "90" → 90.
func ParseMinSE(value string) uint32 {
	value = strings.TrimSpace(value)
	// Min-SE can have params like ";refresher=...", but we only care about the delta-seconds
	if idx := strings.IndexByte(value, ';'); idx >= 0 {
		value = value[:idx]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

// FormatSessionExpires formats a Session-Expires header value.
func FormatSessionExpires(interval uint32, refresher string) string {
	return fmt.Sprintf("%d;refresher=%s", interval, refresher)
}

// requestedSessionTimer returns the session timer that a re-INVITE or UPDATE
// asks for, as the 200 OK echoes it, or nil when the request names none.
func requestedSessionTimer(req *sip.Request) *SessionTimerParams {
	h := req.GetHeader("Session-Expires")
	if h == nil {
		return nil
	}
	interval, refresher := ParseSessionExpires(h.Value())
	if interval == 0 {
		return nil
	}
	if refresher == "" {
		// The choice is ours. A proxy may have added the header for a sender
		// that cannot refresh, so the sender gets the role only if it says
		// it supports the timer (RFC 4028 §9).
		refresher = "uas"
		if sup := req.GetHeader("Supported"); sup != nil && containsToken(sup.Value(), "timer") {
			refresher = "uac"
		}
	}
	// Held to the same minimum as the initial INVITE, so a far end cannot make
	// the leg refresh every few seconds.
	minSE := uint32(DefaultMinSE)
	if h := req.GetHeader("Min-SE"); h != nil {
		if v := ParseMinSE(h.Value()); v > minSE {
			minSE = v
		}
	}
	if interval < minSE {
		interval = minSE
	}
	return &SessionTimerParams{Interval: interval, Refresher: refresher}
}
