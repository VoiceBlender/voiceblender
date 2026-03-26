# VoiceBlender API Reference

Base URL: `http://localhost:8080/v1`

All responses are `Content-Type: application/json`.

---

## Legs

A **leg** represents one side of a voice call — either a SIP dialog or a WebRTC peer connection.

### Leg Object

```json
{
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "sip_inbound",
  "state": "connected",
  "room_id": "room-123",
  "muted": false,
  "held": false,
  "sip_headers": {
    "X-Correlation-ID": "abc-123"
  }
}
```

| Field | Type | Values |
|-------|------|--------|
| `leg_id` | string | UUID |
| `type` | string | `sip_inbound`, `sip_outbound`, `webrtc` |
| `state` | string | `ringing`, `early_media`, `connected`, `held`, `hung_up` |
| `room_id` | string | Room ID if assigned, empty otherwise |
| `muted` | boolean | `true` if the leg is muted |
| `held` | boolean | `true` if the call is on hold (SIP legs only) |
| `sip_headers` | object | `X-*` headers from the inbound INVITE. Only present on `sip_inbound` legs. |

---

### POST /v1/legs

Originate an outbound SIP call.

**Request:**

```json
{
  "type": "sip",
  "uri": "sip:alice@192.168.1.100:5060",
  "from": "+15551234567",
  "privacy": "id",
  "ring_timeout": 30,
  "max_duration": 3600,
  "codecs": ["PCMU", "PCMA", "G722", "opus"],
  "headers": {
    "X-Correlation-ID": "abc-123",
    "X-Account-ID": "acct-456"
  },
  "auth": {
    "username": "trunk-user",
    "password": "trunk-pass"
  },
  "room_id": "room-123"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | yes | `"sip"` |
| `uri` | string | yes | SIP URI to dial |
| `from` | string | no | Caller ID — sets the user part of the SIP From header (e.g. `"+15551234567"`, `"alice"`) |
| `privacy` | string | no | SIP Privacy header value (e.g. `"id"`, `"none"`) |
| `ring_timeout` | integer | no | Seconds to wait for answer; 0 = no timeout |
| `max_duration` | integer | no | Maximum call duration in seconds after connect. The call is automatically hung up when reached. 0 or omitted = no limit. |
| `codecs` | string[] | no | Codec preference order. Supported: `PCMU`, `PCMA`, `G722`, `opus`. Defaults to engine config. |
| `headers` | object | no | Custom SIP headers to include in the outbound INVITE (e.g. `X-Correlation-ID`). Keys are header names, values are header values. |
| `auth` | object | no | SIP digest authentication credentials. If the remote challenges with 401/407, sipgo will retry with these credentials. Contains `username` (string) and `password` (string). |
| `room_id` | string | no | Room ID to auto-add the leg to once media is ready. The leg joins the room on `early_media` (183+SDP) or `connected` (200 OK), whichever comes first. If the room does not exist, it is automatically created. |

**Response:** `201 Created` — Leg object (initially in `ringing` state)

**Early Media:** When the remote sends a 183 Session Progress response with SDP, the leg automatically transitions to `early_media` state and a `leg.early_media` webhook event is emitted. The RTP media pipeline starts immediately, allowing the leg to be added to a room so other participants can hear the remote's early media (custom ringback, IVR prompts, etc.). When the remote answers (200 OK), the leg transitions to `connected` as normal.

**Errors:**
- `400` — Invalid JSON, bad SIP URI, unknown codec, or unsupported type

---

### GET /v1/legs

List all active legs.

**Response:** `200 OK` — Array of Leg objects

---

### GET /v1/legs/{id}

Get a single leg.

**Response:** `200 OK` — Leg object

**Errors:** `404` — Leg not found

---

### POST /v1/legs/{id}/answer

Answer a ringing or early-media inbound SIP leg. This triggers the SIP 200 OK. If the leg is in `early_media` state, the existing media pipeline and SDP are reused; if in `ringing` state, a new RTP session and codec negotiation are performed.

**Request:** Empty body

**Response:** `200 OK`

```json
{ "status": "answering" }
```

**Errors:**
- `400` — Not a SIP inbound leg
- `404` — Leg not found
- `409` — Leg is not in `ringing` or `early_media` state

---

### POST /v1/legs/{id}/early-media

Enable early media on a ringing inbound SIP leg. Sends SIP 183 Session Progress with SDP, sets up the RTP session and media pipeline, and transitions the leg to `early_media` state. Once in this state, audio can be played to the caller (e.g., custom ringback tones, announcements) and the leg can be added to a room — all before answering the call.

**Request:** Empty body

**Response:** `200 OK`

```json
{ "status": "early_media" }
```

**Errors:**
- `400` — Not a SIP inbound leg
- `404` — Leg not found
- `409` — Leg is not in `ringing` state
- `500` — Media setup failed (codec negotiation, RTP session, or SIP 183 send error)

---

### POST /v1/legs/{id}/mute

Mute a leg. A muted leg's audio is excluded from the room mix and speaking events are suppressed. Taps (recording/STT) still receive the muted leg's own audio.

**Request:** Empty body

**Response:** `200 OK`

```json
{ "status": "muted" }
```

**Errors:** `404` — Leg not found

---

### DELETE /v1/legs/{id}/mute

Unmute a leg.

**Response:** `200 OK`

```json
{ "status": "unmuted" }
```

**Errors:** `404` — Leg not found

---

### POST /v1/legs/{id}/hold

Put a SIP call on hold. Sends a re-INVITE with `sendonly` SDP direction. The RTP timeout is paused while held, and a 2-hour auto-hangup timer starts.

**Response:** `200 OK`

```json
{ "status": "held" }
```

**Errors:**
- `404` — Leg not found
- `400` — Not a SIP leg
- `409` — Leg is not in `connected` state, or already held

---

### DELETE /v1/legs/{id}/hold

Resume a held SIP call. Sends a re-INVITE with `sendrecv` SDP direction.

**Response:** `200 OK`

```json
{ "status": "resumed" }
```

**Errors:**
- `404` — Leg not found
- `400` — Not a SIP leg
- `409` — Leg is not held

---

### DELETE /v1/legs/{id}

Hang up a leg. Sends SIP BYE or closes the WebRTC connection.

**Response:** `200 OK`

```json
{ "status": "hung_up" }
```

**Errors:** `404` — Leg not found

---

### POST /v1/legs/{id}/dtmf

Send DTMF digits on a leg (RFC 4733 telephone-event).

**Request:**

```json
{ "digits": "123#" }
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `digits` | string | yes | Digits to send (`0-9`, `*`, `#`) |

**Response:** `200 OK`

```json
{ "status": "sent" }
```

**Errors:**
- `400` — Invalid JSON or empty digits
- `404` — Leg not found
- `500` — DTMF writer unavailable

---

### POST /v1/legs/{id}/play

Start audio playback to a leg. Fetches audio from a URL or generates a built-in telephone tone.

**Request (URL):**

```json
{
  "url": "https://example.com/greeting.wav",
  "mime_type": "audio/wav"
}
```

**Request (tone):**

```json
{
  "tone": "us_ringback"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `url` | string | one of `url` or `tone` | URL of the audio file |
| `tone` | string | one of `url` or `tone` | Built-in telephone tone name (see below) |
| `mime_type` | string | with `url` | MIME type (`audio/wav`) |
| `repeat` | integer | no | Repeat count (0/1=once, -1=infinite) |
| `volume` | integer | no | Volume adjustment (-8 to 8, ~3dB/step) |

`url` and `tone` are mutually exclusive — provide exactly one.

**Tone names:** Format is `{country}_{type}` or bare `{type}` (defaults to US).
- Types: `ringback`, `busy`, `dial`, `congestion`
- Countries: `us`, `gb`, `de`, `fr`, `au`, `jp`, `it`, `in`, `br`, `pl`, `ru`
- Examples: `us_ringback`, `gb_busy`, `dial` (= `us_dial`)

Tones play indefinitely until stopped via `DELETE /v1/legs/{id}/play/{playbackID}`.

**Response:** `200 OK`

```json
{ "playback_id": "pb-a1b2c3d4", "status": "playing" }
```

Playback runs asynchronously. Events `playback.started` and `playback.finished` are emitted.

**Errors:**
- `400` — Invalid JSON, missing url/tone, both url and tone provided
- `404` — Leg not found
- `409` — Leg has no audio writer (not yet connected)

---

### DELETE /v1/legs/{id}/play/{playbackID}

Stop audio playback on a leg.

**Response:** `200 OK`

```json
{ "status": "stopped" }
```

**Errors:** `404` — No playback in progress

---

### POST /v1/legs/{id}/tts

Synthesize speech and play it on a leg.

**Request:**

```json
{
  "text": "Hello, how can I help you?",
  "voice": "Rachel",
  "provider": "elevenlabs",
  "model_id": "eleven_multilingual_v2",
  "volume": 0
}
```

**Request (Google Gemini TTS):**

```json
{
  "text": "Movies, oh my gosh, I just love them.",
  "voice": "Achernar",
  "provider": "google",
  "model_id": "gemini-2.5-pro-tts",
  "language": "en-US",
  "prompt": "Read aloud in a warm, welcoming tone."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `text` | string | yes | Text to synthesize |
| `voice` | string | yes | Provider-specific voice identifier. ElevenLabs: voice name or ID. AWS Polly: voice ID (e.g. `Joanna`, `Matthew`). Google Cloud: voice name — either full format (e.g. `en-US-Neural2-F`) or short name for Gemini models (e.g. `Achernar`, `Kore`). |
| `provider` | string | no | TTS provider: `"elevenlabs"` (default), `"aws"`, or `"google"` |
| `model_id` | string | no | Provider-specific model/engine. ElevenLabs: model ID. AWS Polly: engine (`standard`, `neural`, `long-form`, `generative`; default `neural`). Google Cloud: model name (e.g. `gemini-2.5-pro-tts`, `chirp3-hd`). |
| `language` | string | no | Language code (e.g. `"en-US"`, `"pl-pl"`). Required for Google Gemini TTS voices that use short names (e.g. `Achernar`). Auto-extracted from full voice names like `en-US-Neural2-F`. |
| `prompt` | string | no | Style/tone instruction for promptable voice models (Google Gemini TTS only). E.g. `"Read aloud in a warm, welcoming tone."` |
| `volume` | integer | no | Volume adjustment in dB (`-8` to `8`, default `0`) |
| `api_key` | string | no | ElevenLabs: API key override (falls back to `ELEVENLABS_API_KEY` env var). AWS: optional `ACCESS_KEY:SECRET_KEY` override (falls back to default AWS credential chain). Google Cloud: optional API key override (falls back to Application Default Credentials). |

**Providers:**
- `elevenlabs` — ElevenLabs streaming TTS API (default). Requires an API key.
- `aws` — Amazon Polly. Uses the default AWS credential chain (env vars, IAM role, shared credentials file). No API key required unless overriding credentials per-request.
- `google` — Google Cloud Text-to-Speech. Uses Application Default Credentials (ADC). No API key required unless overriding per-request. Supports all voice types: Standard, WaveNet, Neural2, Studio, Chirp 3 HD, and Gemini TTS. For Gemini models (e.g. `gemini-2.5-pro-tts`), set `model_id` and `language` explicitly; use `prompt` for style instructions.

**Response:** `200 OK`

```json
{ "playback_id": "pb-a1b2c3d4", "status": "playing" }
```

**Errors:**
- `400` — Invalid JSON, missing text/voice, volume out of range
- `404` — Leg not found
- `409` — Leg has no audio writer
- `503` — No API key provided for the selected provider

---

### POST /v1/legs/{id}/record

Start recording a leg to a WAV file.

For SIP legs, recording is **stereo** (16-bit PCM at the codec's native sample rate):
- **Left channel** — incoming audio (what the remote party says)
- **Right channel** — outgoing audio (what we send, including agent TTS)

For legs in a room, recording is stereo at 16kHz:
- **Left channel** — participant's incoming audio (before mix)
- **Right channel** — mixed-minus-self (what the participant hears)

**Request:**

```json
{
  "storage": "s3",
  "s3_bucket": "my-recordings",
  "s3_region": "eu-west-1",
  "s3_endpoint": "https://s3.example.com",
  "s3_prefix": "calls/",
  "s3_access_key": "AKIA...",
  "s3_secret_key": "wJalr..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `storage` | string | no | `"file"` (default) — local disk, `"s3"` — upload to S3 after recording stops |
| `s3_bucket` | string | no | S3 bucket name. Overrides `S3_BUCKET` env var. Required if env var is not set. |
| `s3_region` | string | no | AWS region. Overrides `S3_REGION` env var. Default `us-east-1`. |
| `s3_endpoint` | string | no | Custom S3 endpoint (MinIO, etc.). Overrides `S3_ENDPOINT` env var. |
| `s3_prefix` | string | no | Key prefix (e.g. `recordings/`). Overrides `S3_PREFIX` env var. |
| `s3_access_key` | string | no | AWS access key ID. Overrides default credential chain. |
| `s3_secret_key` | string | no | AWS secret access key. Must be set together with `s3_access_key`. |

When `s3_bucket` is provided, a per-request S3 backend is created using the supplied config. Otherwise the server-level S3 backend (from env vars) is used.

**Response:** `200 OK`

```json
{
  "status": "recording",
  "file": "/tmp/recordings/20260301_110500_a1b2c3d4.wav"
}
```

Recording runs asynchronously. Events `recording.started` and `recording.finished` are emitted. When `storage=s3`, the `file` field in the stop response and the `recording.finished` event will contain an `s3://bucket/key` URI.

**Errors:**
- `400` — Invalid storage type, S3 not configured, or invalid S3 credentials
- `404` — Leg not found
- `409` — Leg has no audio reader
- `500` — Failed to create recording file

---

### DELETE /v1/legs/{id}/record

Stop recording a leg.

**Response:** `200 OK`

```json
{
  "status": "stopped",
  "file": "/tmp/recordings/20260301_110500_a1b2c3d4.wav"
}
```

**Errors:** `404` — No recording in progress

---

### POST /v1/legs/{id}/stt

Start real-time speech-to-text transcription on a leg using ElevenLabs STT.

**Request:**

```json
{
  "language": "en",
  "partial": true
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `language` | string | no | Language code (e.g. `"en"`, `"es"`) |
| `partial` | boolean | no | Emit partial (non-final) transcripts |
| `api_key` | string | no | ElevenLabs API key override (falls back to `ELEVENLABS_API_KEY` env var) |

**Response:** `200 OK`

```json
{ "status": "stt_started", "leg_id": "550e8400-..." }
```

Transcripts are delivered via `stt.text` webhook events.

**Errors:**
- `404` — Leg not found
- `409` — Leg not connected, STT already running, or leg has no audio reader
- `503` — No ElevenLabs API key provided

---

### DELETE /v1/legs/{id}/stt

Stop speech-to-text on a leg.

**Response:** `200 OK`

```json
{ "status": "stt_stopped" }
```

**Errors:** `404` — No STT in progress

---

### POST /v1/legs/{id}/agent

Attach a conversational AI agent to a leg. The agent hears the leg's audio and speaks back. Audio is bridged bidirectionally via the provider's WebSocket API.

**Request:**

```json
{
  "agent_id": "agent-id",
  "provider": "elevenlabs",
  "first_message": "Hello!",
  "language": "en",
  "dynamic_variables": { "name": "Alice" }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agent_id` | string | yes | Provider-specific agent/assistant ID. For Pipecat, this is the WebSocket URL of the bot (e.g. `ws://my-bot:8765`). |
| `provider` | string | no | Agent provider: `"elevenlabs"` (default), `"vapi"`, or `"pipecat"` |
| `first_message` | string | no | Override the agent's first message |
| `language` | string | no | Language code (ElevenLabs only) |
| `dynamic_variables` | object | no | Key-value pairs passed to the agent |
| `api_key` | string | no | API key override (falls back to `ELEVENLABS_API_KEY` or `VAPI_API_KEY` env var depending on provider). Not required for Pipecat. |

**Providers:**
- `elevenlabs` — ElevenLabs ConvAI WebSocket API (default). Uses `agent_id` as the ElevenLabs agent ID.
- `vapi` — VAPI conversational AI platform. Uses `agent_id` as the VAPI assistant ID. Creates a WebSocket call via `POST https://api.vapi.ai/call`.
- `pipecat` — Self-hosted [Pipecat](https://pipecat.ai) bot. Uses `agent_id` as the WebSocket URL of the running bot. Audio is exchanged as protobuf-encoded binary frames (16kHz 16-bit PCM mono). No API key required.

**Standalone leg:** Agent reads/writes audio directly with resampling to 16kHz.

**Leg in a room:** Agent hears only that leg (via mixer tap) and speaks to everyone (via playback source).

**Response:** `200 OK`

```json
{ "status": "agent_started", "leg_id": "550e8400-..." }
```

Agent events (`agent.connected`, `agent.disconnected`, `agent.user_transcript`, `agent.agent_response`) are delivered via webhooks.

**Errors:**
- `400` — Invalid JSON or missing `agent_id`
- `404` — Leg not found
- `409` — Leg not connected, agent already attached, or leg has no audio reader/writer
- `503` — No API key provided for the selected provider

---

### DELETE /v1/legs/{id}/agent

Detach the agent from a leg.

**Response:** `200 OK`

```json
{ "status": "agent_stopped" }
```

**Errors:** `404` — No agent attached to this leg

---

## Rooms

A **room** is a multi-party audio conference. Legs added to a room hear mixed audio from all other participants (mixed-minus-self).

### Room Object

```json
{
  "id": "room-123",
  "participants": [
    { "leg_id": "leg-uuid", "type": "sip_inbound", "state": "connected", "room_id": "room-123" }
  ]
}
```

---

### POST /v1/rooms

Create a room.

**Request:**

```json
{ "id": "my-custom-room-id" }
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string | no | Custom room ID. Auto-generated UUID if omitted. |

**Response:** `201 Created` — Room object (empty participants)

**Errors:** `409` — Room ID already exists

---

### GET /v1/rooms

List all rooms with their participants.

**Response:** `200 OK` — Array of Room objects

---

### GET /v1/rooms/{id}

Get a room with its participants.

**Response:** `200 OK` — Room object

**Errors:** `404` — Room not found

---

### DELETE /v1/rooms/{id}

Delete a room. All participants are hung up.

**Response:** `200 OK`

```json
{ "status": "deleted" }
```

**Errors:** `404` — Room not found

---

### POST /v1/rooms/{id}/legs

Add a leg to a room. The leg must be in `connected` or `early_media` state. If the leg is a ringing inbound SIP leg, it is automatically answered before being added. If the room does not exist, it is automatically created.

**Request:**

```json
{ "leg_id": "550e8400-e29b-41d4-a716-446655440000" }
```

**Response:** `200 OK`

```json
{ "status": "added" }
```

**Errors:** `400` — Invalid JSON, leg not found, or leg not connected

---

### DELETE /v1/rooms/{id}/legs/{legID}

Remove a leg from a room (without hanging it up).

**Response:** `200 OK`

```json
{ "status": "removed" }
```

**Errors:** `400` — Room or leg not found

---

### POST /v1/rooms/{id}/play

Play audio to a room. Accepts a URL or a built-in telephone tone (same tone names as leg playback).

**Request (URL):**

```json
{
  "url": "https://example.com/announcement.wav",
  "mime_type": "audio/wav"
}
```

**Request (tone):**

```json
{
  "tone": "us_ringback"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `url` | string | one of `url` or `tone` | URL of the audio file |
| `tone` | string | one of `url` or `tone` | Built-in telephone tone name |
| `mime_type` | string | with `url` | MIME type (`audio/wav`) |
| `repeat` | integer | no | Repeat count (0/1=once, -1=infinite) |
| `volume` | integer | no | Volume adjustment (-8 to 8, ~3dB/step) |

**Response:** `200 OK`

```json
{ "playback_id": "pb-a1b2c3d4", "status": "playing" }
```

**Errors:**
- `400` — Invalid JSON, missing url/tone, both url and tone provided
- `404` — Room not found
- `409` — Room has no participants

---

### DELETE /v1/rooms/{id}/play/{playbackID}

Stop room playback.

**Response:** `200 OK`

```json
{ "status": "stopped" }
```

**Errors:** `404` — No playback in progress

---

### POST /v1/rooms/{id}/tts

Synthesize speech and play it into a room.

**Request:**

```json
{
  "text": "Attention please.",
  "voice": "Rachel",
  "provider": "elevenlabs",
  "model_id": "eleven_multilingual_v2",
  "volume": 0
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `text` | string | yes | Text to synthesize |
| `voice` | string | yes | Provider-specific voice identifier. ElevenLabs: voice name or ID. AWS Polly: voice ID (e.g. `Joanna`, `Matthew`). Google Cloud: voice name — either full format (e.g. `en-US-Neural2-F`) or short name for Gemini models (e.g. `Achernar`, `Kore`). |
| `provider` | string | no | TTS provider: `"elevenlabs"` (default), `"aws"`, or `"google"` |
| `model_id` | string | no | Provider-specific model/engine. ElevenLabs: model ID. AWS Polly: engine (`standard`, `neural`, `long-form`, `generative`; default `neural`). Google Cloud: model name (e.g. `gemini-2.5-pro-tts`, `chirp3-hd`). |
| `language` | string | no | Language code (e.g. `"en-US"`, `"pl-pl"`). Required for Google Gemini TTS voices that use short names. Auto-extracted from full voice names. |
| `prompt` | string | no | Style/tone instruction for promptable voice models (Google Gemini TTS only). |
| `volume` | integer | no | Volume adjustment in dB (`-8` to `8`, default `0`) |
| `api_key` | string | no | ElevenLabs: API key override (falls back to `ELEVENLABS_API_KEY` env var). AWS: optional `ACCESS_KEY:SECRET_KEY` override (falls back to default AWS credential chain). Google Cloud: optional API key override (falls back to Application Default Credentials). |

**Response:** `200 OK`

```json
{ "playback_id": "pb-a1b2c3d4", "status": "playing" }
```

**Errors:**
- `400` — Invalid JSON, missing text/voice, volume out of range
- `404` — Room not found
- `409` — Room has no participants
- `503` — No API key provided for the selected provider

---

### POST /v1/rooms/{id}/record

Start recording the full room mix to a WAV file (16kHz, 16-bit, mono).

**Request:**

```json
{
  "storage": "s3",
  "s3_bucket": "my-recordings",
  "s3_region": "eu-west-1",
  "s3_access_key": "AKIA...",
  "s3_secret_key": "wJalr..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `storage` | string | no | `"file"` (default) — local disk, `"s3"` — upload to S3 after recording stops |
| `s3_bucket` | string | no | S3 bucket name. Overrides `S3_BUCKET` env var. Required if env var is not set. |
| `s3_region` | string | no | AWS region. Overrides `S3_REGION` env var. Default `us-east-1`. |
| `s3_endpoint` | string | no | Custom S3 endpoint (MinIO, etc.). Overrides `S3_ENDPOINT` env var. |
| `s3_prefix` | string | no | Key prefix (e.g. `recordings/`). Overrides `S3_PREFIX` env var. |
| `s3_access_key` | string | no | AWS access key ID. Overrides default credential chain. |
| `s3_secret_key` | string | no | AWS secret access key. Must be set together with `s3_access_key`. |

When `s3_bucket` is provided, a per-request S3 backend is created. Otherwise the server-level S3 backend (from env vars) is used.

**Response:** `200 OK`

```json
{
  "status": "recording",
  "file": "/tmp/recordings/20260301_110500_a1b2c3d4.wav"
}
```

When `storage=s3`, the `file` field in the stop response and the `recording.finished` event will contain an `s3://bucket/key` URI.

**Errors:**
- `400` — Invalid storage type, S3 not configured, or invalid S3 credentials
- `404` — Room not found
- `409` — Room has no participants
- `500` — Failed to create recording file

---

### DELETE /v1/rooms/{id}/record

Stop room recording.

**Response:** `200 OK`

```json
{
  "status": "stopped",
  "file": "/tmp/recordings/20260301_110500_a1b2c3d4.wav"
}
```

**Errors:** `404` — No recording in progress

---

### POST /v1/rooms/{id}/stt

Start real-time speech-to-text on all participants in a room.

**Request:**

```json
{
  "language": "en",
  "partial": true
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `language` | string | no | Language code |
| `partial` | boolean | no | Emit partial (non-final) transcripts |
| `api_key` | string | no | ElevenLabs API key override (falls back to `ELEVENLABS_API_KEY` env var) |

**Response:** `200 OK`

```json
{ "status": "stt_started", "room_id": "room-123", "leg_ids": ["leg-1", "leg-2"] }
```

Transcripts are delivered via `stt.text` webhook events.

**Errors:**
- `404` — Room not found
- `409` — STT already running on this room, or room has no participants
- `503` — No ElevenLabs API key provided

---

### DELETE /v1/rooms/{id}/stt

Stop speech-to-text on a room.

**Response:** `200 OK`

```json
{ "status": "stt_stopped" }
```

**Errors:** `404` — No STT in progress

---

### POST /v1/rooms/{id}/agent

Attach a conversational AI agent to a room as a virtual participant. The agent hears all participants (mixed-minus-self) and speaks to everyone.

**Request:**

```json
{
  "agent_id": "agent-id",
  "provider": "elevenlabs",
  "first_message": "Hello everyone!",
  "language": "en",
  "dynamic_variables": { "topic": "meeting" }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agent_id` | string | yes | Provider-specific agent/assistant ID. For Pipecat, this is the WebSocket URL of the bot. |
| `provider` | string | no | Agent provider: `"elevenlabs"` (default), `"vapi"`, or `"pipecat"` |
| `first_message` | string | no | Override the agent's first message |
| `language` | string | no | Language code (ElevenLabs only) |
| `dynamic_variables` | object | no | Key-value pairs passed to the agent |
| `api_key` | string | no | API key override (falls back to `ELEVENLABS_API_KEY` or `VAPI_API_KEY` env var depending on provider). Not required for Pipecat. |

**Response:** `200 OK`

```json
{ "status": "agent_started", "room_id": "room-123" }
```

**Errors:**
- `400` — Invalid JSON or missing `agent_id`
- `404` — Room not found
- `409` — Agent already attached to this room
- `503` — No API key provided for the selected provider

---

### DELETE /v1/rooms/{id}/agent

Detach the agent from a room.

**Response:** `200 OK`

```json
{ "status": "agent_stopped" }
```

**Errors:** `404` — No agent attached to this room

---

### GET /v1/rooms/{id}/ws

Upgrade to a WebSocket connection and join the room as a bidirectional audio participant. The client sends and receives 16kHz 16-bit signed little-endian PCM audio (mono), base64-encoded in JSON text frames. Each audio frame is 640 bytes (20ms).

**Upgrade:** Standard HTTP → WebSocket upgrade. No request body.

**Errors:**
- `404` — Room not found (returned before upgrade)

#### Message Format

**Server → Client (on connect):**

```json
{"type": "connected", "participant_id": "ws-a1b2c3d4", "sample_rate": 16000, "format": "pcm_s16le"}
```

**Client → Server (send audio):**

```json
{"audio": "<base64-encoded-16kHz-16bit-PCM>"}
```

**Server → Client (receive mixed audio):**

```json
{"audio": "<base64-encoded-16kHz-16bit-PCM>"}
```

**Server → Client (keepalive ping):**

```json
{"type": "ping", "event_id": 1}
```

**Client → Server (keepalive pong):**

```json
{"type": "pong", "event_id": 1}
```

**Client → Server (disconnect):**

```json
{"type": "stop"}
```

The server sends application-level pings every 30 seconds. The connection is also a full mixer participant — it receives mixed-minus-self audio from all other participants in the room.

---

## WebRTC

### POST /v1/webrtc/offer

Establish a WebRTC leg via SDP offer/answer exchange. The browser sends an SDP offer and receives an SDP answer plus a leg ID. The answer is returned immediately without waiting for ICE gathering to complete — use the trickle ICE endpoints below to exchange candidates incrementally.

**Request:**

```json
{
  "sdp": "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n..."
}
```

**Response:** `200 OK`

```json
{
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "sdp": "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n..."
}
```

The returned `leg_id` can be used with all `/v1/legs` and `/v1/rooms` endpoints.

**Errors:**
- `400` — Invalid JSON or invalid SDP offer
- `500` — Peer connection, track creation, or answer generation failed

**Audio codec:** PCMU (G.711 u-law), 8kHz, mono.

---

### POST /v1/legs/{id}/ice-candidates

Send a remote ICE candidate to the server for a WebRTC leg (trickle ICE).

**Request:**

```json
{
  "candidate": "candidate:842163049 1 udp 1677729535 ...",
  "sdpMid": "0",
  "sdpMLineIndex": 0
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `candidate` | string | yes | ICE candidate string |
| `sdpMid` | string | no | Media stream ID |
| `sdpMLineIndex` | integer | no | Media description index |

**Response:** `200 OK`

```json
{ "status": "added" }
```

**Errors:**
- `400` — Invalid JSON or leg is not a WebRTC leg
- `404` — Leg not found
- `500` — Failed to add ICE candidate

---

### GET /v1/legs/{id}/ice-candidates

Retrieve server-side ICE candidates gathered since the last call (trickle ICE). Poll this endpoint until `done` is `true` and `candidates` is empty.

**Response:** `200 OK`

```json
{
  "candidates": [
    { "candidate": "candidate:...", "sdpMid": "0", "sdpMLineIndex": 0 }
  ],
  "done": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `candidates` | array | ICE candidates gathered since last poll |
| `done` | boolean | `true` when ICE gathering is complete |

**Errors:**
- `400` — Leg is not a WebRTC leg
- `404` — Leg not found

---

## Webhooks

Register HTTP endpoints to receive real-time event notifications.

### Webhook Object

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "url": "https://example.com/webhook",
  "secret": "my-secret"
}
```

---

### POST /v1/webhooks

Register a webhook.

**Request:**

```json
{
  "url": "https://example.com/webhook",
  "secret": "optional-hmac-secret"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `url` | string | yes | Webhook delivery URL |
| `secret` | string | no | HMAC-SHA256 signing secret |

**Response:** `201 Created` — Webhook object

**Errors:** `400` — Invalid JSON or missing URL

---

### GET /v1/webhooks

List all registered webhooks.

**Response:** `200 OK` — Array of Webhook objects

---

### DELETE /v1/webhooks/{id}

Unregister a webhook.

**Response:** `200 OK`

```json
{ "status": "deleted" }
```

**Errors:** `404` — Webhook not found

---

## Webhook Events

Events are delivered as HTTP POST requests to registered webhook URLs.

### Delivery

- **Method:** POST
- **Content-Type:** `application/json`
- **Retries:** 3 attempts with exponential backoff (2s, 4s)
- **Timeout:** 10 seconds per attempt
- **Worker pool:** 10 concurrent delivery goroutines
- **Queue capacity:** 1000 events (dropped if full)

### Signature Verification

When a `secret` is configured, a `X-Signature-256` header is included:

```
X-Signature-256: sha256=<hex-encoded-hmac-sha256>
```

The signature is computed over the raw JSON request body using HMAC-SHA256 with the webhook secret as the key.

### Event Envelope

```json
{
  "type": "leg.ringing",
  "timestamp": "2026-03-01T11:05:00.123Z",
  "data": { ... }
}
```

### Event Types

| Event | Description | Data Fields |
|-------|-------------|-------------|
| `leg.ringing` | SIP call ringing | `leg_id`, `from`, `to` (inbound); `leg_id`, `uri`, `from` (outbound). `sip_headers` included when `X-*` headers are present. |
| `leg.early_media` | Outbound leg received 183 Session Progress with SDP; media pipeline active | `leg_id`, `type` |
| `leg.connected` | Leg answered/connected | `leg_id` |
| `leg.disconnected` | Leg hung up | `leg_id`, `reason`, `duration_total`, `duration_answered` |
| `leg.joined_room` | Leg added to room | `leg_id`, `room_id` |
| `leg.left_room` | Leg removed from room | `leg_id`, `room_id` |
| `leg.muted` | Leg muted | `leg_id` |
| `leg.unmuted` | Leg unmuted | `leg_id` |
| `leg.hold` | Leg put on hold (local or remote) | `leg_id`, `type` |
| `leg.unhold` | Leg taken off hold (local or remote) | `leg_id`, `type` |
| `dtmf.received` | DTMF digit received | `leg_id`, `digit` |
| `speaking.started` | Participant started speaking (room only) | `leg_id`, `room_id` |
| `speaking.stopped` | Participant stopped speaking (room only) | `leg_id`, `room_id` |

> **Note:** `speaking.started` and `speaking.stopped` events require the leg to be in a room. Voice activity detection runs inside the room mixer; standalone legs do not emit speaking events.

| `playback.started` | Playback began | `leg_id` or `room_id`, `playback_id` |
| `playback.finished` | Playback ended | `leg_id` or `room_id`, `playback_id` |
| `playback.error` | Playback failed | `leg_id` or `room_id`, `playback_id`, `error` |
| `recording.started` | Recording began | `leg_id` or `room_id`, `file` |
| `recording.finished` | Recording ended | `leg_id` or `room_id`, `file` |
| `stt.text` | Speech-to-text transcript | `leg_id`, `room_id` (if room STT), `text`, `is_final` |
| `agent.connected` | Agent connected to provider | `leg_id` or `room_id`, `conversation_id` |
| `agent.disconnected` | Agent session ended | `leg_id` or `room_id` |
| `agent.user_transcript` | User speech transcribed by agent | `leg_id` or `room_id`, `text` |
| `agent.agent_response` | Agent generated a response | `leg_id` or `room_id`, `text` |
| `room.created` | Room created | `room_id` |
| `room.deleted` | Room deleted | `room_id` |

#### `leg.disconnected` Duration Fields

| Field | Type | Description |
|-------|------|-------------|
| `duration_total` | float | Seconds from leg creation (INVITE sent/received) to disconnect |
| `duration_answered` | float | Seconds from answer (200 OK) to disconnect. `0` if the leg was never answered. |
| `reason` | string | See **Disconnect Reasons** below |

**Disconnect Reasons:**

| Reason | Description |
|--------|-------------|
| `api_hangup` | Hung up via `DELETE /v1/legs/{id}` |
| `remote_bye` | Remote party sent BYE |
| `caller_cancel` | Inbound caller hung up before answer |
| `ring_timeout` | Outbound `ring_timeout` expired before answer |
| `max_duration` | Outbound `max_duration` reached after connect |
| `busy` | Remote returned 486 Busy Here |
| `unavailable` | Remote returned 480 Temporarily Unavailable |
| `not_found` | Remote returned 404 Not Found |
| `forbidden` | Remote returned 403 Forbidden |
| `unauthorized` | Remote returned 401/407 Authentication Required |
| `timeout` | Remote returned 408 Request Timeout |
| `cancelled` | INVITE was cancelled (487 Request Terminated) |
| `not_acceptable` | Remote returned 488 Not Acceptable Here |
| `service_unavailable` | Remote returned 503 Service Unavailable |
| `declined` | Remote returned 603 Decline |
| `sip_{code}` | Other SIP failure response (e.g. `sip_500`) |
| `rtp_timeout` | No RTP packets received for 30 seconds |
| `session_expired` | SIP session timer expired without refresh (RFC 4028) |
| `invite_failed` | INVITE failed for a non-SIP reason (transport error, DNS failure, etc.) |
| `connect_failed` | Call answered but media/codec negotiation failed |
| `ice_failure` | WebRTC ICE connection failed |

---

## Error Format

All errors return:

```json
{ "error": "description of what went wrong" }
```

---

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `HTTP_ADDR` | `:8080` | REST API listen address |
| `SIP_BIND_IP` | `127.0.0.1` | IP advertised in SDP/Contact/Via headers (auto-detected if `0.0.0.0`) |
| `SIP_LISTEN_IP` | _(same as SIP_BIND_IP)_ | IP to bind the UDP socket on |
| `SIP_PORT` | `5060` | SIP listen port |
| `SIP_HOST` | `voiceblender` | SIP User-Agent name |
| `ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs (comma-separated) |
| `RECORDING_DIR` | `/tmp/recordings` | Recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `WEBHOOK_URL` | _(none)_ | Default webhook URL for inbound calls (overridden by `X-Webhook-URL` SIP header) |
| `ELEVENLABS_API_KEY` | _(none)_ | Default ElevenLabs API key for TTS, STT, and Agent features (can be overridden per-request via `api_key` in the request body) |
| `VAPI_API_KEY` | _(none)_ | Default VAPI API key for Agent features when `provider=vapi` (can be overridden per-request via `api_key` in the request body) |
| `S3_BUCKET` | _(none)_ | S3 bucket name (required for `storage=s3` recordings) |
| `S3_REGION` | `us-east-1` | AWS region for S3 |
| `S3_ENDPOINT` | _(none)_ | Custom S3 endpoint for S3-compatible stores (MinIO, etc.) |
| `S3_PREFIX` | _(none)_ | Key prefix for S3 objects (e.g. `recordings/`) |

---

## SIP Session Timers (RFC 4028)

- Accepts session timers requested by the remote UA (inbound and outbound)
- Minimum session interval: 90 seconds (`Min-SE`)
- Supports both `refresher=uac` and `refresher=uas` roles
- Re-INVITEs (including hold/unhold) reset the session timer
- Expired sessions disconnect with reason `session_expired`

---

## Typical Workflow

```
1. Register webhook
   POST /v1/webhooks  {"url": "https://my-app.com/events"}

2. Receive inbound call -> webhook: leg.ringing {leg_id, from, to}

3. Answer the call
   POST /v1/legs/{leg_id}/answer

4. Attach an AI agent to the leg
   POST /v1/legs/{leg_id}/agent  {"agent_id": "your-agent-id"}

5. Agent converses with the caller. Webhooks deliver:
   - agent.connected {leg_id, conversation_id}
   - agent.user_transcript {leg_id, text}
   - agent.agent_response {leg_id, text}

6. Or: create a room for multi-party conferencing
   POST /v1/rooms  {"id": "conference-1"}

7. Add legs to the room
   POST /v1/rooms/conference-1/legs  {"leg_id": "..."}

8. Originate a second call and add to room
   POST /v1/legs  {"type": "sip", "uri": "sip:bob@10.0.0.1", "codecs": ["PCMU"]}
   POST /v1/rooms/conference-1/legs  {"leg_id": "..."}

9. Attach a room-level agent (hears everyone, speaks to everyone)
   POST /v1/rooms/conference-1/agent  {"agent_id": "your-agent-id"}

10. Start recording
    POST /v1/rooms/conference-1/record

11. Play announcement
    POST /v1/rooms/conference-1/play  {"url": "...", "mime_type": "audio/wav"}

12. Cleanup
    DELETE /v1/rooms/conference-1
```
