package sip

import (
	"context"
	"net/netip"

	"github.com/emiago/sipgo/sip"
)

// TrunkType discriminates the upstream connection style of a SIP trunk.
type TrunkType string

const (
	// TrunkTypeSIPRegister: VoiceBlender acts as a UAC and REGISTERs to an
	// upstream registrar, refreshing periodically; calls in either direction
	// flow through that registered identity.
	TrunkTypeSIPRegister TrunkType = "sip_register"

	// TrunkTypeIPIP: static peering with a fixed upstream, no REGISTER.
	// Outbound calls are routed at the peer; inbound calls are recognised by
	// their source address.
	TrunkTypeIPIP TrunkType = "ip_ip"
)

// TrunkStatus is the runtime state of a trunk's upstream connection.
type TrunkStatus string

const (
	TrunkStatusRegistering   TrunkStatus = "registering"
	TrunkStatusActive        TrunkStatus = "active"
	TrunkStatusFailed        TrunkStatus = "failed"
	TrunkStatusUnregistering TrunkStatus = "unregistering"
	TrunkStatusExpired       TrunkStatus = "expired"
)

// OutboundRoute is what a trunk contributes to an outbound INVITE placed
// through it.
type OutboundRoute struct {
	// RouteURI is the trunk's upstream as a loose-route hop. nil when the
	// Request-URI already reaches it.
	RouteURI *sip.Uri
	// ProxyURI is the trunk's outbound proxy; it outranks RouteURI.
	ProxyURI *sip.Uri
	// FromHost is the realm for From / P-Asserted-Identity. Empty leaves the
	// engine's public host.
	FromHost     string
	AuthUsername string
	AuthPassword string
}

// InboundSourceMatcher is implemented by trunks that claim inbound INVITEs by
// a static source list rather than by PeerSocket. bits is the length of the
// longest matching prefix; exact reports a full host:port match.
type InboundSourceMatcher interface {
	MatchInboundSource(src netip.AddrPort) (bits int, exact bool, ok bool)
}

// Trunk is the abstract resource managed by the TrunkManager. Each concrete
// type (sip_register, ip_ip) implements this interface; lookups over the
// manager are type-agnostic.
type Trunk interface {
	ID() string
	Type() TrunkType
	// AOR returns the canonical identity used to match outbound POST /v1/legs
	// `from` against this trunk. Empty when the trunk type has no AOR concept.
	AOR() string
	// PeerSocket returns the upstream peer's transport address used to tag
	// inbound INVITEs delivered by this trunk. Returns empty host with port 0
	// when not yet known.
	PeerSocket() (host string, port int, transport string)
	// ContactUser is the user part of the Contact this trunk registers; the
	// upstream addresses inbound INVITEs to it. Empty when not applicable.
	ContactUser() string
	AppID() string
	// OutboundRoute returns the routing, identity and credentials for an
	// outbound INVITE to recipient placed through this trunk.
	OutboundRoute(recipient sip.Uri) OutboundRoute
	Snapshot() TrunkView
	// Start launches the background lifecycle (REGISTER + refresh for
	// sip_register, the OPTIONS health check for ip_ip). Returns immediately.
	Start(ctx context.Context)
	// Stop tears the trunk down (de-register for sip_register). Best-effort;
	// honours ctx for timeout.
	Stop(ctx context.Context) error
}

// TrunkView is the JSON-friendly snapshot of a trunk's current state. Each
// type populates its own sub-struct; only one of `SIPRegister` / `IPIP` is
// non-nil per snapshot.
type TrunkView struct {
	ID        string      `json:"id"`
	Type      TrunkType   `json:"type"`
	AppID     string      `json:"app_id,omitempty"`
	Status    TrunkStatus `json:"status"`
	LastError string      `json:"last_error,omitempty"`
	CreatedAt string      `json:"created_at"`

	SIPRegister *SIPRegisterTrunkView `json:"sip_register,omitempty"`
	IPIP        *IPIPTrunkView        `json:"ip_ip,omitempty"`
}

// SIPRegisterTrunkView holds the sip_register-specific runtime fields.
// Credentials (password) are never exposed.
type SIPRegisterTrunkView struct {
	RegistrarURI string `json:"registrar_uri"`
	// OutboundProxy is the next hop this trunk actually routes through, be it
	// set per-trunk or inherited from SIP_OUTBOUND_PROXY. Empty means requests
	// go straight to RegistrarURI.
	OutboundProxy           string `json:"outbound_proxy,omitempty"`
	AOR                     string `json:"aor"`
	Username                string `json:"username,omitempty"`
	ContactURI              string `json:"contact_uri,omitempty"`
	RequestedExpiresSeconds int    `json:"requested_expires_seconds"`
	GrantedExpiresSeconds   int    `json:"granted_expires_seconds,omitempty"`
	LastRegisteredAt        string `json:"last_registered_at,omitempty"`
	NextRefreshAt           string `json:"next_refresh_at,omitempty"`
	CallID                  string `json:"call_id,omitempty"`
	CSeq                    uint32 `json:"cseq,omitempty"`
	// SourceAddress is the host:port the registrar's most recent response
	// actually came from. Initially set from the configured RegistrarURI;
	// updated to the real transport address (which may differ from the URI
	// when DNS / load-balancing fronts the registrar) on each 2xx response.
	// Used as the key for tagging inbound INVITEs back to this trunk.
	SourceAddress string `json:"source_address,omitempty"`
	// TLSInsecureSkipVerify echoes the per-trunk certificate exemption.
	TLSInsecureSkipVerify bool `json:"tls_insecure_skip_verify,omitempty"`
}

// IPIPTrunkView holds the ip_ip-specific fields. Credentials (password) are
// never exposed.
type IPIPTrunkView struct {
	PeerURI       string `json:"peer_uri"`
	OutboundProxy string `json:"outbound_proxy,omitempty"`
	AOR           string `json:"aor,omitempty"`
	Username      string `json:"username,omitempty"`
	// InboundSources is the effective source list inbound INVITEs are matched
	// against, in canonical CIDR form: the configured entries plus the
	// peer_uri host when that is an IP literal.
	InboundSources             []string `json:"inbound_sources"`
	OptionsPingIntervalSeconds int      `json:"options_ping_interval_seconds,omitempty"`
	LastPingAt                 string   `json:"last_ping_at,omitempty"`
	// LastPingStatusCode is the SIP status of the most recent OPTIONS reply;
	// absent when the peer did not answer.
	LastPingStatusCode    int  `json:"last_ping_status_code,omitempty"`
	TLSInsecureSkipVerify bool `json:"tls_insecure_skip_verify,omitempty"`
}
