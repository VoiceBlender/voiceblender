// Package peers drives third-party SIP endpoints (FreeSWITCH today, others
// later) as the far end of VoiceBlender interop tests. Each endpoint lives in
// its own sub-package, implements Peer and Call, and registers itself with
// Register from an init function.
package peers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// Caps declares what an endpoint can do. Tests skip a scenario whose
// capability is missing rather than fail it.
type Caps struct {
	// Codecs the endpoint really has loaded, by VoiceBlender name: PCMU, PCMA,
	// G722, OPUS.
	Codecs []string

	SRTP          bool // SDES-SRTP in both directions
	TLS           bool // listens for SIP over TLS (VoiceBlender dials it)
	EarlyMedia    bool // 183 with SDP before answer
	Hold          bool // sends hold/unhold re-INVITEs and reports received ones
	Refer         bool // sends REFER and accepts one
	SessionTimers bool // RFC 4028
	Reneg         bool // re-offers a different codec mid-call
	RTCPStats     bool // reports RTCP received from VoiceBlender
	Ptime         bool // sends audio at a non-20 ms packetization
	Register      bool // registers to VoiceBlender as a UAC
	Registrar     bool // accepts REGISTER and challenges calls with digest auth
	Flap          bool // has a SIP listener the test can stop and start
}

// HasCodec reports whether the endpoint has codec name loaded.
func (c Caps) HasCodec(name string) bool {
	return slices.ContainsFunc(c.Codecs, func(s string) bool { return strings.EqualFold(s, name) })
}

// ErrUnsupported is returned by a Call method the endpoint cannot perform.
var ErrUnsupported = errors.New("peers: not supported by this endpoint")

// TagHeader is the header that correlates a call VoiceBlender placed with
// the channel it created on the endpoint.
const TagHeader = "X-Interop-Tag"

// URIOpts selects which of the endpoint's listeners a URI points at.
type URIOpts struct {
	TLS  bool // the TLS listener (";transport=tls")
	Auth bool // the listener that challenges INVITEs with digest auth
}

// OriginateOpts describes a call the endpoint places to VoiceBlender.
type OriginateOpts struct {
	To      string            // SIP URI on VoiceBlender
	Tag     string            // sent as TagHeader
	Codecs  []string          // offer in this order; empty = endpoint default
	Ptime   int               // ms; 0 = 20
	SRTP    bool              // offer RTP/SAVP only
	TLS     bool              // send the INVITE over TLS
	Headers map[string]string // extra INVITE headers
	Timeout time.Duration     // give up if unanswered after this; 0 = 60 s

	SessionTimer int // offer Session-Expires with this interval (s); 0 = endpoint default

	AuthUser     string // answer a digest challenge with these credentials
	AuthPassword string
}

// AnswerOpts shapes the endpoint's answer to a call VoiceBlender placed.
type AnswerOpts struct {
	Codec  string // answer with this codec only; "" = endpoint preference
	Ptime  int    // ms; 0 = 20
	NoSRTP bool   // refuse SRTP even when it is offered
}

// HangupInfo is how a call ended, from the endpoint's point of view.
type HangupInfo struct {
	Cause       string // Q.850 cause name, e.g. NORMAL_CLEARING, USER_BUSY
	SIPCode     int    // final SIP status sent or received, 0 if none
	Disposition string // endpoint-specific, e.g. recv_bye, send_cancel
}

// EventKind is a call event a test can wait for.
type EventKind string

const (
	EventAnswered   EventKind = "answered"
	EventEarlyMedia EventKind = "early_media"
	EventHeld       EventKind = "held"   // the far end (VoiceBlender) put us on hold
	EventUnheld     EventKind = "unheld" // the far end took us off hold
	EventReInvite   EventKind = "reinvite"
	EventRTCP       EventKind = "rtcp" // an RTCP report arrived from VoiceBlender
	EventHangup     EventKind = "hangup"
)

// RTCPStats summarises RTCP the endpoint received from VoiceBlender.
type RTCPStats struct {
	Reports int // RTCP reports received
}

// RegisterOpts asks the endpoint to register to VoiceBlender as a UAC.
type RegisterOpts struct {
	Registrar string // host:port of VoiceBlender
	User      string
	Password  string
	Expires   int // seconds; 0 = 60
}

// RegistrarInfo describes the endpoint acting as registrar.
type RegistrarInfo struct {
	HostPort     string            // host:port that accepts REGISTER and challenges INVITEs
	Domain       string            // SIP domain of the provisioned users
	Users        map[string]string // user -> password
	FlapHostPort string            // host:port of a listener Flap can stop and start
}

// Registration is the endpoint's registration to VoiceBlender.
type Registration interface {
	Name() string
	// WaitRegistered blocks until the registrar accepted the REGISTER.
	WaitRegistered(ctx context.Context) error
	// Unregister sends REGISTER with Expires: 0 and removes the binding.
	Unregister() error
}

// Peer is one running third-party SIP endpoint.
type Peer interface {
	Name() string
	Caps() Caps

	// URI that VoiceBlender dials to reach the endpoint. The call rings there,
	// unanswered, until the test drives it through the Call.
	URI(user string, o URIOpts) string
	// ToneURI is a URI that answers immediately and plays hz forever. Used as
	// a REFER target.
	ToneURI(hz int) string
	// BridgedToneURI answers and bridges onward to a tone, so the endpoint
	// has a second leg to act on when it receives a REFER.
	BridgedToneURI(hz int) string

	// Originate places a call from the endpoint to VoiceBlender. It returns
	// once the INVITE is on its way; use Call.WaitAnswered for the outcome.
	Originate(ctx context.Context, o OriginateOpts) (Call, error)
	// WaitInbound returns the endpoint's side of a call VoiceBlender placed
	// with TagHeader set to tag.
	WaitInbound(ctx context.Context, tag string) (Call, error)
	// HangupAll hangs up every call carrying tag. Safe to call at test cleanup.
	HangupAll(tag string)

	Register(ctx context.Context, o RegisterOpts) (Registration, error)
	Registrar() RegistrarInfo
	// WaitRegistered returns the contact user has registered to the endpoint.
	WaitRegistered(ctx context.Context, user string) (contact string, err error)
	// StopFlap and StartFlap stop and restart the listener at
	// RegistrarInfo.FlapHostPort.
	StopFlap() error
	StartFlap() error

	Close() error
}

// Call is the endpoint's side of one call.
type Call interface {
	ID() string

	Ring() error
	EarlyMedia(toneHz int) error
	Answer(o AnswerOpts) error
	Reject(sipCode int) error
	Hangup() error

	PlayTone(hz int) error
	StopMedia() error
	Echo() error
	// StartRecording records what the endpoint receives (not what it sends).
	// stop returns the path of the WAV on the host.
	StartRecording() (stop func() (string, error), err error)

	SendDTMF(digits string) error
	// WaitDTMF returns once n digits arrived, in arrival order.
	WaitDTMF(ctx context.Context, n int) (string, error)

	Hold() error
	Unhold() error
	Renegotiate(codec string, ptime int) error
	// DropSRTP re-offers the current codec over plain RTP on an SRTP call.
	DropSRTP() error
	// Refer sends a blind REFER to target.
	Refer(target string) error
	// ReferStatus returns the final status of the last Refer: VoiceBlender's
	// final response to it, or the sipfrag of its final NOTIFY. ErrUnsupported
	// means the endpoint cannot see the NOTIFY outcome.
	ReferStatus(ctx context.Context) (int, error)

	WaitAnswered(ctx context.Context) error
	WaitHangup(ctx context.Context) (HangupInfo, error)
	// WaitEvent returns at the next occurrence of kind not yet consumed by an
	// earlier WaitEvent.
	WaitEvent(ctx context.Context, kind EventKind) error

	// Header returns a header of the INVITE the endpoint received.
	Header(name string) string
	// Codec is the codec the endpoint is running, by VoiceBlender name.
	Codec() string
	// Ptime is the packetization the endpoint sends at, in ms.
	Ptime() int
	// SRTP reports whether the media is SRTP.
	SRTP() bool
	// Transport is the SIP transport of the call: "udp", "tcp" or "tls".
	Transport() string
	// RemoteSDP is the last SDP the endpoint received from VoiceBlender, or ""
	// if the endpoint cannot expose it.
	RemoteSDP() string
	RTCPStats() (RTCPStats, error)
	// Var exposes endpoint-specific channel state for diagnostics.
	Var(name string) string
}

// SIPCodeToQ850 maps a SIP rejection to the Q.850 cause most endpoints use to
// produce it.
func SIPCodeToQ850(code int) string {
	switch code {
	case 404:
		return "UNALLOCATED_NUMBER"
	case 480:
		return "NO_USER_RESPONSE"
	case 486:
		return "USER_BUSY"
	case 487:
		return "ORIGINATOR_CANCEL"
	case 488:
		return "INCOMPATIBLE_DESTINATION"
	case 503:
		return "NORMAL_TEMPORARY_FAILURE"
	case 603:
		return "CALL_REJECTED"
	default:
		return "CALL_REJECTED"
	}
}
