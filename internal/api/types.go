package api

import "github.com/VoiceBlender/voiceblender/internal/leg"

// FieldEnrichment holds additional OpenAPI metadata for a struct field
// that cannot be derived from Go types and tags alone.
type FieldEnrichment struct {
	Description string
	Enum        []string
	Format      string
	Default     interface{} // string, int, bool, float64
	Minimum     *int
	Maximum     *int
}

func intPtr(v int) *int { return &v }

// CodecsItemEnum provides the enum for CreateLegRequest.codecs array items.
var CodecsItemEnum = []string{"PCMU", "PCMA", "G722", "opus", "AMR-WB", "AMR-NB"}

// ── Request types ───────────────────────────────────────────────────────

// CreateLegRequest is the request body for POST /v1/legs.
type CreateLegRequest struct {
	Type            string            `json:"type"`                       // "sip", "whatsapp", or "websocket"
	To              string            `json:"to,omitempty"`               // destination — SIP URI for sip legs, E.164 phone number for whatsapp legs
	URI             string            `json:"uri,omitempty"`              // deprecated alias for `to` (sip legs only)
	From            string            `json:"from,omitempty"`             // caller ID — a bare user-part ("+15551234567") or a full SIP URI ("sip:alice@pbx.example.com")
	Privacy         string            `json:"privacy,omitempty"`          // SIP Privacy header value (e.g. "id", "none")
	RingTimeout     int               `json:"ring_timeout,omitempty"`     // seconds; 0 = no timeout
	MaxDuration     int               `json:"max_duration,omitempty"`     // seconds; 0 = no limit
	Codecs          []string          `json:"codecs,omitempty"`           // codec preference order, e.g. ["PCMU","PCMA","G722","opus"]
	Headers         map[string]string `json:"headers,omitempty"`          // custom SIP/WS headers for outbound INVITE or WS handshake
	RoomID          string            `json:"room_id,omitempty"`          // add leg to this room once media is ready (early_media or connected)
	Auth            *SIPAuth          `json:"auth,omitempty"`             // digest auth credentials — required for whatsapp, optional for sip
	WebhookURL      string            `json:"webhook_url,omitempty"`      // route events for this leg to this URL
	WebhookSecret   string            `json:"webhook_secret,omitempty"`   // HMAC secret for webhook signature
	AMD             *AMDParams        `json:"amd,omitempty"`              // enable answering machine detection on outbound calls
	AcceptDTMF      *bool             `json:"accept_dtmf,omitempty"`      // if false, leg will not receive DTMF broadcast from other legs in the same room
	AppID           string            `json:"app_id,omitempty"`           // application identifier for event stream filtering
	SpeechDetection *bool             `json:"speech_detection,omitempty"` // override server default for speaking.started/speaking.stopped events
	RTT             bool              `json:"rtt,omitempty"`              // offer Real-Time Text (T.140 / RFC 4103) on the outbound INVITE, or enable bidi text channel for websocket legs

	// Streams describes audio streams to offer *in addition to* the call's
	// primary bidirectional audio, so a multi-stream call is up from the first
	// INVITE rather than after a re-INVITE. SIP outbound only.
	Streams []CreateLegStream `json:"streams,omitempty"`

	// WebSocket leg fields (only used when Type == "websocket"):
	URL          string `json:"url,omitempty"`           // ws:// or wss:// target for outbound dial
	SampleRate   int    `json:"sample_rate,omitempty"`   // 8000/16000/24000/48000; default 16000
	WireFormat   string `json:"wire_format,omitempty"`   // "binary" (default) or "json_base64"
	SampleFormat string `json:"sample_format,omitempty"` // "s16le" (default; only format in v1)

	// LiveKit leg fields (only used when Type == "livekit_room"):
	LiveKit *LiveKitParams `json:"livekit,omitempty"`
}

// CreateLegStream is one extra m=audio section to offer in the initial INVITE.
// The call's primary stream is implicit and always first; these follow it, each
// on its own RTP port.
type CreateLegStream struct {
	Direction string `json:"direction,omitempty"`
	Lang      string `json:"lang,omitempty"`
	Content   string `json:"content,omitempty"`
	Label     string `json:"label,omitempty"`
	RoomID    string `json:"room_id,omitempty"`
	Role      string `json:"role,omitempty"`
}

var createLegStreamFields = map[string]FieldEnrichment{
	"direction": {Description: "Media direction for this stream, from this server's point of view. Defaults to sendrecv.", Enum: []string{"sendrecv", "sendonly", "recvonly", "inactive"}, Default: "sendrecv"},
	"lang":      {Description: "BCP 47 language tag advertised as a=lang (RFC 8866), e.g. \"es-ES\" for a Spanish translation feed."},
	"content":   {Description: "Value advertised as a=content (RFC 4796). Use \"alt\" for an alternative feed such as a translation.", Enum: []string{"main", "alt", "speaker", "slides", "sl"}},
	"label":     {Description: "Value advertised as a=label (RFC 4574), for correlating the stream with external metadata."},
	"room_id":   {Description: "Room to mix this stream into once the call connects. May differ from the leg's own room_id, which governs the primary stream."},
	"role":      {Description: "Routing role for this stream inside its room."},
}

// LiveKitParams configures a livekit_room leg. Provide either Token (a
// pre-signed JWT) OR Room+Identity (the leg-create handler will mint a
// JWT when LIVEKIT_TOKEN_SIGNING_ENABLED=true). If both are provided, the
// explicit Token wins.
type LiveKitParams struct {
	URL             string              `json:"url,omitempty"`              // overrides LIVEKIT_URL
	Token           string              `json:"token,omitempty"`            // pre-signed JWT
	Room            string              `json:"room,omitempty"`             // required when minting
	Identity        string              `json:"identity,omitempty"`         // required when minting
	ParticipantName string              `json:"participant_name,omitempty"` // optional display name
	Permissions     *LiveKitPermissions `json:"permissions,omitempty"`      // defaults: publish+subscribe true, data false
	TokenTTL        string              `json:"token_ttl,omitempty"`        // Go duration string; default 6h
	OpusBitrate     int                 `json:"opus_bitrate,omitempty"`     // override LIVEKIT_OPUS_BITRATE
}

// LiveKitPermissions mirrors the LiveKit video grant flags. Nil pointers
// fall back to defaults (publish=true, subscribe=true, data=false).
type LiveKitPermissions struct {
	CanPublish     *bool `json:"can_publish,omitempty"`
	CanSubscribe   *bool `json:"can_subscribe,omitempty"`
	CanPublishData *bool `json:"can_publish_data,omitempty"`
	RoomAdmin      *bool `json:"room_admin,omitempty"` // grants admin actions like server-side MuteTrack on remote participants
}

var createLegRequestFields = map[string]FieldEnrichment{
	"type":             {Description: "Leg type", Enum: []string{"sip", "whatsapp", "websocket", "livekit_room"}},
	"to":               {Description: "Destination. For sip legs, a SIP URI (e.g. \"sip:alice@example.com\"). For whatsapp legs, an E.164 phone number (with or without '+')."},
	"uri":              {Description: "Deprecated alias for `to` (sip legs only). Prefer `to`."},
	"from":             {Description: `Caller ID. A bare user-part (e.g. "+15551234567", "alice") sets the user of the SIP From header. A full SIP URI (e.g. "sip:alice@pbx.example.com") sets both the user and the host; otherwise the host comes from the matched trunk's AOR realm, falling back to SIP_DOMAIN.`},
	"privacy":          {Description: `SIP Privacy header value (e.g. "id", "none")`},
	"ring_timeout":     {Description: "Seconds to wait for answer; 0 = no timeout", Default: 0},
	"max_duration":     {Description: "Maximum call duration in seconds after connect. Automatically hung up when reached. 0 or omitted = no limit.", Default: 0},
	"codecs":           {Description: "Codec preference order (sip legs only)"},
	"headers":          {Description: "Custom headers to include in the outbound INVITE (sip/whatsapp) or the WebSocket upgrade request (websocket)"},
	"room_id":          {Description: "Room ID to auto-add the leg to once media is ready (early_media or connected). If the room does not exist, it is automatically created."},
	"auth":             {Description: "Digest auth credentials. Required for whatsapp legs (Meta-issued password; username defaults to `from` with '+' stripped). Optional for sip legs (sipgo retries on 401/407 challenge)."},
	"webhook_url":      {Description: "Route all events for this leg exclusively to this URL instead of global webhooks.", Format: "uri"},
	"webhook_secret":   {Description: "HMAC-SHA256 signing secret for the per-leg webhook."},
	"amd":              {Description: "Enable Answering Machine Detection on outbound calls. Include the object (even empty) to enable with defaults; omit to disable."},
	"accept_dtmf":      {Description: "If false, this leg will not receive DTMF digits broadcast from other legs in the same room. Defaults to true.", Default: true},
	"app_id":           {Description: "Application identifier. Carried through to all events for this leg. Use to filter the WebSocket event stream by app."},
	"speech_detection": {Description: "If true, emit speaking.started and speaking.stopped events for this leg. If false, suppress them. Omit to use the server default (SPEECH_DETECTION_ENABLED env var, default false)."},
	"rtt":              {Description: "For sip legs: offer Real-Time Text (ITU-T T.140 over RTP per RFC 4103) alongside audio. For websocket legs: enable the bidirectional text-message channel. Default: false.", Default: false},
	"streams":          {Description: "SIP outbound only. Extra m=audio sections to offer alongside the call's primary bidirectional audio, so a multi-stream call is established by the first INVITE instead of a follow-up re-INVITE. Each entry binds its own RTP port and may be mixed into its own room. To add a stream to a call that is already up, use POST /v1/legs/{id}/streams instead."},
	"url":              {Description: "WebSocket target URL (ws:// or wss://) for outbound websocket legs. Required when type=websocket.", Format: "uri"},
	"sample_rate":      {Description: "PCM sample rate for websocket legs. The room's mixer automatically resamples between this and the room rate.", Enum: []string{"8000", "16000", "24000", "48000"}, Default: 16000},
	"wire_format":      {Description: "Audio framing for websocket legs. `binary` ships raw PCM as WebSocket binary frames; `json_base64` wraps PCM as `{\"type\":\"audio\",\"audio\":\"<base64>\"}` text frames (browser-friendly).", Enum: []string{"binary", "json_base64"}, Default: "binary"},
	"sample_format":    {Description: "On-the-wire PCM sample encoding for websocket legs. v1 only supports `s16le`.", Enum: []string{"s16le"}, Default: "s16le"},
	"livekit":          {Description: "LiveKit room join parameters (only used when type=livekit_room)."},
}

// AnswerLegRequest is the optional request body for POST /v1/legs/{id}/answer.
type AnswerLegRequest struct {
	SpeechDetection *bool  `json:"speech_detection,omitempty"` // override server default for speaking.started/speaking.stopped events
	Codec           string `json:"codec,omitempty"`            // explicit codec to use (must be in the remote offer)

	// Streams routes the caller's additional audio streams to rooms once the
	// answer is negotiated. Positional: entry i addresses the i-th accepted
	// stream beyond the primary, in m-line order.
	Streams []AnswerLegStream `json:"streams,omitempty"`
}

// AnswerLegStream places one of an inbound call's additional audio streams into
// a room at answer time.
type AnswerLegStream struct {
	RoomID string `json:"room_id,omitempty"`
	Role   string `json:"role,omitempty"`
}

var answerLegStreamFields = map[string]FieldEnrichment{
	"room_id": {Description: "Room to mix this stream into once the answer is negotiated. May differ from the room the leg itself joins."},
	"role":    {Description: "Routing role for this stream inside its room."},
}

var answerLegRequestFields = map[string]FieldEnrichment{
	"speech_detection": {Description: "If true, emit speaking.started and speaking.stopped events for this leg. If false, suppress them. Omit to use the server default (SPEECH_DETECTION_ENABLED env var, default false)."},
	"codec":            {Description: "Explicit codec for the answer SDP. Must appear in the remote offer's offered_codecs list. Omit to use the server's default preference order.", Enum: CodecsItemEnum},
	"streams":          {Description: "Rooms for the caller's additional audio streams, applied once the answer is negotiated. Positional: entry i addresses the i-th accepted stream beyond the primary, in m-line order — the caller's offer decides how many exist, so an entry with no matching stream is ignored. Use POST /v1/legs/{id}/streams/{streamId}/room to re-route a stream later."},
}

// EarlyMediaLegRequest is the optional request body for POST /v1/legs/{id}/early-media.
type EarlyMediaLegRequest struct {
	Codec string `json:"codec,omitempty"` // explicit codec to use (must be in the remote offer)
}

var earlyMediaLegRequestFields = map[string]FieldEnrichment{
	"codec": {Description: "Explicit codec for the 183 Session Progress SDP. Must appear in the remote offer's offered_codecs list. Omit to use the server's default preference order.", Enum: CodecsItemEnum},
}

// DeleteLegRequest is the optional request body for DELETE /v1/legs/{id}.
// Honored only for unanswered SIP inbound legs (state ringing or
// early_media). For connected legs the body is ignored and the leg is hung
// up via SIP BYE with the legacy `api_hangup` reason.
type DeleteLegRequest struct {
	Reason string `json:"reason,omitempty"`
}

// DeleteReasonEnum lists the reason values accepted on DELETE /v1/legs/{id}.
// Each maps to a SIP final response code on unanswered legs and ends up in
// the resulting leg.disconnected event's cdr.reason.
var DeleteReasonEnum = []string{"busy", "declined", "rejected", "unavailable", "not_found", "forbidden", "server_error"}

var deleteLegRequestFields = map[string]FieldEnrichment{
	"reason": {Description: "Disconnect reason. Only honored for unanswered SIP inbound legs (state `ringing` or `early_media`); on connected legs the body is ignored and the leg is hung up with the legacy `api_hangup` reason. The value flows through to `leg.disconnected`'s `cdr.reason` and selects the SIP final response: `busy`→486, `declined`/`rejected`→603, `unavailable`→480, `not_found`→404, `forbidden`→403, `server_error`→500.", Enum: DeleteReasonEnum},
}

// TransferRequest is the body for POST /v1/legs/{id}/transfer.
type TransferRequest struct {
	Target        string `json:"target"`                    // SIP URI of the third party
	ReplacesLegID string `json:"replaces_leg_id,omitempty"` // attended transfer: existing leg whose dialog is replaced
}

var transferRequestFields = map[string]FieldEnrichment{
	"target":          {Description: "SIP URI to transfer the call to (e.g. \"sip:bob@example.com\")."},
	"replaces_leg_id": {Description: "ID of an existing connected SIP leg whose dialog should be replaced (attended transfer). Omit for blind transfer."},
}

// SIPAuth holds digest authentication credentials.
type SIPAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password"`
}

var sipAuthFields = map[string]FieldEnrichment{
	"username": {Description: "Digest auth username. Optional for whatsapp legs (defaults to `from` with '+' stripped, per Meta's spec)."},
	"password": {Description: "Digest auth password."},
}

// AMDParams configures per-call Answering Machine Detection thresholds.
// All durations are in milliseconds. Zero values fall back to global defaults.
type AMDParams struct {
	InitialSilenceTimeout int `json:"initial_silence_timeout,omitempty"` // ms — max silence before no_speech
	GreetingDuration      int `json:"greeting_duration,omitempty"`       // ms — speech threshold for machine
	AfterGreetingSilence  int `json:"after_greeting_silence,omitempty"`  // ms — silence after speech for human
	TotalAnalysisTime     int `json:"total_analysis_time,omitempty"`     // ms — hard analysis deadline
	MinimumWordLength     int `json:"minimum_word_length,omitempty"`     // ms — min speech burst to count
	BeepTimeout           int `json:"beep_timeout,omitempty"`            // ms — time to wait for beep after machine (0=disabled)
}

var liveKitParamsFields = map[string]FieldEnrichment{
	"url":              {Description: "LiveKit server endpoint (wss://...). Overrides LIVEKIT_URL.", Format: "uri"},
	"token":            {Description: "Pre-signed LiveKit JWT. Mutually exclusive with `room`/`identity` (mint mode); if both are present the token wins."},
	"room":             {Description: "LiveKit room name. Required when minting (i.e. `token` is empty AND LIVEKIT_TOKEN_SIGNING_ENABLED=true)."},
	"identity":         {Description: "LiveKit participant identity. Required when minting."},
	"participant_name": {Description: "Display name for the participant; surfaces in LK Room UIs."},
	"permissions":      {Description: "LiveKit grant flags. Nil pointers default to publish=true, subscribe=true, data=false, admin=false."},
	"token_ttl":        {Description: "Go duration string (e.g. \"30m\", \"6h\"). Used only when minting. Defaults to LIVEKIT_DEFAULT_TOKEN_TTL (6h)."},
	"opus_bitrate":     {Description: "Override LIVEKIT_OPUS_BITRATE for this leg. 6000..510000.", Minimum: intPtr(6000), Maximum: intPtr(510000)},
}

var liveKitPermissionsFields = map[string]FieldEnrichment{
	"can_publish":      {Description: "Allow publishing tracks. Default true."},
	"can_subscribe":    {Description: "Allow subscribing to remote tracks. Default true."},
	"can_publish_data": {Description: "Allow publishing data channel messages. Default false (audio bridge does not use data)."},
	"room_admin":       {Description: "Grant admin actions on the room (e.g., server-side MuteTrack of remote participants). Default false."},
}

var amdParamsFields = map[string]FieldEnrichment{
	"initial_silence_timeout": {Description: "Max milliseconds of silence before declaring no_speech", Default: 2500},
	"greeting_duration":       {Description: "Speech duration threshold (ms) above which answerer is classified as machine", Default: 1500},
	"after_greeting_silence":  {Description: "Silence duration (ms) after initial speech to declare human", Default: 800},
	"total_analysis_time":     {Description: "Max analysis window in milliseconds. A threshold longer than this window suppresses that verdict; a window shorter than all of initial_silence_timeout, greeting_duration and after_greeting_silence is rejected, since the call could only end not_sure.", Default: 5000},
	"minimum_word_length":     {Description: "Minimum speech burst duration (ms) to count as a word", Default: 100},
	"beep_timeout":            {Description: "Max time (ms) to wait for the voicemail beep after machine detection. 0 or omitted = disabled.", Default: 0},
}

// LegView is the JSON representation of a leg.
type LegView struct {
	ID         string            `json:"id"`
	Type       leg.LegType       `json:"type"`
	State      leg.LegState      `json:"state"`
	RoomID     string            `json:"room_id,omitempty"`
	Muted      bool              `json:"muted"`
	Deaf       bool              `json:"deaf"`
	AcceptDTMF bool              `json:"accept_dtmf"`
	Held       bool              `json:"held"`
	Role       string            `json:"role,omitempty"`
	AppID      string            `json:"app_id,omitempty"`
	SIPHeaders map[string]string `json:"sip_headers,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

var legViewFields = map[string]FieldEnrichment{
	"id":          {Description: "Unique leg identifier (UUID)"},
	"type":        {Description: "Leg type", Enum: []string{"sip_inbound", "sip_outbound", "webrtc", "whatsapp_in", "whatsapp_out", "websocket_in", "websocket_out", "moq_in", "livekit_publish", "livekit_participant"}},
	"state":       {Description: "Leg state", Enum: []string{"ringing", "early_media", "connected", "held", "hung_up"}},
	"room_id":     {Description: "Room ID if the leg is in a room, empty otherwise"},
	"muted":       {Description: "Whether the leg is muted (cannot be heard by others)"},
	"deaf":        {Description: "Whether the leg is deaf (cannot hear others)"},
	"accept_dtmf": {Description: "Whether the leg receives DTMF digits broadcast from other legs in the same room. Defaults to true."},
	"held":        {Description: "Whether the call is on hold (SIP legs only)"},
	"role":        {Description: "Routing role used by the room's audio routing matrix (e.g. \"customer\", \"agent\", \"supervisor\"). Empty string means unroled (full mesh)."},
	"app_id":      {Description: "Application identifier for event stream filtering."},
	"sip_headers": {Description: "Deprecated: X-* headers from the inbound INVITE. Only present on sip_inbound legs. Use `headers` for new code; it carries the same map plus surfaces handshake headers for websocket legs."},
	"headers":     {Description: "Custom protocol headers exposed by the leg's transport — X-/P- headers from a SIP INVITE, the WebSocket upgrade request, or supplied at outbound dial time."},
}

// CreateRoomRequest is the request body for POST /v1/rooms.
type CreateRoomRequest struct {
	ID            string `json:"id"`
	WebhookURL    string `json:"webhook_url,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
	AppID         string `json:"app_id,omitempty"`
	SampleRate    int    `json:"sample_rate,omitempty"`
}

var createRoomRequestFields = map[string]FieldEnrichment{
	"id":             {Description: "Custom room ID (auto-generated UUID if omitted)"},
	"webhook_url":    {Description: "Route all events for this room exclusively to this URL instead of global webhooks.", Format: "uri"},
	"webhook_secret": {Description: "HMAC-SHA256 signing secret for the per-room webhook."},
	"app_id":         {Description: "Application identifier. Carried through to all events for this room. Use to filter the WebSocket event stream by app."},
	"sample_rate":    {Description: "Mixer sample rate in Hz. Allowed values: 8000, 16000, 48000. Default: 16000.", Enum: []string{"8000", "16000", "48000"}, Default: 16000},
}

// RoomView is the JSON representation of a room.
type RoomView struct {
	ID           string    `json:"id"`
	AppID        string    `json:"app_id,omitempty"`
	SampleRate   int       `json:"sample_rate"`
	Participants []LegView `json:"participants"`
}

var roomViewFields = map[string]FieldEnrichment{
	"id":           {Description: "Room identifier"},
	"app_id":       {Description: "Application identifier for event stream filtering."},
	"sample_rate":  {Description: "Mixer sample rate in Hz (8000, 16000, or 48000)."},
	"participants": {Description: "Legs currently in this room"},
}

// AddLegRequest is the request body for POST /v1/rooms/{id}/legs.
type AddLegRequest struct {
	LegID      string  `json:"leg_id"`
	Mute       *bool   `json:"mute,omitempty"`
	Deaf       *bool   `json:"deaf,omitempty"`
	AcceptDTMF *bool   `json:"accept_dtmf,omitempty"`
	Role       *string `json:"role,omitempty"`

	// Streams additionally mixes named audio streams of the leg into this room.
	// Omit to add only the leg's primary stream, which is the classic behaviour.
	Streams []AddRoomStream `json:"streams,omitempty"`
}

// AddRoomStream names one of a leg's additional audio streams to mix into the
// room being added to.
type AddRoomStream struct {
	StreamID string `json:"stream_id"`
	Role     string `json:"role,omitempty"`
}

var addRoomStreamFields = map[string]FieldEnrichment{
	"stream_id": {Description: "Stream identifier from GET /v1/legs/{id}/streams. The primary stream is not addressable here — it joins with the leg itself."},
	"role":      {Description: "Routing role for this stream inside the room."},
}

var addLegRequestFields = map[string]FieldEnrichment{
	"leg_id":      {Description: "ID of the leg to add"},
	"mute":        {Description: "If set, apply this mute state to the leg atomically before it joins the mixer (no race where un-muted audio enters the mix). Omit to leave current state untouched (useful when moving between rooms)."},
	"deaf":        {Description: "If set, apply this deaf state to the leg atomically before it joins the mixer. Omit to leave current state untouched."},
	"accept_dtmf": {Description: "If set, control whether this leg receives DTMF digits broadcast from other legs in the same room. Omit to leave current state untouched (default for new legs is true)."},
	"role":        {Description: "If set, apply this routing role to the leg atomically before it joins the mixer. The room's routing matrix (see PUT /v1/rooms/{id}/routing) decides which other legs this leg hears and is heard by based on roles. Pass \"\" to clear the role (full mesh). Omit to leave the current role untouched."},
	"streams":     {Description: "Additional audio streams of the leg to mix into this room, each with its own routing role. Omit to add only the leg's primary stream. A stream already mixed elsewhere is moved here."},
}

// LegStreamView is one negotiated audio stream (one m=audio section) of a SIP
// leg. A single-stream call has exactly one, the primary.
type LegStreamView struct {
	ID               string `json:"id"`
	MID              string `json:"mid,omitempty"`
	Index            int    `json:"index"`
	Primary          bool   `json:"primary"`
	State            string `json:"state"`
	Direction        string `json:"direction"`
	DesiredDirection string `json:"desired_direction,omitempty"`
	Codec            string `json:"codec,omitempty"`
	SampleRate       int    `json:"sample_rate,omitempty"`
	LocalPort        int    `json:"local_port,omitempty"`
	RemoteAddr       string `json:"remote_addr,omitempty"`
	Label            string `json:"label,omitempty"`
	Content          string `json:"content,omitempty"`
	Lang             string `json:"lang,omitempty"`
	RoomID           string `json:"room_id,omitempty"`
	Role             string `json:"role,omitempty"`
}

var legStreamViewFields = map[string]FieldEnrichment{
	"id":                {Description: "Stream identifier, stable for the life of the dialog. The primary stream is always \"0\"."},
	"mid":               {Description: "The stream's SDP a=mid token (RFC 5888), used to correlate it across offer/answer."},
	"index":             {Description: "Position of the stream's m= line in the SDP. Fixed for the life of the dialog (RFC 3264 §8)."},
	"primary":           {Description: "True for the call's main bidirectional audio stream, which cannot be removed or attached to a room independently."},
	"state":             {Description: "Negotiation state.", Enum: []string{"pending", "active", "removed"}},
	"direction":         {Description: "Negotiated media direction from this server's point of view.", Enum: []string{"sendrecv", "sendonly", "recvonly", "inactive"}},
	"desired_direction": {Description: "Direction requested by the application. Survives hold/unhold, unlike the negotiated direction.", Enum: []string{"sendrecv", "sendonly", "recvonly", "inactive"}},
	"codec":             {Description: "Codec negotiated for this stream. Streams on one leg may use different codecs."},
	"sample_rate":       {Description: "Native sample rate of the stream's codec, in Hz."},
	"local_port":        {Description: "Local RTP port. Each stream binds its own port; a shared transport is undefined without BUNDLE (RFC 9143)."},
	"remote_addr":       {Description: "Remote RTP address media is currently sent to."},
	"label":             {Description: "The stream's a=label value (RFC 4574), for correlating it with external metadata."},
	"content":           {Description: "The stream's a=content value (RFC 4796), e.g. \"main\" for original audio and \"alt\" for a translated feed."},
	"lang":              {Description: "The stream's a=lang value (RFC 8866): a BCP 47 language tag such as \"en\" or \"es-ES\"."},
	"room_id":           {Description: "Room this stream's audio is mixed into. A secondary stream may sit in a different room than its leg."},
	"role":              {Description: "Routing role of this stream within its room. Streams carry their own role, independent of their leg's."},
}

// AddLegStreamRequest is the request body for POST /v1/legs/{id}/streams.
type AddLegStreamRequest struct {
	Direction string `json:"direction,omitempty"`
	Lang      string `json:"lang,omitempty"`
	Content   string `json:"content,omitempty"`
	Label     string `json:"label,omitempty"`
	RoomID    string `json:"room_id,omitempty"`
	Role      string `json:"role,omitempty"`
}

var addLegStreamRequestFields = map[string]FieldEnrichment{
	"direction": {Description: "Media direction for the new stream, from this server's point of view. Defaults to sendrecv.", Enum: []string{"sendrecv", "sendonly", "recvonly", "inactive"}, Default: "sendrecv"},
	"lang":      {Description: "BCP 47 language tag advertised as a=lang (RFC 8866), e.g. \"es-ES\" for a Spanish translation feed."},
	"content":   {Description: "Value advertised as a=content (RFC 4796). Use \"main\" for original audio and \"alt\" for an alternative feed such as a translation.", Enum: []string{"main", "alt", "speaker", "slides", "sl"}},
	"label":     {Description: "Value advertised as a=label (RFC 4574), for correlating the stream with external metadata."},
	"room_id":   {Description: "If set, attach the new stream to this room once it is negotiated."},
	"role":      {Description: "Routing role to apply when room_id is set."},
}

// UpdateLegStreamRequest is the request body for
// PATCH /v1/legs/{id}/streams/{streamId}. Only the routing role is mutable in
// place; the SDP-level attributes (direction, lang, content, label) are fixed
// at negotiation time.
type UpdateLegStreamRequest struct {
	Role *string `json:"role,omitempty"`
}

var updateLegStreamRequestFields = map[string]FieldEnrichment{
	"role": {Description: "New routing role for this stream inside its room. The room's routing matrix decides who hears it. Pass an empty string to clear the role (full mesh). Omit to leave it untouched. Applied atomically — the room's allow-sets are recomputed in a single mixer-mutex acquisition, so no audio bleeds through mid-change."},
}

// AttachStreamRoomRequest is the request body for
// POST /v1/legs/{id}/streams/{streamId}/room.
type AttachStreamRoomRequest struct {
	RoomID string `json:"room_id"`
	Role   string `json:"role,omitempty"`
}

var attachStreamRoomRequestFields = map[string]FieldEnrichment{
	"room_id": {Description: "Room to mix this stream into. May differ from the leg's own room."},
	"role":    {Description: "Routing role for the stream inside that room. The room's routing matrix decides who hears it."},
}

// SetLegRoleRequest is the request body for PATCH /v1/legs/{id}/role.
// Empty string clears the role (full mesh for that leg).
type SetLegRoleRequest struct {
	Role string `json:"role"`
}

var setLegRoleRequestFields = map[string]FieldEnrichment{
	"role": {Description: "New routing role for the leg. The room's routing matrix decides which other legs this leg hears and is heard by based on roles. Pass an empty string to clear the role (full mesh)."},
}

// RoomRoutingRequest is the request body for PUT /v1/rooms/{id}/routing.
// Matrix maps a listener-role to the set of source roles that legs with
// that role are allowed to hear. A listener role with no entry defaults
// to full mesh (hears every other leg). An entry with an empty list is
// an explicit "hear nothing" (isolated listener role).
type RoomRoutingRequest struct {
	Matrix map[string][]string `json:"matrix"`
}

var roomRoutingRequestFields = map[string]FieldEnrichment{
	"matrix": {Description: "Listener-role → list of allowed source roles. Omitted listener roles default to full mesh. Empty list = hears nothing."},
}

// RoutingRowUpdate is one entry in RoomRoutingUpdateRequest.Updates. Sources
// == nil clears the row (full mesh for that listener role).
type RoutingRowUpdate struct {
	ListenerRole string   `json:"listener_role"`
	Sources      []string `json:"sources"`
}

var routingRowUpdateFields = map[string]FieldEnrichment{
	"listener_role": {Description: "The role whose row is being replaced."},
	"sources":       {Description: "New list of allowed source roles for this listener role. Pass null to clear the row (full mesh)."},
}

// RoomRoutingUpdateRequest is the request body for PATCH /v1/rooms/{id}/routing.
type RoomRoutingUpdateRequest struct {
	Updates []RoutingRowUpdate `json:"updates"`
}

var roomRoutingUpdateRequestFields = map[string]FieldEnrichment{
	"updates": {Description: "Per-listener-role row replacements applied as a single atomic update."},
}

// RoomRoutingView is the response body for GET/PUT/PATCH /v1/rooms/{id}/routing.
type RoomRoutingView struct {
	Matrix map[string][]string `json:"matrix"`
}

var roomRoutingViewFields = map[string]FieldEnrichment{
	"matrix": {Description: "Listener-role → list of allowed source roles. Roles absent from the matrix default to full mesh."},
}

// CreateRoomBridgeRequest is the request body for
// POST /v1/rooms/{id}/bridges. Direction is relative to the room in the path.
type CreateRoomBridgeRequest struct {
	ID        string `json:"id,omitempty"`
	RoomID    string `json:"room_id"`
	Direction string `json:"direction,omitempty"`
}

var createRoomBridgeRequestFields = map[string]FieldEnrichment{
	"id":        {Description: "Custom bridge ID (auto-generated UUID if omitted)"},
	"room_id":   {Description: "The other room to join. Must use the same sample rate as the room in the path."},
	"direction": {Description: "Audio flow relative to the room in the path: bidirectional (both hear each other), send (path room → other only), receive (other → path room only), none (allocated but silent). Default: bidirectional.", Enum: []string{"bidirectional", "send", "receive", "none"}, Default: "bidirectional"},
}

// UpdateRoomBridgeRequest is the request body for
// PATCH /v1/rooms/{id}/bridges/{bridgeID}.
type UpdateRoomBridgeRequest struct {
	Direction string `json:"direction"`
}

var updateRoomBridgeRequestFields = map[string]FieldEnrichment{
	"direction": {Description: "New audio flow relative to the room in the path: bidirectional, send, receive, or none.", Enum: []string{"bidirectional", "send", "receive", "none"}},
}

// BridgeView is the JSON representation of a bridge, relative to the room in
// the request path.
type BridgeView struct {
	ID         string `json:"id"`
	RoomID     string `json:"room_id"`
	Direction  string `json:"direction"`
	SampleRate int    `json:"sample_rate"`
}

var bridgeViewFields = map[string]FieldEnrichment{
	"id":          {Description: "Bridge identifier"},
	"room_id":     {Description: "The peer room joined to the room in the path"},
	"direction":   {Description: "Audio flow relative to the room in the path: bidirectional, send, receive, or none.", Enum: []string{"bidirectional", "send", "receive", "none"}},
	"sample_rate": {Description: "Shared mixer sample rate in Hz (both rooms must match)."},
}

// PlaybackRequest is the request body for POST /v1/legs/{id}/play and POST /v1/rooms/{id}/play.
type PlaybackRequest struct {
	URL      string `json:"url"`
	Tone     string `json:"tone"`
	MimeType string `json:"mime_type"`
	Repeat   int    `json:"repeat"`
	Volume   int    `json:"volume"`
}

var playbackRequestFields = map[string]FieldEnrichment{
	"url":       {Description: "URL of the audio file (mutually exclusive with tone)", Format: "uri"},
	"tone":      {Description: "Built-in telephone tone name. Format: {country}_{type} or bare {type} (defaults to US). Types: ringback, busy, dial, congestion. Countries: us, gb, de, fr, au, jp, it, in, br, pl, ru. Examples: us_ringback, gb_busy, dial."},
	"mime_type": {Description: "MIME type (e.g. audio/wav). Required when using url."},
	"repeat":    {Description: "Number of times to repeat playback (url only)", Default: 0},
	"volume":    {Description: "Volume adjustment in dB (-8 to 8)", Minimum: intPtr(-8), Maximum: intPtr(8), Default: 0},
}

// VolumeRequest is the request body for PATCH /v1/legs/{id}/play/{playbackID}.
type VolumeRequest struct {
	Volume int `json:"volume"`
}

var volumeRequestFields = map[string]FieldEnrichment{
	"volume": {Description: "Volume adjustment (-8 to 8, ~3dB per step, 0 = unchanged)", Minimum: intPtr(-8), Maximum: intPtr(8)},
}

// DTMFRequest is the request body for POST /v1/legs/{id}/dtmf.
type DTMFRequest struct {
	Digits string `json:"digits"`
}

var dtmfRequestFields = map[string]FieldEnrichment{
	"digits": {Description: "DTMF digits to send (0-9, *, #)"},
}

// RTTRequest is the request body for POST /v1/legs/{id}/rtt.
type RTTRequest struct {
	Text string `json:"text"`
}

var rttRequestFields = map[string]FieldEnrichment{
	"text": {Description: "UTF-8 text to send. May be one or more characters and may include T.140 control codes (e.g. backspace U+0008, CR/LF)."},
}

// TTSRequest is the request body for POST /v1/legs/{id}/tts and POST /v1/rooms/{id}/tts.
type TTSRequest struct {
	Text     string `json:"text"`
	Voice    string `json:"voice"`
	ModelID  string `json:"model_id"`
	Language string `json:"language,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Volume   int    `json:"volume"`
	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

var ttsRequestFields = map[string]FieldEnrichment{
	"text":     {Description: "Text to synthesize"},
	"voice":    {Description: "Provider-specific voice identifier. ElevenLabs: voice name or ID. AWS Polly: voice ID (e.g. Joanna, Matthew). Google Cloud: voice name — either full format (e.g. en-US-Neural2-F) or short name for Gemini models (e.g. Achernar, Kore). Deepgram: model name (e.g. aura-2-asteria-en)."},
	"model_id": {Description: "Provider-specific model/engine. ElevenLabs: model ID. AWS Polly: engine (standard, neural, long-form, generative; default neural). Google Cloud: model name (e.g. gemini-2.5-pro-tts, chirp3-hd)."},
	"language": {Description: `Language code (e.g. "en-US", "pl-pl"). Required for Google Gemini TTS voices that use short names (e.g. Achernar). Auto-extracted from full voice names like en-US-Neural2-F.`},
	"prompt":   {Description: `Style/tone instruction for promptable voice models (Google Gemini TTS only). E.g. "Read aloud in a warm, welcoming tone."`},
	"volume":   {Description: "Volume adjustment in dB (-8 to 8)", Minimum: intPtr(-8), Maximum: intPtr(8), Default: 0},
	"provider": {Description: `TTS provider: "elevenlabs" (default), "aws", "google", or "deepgram"`, Enum: []string{"elevenlabs", "aws", "google", "deepgram"}},
	"api_key":  {Description: "ElevenLabs: API key override (falls back to ELEVENLABS_API_KEY env var). AWS: optional ACCESS_KEY:SECRET_KEY override (falls back to default AWS credential chain). Google Cloud: optional API key override (falls back to Application Default Credentials). Deepgram: API key override (falls back to DEEPGRAM_API_KEY env var)."},
}

// STTRequest is the request body for POST /v1/legs/{id}/stt and POST /v1/rooms/{id}/stt.
type STTRequest struct {
	Language string `json:"language"`
	Partial  bool   `json:"partial"`
	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

var sttRequestFields = map[string]FieldEnrichment{
	"language": {Description: `Language code (e.g. "en", "es")`},
	"partial":  {Description: "Emit partial (non-final) transcripts", Default: false},
	"provider": {Description: `STT provider: "elevenlabs" (default) or "deepgram"`, Enum: []string{"elevenlabs", "deepgram"}},
	"api_key":  {Description: "API key override (falls back to ELEVENLABS_API_KEY or DEEPGRAM_API_KEY env var depending on provider)"},
}

// RecordRequest is the request body for POST /v1/legs/{id}/record and POST /v1/rooms/{id}/record.
type RecordRequest struct {
	Storage             string `json:"storage"`
	MultiChannel        bool   `json:"multi_channel"`
	S3Bucket            string `json:"s3_bucket"`
	S3Region            string `json:"s3_region"`
	S3Endpoint          string `json:"s3_endpoint"`
	S3Prefix            string `json:"s3_prefix"`
	S3AccessKey         string `json:"s3_access_key"`
	S3SecretKey         string `json:"s3_secret_key"`
	GCSBucket           string `json:"gcs_bucket"`
	GCSObjectNamePrefix string `json:"gcs_object_name_prefix"`
	Filename            string `json:"filename"`
}

var recordRequestFields = map[string]FieldEnrichment{
	"storage":                {Description: `"file" (default) — local disk, "s3" — upload to S3 after recording stops, "gcs" — upload to Google Cloud Storage via the native GCS API (Application Default Credentials / Workload Identity)`, Enum: []string{"file", "s3", "gcs"}},
	"multi_channel":          {Description: "When true, record each participant to a separate mono WAV file in addition to the full mix. Only applies to room recordings.", Default: false},
	"s3_bucket":              {Description: "S3 bucket name. Overrides S3_BUCKET env var. Required if env var is not set."},
	"s3_region":              {Description: "AWS region. Overrides S3_REGION env var. Default us-east-1."},
	"s3_endpoint":            {Description: "Custom S3 endpoint (MinIO, etc.). Overrides S3_ENDPOINT env var."},
	"s3_prefix":              {Description: "Key prefix (e.g. recordings/). Overrides S3_PREFIX env var."},
	"s3_access_key":          {Description: "AWS access key ID. Overrides default credential chain."},
	"s3_secret_key":          {Description: "AWS secret access key. Must be set together with s3_access_key."},
	"gcs_bucket":             {Description: "GCS bucket name. Overrides GCS_BUCKET env var. Required if env var is not set when storage=gcs."},
	"gcs_object_name_prefix": {Description: "Object name prefix (e.g. recordings or recordings/). Overrides GCS_OBJECT_NAME_PREFIX env var. A trailing slash is added automatically when missing."},
	"filename":               {Description: "Optional output basename for the WAV file. A .wav suffix is added when missing. Must be a single path segment (no directories). Dots inside the name are preserved (only a trailing .wav is treated as the extension). Rejected with 409 if the file already exists or another recording is using the same name. When omitted, a timestamped name is generated."},
}

// ElevenLabsAgentRequest is the request body for POST /v1/legs/{id}/agent/elevenlabs
// and POST /v1/rooms/{id}/agent/elevenlabs.
type ElevenLabsAgentRequest struct {
	AgentID          string            `json:"agent_id"`
	FirstMessage     string            `json:"first_message,omitempty"`
	Language         string            `json:"language,omitempty"`
	DynamicVariables map[string]string `json:"dynamic_variables,omitempty"`
	APIKey           string            `json:"api_key,omitempty"`
}

var elevenLabsAgentRequestFields = map[string]FieldEnrichment{
	"agent_id":          {Description: "ElevenLabs agent ID"},
	"first_message":     {Description: "Override the agent's first message"},
	"language":          {Description: `Language code (e.g. "en", "es")`},
	"dynamic_variables": {Description: "Key-value pairs passed to the agent as dynamic variables"},
	"api_key":           {Description: "API key override (falls back to ELEVENLABS_API_KEY env var)"},
}

// VAPIAgentRequest is the request body for POST /v1/legs/{id}/agent/vapi
// and POST /v1/rooms/{id}/agent/vapi.
type VAPIAgentRequest struct {
	AssistantID    string            `json:"assistant_id"`
	FirstMessage   string            `json:"first_message,omitempty"`
	VariableValues map[string]string `json:"variable_values,omitempty"`
	APIKey         string            `json:"api_key,omitempty"`
}

var vapiAgentRequestFields = map[string]FieldEnrichment{
	"assistant_id":    {Description: "VAPI assistant ID"},
	"first_message":   {Description: "Override the agent's first message"},
	"variable_values": {Description: "Key-value pairs passed as VAPI variable values (assistantOverrides.variableValues)"},
	"api_key":         {Description: "API key override (falls back to VAPI_API_KEY env var)"},
}

// PipecatAgentRequest is the request body for POST /v1/legs/{id}/agent/pipecat
// and POST /v1/rooms/{id}/agent/pipecat.
type PipecatAgentRequest struct {
	WebsocketURL string `json:"websocket_url"`
}

var pipecatAgentRequestFields = map[string]FieldEnrichment{
	"websocket_url": {Description: "WebSocket URL of the Pipecat bot (e.g. ws://my-bot:8765)", Format: "uri"},
}

// DeepgramAgentRequest is the request body for POST /v1/legs/{id}/agent/deepgram
// and POST /v1/rooms/{id}/agent/deepgram.
type DeepgramAgentRequest struct {
	Settings map[string]interface{} `json:"settings,omitempty"`
	Greeting string                 `json:"greeting,omitempty"`
	Language string                 `json:"language,omitempty"`
	APIKey   string                 `json:"api_key,omitempty"`
}

var deepgramAgentRequestFields = map[string]FieldEnrichment{
	"settings": {Description: "Full Deepgram agent settings object (agent.listen, agent.think, agent.speak, etc.). When omitted, sensible defaults are used (nova-3 STT, gpt-4o-mini LLM, aura-2-asteria-en TTS)."},
	"greeting": {Description: "Agent greeting message"},
	"language": {Description: `Language code (e.g. "en", "es")`},
	"api_key":  {Description: "API key override (falls back to DEEPGRAM_API_KEY env var)"},
}

// AgentMessageRequest is the request body for POST /v1/legs/{id}/agent/message
// and POST /v1/rooms/{id}/agent/message.
type AgentMessageRequest struct {
	Message string `json:"message"`
}

var agentMessageRequestFields = map[string]FieldEnrichment{
	"message": {Description: "Context or instruction to inject into the running agent session"},
}

// WebRTCOfferRequest is the request body for POST /v1/webrtc/offer.
type WebRTCOfferRequest struct {
	SDP   string `json:"sdp"`
	AppID string `json:"app_id,omitempty"`
}

var webRTCOfferRequestFields = map[string]FieldEnrichment{
	"sdp":    {Description: "SDP offer from the browser"},
	"app_id": {Description: "Application identifier. Carried through to all events emitted for this leg, and matched against the VSI `app_id` filter."},
}

// CreateTrunkRequest is the request body for POST /v1/sip/trunks. The shape
// is typed by `type`; today only "sip_register" is implemented. "ip_ip"
// (static-IP peering, no REGISTER) is reserved and rejected with 501.
type CreateTrunkRequest struct {
	Type        string                `json:"type"`
	AppID       string                `json:"app_id,omitempty"`
	SIPRegister *SIPRegisterTrunkSpec `json:"sip_register,omitempty"`
	IPIP        *IPIPTrunkSpec        `json:"ip_ip,omitempty"`
}

// TrunkTypeEnum lists every trunk type understood by the request schema.
// "ip_ip" is reserved (handler returns 501); "sip_register" is implemented.
var TrunkTypeEnum = []string{"sip_register", "ip_ip"}

var createTrunkRequestFields = map[string]FieldEnrichment{
	"type":         {Description: "Trunk type discriminator. Only `sip_register` is implemented today; `ip_ip` is reserved and returns 501.", Enum: TrunkTypeEnum},
	"app_id":       {Description: "Application identifier carried through to every event emitted by this trunk."},
	"sip_register": {Description: "Required when type == \"sip_register\". Configures the outbound REGISTER (registrar URI, AOR, digest credentials, expiry)."},
	"ip_ip":        {Description: "Reserved for static-IP peering (no REGISTER). Not yet implemented; supplying this returns 501."},
}

// SIPRegisterTrunkSpec is the per-type body for sip_register trunks. The
// password is required on creation but never returned in any response.
type SIPRegisterTrunkSpec struct {
	RegistrarURI   string `json:"registrar_uri"`
	AOR            string `json:"aor"`
	Username       string `json:"username,omitempty"`
	Password       string `json:"password"`
	ContactUser    string `json:"contact_user,omitempty"`
	ExpiresSeconds int    `json:"expires_seconds,omitempty"`
}

var sipRegisterTrunkSpecFields = map[string]FieldEnrichment{
	"registrar_uri":   {Description: "Upstream registrar SIP URI (e.g. \"sip:pbx.example.com:5060\" or \"sips:pbx.example.com:5061;transport=tls\")."},
	"aor":             {Description: "Address-of-record this trunk REGISTERs (e.g. \"sip:alice@pbx.example.com\"). Becomes the From URI on outbound REGISTER, and the From / P-Asserted-Identity host on outbound INVITEs placed `from` this AOR."},
	"username":        {Description: "Digest auth username. Defaults to the AOR user-part when empty."},
	"password":        {Description: "Digest auth password. Required. Never returned in any response."},
	"contact_user":    {Description: "Override the user-part of the Contact header sent in REGISTER. Defaults to the AOR user-part."},
	"expires_seconds": {Description: "Requested registration lifetime in seconds. Clamped to [SIP_OUTBOUND_REGISTRATION_MIN_EXPIRES_SECONDS, SIP_OUTBOUND_REGISTRATION_MAX_EXPIRES_SECONDS]. Default: SIP_OUTBOUND_REGISTRATION_DEFAULT_EXPIRES_SECONDS (3600)."},
}

// IPIPTrunkSpec is the placeholder shape for the unimplemented ip_ip type.
type IPIPTrunkSpec struct {
	PeerURI string `json:"peer_uri,omitempty"`
}

var ipipTrunkSpecFields = map[string]FieldEnrichment{
	"peer_uri": {Description: "Static peer SIP URI for IP-IP peering. Reserved; not yet implemented."},
}

// SchemaEnrichments maps "TypeName.json_field_name" → enrichment metadata.
// These are the descriptions, enums, formats, defaults, and constraints
// that cannot be derived from Go struct definitions alone.
func SchemaEnrichments() map[string]FieldEnrichment {
	all := make(map[string]FieldEnrichment)
	collect := func(typeName string, fields map[string]FieldEnrichment) {
		for k, v := range fields {
			all[typeName+"."+k] = v
		}
	}
	collect("LegView", legViewFields)
	collect("RoomView", roomViewFields)
	collect("CreateLegRequest", createLegRequestFields)
	collect("AnswerLegRequest", answerLegRequestFields)
	collect("EarlyMediaLegRequest", earlyMediaLegRequestFields)
	collect("DeleteLegRequest", deleteLegRequestFields)
	collect("SIPAuth", sipAuthFields)
	collect("AMDParams", amdParamsFields)
	collect("LiveKitParams", liveKitParamsFields)
	collect("LiveKitPermissions", liveKitPermissionsFields)
	collect("TransferRequest", transferRequestFields)
	collect("CreateRoomRequest", createRoomRequestFields)
	collect("AddLegRequest", addLegRequestFields)
	collect("CreateRoomBridgeRequest", createRoomBridgeRequestFields)
	collect("UpdateRoomBridgeRequest", updateRoomBridgeRequestFields)
	collect("BridgeView", bridgeViewFields)
	collect("PlaybackRequest", playbackRequestFields)
	collect("VolumeRequest", volumeRequestFields)
	collect("DTMFRequest", dtmfRequestFields)
	collect("RTTRequest", rttRequestFields)
	collect("TTSRequest", ttsRequestFields)
	collect("STTRequest", sttRequestFields)
	collect("RecordRequest", recordRequestFields)
	collect("ElevenLabsAgentRequest", elevenLabsAgentRequestFields)
	collect("VAPIAgentRequest", vapiAgentRequestFields)
	collect("PipecatAgentRequest", pipecatAgentRequestFields)
	collect("DeepgramAgentRequest", deepgramAgentRequestFields)
	collect("AgentMessageRequest", agentMessageRequestFields)
	collect("WebRTCOfferRequest", webRTCOfferRequestFields)
	collect("SetLegRoleRequest", setLegRoleRequestFields)
	collect("LegStreamView", legStreamViewFields)
	collect("CreateLegStream", createLegStreamFields)
	collect("AnswerLegStream", answerLegStreamFields)
	collect("UpdateLegStreamRequest", updateLegStreamRequestFields)
	collect("AddRoomStream", addRoomStreamFields)
	collect("AddLegStreamRequest", addLegStreamRequestFields)
	collect("AttachStreamRoomRequest", attachStreamRoomRequestFields)
	collect("RoomRoutingRequest", roomRoutingRequestFields)
	collect("RoomRoutingUpdateRequest", roomRoutingUpdateRequestFields)
	collect("RoutingRowUpdate", routingRowUpdateFields)
	collect("RoomRoutingView", roomRoutingViewFields)
	collect("CreateTrunkRequest", createTrunkRequestFields)
	collect("SIPRegisterTrunkSpec", sipRegisterTrunkSpecFields)
	collect("IPIPTrunkSpec", ipipTrunkSpecFields)
	return all
}
