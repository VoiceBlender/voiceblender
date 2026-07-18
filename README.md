# VoiceBlender

A Go service that bridges SIP and WebRTC voice calls with multi-party audio mixing, a REST API, and real-time webhooks.

[![Join our Discord](https://img.shields.io/badge/Discord-Join%20our%20community-5865F2?logo=discord&logoColor=white)](https://discord.gg/HE9WDMzavN)

## Features

- **SIP inbound & outbound** -- receive and originate SIP calls with codec negotiation (PCMU, PCMA, G.722, Opus, AMR-WB, AMR-NB), digest auth, session timers (RFC 4028)
- **SIP over TLS** -- optional TLS transport on a second port alongside UDP, reusable by classic SIP trunks and required by WhatsApp
- **Early media** -- SIP 183 Session Progress with SDP for pre-answer audio (custom ringback, IVR)
- **Hold/unhold** -- SIP re-INVITE with sendonly/sendrecv direction
- **WebRTC** -- browser-based voice via SDP offer/answer with trickle ICE
- **WhatsApp Business Calling** -- inbound and outbound calls over SIP-TLS + ICE/DTLS-SRTP + Opus 
- **WebSocket legs** -- inbound (HTTP upgrade) and outbound (dial) PCM-over-WebSocket legs with binary or `json_base64` framing, configurable sample rate (8/16/24/48 kHz), bidirectional text, and caller-supplied X-/P- headers — designed to also back a future generic Agent API
- **MoQ legs (experimental, PoC)** -- inbound Media-over-QUIC legs over WebTransport/HTTP/3 with Opus framed one frame per MoQ Object (LOC-style). Tracks `mengelbart/moqtransport` (IETF draft-11); browser interop with draft-16 clients (moqtail, moq.dev) is not expected to work out of the box. Disabled by default; enable with `MOQ_ENABLED=true` + `MOQ_TLS_CERT_FILE` / `MOQ_TLS_KEY_FILE`
- **Multi-party rooms** -- mix N participants with mixed-minus-self audio at a configurable sample rate (8 kHz, 16 kHz, or 48 kHz per room; default 16 kHz)
- **Room bridging** -- join two rooms' mixers (same sample rate) with live-configurable direction (bidirectional, one-way each way, or parked); echo-free via mixed-minus-self
- **Audio routing matrix** -- per-room role-based routing for asymmetric audio (barge-in / whisper / supervisor monitor). Tag legs with a free-form `role` and declare a matrix of who-hears-whom by role. Applied atomically at leg-join time so a supervisor cannot momentarily bleed into the customer's audio. See [API.md](API.md#audio-routing-matrix).
- **WebSocket room access** -- join rooms from any client over a WebSocket with base64 PCM frames
- **DTMF** -- send and receive RFC 4733 telephone-events
- **Real-Time Text (RTT)** -- ITU-T T.140 over RTP per RFC 4103 with RFC 2198 redundancy;
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

### Multi-instance cluster

`docker/docker-compose.cluster.yml` brings up two VoiceBlender containers (`dialer` on host port 8080, `peer` on 8081) sharing the same `voiceblender.env`. Useful for end-to-end testing of inter-instance calls, REFER transfers, and webhook delivery between peers. Bring it up with:

```bash
docker compose -f docker/docker-compose.cluster.yml up --build
```

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `INSTANCE_ID` | *(auto-generated UUID)* | Instance identifier, included in API responses and webhooks |
| `HTTP_ADDR` | `:8080` | REST API listen address |
| `ALLOWED_IPS` | _(empty = allow all)_ | Comma-separated allowlist of IPs and CIDR ranges (IPv4 and IPv6, in any mix) gating **every** HTTP endpoint, including the `/v1/vsi` event WebSocket, `/v1/legs/websocket`, the `/v1/legs/moq` WebTransport endpoint, `/metrics`, and pprof. Empty or unset disables the check. Whitespace around entries is trimmed; bare addresses are treated as host routes (`/32` for v4, `/128` for v6); malformed entries fail server startup. Only `X-Forwarded-For` is consulted as a proxy header (see `TRUST_PROXY_HEADERS`); `X-Real-IP` and RFC 7239 `Forwarded` are ignored. Examples: `127.0.0.1`, `10.0.0.0/8,192.168.0.0/16`, `2001:db8::/32,::1`. |
| `TRUST_PROXY_HEADERS` | `false` | When `true`, the client IP used for the `ALLOWED_IPS` check is taken from the leftmost entry in `X-Forwarded-For` (falling back to the socket peer when the header is absent). When `false` (default), `X-Forwarded-For` is ignored and only the socket peer (`r.RemoteAddr`) is consulted. Enable only when VoiceBlender sits behind a trusted reverse proxy that unconditionally overwrites `X-Forwarded-For` — otherwise the header is client-spoofable. |
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
| `WEBRTC_EXTERNAL_IPS` | *(empty)* | Comma-separated public IPs advertised as host ICE candidates (pion `SetNAT1To1IPs`). Set this when VoiceBlender runs behind NAT/Docker and the gathered host interface IPs aren't routable from the remote peer — otherwise WebRTC peers behind firewalls won't be able to reach VB. Supports IPv4 and IPv6 literals. The literal value `auto` performs STUN-based public-IP discovery at startup against the first reachable `ICE_SERVERS` entry; discovery failure is non-fatal and logs a warning. |
| `RECORDING_DIR` | `/tmp/recordings` | Local recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |
| `WEBHOOK_URL` | | Default webhook URL for inbound calls |
| `WEBHOOK_SECRET` | | HMAC-SHA256 signing secret for the global webhook. Applied to events delivered to `WEBHOOK_URL`; per-leg/per-room webhooks set via the API can supply their own secret. |
| `ELEVENLABS_API_KEY` | | API key for ElevenLabs TTS, STT, and Agent |
| `VAPI_API_KEY` | | API key for VAPI Agent provider |
| `DEEPGRAM_API_KEY` | | API key for Deepgram STT and TTS |
| `AZURE_SPEECH_KEY` | | Subscription key for Azure Cognitive Speech Services (TTS and STT) |
| `AZURE_SPEECH_REGION` | `eastus` | Azure region for Speech Services (e.g. `eastus`, `westeurope`) |
| `S3_BUCKET` | | S3 bucket for recording uploads. The bucket is probed with a bounded (10s) `HeadBucket` call when the backend is built — at startup, and again per request when `s3_bucket` is supplied — so a missing bucket or unreachable endpoint fails immediately (startup: exit 1; per request: `400`) instead of after a recording has already been captured. **IAM:** `HeadBucket` requires the `s3:ListBucket` action on the bucket; credentials scoped to `s3:PutObject` alone will now fail this check. |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_ENDPOINT` | | Custom S3 endpoint (MinIO, etc.) |
| `S3_PREFIX` | | Key prefix for S3 objects |
| `S3_ALLOW_INSECURE_ENDPOINT` | `false` | Allow a plaintext `http://` `S3_ENDPOINT`. Off by default: an `http://` endpoint is rejected when the backend is built (startup: exit 1; per request: `400`) rather than shipping recording audio in cleartext. Set `true` only when the endpoint is reachable over a trusted network (e.g. a local MinIO). Only an explicit `http://` scheme is rejected; `https://` is accepted. `S3_ENDPOINT` must include an `http://` or `https://` scheme — the AWS SDK rejects a scheme-less value such as `minio.internal:9000` as an invalid URI, which fails the preflight. For a plaintext MinIO, write the endpoint as `http://minio.internal:9000` and set this to `true`. This is an operator-level decision and governs both the startup backend and per-request `s3_*` uploads; there is no per-request override. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE` | | AWS credentials for S3 uploads and AWS Polly TTS. Resolved by the AWS SDK's default credential chain (env vars → `~/.aws/credentials` → EC2/ECS/EKS instance role), not by VoiceBlender directly. `AWS_REGION` is honored only when `S3_REGION` is empty. |
| `GOOGLE_APPLICATION_CREDENTIALS` | | Path to a Google Cloud service-account JSON file used by Google Cloud TTS when no per-request `api_key` is supplied. Resolved by Google's Application Default Credentials chain (env var → `~/.config/gcloud/application_default_credentials.json` → GCE/Cloud Run/GKE metadata), not by VoiceBlender directly. |
| `TTS_CACHE_ENABLED` | `false` | Enable disk-backed TTS audio cache. Cached audio persists across restarts. |
| `TTS_CACHE_DIR` | `/tmp/tts_cache` | Directory for cached TTS audio files (used when `TTS_CACHE_ENABLED=true`) |
| `TTS_CACHE_INCLUDE_API_KEY` | `false` | Include API key in TTS cache key (set `true` if different keys map to different voice clones) |
| `RTP_PORT_MIN` | `10000` | Minimum UDP port for RTP/RTCP media |
| `RTP_PORT_MAX` | `20000` | Maximum UDP port for RTP/RTCP media |
| `SIP_JITTER_BUFFER_MS` | `0` | SIP ingress jitter buffer target delay in ms (0 = disabled passthrough). Applies to every SIP leg. |
| `SIP_JITTER_BUFFER_MAX_MS` | `300` | Max depth of the SIP ingress jitter buffer (ms); frames beyond this are dropped oldest-first. |
| `SIP_EXTERNAL_IP` | *(empty)* | Public IPv4 address for NAT/Docker deployments. When set, used in SIP Contact headers and SDP media (c=) lines instead of the auto-detected or bind IP. IPv6 has no equivalent: set `SIP_BIND_IPV6` directly to the address you want advertised. |
| `DEFAULT_SAMPLE_RATE` | `16000` | Default mixer sample rate (Hz) for new rooms when `sample_rate` is not specified. Allowed: `8000`, `16000`, `48000`. |
| `SIP_CODECS` | `PCMU,PCMA` | Comma-separated, preference-ordered list of codecs the SIP engine offers on outbound INVITEs **and** accepts on inbound INVITEs (a codec absent from this list cannot be negotiated in either direction). Recognized names (case-insensitive): `PCMU`, `PCMA`, `G722`, `opus`, `AMR-WB`, `AMR-NB` (the bare token `AMR` also resolves to AMR-NB per RFC 4867 §8.1). Unknown names and duplicates are dropped silently; if the parsed list ends up empty the default is used. Example: `SIP_CODECS=opus,G722,PCMU,PCMA,AMR-WB,AMR-NB` enables every supported codec, ranked Opus-first. |
| `SIP_REFER_AUTO_DIAL` | `false` | When `true`, the server accepts an incoming SIP REFER (202) and **dials the target itself**. When `false` (default), the REFER is parked and surfaced as `leg.transfer_requested` for the app to drive via the transfer commands (`accept`/`progress`/`complete`/`decline`); an undecided REFER auto-declines (603, **default-deny** — toll-fraud risk). Outbound transfers via the REST API are unaffected. |
| `SIP_REFER_CONSULT_TIMEOUT_MS` | `2000` | How long an inbound REFER is parked awaiting an app accept/decline decision before it auto-declines with `603` (fail-closed). Only used when `SIP_REFER_AUTO_DIAL=false`. |
| `SIP_AUTO_RINGING` | `false` | **Behavior change vs prior releases**: previously the server always sent `180 Ringing` after `100 Trying`. The new default sends only `100 Trying`; the API caller drives ringing explicitly via `POST /v1/legs/{id}/ring`, `/early-media`, or `/answer`. Set to `true` to restore the legacy auto-180 behavior. |
| `SIP_USE_SOURCE_SOCKET` | `false` | When `true`, route SIP responses **and** in-dialog requests (BYE, re-INVITE, UPDATE, INFO, NOTIFY, REFER) back to the request's source UDP socket instead of the peer's `Contact` URI / Via sent-by. Enable when peers advertise unroutable addresses (e.g. private IPs in `Contact` from behind NAT, or Via sent-by hosts that don't resolve). Equivalent to sipgo's `DialogUA.RewriteContact` plus per-response `SetDestination(req.Source())`. |
| `SIP_REGISTRATION_DEFAULT_EXPIRES_SECONDS` | `3600` | Expiry used when an inbound `REGISTER` carries no `Expires` value. |
| `SIP_REGISTRATION_MAX_EXPIRES_SECONDS` | `7200` | Upper clamp on the granted expiry. Requests above this value are honored at this maximum. |
| `SIP_REGISTRATION_SWEEP_INTERVAL_MS` | `1000` | Sweeper period for evicting expired AOR bindings. |
| `SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS` | `true` | When `true`, the same AOR may be bound from multiple Contacts simultaneously (and `POST /v1/legs` parallel-forks to every bound contact). When `false`, each `REGISTER` replaces any prior Contacts for the AOR. |
| `SIP_INBOUND_AUTH_CONSULT_TIMEOUT_MS` | `2000` | How long an inbound `REGISTER` is parked awaiting a challenge/accept/reject decision (surfaced via the `sip.registration_attempt` event) before the fallback (`SIP_INBOUND_REGISTER_DEFAULT`) applies. Every REGISTER is surfaced for a decision — symmetric with inbound INVITE, which always surfaces `leg.ringing` and waits for the client. |
| `SIP_INBOUND_REGISTER_DEFAULT` | `reject` | Fallback for an inbound `REGISTER` that no client decides within the consult window: `reject` (reply `403`, **fail-closed default**) or `accept` (bind and reply `200 OK` — the legacy fail-open behaviour). |
| `SIP_INBOUND_AUTH_NONCE_TTL_SECONDS` | `60` | Lifetime of an issued inbound-auth digest challenge nonce. A credentialed retry arriving after this elapses must be re-challenged. |
| `SIP_OUTBOUND_REGISTRATION_DEFAULT_EXPIRES_SECONDS` | `3600` | Default `Expires` value sent on outbound REGISTER (sip_register trunks) when the create-trunk request does not specify one. |
| `SIP_OUTBOUND_REGISTRATION_MIN_EXPIRES_SECONDS` | `60` | Lower clamp on the requested outbound REGISTER expiry. |
| `SIP_OUTBOUND_REGISTRATION_MAX_EXPIRES_SECONDS` | `7200` | Upper clamp on the requested outbound REGISTER expiry. |
| `SIP_OUTBOUND_REGISTRATION_REFRESH_RATIO` | `0.5` | Fraction of the **granted** expiry at which the trunk refreshes (e.g. `0.5` of a 600 s grant → refresh every 300 s). Must be `(0, 1)`; out-of-range values fall back to `0.5`. |
| `SIP_OUTBOUND_REGISTRATION_FAILURE_BACKOFF_MAX_MS` | `300000` | Upper cap on the exponential backoff between failed outbound REGISTER attempts. Failures emit `sip.outbound_registration_failed`; the trunk stays in the manager and keeps retrying. |
| `SPEECH_DETECTION_ENABLED` | `false` | Emit `speaking.started` / `speaking.stopped` events for every connected leg by default. Per-call `speech_detection` on `POST /v1/legs` or `POST /v1/legs/{id}/answer` overrides this. |
| `AMRWB_MODE` | `2` | AMR-WB (G.722.2) encoder speech-mode **ceiling** `0..8`: `0`=6.60, `1`=8.85, `2`=12.65, `3`=14.25, `4`=15.85, `5`=18.25, `6`=19.85, `7`=23.05, `8`=23.85 kbit/s. The actual transmit mode is this ceiling clamped to the peer's negotiated `mode-set` (so e.g. `8` yields HD 23.85 only when the peer allows it, falling back automatically). Default `2` (12.65) matches the GSMA IR.92 / VoLTE common rate. Out-of-range values clamp to `0..8`. |
| `AMRWB_OCTET_ALIGNED` | `true` | Offer octet-aligned AMR-WB framing (RFC 4867) in outbound SDP. When `false`, offers bandwidth-efficient framing. On answers, VoiceBlender always echoes the framing the peer negotiated. |
| `AMRNB_MODE` | `7` | AMR-NB (RFC 4867) encoder speech-mode **ceiling** `0..7`: `0`=4.75, `1`=5.15, `2`=5.90, `3`=6.70, `4`=7.40, `5`=7.95, `6`=10.2, `7`=12.2 kbit/s. The actual transmit mode is this ceiling clamped to the peer's negotiated `mode-set`. Default `7` is the GSM-EFR-equivalent 12.2 kbit/s, the highest AMR-NB quality and the rate most enterprise PBXes and mobile networks default to. Out-of-range values clamp to `0..7`. |
| `AMRNB_OCTET_ALIGNED` | `true` | Offer octet-aligned AMR-NB framing (RFC 4867) in outbound SDP. When `false`, offers bandwidth-efficient framing. On answers, VoiceBlender always echoes the framing the peer negotiated. |
| `VSI_EVENT_BUFFER_SIZE` | `256` | Per-client buffer (in events) on the `/v1/vsi` WebSocket. When the client consumes events slower than they're produced, the buffer fills and new events are dropped (with a warn log on the leading edge of each drop burst and at every 10× threshold; the next delivered event also includes an `events_dropped` notification to the client). Clamped to `[16, 1_000_000]`. **Tuning:** larger values absorb longer back-pressure spikes at the cost of higher peak memory per client (roughly the average JSON event size × buffer size, e.g. ~1 KB × 256 ≈ 256 KB per connection at the default) and longer end-to-end latency for buffered events when the client recovers. Increase only if you observe drops on legitimate slow-consumer scenarios you can't fix at the client. |
| `MOQ_ENABLED` | `false` | Enable the experimental MoQ (Media over QUIC) inbound leg endpoint at `CONNECT /v1/legs/moq` over WebTransport/HTTP/3. PoC quality: tracks IETF draft-11 via `mengelbart/moqtransport`, single MoQ session per leg, Opus framed one frame per MoQ Object (LOC-style). When enabled, both `MOQ_TLS_CERT_FILE` and `MOQ_TLS_KEY_FILE` must be set. |
| `MOQ_LISTEN_ADDR` | `:8443` | UDP address for the HTTP/3 listener that backs the MoQ leg. Independent of `HTTP_ADDR` — TCP/`:8080` and UDP/`:8443` can run side-by-side. |
| `MOQ_TLS_CERT_FILE` | _(none)_ | Path to the TLS certificate used by the HTTP/3 listener. Required when `MOQ_ENABLED=true`. |
| `MOQ_TLS_KEY_FILE` | _(none)_ | Path to the TLS private key used by the HTTP/3 listener. Required when `MOQ_ENABLED=true`. |
| `MOQ_OPUS_BITRATE` | `24000` | Target bitrate (bps) for the Opus encoder feeding the MoQ leg's `mix` track. Must be in `6000..510000`. |
| `LIVEKIT_ENABLED` | `false` | Enable the `livekit_room` leg type at `POST /v1/legs` (`type=livekit_room`). Lets VoiceBlender join a LiveKit room as a participant and bridge audio between SIP and LiveKit. No LiveKit SDK is used — the signaling protocol is spoken directly via `github.com/livekit/protocol` protobufs over the existing pion stack. |
| `LIVEKIT_URL` | _(none)_ | Default LiveKit server endpoint (`wss://...`). Required when `LIVEKIT_ENABLED=true` unless every request supplies `livekit.url`. Overridable per-request. |
| `LIVEKIT_OPUS_BITRATE` | `24000` | Target bitrate (bps) for the Opus encoder publishing audio into LiveKit. Must be in `6000..510000`. Overridable per-request via `livekit.opus_bitrate`. |
| `LIVEKIT_TOKEN_SIGNING_ENABLED` | `false` | Opt-in: when `true`, callers may omit `livekit.token` and instead pass `{room,identity,permissions}`; VoiceBlender mints the JWT itself. **Security caveat:** enabling this stores the LiveKit API secret (a high-privilege credential that can mint tokens for any room/identity on the LiveKit deployment) in VoiceBlender. Keep off in multi-tenant deployments. |
| `LIVEKIT_API_KEY` | _(none)_ | LiveKit API key used to sign minted JWTs. Required only when `LIVEKIT_TOKEN_SIGNING_ENABLED=true`. |
| `LIVEKIT_API_SECRET` | _(none)_ | LiveKit API secret used to sign minted JWTs. Required only when `LIVEKIT_TOKEN_SIGNING_ENABLED=true`. Treat as a high-value secret; redact in logs. |
| `LIVEKIT_DEFAULT_TOKEN_TTL` | `6h` | Default TTL applied to minted JWTs when the request omits `livekit.token_ttl`. Go duration string. LiveKit recommends ≤ 6 hours. |

## Links

- **Website:** [voiceblender.org](https://voiceblender.org/)
- **Documentation:** [voiceblender.org/docs](https://voiceblender.org/docs)

## API Overview

Full reference: [API.md](API.md)

> **API authentication.** The HTTP surface — REST, the `/v1/vsi` event
> WebSocket, `/v1/legs/websocket`, the `/v1/legs/moq` WebTransport endpoint,
> `/metrics`, and pprof — has **no built-in credential authentication** (no API
> key, bearer token, or session). Access is gated **solely** by the `ALLOWED_IPS`
> allowlist and your network placement. Do not expose it to untrusted networks;
> front it with a reverse proxy or gateway that enforces auth if you need
> per-caller credentials. This is distinct from SIP-layer auth (digest challenge
> of inbound INVITE/REGISTER) and outbound webhook signing (`WEBHOOK_SECRET`),
> which are covered separately below.

### Legs

```
POST   /v1/legs                    # Originate outbound leg (sip / whatsapp / websocket / livekit_room)
GET    /v1/legs                    # List all legs
GET    /v1/legs/websocket          # Connect a WebSocket leg (HTTP upgrade)
GET    /v1/legs/{id}               # Get leg details
POST   /v1/legs/{id}/answer        # Answer ringing inbound leg
POST   /v1/legs/{id}/early-media   # Enable early media (183)
DELETE /v1/legs/{id}               # Hang up
POST   /v1/legs/{id}/mute          # Mute
DELETE /v1/legs/{id}/mute          # Unmute
POST   /v1/legs/{id}/hold          # Put on hold
DELETE /v1/legs/{id}/hold          # Resume from hold
POST   /v1/legs/{id}/transfer            # Initiate a SIP REFER (blind or attended)
POST   /v1/legs/{id}/transfer/accept     # Accept a parked inbound REFER (202 + NOTIFY 100)
POST   /v1/legs/{id}/transfer/progress   # Interim sipfrag NOTIFY (e.g. 180 Ringing)
POST   /v1/legs/{id}/transfer/complete   # Terminal NOTIFY (200 OK or failure)
POST   /v1/legs/{id}/transfer/decline    # Reject a parked inbound REFER (603 default)
POST   /v1/legs/{id}/dtmf          # Send DTMF digits
POST   /v1/legs/{id}/dtmf/accept   # Re-enable DTMF reception (default)
POST   /v1/legs/{id}/dtmf/reject   # Stop receiving DTMF broadcast from peers
POST   /v1/legs/{id}/rtt           # Send Real-Time Text chunk (T.140 / RFC 4103)
POST   /v1/legs/{id}/rtt/accept    # Re-enable RTT reception (default)
POST   /v1/legs/{id}/rtt/reject    # Stop emitting rtt.received events
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
                                   # LiveKit: each remote LK participant becomes its own `livekit_participant` leg in the same VB room.
                                   # Per-LK operations (mute, recording, role, hangup) use the standard /v1/legs/{id}/* endpoints.
```

### Rooms

```
POST   /v1/rooms                   # Create room
GET    /v1/rooms                   # List rooms
GET    /v1/rooms/{id}              # Get room
DELETE /v1/rooms/{id}              # Delete room (hangs up all legs)
POST   /v1/rooms/{id}/legs         # Add or move leg to room
DELETE /v1/rooms/{id}/legs/{legID}      # Remove leg from room
POST   /v1/rooms/{id}/bridges      # Bridge this room's mixer to another room
GET    /v1/rooms/{id}/bridges      # List bridges involving this room
GET    /v1/rooms/{id}/bridges/{bridgeID}    # Get a bridge
PATCH  /v1/rooms/{id}/bridges/{bridgeID}    # Change bridge direction
DELETE /v1/rooms/{id}/bridges/{bridgeID}    # Tear down a bridge
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

### SIP Registrations

```
GET    /v1/sip/registrations               # List current AOR bindings
DELETE /v1/sip/registrations/{aor}         # Force-unbind an AOR (or one contact via ?contact=)

# Inbound digest auth — decide per attempt whether to challenge
POST   /v1/legs/{id}/challenge                              # 401-challenge a ringing inbound INVITE
POST   /v1/sip/registrations/attempts/{id}/challenge        # 401-challenge a parked inbound REGISTER
POST   /v1/sip/registrations/attempts/{id}/accept           # Accept a parked inbound REGISTER
POST   /v1/sip/registrations/attempts/{id}/reject           # Reject a parked inbound REGISTER
```

Inbound INVITE and REGISTER are handled symmetrically: every inbound request is
surfaced to the client (an INVITE via `leg.ringing`, a REGISTER via
`sip.registration_attempt`), which may **challenge** it (e.g. based on its source
address). VoiceBlender replies `401` with a digest `WWW-Authenticate` and
verifies the credentialed retry itself against the supplied `password`/`ha1`.
INVITE challenges target the ringing leg by id; REGISTER attempts are
challenged/accepted/rejected by `attempt_id`.

If the client does not decide, the two differ: an unchallenged INVITE simply
keeps ringing and remains answerable (nothing is auto-answered), while an
undecided REGISTER falls back to `SIP_INBOUND_REGISTER_DEFAULT` after the consult
window — **`reject` (403) by default** (fail-closed), or `accept` to bind it.

### SIP Trunks (outbound REGISTER)

VoiceBlender can REGISTER itself to an upstream SIP registrar/PBX as a UAC,
refresh the binding before expiry, place outbound calls under the registered
identity, and accept inbound INVITEs the registrar delivers. Only the
`sip_register` trunk type is implemented today; `ip_ip` is reserved.

```
POST   /v1/sip/trunks                      # Create a trunk; 202 Accepted, REGISTER runs async
GET    /v1/sip/trunks                      # List configured trunks
GET    /v1/sip/trunks/{id}                 # Trunk status snapshot (never returns password)
DELETE /v1/sip/trunks/{id}                 # Unregister + remove (202 Accepted, async)
```

Outbound calls placed with `POST /v1/legs` whose `from` matches a registered
trunk's AOR (or AOR user-part) automatically attach the trunk's digest
credentials and traverse the trunk's upstream proxy via a Route header.
Inbound INVITEs arriving from a registered trunk's registrar are tagged with
`trunk_id` on the `leg.ringing` event.

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

## SIP Registrations (AOR)

VoiceBlender accepts inbound SIP `REGISTER` requests on UDP, TCP, and TLS and
maintains an in-memory map of canonical Address-of-Record (AOR) URIs to the
exact transport sockets the REGISTERs arrived on. `POST /v1/legs` looks up the
`to` value in this map: if there's a match, the outbound INVITE is routed to
the bound socket(s) — reusing the persistent TCP/TLS connection from the
REGISTER where applicable. When an AOR has multiple bound contacts (e.g. a
softphone on desktop plus a mobile client) the INVITE is **parallel-forked**;
the first contact to answer wins and the others are CANCELled (RFC 3261 §16).

Bindings emit `sip.registration_active` / `sip.registration_expired` events
over both webhooks and the `/v1/vsi` WebSocket. Tuning lives behind the
`SIP_REGISTRATION_*` env vars (see [Configuration](#configuration)). A
challenge or accept decision on an inbound REGISTER may also cap that single
binding's lifetime via an optional `max_expires` (floored at the 60 s minimum),
forcing short-lived registrations (and, when challenging, frequent re-auth)
without lowering the global clamp. Full endpoint and event reference is in
[API.md](API.md#sip-registrations-aor).

**Authentication.** Every inbound REGISTER (that creates or removes a binding)
is surfaced as a `sip.registration_attempt` event and parked for a client
decision — challenge (401 digest), accept, or reject — for up to
`SIP_INBOUND_AUTH_CONSULT_TIMEOUT_MS`. If no client decides within that window,
the `SIP_INBOUND_REGISTER_DEFAULT` fallback applies: **`reject` (403) by
default**, so an unanswered REGISTER is denied rather than blindly bound. Set it
to `accept` for the legacy fail-open behaviour (e.g. when authentication is
terminated at a SIP proxy such as [Kamailio](https://www.kamailio.org/) or
[OpenSIPS](https://opensips.org/) in front of VoiceBlender). Either way, enforce
credentials via your `sip.registration_attempt` handler or a front proxy before
exposing VoiceBlender's SIP port to the public internet.

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
  codec/                Codec adapters (PCMU, PCMA, G.722, Opus, AMR-WB, AMR-NB)
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
