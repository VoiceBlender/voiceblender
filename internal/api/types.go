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
var CodecsItemEnum = []string{"PCMU", "PCMA", "G722", "opus"}

// ── Request types ───────────────────────────────────────────────────────

// CreateLegRequest is the request body for POST /v1/legs.
type CreateLegRequest struct {
	Type          string            `json:"type"`                     // "sip" or "webrtc"
	URI           string            `json:"uri"`                      // SIP URI for outbound
	From          string            `json:"from,omitempty"`           // caller ID (user part of the SIP From header, e.g. "+15551234567")
	Privacy       string            `json:"privacy,omitempty"`        // SIP Privacy header value (e.g. "id", "none")
	RingTimeout   int               `json:"ring_timeout,omitempty"`   // seconds; 0 = no timeout
	MaxDuration   int               `json:"max_duration,omitempty"`   // seconds; 0 = no limit
	Codecs        []string          `json:"codecs,omitempty"`         // codec preference order, e.g. ["PCMU","PCMA","G722","opus"]
	Headers       map[string]string `json:"headers,omitempty"`        // custom SIP headers for outbound INVITE
	RoomID        string            `json:"room_id,omitempty"`        // add leg to this room once media is ready (early_media or connected)
	Auth          *SIPAuth          `json:"auth,omitempty"`           // SIP digest auth credentials (optional)
	WebhookURL    string            `json:"webhook_url,omitempty"`    // route events for this leg to this URL
	WebhookSecret string            `json:"webhook_secret,omitempty"` // HMAC secret for webhook signature
	AMD           *AMDParams        `json:"amd,omitempty"`            // enable answering machine detection on outbound calls
}

var createLegRequestFields = map[string]FieldEnrichment{
	"type":           {Description: "Leg type", Enum: []string{"sip"}},
	"uri":            {Description: "SIP URI to dial"},
	"from":           {Description: `Caller ID — sets the user part of the SIP From header (e.g. "+15551234567", "alice")`},
	"privacy":        {Description: `SIP Privacy header value (e.g. "id", "none")`},
	"ring_timeout":   {Description: "Seconds to wait for answer; 0 = no timeout", Default: 0},
	"max_duration":   {Description: "Maximum call duration in seconds after connect. Automatically hung up when reached. 0 or omitted = no limit.", Default: 0},
	"codecs":         {Description: "Codec preference order"},
	"headers":        {Description: "Custom SIP headers to include in the outbound INVITE (e.g. X-Correlation-ID)"},
	"room_id":        {Description: "Room ID to auto-add the leg to once media is ready (early_media or connected). If the room does not exist, it is automatically created."},
	"auth":           {Description: "SIP digest authentication credentials. If the remote challenges with 401/407, sipgo will retry with these credentials."},
	"webhook_url":    {Description: "Route all events for this leg exclusively to this URL instead of global webhooks.", Format: "uri"},
	"webhook_secret": {Description: "HMAC-SHA256 signing secret for the per-leg webhook."},
	"amd":            {Description: "Enable Answering Machine Detection on outbound calls. Include the object (even empty) to enable with defaults; omit to disable."},
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

// SIPAuth holds SIP digest authentication credentials.
type SIPAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

var sipAuthFields = map[string]FieldEnrichment{
	"username": {Description: "SIP auth username"},
	"password": {Description: "SIP auth password"},
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

var amdParamsFields = map[string]FieldEnrichment{
	"initial_silence_timeout": {Description: "Max milliseconds of silence before declaring no_speech", Default: 2500},
	"greeting_duration":       {Description: "Speech duration threshold (ms) above which answerer is classified as machine", Default: 1500},
	"after_greeting_silence":  {Description: "Silence duration (ms) after initial speech to declare human", Default: 800},
	"total_analysis_time":     {Description: "Max analysis window in milliseconds", Default: 5000},
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
	Held       bool              `json:"held"`
	SIPHeaders map[string]string `json:"sip_headers,omitempty"`
}

var legViewFields = map[string]FieldEnrichment{
	"id":          {Description: "Unique leg identifier (UUID)"},
	"type":        {Description: "Leg type", Enum: []string{"sip_inbound", "sip_outbound", "webrtc"}},
	"state":       {Description: "Leg state", Enum: []string{"ringing", "early_media", "connected", "held", "hung_up"}},
	"room_id":     {Description: "Room ID if the leg is in a room, empty otherwise"},
	"muted":       {Description: "Whether the leg is muted (cannot be heard by others)"},
	"deaf":        {Description: "Whether the leg is deaf (cannot hear others)"},
	"held":        {Description: "Whether the call is on hold (SIP legs only)"},
	"sip_headers": {Description: "X-* headers from the inbound INVITE. Only present on sip_inbound legs."},
}

// CreateRoomRequest is the request body for POST /v1/rooms.
type CreateRoomRequest struct {
	ID            string `json:"id"`
	WebhookURL    string `json:"webhook_url,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

var createRoomRequestFields = map[string]FieldEnrichment{
	"id":             {Description: "Custom room ID (auto-generated UUID if omitted)"},
	"webhook_url":    {Description: "Route all events for this room exclusively to this URL instead of global webhooks.", Format: "uri"},
	"webhook_secret": {Description: "HMAC-SHA256 signing secret for the per-room webhook."},
}

// RoomView is the JSON representation of a room.
type RoomView struct {
	ID           string    `json:"id"`
	Participants []LegView `json:"participants"`
}

var roomViewFields = map[string]FieldEnrichment{
	"id":           {Description: "Room identifier"},
	"participants": {Description: "Legs currently in this room"},
}

// AddLegRequest is the request body for POST /v1/rooms/{id}/legs.
type AddLegRequest struct {
	LegID string `json:"leg_id"`
	Mute  *bool  `json:"mute,omitempty"`
	Deaf  *bool  `json:"deaf,omitempty"`
}

var addLegRequestFields = map[string]FieldEnrichment{
	"leg_id": {Description: "ID of the leg to add"},
	"mute":   {Description: "If set, apply this mute state to the leg atomically before it joins the mixer (no race where un-muted audio enters the mix). Omit to leave current state untouched (useful when moving between rooms)."},
	"deaf":   {Description: "If set, apply this deaf state to the leg atomically before it joins the mixer. Omit to leave current state untouched."},
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
	Storage      string `json:"storage"`
	MultiChannel bool   `json:"multi_channel"`
	S3Bucket     string `json:"s3_bucket"`
	S3Region     string `json:"s3_region"`
	S3Endpoint   string `json:"s3_endpoint"`
	S3Prefix     string `json:"s3_prefix"`
	S3AccessKey  string `json:"s3_access_key"`
	S3SecretKey  string `json:"s3_secret_key"`
}

var recordRequestFields = map[string]FieldEnrichment{
	"storage":       {Description: `"file" (default) — local disk, "s3" — upload to S3 after recording stops`, Enum: []string{"file", "s3"}},
	"multi_channel": {Description: "When true, record each participant to a separate mono WAV file in addition to the full mix. Only applies to room recordings.", Default: false},
	"s3_bucket":     {Description: "S3 bucket name. Overrides S3_BUCKET env var. Required if env var is not set."},
	"s3_region":     {Description: "AWS region. Overrides S3_REGION env var. Default us-east-1."},
	"s3_endpoint":   {Description: "Custom S3 endpoint (MinIO, etc.). Overrides S3_ENDPOINT env var."},
	"s3_prefix":     {Description: "Key prefix (e.g. recordings/). Overrides S3_PREFIX env var."},
	"s3_access_key": {Description: "AWS access key ID. Overrides default credential chain."},
	"s3_secret_key": {Description: "AWS secret access key. Must be set together with s3_access_key."},
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
	SDP string `json:"sdp"`
}

var webRTCOfferRequestFields = map[string]FieldEnrichment{
	"sdp": {Description: "SDP offer from the browser"},
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
	collect("SIPAuth", sipAuthFields)
	collect("AMDParams", amdParamsFields)
	collect("TransferRequest", transferRequestFields)
	collect("CreateRoomRequest", createRoomRequestFields)
	collect("AddLegRequest", addLegRequestFields)
	collect("PlaybackRequest", playbackRequestFields)
	collect("VolumeRequest", volumeRequestFields)
	collect("DTMFRequest", dtmfRequestFields)
	collect("TTSRequest", ttsRequestFields)
	collect("STTRequest", sttRequestFields)
	collect("RecordRequest", recordRequestFields)
	collect("ElevenLabsAgentRequest", elevenLabsAgentRequestFields)
	collect("VAPIAgentRequest", vapiAgentRequestFields)
	collect("PipecatAgentRequest", pipecatAgentRequestFields)
	collect("DeepgramAgentRequest", deepgramAgentRequestFields)
	collect("AgentMessageRequest", agentMessageRequestFields)
	collect("WebRTCOfferRequest", webRTCOfferRequestFields)
	return all
}
