# VoiceBlender

A Go service that bridges SIP and WebRTC voice calls with multi-party audio mixing, a REST API, and real-time webhooks.

## Features

- **SIP inbound & outbound** -- receive and originate SIP calls with codec negotiation (PCMU, PCMA, G.722, Opus), session timers (RFC 4028)
- **WebRTC** -- browser-based voice via SDP offer/answer (PCMU at 8 kHz)
- **Multi-party rooms** -- mix N participants with mixed-minus-self audio at 16 kHz
- **WebSocket room access** -- join rooms from any client over a WebSocket with base64 PCM frames
- **DTMF** -- send and receive RFC 4733 telephone-events
- **Recording** -- stereo WAV recording per-leg or per-room, with optional S3 upload
- **Playback** -- stream WAV/MP3 audio into legs or rooms
- **TTS** -- text-to-speech into legs or rooms (ElevenLabs, Google Cloud, AWS Polly)
- **STT** -- real-time speech-to-text with partial transcripts (ElevenLabs)
- **AI Agent** -- attach a conversational AI agent to a leg or room (ElevenLabs, VAPI, Pipecat)
- **Webhooks** -- real-time event delivery with HMAC-SHA256 signing and retry

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
| `HTTP_ADDR` | `:8080` | REST API listen address |
| `SIP_BIND_IP` | `127.0.0.1` | IP for SDP/Contact/Via headers |
| `SIP_LISTEN_IP` | *(same as SIP_BIND_IP)* | UDP socket bind IP |
| `SIP_PORT` | `5060` | SIP listen port |
| `SIP_HOST` | `voiceblender` | SIP User-Agent name |
| `ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs (comma-separated) |
| `RECORDING_DIR` | `/tmp/recordings` | Local recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `WEBHOOK_URL` | | Default webhook URL for inbound calls |
| `ELEVENLABS_API_KEY` | | API key for TTS, STT, and Agent features |
| `S3_BUCKET` | | S3 bucket for recording uploads |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_ENDPOINT` | | Custom S3 endpoint (MinIO, etc.) |
| `S3_PREFIX` | | Key prefix for S3 objects |

## API Overview

Full reference: [API.md](API.md)

### Legs

```
POST   /v1/legs                    # Originate outbound SIP call
GET    /v1/legs                    # List all legs
GET    /v1/legs/{id}               # Get leg details
POST   /v1/legs/{id}/answer        # Answer ringing inbound leg
DELETE /v1/legs/{id}               # Hang up
POST   /v1/legs/{id}/mute          # Mute
DELETE /v1/legs/{id}/mute          # Unmute
POST   /v1/legs/{id}/dtmf          # Send DTMF digits
POST   /v1/legs/{id}/play          # Play audio
POST   /v1/legs/{id}/tts           # Text-to-speech
POST   /v1/legs/{id}/record        # Start recording
POST   /v1/legs/{id}/stt           # Start speech-to-text
POST   /v1/legs/{id}/agent         # Attach AI agent
```

### Rooms

```
POST   /v1/rooms                   # Create room
GET    /v1/rooms                   # List rooms
GET    /v1/rooms/{id}              # Get room
DELETE /v1/rooms/{id}              # Delete room (hangs up all legs)
POST   /v1/rooms/{id}/legs         # Add leg to room
DELETE /v1/rooms/{id}/legs/{legID} # Remove leg from room
GET    /v1/rooms/{id}/ws           # Join room via WebSocket
POST   /v1/rooms/{id}/play         # Play audio to room
POST   /v1/rooms/{id}/tts          # TTS to room
POST   /v1/rooms/{id}/record       # Record room mix
POST   /v1/rooms/{id}/stt          # STT on all participants
POST   /v1/rooms/{id}/agent        # Attach AI agent to room
```

### WebRTC & Webhooks

```
POST   /v1/webrtc/offer            # SDP offer/answer exchange
POST   /v1/webhooks                # Register webhook
GET    /v1/webhooks                # List webhooks
DELETE /v1/webhooks/{id}           # Unregister webhook
```

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

- [sipgo](https://github.com/emiago/sipgo) -- SIP stack
- [pion/webrtc](https://github.com/pion/webrtc) -- WebRTC
- [go-chi](https://github.com/go-chi/chi) -- HTTP router
- [zaf/g711](https://github.com/zaf/g711) -- G.711 codec
- [gobwas/ws](https://github.com/gobwas/ws) -- WebSocket
- [go-audio/wav](https://github.com/go-audio/wav) -- WAV encoding

## License

See LICENSE file.
