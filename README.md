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
- **Multi-stream SIP calls** -- several `m=audio` sections in one dialog (RFC 3264), each with its own RTP port, direction, language and mixer room; built for live translation, where the original audio and a translated feed are mixed separately. Follows the SIPREC (RFC 7866) wire profile for interoperability. See [API.md](API.md#per-leg-audio-streams-multiple-maudio-lines).
- **SIPREC session recording server (RFC 7865 / RFC 7866)** -- accept recording sessions forked from an SBC or PBX: multipart `SDP` + `rs-metadata` INVITEs are answered receive-only on every `m=audio` section, and each section is bound to the participant the metadata names. The received audio is an ordinary set of leg streams, so it can be recorded to file *and* attached to rooms for live STT/agents. Disabled by default; enable with `SIPREC_ENABLED=true` (needs `SIP_TCP_ENABLED=true`). The metadata is checked against the SDP it arrived with, so a client that binds a participant to the wrong `a=label` -- a document that is otherwise valid and would silently attribute audio to the wrong party -- is reported in `warnings` on `GET /v1/legs/{id}/siprec` instead of being recorded as if it were correct. VoiceBlender can also act as the **recording client**, forking a room's participants to an external recording server one stream each (`POST /v1/rooms/{id}/siprec`, `SIPREC_SRC_ENABLED=true`). See [API.md](API.md#siprec-session-recording).
- **WebSocket room access** -- join rooms from any client over a WebSocket with base64 PCM frames
- **DTMF** -- send and receive RFC 4733 telephone-events
- **Real-Time Text (RTT)** -- ITU-T T.140 over RTP per RFC 4103 with RFC 2198 redundancy;
- **Recording** -- stereo WAV recording per-leg or per-room, multi-channel per-participant tracks, pause/resume (writes silence to preserve timeline while sensitive data is exchanged), optional S3 or Google Cloud Storage upload
- **Playback** -- stream WAV/MP3 audio or built-in telephone tones into legs or rooms
- **TTS** -- text-to-speech into legs or rooms (ElevenLabs, Google Cloud, AWS Polly, Deepgram, Azure), with optional **preflight staging**: synthesize a speculative reply off the critical path, then commit it for instant playback or discard it
- **STT** -- real-time speech-to-text with partial transcripts (ElevenLabs, Deepgram, Deepgram Flux, Azure, Speechmatics). Deepgram Flux adds conversational turn detection (`stt.turn`), including eager end-of-turn signals for speculative generation; Speechmatics reports end-of-turn from server-side silence detection and supports mid-stream finalize
- **AI Agent** -- attach a conversational AI agent to a leg or room (ElevenLabs, VAPI, Pipecat, Deepgram) with mid-session context injection
- **Answering Machine Detection (AMD)** -- per-call analysis of outbound call audio to classify the answerer as human, machine, no-speech, or not-sure; optional voicemail beep detection via Goertzel frequency analysis
- **Webhooks** -- real-time event delivery with HMAC-SHA256 signing and retry; a stable per-event `event_id` (also sent as `X-Event-Id`) for receiver-side deduplication; typed event data with CDR-style `leg.disconnected` (disposition, timing, quality)
- **WebSocket event stream (VSI)** -- `GET /v1/vsi` streams all events and accepts commands (mute, hold, DTMF, room management) over a single persistent WebSocket; filter by `app_id` regex for multi-tenant isolation
- **Prometheus metrics** -- operational metrics exposed at `GET /metrics` (active legs/rooms, call durations, disconnect reasons, event-egress drop/delivery counters, Go runtime). See [API.md](API.md) for the full metric reference. Profiling via `go tool pprof` is available at `/debug/pprof/` when built with `-tags pprof`.

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

All configuration is via environment variables. The full reference — every variable,
its default and behaviour, plus the S3 bucket preflight rules — lives in
**[CONFIGURATION.md](CONFIGURATION.md)**.

A ready-to-edit file with every variable is committed as
[voiceblender.env.example](voiceblender.env.example).

Online documentation: <https://voiceblender.org/docs/>

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
POST   /v1/legs/{id}/tts/preflight # Synthesize and hold for a later commit (speculative reply)
POST   /v1/legs/{id}/tts/{ttsID}/commit # Play a staged utterance
DELETE /v1/legs/{id}/tts/{ttsID}   # Drop a staged utterance without playing it
POST   /v1/legs/{id}/record        # Start recording
DELETE /v1/legs/{id}/record        # Stop recording
POST   /v1/legs/{id}/record/pause  # Pause recording (writes silence)
POST   /v1/legs/{id}/record/resume # Resume recording
POST   /v1/legs/{id}/stt           # Start speech-to-text
POST   /v1/legs/{id}/stt/finalize  # Flush STT and emit a final transcript
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
credentials and traverse the trunk's upstream via a Route header — the trunk's
`outbound_proxy` when one is configured, otherwise its `registrar_uri`.
Such calls also send `From` and `P-Asserted-Identity` in the trunk's AOR realm
rather than `SIP_DOMAIN`, so the call claims the identity the registrar
actually authenticated. An AOR port, if configured, is not carried into the
From — `sip:alice@pbx.example.com:5070` yields a From host of
`pbx.example.com`.

> **Changed outcome.** This can flip how an upstream responds to calls that
> previously went out under `SIP_DOMAIN`. A registrar that accepted them
> un-challenged may now challenge them — which resolves on its own for a
> correctly provisioned trunk, since the digest credentials are already
> attached, but surfaces as a `401`/`407` failure for one with stale
> credentials. It can flip the other way too: a registrar that rejected an
> unknown From domain may now accept the call. There is no toggle. The only
> way to keep `SIP_DOMAIN` on the From is to use a `from` that matches no
> trunk AOR and no trunk AOR user-part, which also drops the trunk's
> credentials and Route.

#### Routing through an outbound proxy

By default a trunk talks straight to its `registrar_uri`. Set
`sip_register.outbound_proxy` to put an SBC or edge proxy in front:

```json
{
  "type": "sip_register",
  "sip_register": {
    "registrar_uri": "sip:pbx.example.com:5060",
    "outbound_proxy": "sip:edge.acme.net:5060;transport=tcp",
    "aor": "sip:alice@pbx.example.com",
    "password": "s3cret"
  }
}
```

Every INVITE placed from that AOR then carries
`Route: <sip:edge.acme.net:5060;transport=tcp;lr>`. The REGISTER is sent to the
same hop but **without** a pre-loaded Route — a proxy that does not recognise
the Route URI as one of its own forwards the REGISTER back at itself instead of
popping the header, and the registration fails with no response at all. Either
way the Request-URI still names the registrar, and digest authentication still
computes against it — only the next hop changes. The field defaults to `SIP_OUTBOUND_PROXY` and is resolved
when the trunk is created, so `GET /v1/sip/trunks/{id}` reports the hop actually
in effect.

A single call can override this with `outbound_proxy` on `POST /v1/legs`, which
also works with no trunk involved at all.

Inbound INVITEs arriving from a registered trunk's peer socket are tagged with
`trunk_id` on the `leg.ringing` event. That socket is the proxy when one is
configured, so several trunks sharing a proxy cannot be told apart this way; the
tag is informational and never gates anything.

A leg associated with a trunk — in either direction — transfers out over that
same trunk. With `SIP_REFER_AUTO_DIAL=true`, an inbound REFER on such a leg
originates the target under the trunk's AOR, with its credentials and Route
attached, rather than under the transferor's caller ID (which would usually
match no AOR and be rejected upstream). See
[Receiving a transfer](API.md#receiving-a-transfer-inbound-refer).

## WhatsApp Business Calling

VoiceBlender bridges WhatsApp consumer voice calls to and from your stack via Meta's [Business Calling API](https://developers.facebook.com/docs/whatsapp/cloud-api/calling/sip/). Signalling is SIP over TLS to `wa.meta.vc:5061` with HTTP Digest auth; media is Opus over ICE + DTLS-SRTP (pion-driven). Once connected, a WhatsApp leg looks identical to any other leg — same `/v1/legs/{id}/...` operations, same event payloads, same room mechanics.

### Capabilities

- **Inbound** — Meta-originated INVITEs are auto-routed to a WhatsApp handler when the From URI host ends in `meta.vc` **and** the offer still carries DTLS-SRTP (`a=fingerprint`). A fronting SIP proxy that terminates encryption into plain RTP keeps the Meta From but takes the classic SIP path; one that re-encrypts with SDES-SRTP is rejected with `488 Not Acceptable Here` rather than answered with media nothing can decrypt. On the WhatsApp path the leg comes up in `ringing`, fires `leg.ringing` (`leg_type: "whatsapp_in"`), and waits for `POST /v1/legs/{id}/answer`. The 200 OK then carries the pre-gathered ICE/DTLS-SRTP answer.
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
`SIP_REGISTRATION_*` env vars (see [CONFIGURATION.md](CONFIGURATION.md)). A
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
| [`examples/voice-agent-testops/`](examples/voice-agent-testops/) | Public-safe regression test sketch for VoiceBlender-backed voice agents |
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
  stt/                  STT (ElevenLabs, Deepgram, Deepgram Flux, Azure, Speechmatics)
  agent/                AI agent (ElevenLabs, VAPI, Pipecat)
  storage/              S3 / GCS upload backends
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

## Contributing

[CLAUDE.md](CLAUDE.md) is the canonical checklist for changes to this repository. Read it before opening a pull request.

It is named for [Claude Code](https://claude.com/claude-code), which picks it up automatically — if you are working with Claude, no setup is needed. **Nothing in it is Claude-specific, though: the same rules apply to human contributors and to any other agentic coding tool** (Codex, Cursor, Copilot, Aider, and so on). If your tool expects a project instructions file under a different name, point it at `CLAUDE.md` rather than adding a second copy that will drift out of sync.

In short, every change is expected to:

- **Format** — run `gofmt` on all changed Go files.
- **Regenerate specs** — run `make specs` when REST endpoints, request/response schemas, VSI commands, events, or config env vars change. `openapi.yaml` and `asyncapi.yaml` are generated; never hand-edit them.
- **Update the docs it affects** — `API.md`, `README.md`, `TESTING.md`, and `voiceblender.env.example`.
- **Ship tests** — unit tests for every new package or feature, plus integration tests in `tests/integration/`.
- **Preserve the public API** — backwards compatibility is the default; break it only when there is no alternative.
- **Keep comments minimal** — explain a non-obvious *why*, never restate what the code already says.

CI enforces the first of these plus `go vet` and the unit tests, so run them locally before pushing:

```bash
gofmt -l .
go vet ./...
go test ./internal/... -count=1 -timeout=60s
```

## AI-Assisted Development

This project was developed with the assistance of [Claude Code](https://claude.com/claude-code), Anthropic's AI coding assistant. 

## License

See LICENSE file.
