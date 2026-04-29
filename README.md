# VoiceBlender

A Go service that bridges SIP and WebRTC voice calls with multi-party audio mixing, a REST API, and real-time webhooks.

## Features

- **SIP inbound & outbound** -- receive and originate SIP calls with codec negotiation (PCMU, PCMA, G.722, Opus), digest auth, session timers (RFC 4028)
- **SIP over TLS** -- optional TLS transport on a second port alongside UDP, reusable by classic SIP trunks and required by WhatsApp
- **Early media** -- SIP 183 Session Progress with SDP for pre-answer audio (custom ringback, IVR)
- **Hold/unhold** -- SIP re-INVITE with sendonly/sendrecv direction
- **WebRTC** -- browser-based voice via SDP offer/answer with trickle ICE
- **WhatsApp Business Calling** -- inbound and outbound calls over SIP-TLS + ICE/DTLS-SRTP + Opus via [wa.meta.vc:5061](https://developers.facebook.com/docs/whatsapp/cloud-api/calling/sip/) with HTTP Digest auth; hold/transfer are 409 (Meta disallows re-INVITE)
- **Multi-party rooms** -- mix N participants with mixed-minus-self audio at a configurable sample rate (8 kHz, 16 kHz, or 48 kHz per room; default 16 kHz)
- **WebSocket room access** -- join rooms from any client over a WebSocket with base64 PCM frames
- **DTMF** -- send and receive RFC 4733 telephone-events; digits received on a leg are auto-broadcast to other legs in the same room (per-leg `accept_dtmf` opt-out)
- **Recording** -- stereo WAV recording per-leg or per-room, multi-channel per-participant tracks, pause/resume (writes silence to preserve timeline while sensitive data is exchanged), optional S3 upload
- **Playback** -- stream WAV/MP3 audio or built-in telephone tones into legs or rooms
- **TTS** -- text-to-speech into legs or rooms (ElevenLabs, Google Cloud, AWS Polly)
- **STT** -- real-time speech-to-text with partial transcripts (ElevenLabs)
- **AI Agent** -- attach a conversational AI agent to a leg or room (ElevenLabs, VAPI, Pipecat, Deepgram) with mid-session context injection
- **Answering Machine Detection (AMD)** -- per-call analysis of outbound call audio to classify the answerer as human, machine, no-speech, or not-sure; optional voicemail beep detection via Goertzel frequency analysis
- **Webhooks** -- real-time event delivery with HMAC-SHA256 signing and retry; typed event data with CDR-style `leg.disconnected` (disposition, timing, quality)
- **WebSocket event stream (VSI)** -- `GET /v1/vsi` streams all events and accepts commands (mute, hold, DTMF, room management) over a single persistent WebSocket; filter by `app_id` regex for multi-tenant isolation
- **Prometheus metrics** -- operational metrics exposed at `GET /metrics` (active legs/rooms, call durations, disconnect reasons, Go runtime). See [API.md](API.md) for the full metric reference. Profiling via `go tool pprof` is available at `/debug/pprof/` when built with `-tags pprof`.

## Quick Start

```bash
# Build and run
go build -o voiceblender ./cmd/voiceblender
./voiceblender

# Or run directly
go run ./cmd/voiceblender
```

The REST API listens on `:8080` and SIP on `127.0.0.1:5060` by default.

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `INSTANCE_ID` | *(auto-generated UUID)* | Instance identifier, included in API responses and webhooks |
| `HTTP_ADDR` | `:8080` | REST API listen address |
| `SIP_BIND_IP` | `127.0.0.1` | IPv4 address advertised in SDP/Contact/Via headers (and used as the listen address when `SIP_LISTEN_IP` is empty). Set to `0.0.0.0` for v4 wildcard, `::` for dual-stack on Linux when `bindv6only=0`. |
| `SIP_LISTEN_IP` | *(same as SIP_BIND_IP)* | UDP socket bind IP. Accepts `127.0.0.1`, `0.0.0.0`, `::`, or any literal v4/v6 address. |
| `SIP_BIND_IPV6` | *(empty = v4-only)* | IPv6 address advertised in SDP/Contact/Via for IPv6 calls. Set this for IPv6-only or dual-stack deployments. |
| `SIP_LISTEN_IPV6` | *(same as SIP_BIND_IPV6)* | Optional separate IPv6 socket bind address (e.g. when running with both `0.0.0.0` and a specific v6 literal). |
| `SIP_PORT` | `5060` | SIP listen port (UDP) |
| `SIP_TLS_PORT` | *(disabled)* | SIP-over-TLS listen port (typically `5061`). When set, `SIP_TLS_CERT` and `SIP_TLS_KEY` must also be provided. Required for WhatsApp Business Calling integration. |
| `SIP_TLS_CERT` | | Path to PEM-encoded TLS certificate (e.g. `fullchain.pem`). Meta rejects self-signed certs — use a CA-signed cert matching a public FQDN. |
| `SIP_TLS_KEY` | | Path to PEM-encoded TLS private key (e.g. `privkey.pem`). |
| `SIP_DEBUG` | `false` | When `true`, log the full RFC 3261 wire form of every inbound and outbound SIP request and response. Very verbose — use only for troubleshooting. |
| `SIP_DOMAIN` | *(falls back to advertised IP)* | FQDN advertised in From, Contact and Via on **all** outbound SIP signalling (classic trunks and WhatsApp). Should match the SAN on `SIP_TLS_CERT` and any allowlist your carrier or Meta keeps. |
| `SIP_HOST` | `voiceblender` | SIP User-Agent name |
| `ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs (comma-separated) |
| `RECORDING_DIR` | `/tmp/recordings` | Local recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `WEBHOOK_URL` | | Default webhook URL for inbound calls |
| `ELEVENLABS_API_KEY` | | API key for ElevenLabs TTS, STT, and Agent |
| `VAPI_API_KEY` | | API key for VAPI Agent provider |
| `DEEPGRAM_API_KEY` | | API key for Deepgram STT and TTS |
| `AZURE_SPEECH_KEY` | | Subscription key for Azure Cognitive Speech Services (TTS and STT) |
| `AZURE_SPEECH_REGION` | `eastus` | Azure region for Speech Services (e.g. `eastus`, `westeurope`) |
| `S3_BUCKET` | | S3 bucket for recording uploads |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_ENDPOINT` | | Custom S3 endpoint (MinIO, etc.) |
| `S3_PREFIX` | | Key prefix for S3 objects |
| `TTS_CACHE_ENABLED` | `false` | Enable disk-backed TTS audio cache. Cached audio persists across restarts. |
| `TTS_CACHE_DIR` | `/tmp/tts_cache` | Directory for cached TTS audio files (used when `TTS_CACHE_ENABLED=true`) |
| `TTS_CACHE_INCLUDE_API_KEY` | `false` | Include API key in TTS cache key (set `true` if different keys map to different voice clones) |
| `RTP_PORT_MIN` | `10000` | Minimum UDP port for RTP/RTCP media |
| `RTP_PORT_MAX` | `20000` | Maximum UDP port for RTP/RTCP media |
| `SIP_JITTER_BUFFER_MS` | `0` | SIP ingress jitter buffer target delay in ms (0 = disabled passthrough). Applies to every SIP leg. |
| `SIP_JITTER_BUFFER_MAX_MS` | `300` | Max depth of the SIP ingress jitter buffer (ms); frames beyond this are dropped oldest-first. |
| `SIP_EXTERNAL_IP` | *(empty)* | Public IPv4 address for NAT/Docker deployments. When set, used in SIP Contact headers and SDP media (c=) lines instead of the auto-detected or bind IP. An IPv6 literal here is accepted as the v6 advertised address when `SIP_EXTERNAL_IPV6` is unset. |
| `SIP_EXTERNAL_IPV6` | *(empty)* | Public IPv6 address override (for dual-stack deployments where v4 is NAT'd and v6 is native). |
| `DEFAULT_SAMPLE_RATE` | `16000` | Default mixer sample rate (Hz) for new rooms when `sample_rate` is not specified. Allowed: `8000`, `16000`, `48000`. |
| `SIP_REFER_AUTO_DIAL` | `false` | Accept incoming SIP REFER requests and auto-dial the transferred call. **Default-deny** (toll-fraud risk). Outbound transfers via the REST API are unaffected. |
| `SPEECH_DETECTION_ENABLED` | `false` | Emit `speaking.started` / `speaking.stopped` events for every connected leg by default. Per-call `speech_detection` on `POST /v1/legs` or `POST /v1/legs/{id}/answer` overrides this. |

## Links

- **Website:** [voiceblender.org](https://voiceblender.org/)
- **Documentation:** [voiceblender.org/docs](https://voiceblender.org/docs)

## API Overview

Full reference: [API.md](API.md)

### Legs

```
POST   /v1/legs                    # Originate outbound SIP call
GET    /v1/legs                    # List all legs
GET    /v1/legs/{id}               # Get leg details
POST   /v1/legs/{id}/answer        # Answer ringing inbound leg
POST   /v1/legs/{id}/early-media   # Enable early media (183)
DELETE /v1/legs/{id}               # Hang up
POST   /v1/legs/{id}/mute          # Mute
DELETE /v1/legs/{id}/mute          # Unmute
POST   /v1/legs/{id}/hold          # Put on hold
DELETE /v1/legs/{id}/hold          # Resume from hold
POST   /v1/legs/{id}/transfer      # SIP REFER (blind or attended)
POST   /v1/legs/{id}/dtmf          # Send DTMF digits
POST   /v1/legs/{id}/dtmf/accept   # Re-enable DTMF reception (default)
POST   /v1/legs/{id}/dtmf/reject   # Stop receiving DTMF broadcast from peers
POST   /v1/legs/{id}/play          # Play audio or tone
DELETE /v1/legs/{id}/play/{pbID}   # Stop playback
POST   /v1/legs/{id}/tts           # Text-to-speech
POST   /v1/legs/{id}/record        # Start recording
DELETE /v1/legs/{id}/record        # Stop recording
POST   /v1/legs/{id}/record/pause  # Pause recording (writes silence)
POST   /v1/legs/{id}/record/resume # Resume recording
POST   /v1/legs/{id}/stt           # Start speech-to-text
DELETE /v1/legs/{id}/stt           # Stop speech-to-text
POST   /v1/legs/{id}/amd            # Start answering machine detection
POST   /v1/legs/{id}/agent         # Attach AI agent
POST   /v1/legs/{id}/agent/message # Inject message into agent
DELETE /v1/legs/{id}/agent         # Detach AI agent
```

### Rooms

```
POST   /v1/rooms                   # Create room
GET    /v1/rooms                   # List rooms
GET    /v1/rooms/{id}              # Get room
DELETE /v1/rooms/{id}              # Delete room (hangs up all legs)
POST   /v1/rooms/{id}/legs         # Add or move leg to room
DELETE /v1/rooms/{id}/legs/{legID}      # Remove leg from room
GET    /v1/rooms/{id}/ws           # Join room via WebSocket
POST   /v1/rooms/{id}/play         # Play audio or tone to room
DELETE /v1/rooms/{id}/play/{pbID}  # Stop room playback
POST   /v1/rooms/{id}/tts          # TTS to room
POST   /v1/rooms/{id}/record       # Record room mix
DELETE /v1/rooms/{id}/record       # Stop room recording
POST   /v1/rooms/{id}/record/pause # Pause room recording
POST   /v1/rooms/{id}/record/resume # Resume room recording
POST   /v1/rooms/{id}/stt          # STT on all participants
DELETE /v1/rooms/{id}/stt          # Stop room STT
POST   /v1/rooms/{id}/agent        # Attach AI agent to room
POST   /v1/rooms/{id}/agent/message # Inject message into agent
DELETE /v1/rooms/{id}/agent        # Detach AI agent from room
```

### Events

```
GET    /v1/vsi                              # VoiceBlender Streaming Interface (VSI)
```

### WebRTC

```
POST   /v1/webrtc/offer                    # SDP offer/answer exchange
POST   /v1/legs/{id}/ice-candidates        # Add trickle ICE candidate
GET    /v1/legs/{id}/ice-candidates        # Get gathered ICE candidates
```

### Webhooks

```
POST   /v1/webhooks                # Register webhook
GET    /v1/webhooks                # List webhooks
DELETE /v1/webhooks/{id}           # Unregister webhook
```

## WhatsApp Business Calling

VoiceBlender bridges WhatsApp consumer voice calls to and from your stack via Meta's [Business Calling API](https://developers.facebook.com/docs/whatsapp/cloud-api/calling/sip/). Signalling is SIP over TLS to `wa.meta.vc:5061` with HTTP Digest auth; media is Opus over ICE + DTLS-SRTP (pion-driven). Once connected, a WhatsApp leg looks identical to any other leg — same `/v1/legs/{id}/...` operations, same event payloads, same room mechanics.

### Capabilities

- **Inbound** — Meta-originated INVITEs are auto-routed to a WhatsApp handler when the From URI host ends in `meta.vc`. The leg comes up in `ringing`, fires `leg.ringing` (`leg_type: "whatsapp_in"`), and waits for `POST /v1/legs/{id}/answer`. The 200 OK then carries the pre-gathered ICE/DTLS-SRTP answer.
- **Outbound** — `POST /v1/legs {"type":"whatsapp", ...}` returns `201` immediately with the leg in `ringing`. ICE gathering, the digest 401/407 round-trip, and the SDP-answer apply happen asynchronously; outcome is signalled via `leg.connected` or `leg.disconnected`.
- **Audio** — full-duplex Opus at 48 kHz with mixed-minus-self room participation, recording, TTS, STT, agent attachment, speaking detection, playback. The mixer auto-resamples between WhatsApp's 48 kHz and your room's configured rate.
- **DTMF** — inbound RFC 4733 telephone-events are decoded and emitted as `dtmf.received` plus the standard cross-leg broadcast.
- **Webhooks + WebSocket events** — `leg.ringing` / `leg.connected` / `leg.disconnected` / `dtmf.received` / `speaking.started` / `speaking.stopped` all carry `leg_type` set to `whatsapp_in` or `whatsapp_out` so multi-tenant filtering works as it does for SIP and WebRTC legs.

### Limitations

- **No re-INVITE.** Meta's SIP gateway rejects re-INVITE entirely, so `hold` / `unhold` / `transfer` return `409 Conflict` on WhatsApp legs. There is no workaround at the protocol level.
- **No outbound DTMF.** `POST /v1/legs/{id}/dtmf` on a WhatsApp leg currently returns an error. Inbound (caller pressing keys) works.
- **No early media.** Meta does not send `183 Session Progress` with SDP — outbound calls go straight from `ringing` to `connected`. Pre-answer audio (custom ringback) is not available.
- **No session timers** (RFC 4028) and no AMD support — Meta's consumer call flow doesn't apply.
- **TLS cert must be CA-signed.** Meta rejects self-signed certs. The cert's SAN must match the public FQDN you register with Meta and the value of `SIP_DOMAIN`.
- **Public reachability required.** Meta's gateway needs to reach your `SIP_TLS_PORT` (default 5061) over TCP/TLS and your ICE candidates over UDP. NAT/firewalls must forward both.
- **One business number per leg.** The `from` field carries the business phone, and Meta validates it server-side against the registered SIP server for that exact number.
- **Codec is fixed to Opus 48 kHz mono.** No PCMU/PCMA fallback path.

### Provisioning a number on Meta

Before any call works, the business phone number must be onboarded to WhatsApp Business Calling and your VoiceBlender host must be registered as its SIP server. VoiceBlender does not manage this — it is a one-time operator step performed via Meta's [Graph API](https://developers.facebook.com/docs/graph-api/).

Prerequisites:

1. A WhatsApp Business Account with the phone number already added and verified. The number must be enabled for Business Calling (currently a closed beta; enrolment via your Meta business representative).
2. A long-lived Graph API access token with `whatsapp_business_management` permission.
3. The phone number's **Phone Number ID** (visible in the Meta Business Manager UI or via `GET /me/phone_numbers`).
4. A public FQDN that resolves to your VoiceBlender host and a CA-signed TLS certificate whose SAN matches it.

Register VoiceBlender as the SIP server for the number:

```sh
curl -X POST "https://graph.facebook.com/v25.0/{PHONE_NUMBER_ID}/settings" \
  -H "Authorization: Bearer $META_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "calling": {
      "status": "ENABLED",
      "call_routing": {
        "default": "SIP",
        "fallback": "VOICEMAIL"
      },
      "sip": {
        "status": "ENABLED",
        "servers": [
          { "hostname": "voiceblender.your-domain.example" }
        ]
      }
    }
  }'
```

Meta returns a **digest password** in the response; this is the secret you pass as `auth.password` on `POST /v1/legs`. Each phone number gets its own password — the digest username is the E.164 number with the leading `+` stripped.

Verify the configuration was accepted:

```sh
curl -s "https://graph.facebook.com/v25.0/{PHONE_NUMBER_ID}/settings?fields=calling" \
  -H "Authorization: Bearer $META_TOKEN" | jq .
```

The response should show your hostname under `calling.sip.servers[]` and `calling.status: "ENABLED"`.

### VoiceBlender configuration

Set these env vars before starting `voiceblender`:

| Variable | Value |
|---|---|
| `SIP_TLS_PORT` | `5061` |
| `SIP_TLS_CERT` | path to `fullchain.pem` for your FQDN |
| `SIP_TLS_KEY` | path to `privkey.pem` |
| `SIP_DOMAIN` | the FQDN you registered with Meta (must match the cert SAN) |

Make a test outbound call:

```sh
curl -X POST http://localhost:8080/v1/legs \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "whatsapp",
    "to": "+447900000000",
    "from": "+441300000000",
    "auth": { "password": "<meta-issued-digest-password>" },
    "room_id": "wa-test"
  }'
```

The HTTP response returns immediately with the leg in `ringing`; subscribe to the webhook or `/v1/vsi` event stream to see `leg.connected` (or `leg.disconnected` with a reason if Meta rejects the INVITE).

### Troubleshooting

- `403 SIP server X.X.X.X from INVITE does not match any SIP server configured for phone number ...` — `SIP_DOMAIN` doesn't match what's registered with Meta. Set it to the FQDN, not the IP, and confirm via the `GET /settings` query above.
- `404 Not Found` on outbound — usually means the recipient phone number isn't a valid WhatsApp user, or the destination URI is malformed. Confirm the digits in `to` are the actual user's E.164 number.
- Call connects but Meta sends BYE after 20 s with `Reason: ... not receiving any media for a long time` — your audio path (RTP/UDP egress) is being dropped before reaching Meta. Check firewall rules for outbound UDP from the `RTP_PORT_MIN`–`RTP_PORT_MAX` range and that ICE-srflx candidates are correct.
- DTLS handshake stalls — Meta's offer is `setup:actpass` + `ice-lite`, and they don't initiate DTLS. VoiceBlender forces `setup:active` automatically; if you see `pcmedia: DTLS state state=connecting` for >5 s, run with `LOG_LEVEL=debug` and inspect pion's DTLS scope for the actual error.
- Set `SIP_DEBUG=true` to log the full RFC 3261 wire form of every SIP message, including the auth-bearing retry after the 401/407 challenge — that's the most useful diagnostic for any signalling-layer issue.

## Typical Workflow

```
1. Register a webhook        POST /v1/webhooks
2. Receive inbound call      --> webhook: leg.ringing {leg_id, from, to}
3. Answer                    POST /v1/legs/{id}/answer
4. Create a room             POST /v1/rooms
5. Add legs to room          POST /v1/rooms/{id}/legs
6. Attach AI agent           POST /v1/legs/{id}/agent
7. Start recording           POST /v1/legs/{id}/record
8. Hang up                   DELETE /v1/legs/{id}
```

## Examples

| Example | Description |
|---------|-------------|
| [`examples/call_handler.py`](examples/call_handler.py) | Python webhook listener for inbound SIP calls with room conferencing |
| [`examples/webrtc-client/`](examples/webrtc-client/) | Browser-based WebRTC voice client with room management and DTMF |
| [`examples/gen_test_wav.py`](examples/gen_test_wav.py) | Generate test WAV files for playback testing |

## Project Structure

```
cmd/voiceblender/       Entry point
internal/
  api/                  REST API (chi router)
  sip/                  SIP engine (sipgo)
  leg/                  Leg interface, SIPLeg, WebRTCLeg
  room/                 Room + Manager
  mixer/                Multi-party audio mixer (mixed-minus-self)
  codec/                Codec adapters (PCMU, PCMA, G.722, Opus)
  amd/                  Answering machine detection (Goertzel beep detector)
  events/               Event bus + webhook delivery
  playback/             Audio file playback
  recording/            WAV recording
  tts/                  TTS (ElevenLabs, Google Cloud, AWS Polly)
  stt/                  STT (ElevenLabs)
  agent/                AI agent (ElevenLabs, VAPI, Pipecat)
  storage/              S3 upload backend
  config/               Environment variable config
tests/integration/      Integration and benchmark tests
```

## Testing

```bash
# Unit tests
go test ./internal/...

# Integration tests (requires two SIP instances)
go test -tags integration -v -timeout 60s ./tests/integration/

# Benchmark (concurrent rooms)
go test -tags integration -v -timeout 120s -run TestConcurrentRoomsScale ./tests/integration/
```

See [TESTING.md](TESTING.md) for details.

## Dependencies

| Library | Description | Notes |
|---------|-------------|-------|
| [sipgo](https://github.com/emiago/sipgo) | SIP stack | Excellent SIP stack in go |
| [pion/webrtc](https://github.com/pion/webrtc) | WebRTC | Nothing is better than Pion |
| [go-chi](https://github.com/go-chi/chi) | HTTP router | |
| [zaf/g711](https://github.com/zaf/g711) | G.711 codec | |
| [gobwas/ws](https://github.com/gobwas/ws) | WebSocket | |
| [go-audio/wav](https://github.com/go-audio/wav) | WAV encoding | |
| [gopus](https://github.com/thesyncim/gopus) | Opus codec | Thanks Marcelo! (Claude and Codex too!) |
| [go-mp3](https://github.com/hajimehoshi/go-mp3) | MP3 decoder | Pure Go |
| [go-audio/audio](https://github.com/go-audio/audio) | Audio buffer types | |
| [google/uuid](https://github.com/google/uuid) | UUID generation | |
| [prometheus/client_golang](https://github.com/prometheus/client_golang) | Prometheus metrics | |
| [aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) | AWS SDK (S3, Polly) | |
| [cloud.google.com/go/texttospeech](https://cloud.google.com/go/docs/reference/cloud.google.com/go/texttospeech/latest) | Google Cloud TTS | |
| [protobuf](https://github.com/protocolbuffers/protobuf-go) | Protocol Buffers | Pipecat agent |
| [x/sync](https://pkg.go.dev/golang.org/x/sync) | Concurrency utilities | |

## AI-Assisted Development

This project was developed with the assistance of [Claude Code](https://claude.com/claude-code), Anthropic's AI coding assistant. 

## License

See LICENSE file.
