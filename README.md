# VoiceBlender

A Go service that bridges SIP and WebRTC voice calls with multi-party audio mixing, a REST API, and real-time webhooks.

## Features

- **SIP inbound & outbound** -- receive and originate SIP calls with codec negotiation (PCMU, PCMA, G.722, Opus), digest auth, session timers (RFC 4028)
- **Early media** -- SIP 183 Session Progress with SDP for pre-answer audio (custom ringback, IVR)
- **Hold/unhold** -- SIP re-INVITE with sendonly/sendrecv direction
- **WebRTC** -- browser-based voice via SDP offer/answer with trickle ICE
- **Multi-party rooms** -- mix N participants with mixed-minus-self audio at 16 kHz
- **WebSocket room access** -- join rooms from any client over a WebSocket with base64 PCM frames
- **DTMF** -- send and receive RFC 4733 telephone-events
- **Recording** -- stereo WAV recording per-leg or per-room, multi-channel per-participant tracks, optional S3 upload
- **Playback** -- stream WAV/MP3 audio or built-in telephone tones into legs or rooms
- **TTS** -- text-to-speech into legs or rooms (ElevenLabs, Google Cloud, AWS Polly)
- **STT** -- real-time speech-to-text with partial transcripts (ElevenLabs)
- **AI Agent** -- attach a conversational AI agent to a leg or room (ElevenLabs, VAPI, Pipecat, Deepgram) with mid-session context injection
- **Webhooks** -- real-time event delivery with HMAC-SHA256 signing and retry; typed event data with CDR-style `leg.disconnected` (disposition, timing, quality)
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
| `SIP_BIND_IP` | `127.0.0.1` | IP for SDP/Contact/Via headers |
| `SIP_LISTEN_IP` | *(same as SIP_BIND_IP)* | UDP socket bind IP |
| `SIP_PORT` | `5060` | SIP listen port |
| `SIP_HOST` | `voiceblender` | SIP User-Agent name |
| `ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs (comma-separated) |
| `RECORDING_DIR` | `/tmp/recordings` | Local recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `WEBHOOK_URL` | | Default webhook URL for inbound calls |
| `ELEVENLABS_API_KEY` | | API key for ElevenLabs TTS, STT, and Agent |
| `VAPI_API_KEY` | | API key for VAPI Agent provider |
| `S3_BUCKET` | | S3 bucket for recording uploads |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_ENDPOINT` | | Custom S3 endpoint (MinIO, etc.) |
| `S3_PREFIX` | | Key prefix for S3 objects |
| `TTS_CACHE_ENABLED` | `false` | Enable disk-backed TTS audio cache. Cached audio persists across restarts. |
| `TTS_CACHE_DIR` | `/tmp/tts_cache` | Directory for cached TTS audio files (used when `TTS_CACHE_ENABLED=true`) |
| `TTS_CACHE_INCLUDE_API_KEY` | `false` | Include API key in TTS cache key (set `true` if different keys map to different voice clones) |
| `RTP_PORT_MIN` | `10000` | Minimum UDP port for RTP/RTCP media |
| `RTP_PORT_MAX` | `20000` | Maximum UDP port for RTP/RTCP media |

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
POST   /v1/legs/{id}/dtmf          # Send DTMF digits
POST   /v1/legs/{id}/play          # Play audio or tone
DELETE /v1/legs/{id}/play/{pbID}   # Stop playback
POST   /v1/legs/{id}/tts           # Text-to-speech
POST   /v1/legs/{id}/record        # Start recording
DELETE /v1/legs/{id}/record        # Stop recording
POST   /v1/legs/{id}/stt           # Start speech-to-text
DELETE /v1/legs/{id}/stt           # Stop speech-to-text
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
POST   /v1/rooms/{id}/stt          # STT on all participants
DELETE /v1/rooms/{id}/stt          # Stop room STT
POST   /v1/rooms/{id}/agent        # Attach AI agent to room
POST   /v1/rooms/{id}/agent/message # Inject message into agent
DELETE /v1/rooms/{id}/agent        # Detach AI agent from room
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
