package sip

import "context"

// TrunkType discriminates the upstream connection style of a SIP trunk.
// Only TrunkTypeSIPRegister is implemented today; other values are reserved
// for the OpenAPI/AsyncAPI contract and rejected at request time.
type TrunkType string

const (
	// TrunkTypeSIPRegister: VoiceBlender acts as a UAC and REGISTERs to an
	// upstream registrar, refreshing periodically; calls in either direction
	// flow through that registered identity.
	TrunkTypeSIPRegister TrunkType = "sip_register"

	// TrunkTypeIPIP: reserved for static-IP peering (no REGISTER). Not yet
	// implemented; the trunks handler returns 501 when requested.
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

// Trunk is the abstract resource managed by the TrunkManager. Each concrete
// type (sip_register, future ip_ip, ...) implements this interface; lookups
// over the manager are type-agnostic.
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
	AppID() string
	Snapshot() TrunkView
	// Start launches the background lifecycle (REGISTER + refresh for
	// sip_register). Returns immediately.
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
	RegistrarURI            string `json:"registrar_uri"`
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
}

// IPIPTrunkView is the placeholder shape for the unimplemented ip_ip type.
type IPIPTrunkView struct {
	PeerURI string `json:"peer_uri,omitempty"`
}
