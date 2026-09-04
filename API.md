# VoiceBlender API Reference

Base URL: `http://localhost:8080/v1`

All responses are `Content-Type: application/json`.

## Asynchronous endpoints

Every endpoint that triggers a SIP request or response (e.g. INVITE, BYE, re-INVITE for hold/unhold, REFER for transfer, 100/180/183/200 for inbound calls) is **asynchronous**. The HTTP handler validates inputs synchronously (returning 4xx if anything fails up front) then queues the SIP work on a goroutine and returns **`202 Accepted`** with a progressive-form status string (e.g. `holding`, `unholding`, `hanging_up`, `early_media`, `ringing`, `answering`).

The actual outcome of the SIP-level work is observed via webhook/WebSocket events:

| Event | When |
|---|---|
| `leg.connected`, `leg.early_media`, `leg.hold`, `leg.unhold`, `leg.disconnected`, `leg.transfer_*` | Successful completion |
| `leg.command_failed` | The SIP-level work failed *after* the HTTP `202` was returned. Payload: `{leg_id, command, error}` where `command` is one of `ring`, `early_media`, `hold`, `unhold`, `add_to_room`, etc. |
| `leg.stream_added` | An additional `m=audio` stream was negotiated on a live dialog. Payload: `{leg_id, stream_id, mid, direction, lang}` |
| `leg.stream_removed` | A stream was disabled with a port-0 re-INVITE; its m-line slot survives as a tombstone. Payload: `{leg_id, stream_id, mid}` |
| `leg.stream_rejected` | The peer refused an additional stream, or it could not be negotiated. The call is unaffected. Payload: `{leg_id, reason}` |
| `leg.stream_failed` | A stream's media loop failed and the stream was torn down; the call continues on its remaining streams. Payload: `{leg_id, stream_id, reason}` |
| `leg.stream_room_changed` | A stream was attached to or detached from a room (empty `room_id` means detached). Payload: `{leg_id, stream_id, mid, room_id, role}` |
| `leg.stream_role_changed` | A stream's routing role changed; the room's allow-sets were recomputed atomically. Payload: `{leg_id, stream_id, room_id, role}` |

GET endpoints, in-memory state-change endpoints (`/mute`, `/deaf`, `/dtmf/accept`, `/dtmf/reject`), audio-pipeline endpoints (`/play`, `/record`, `/tts`, `/stt`, `/agent/*`), `/dtmf` (sends RTP, not SIP), and room CRUD remain synchronous.

---

## Legs

A **leg** represents one side of a voice call — a SIP dialog, a WebRTC peer connection, a WhatsApp call, or a WebSocket session.

### Leg Object

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "sip_inbound",
  "state": "connected",
  "room_id": "room-123",
  "muted": false,
  "deaf": false,
  "held": false,
  "role": "agent",
  "sip_headers": {
    "X-Correlation-ID": "abc-123"
  },
  "headers": {
    "X-Correlation-ID": "abc-123"
  },
  "custom_data": {
    "order_id": "A-991",
    "tenant": 42
  }
}
```

| Field | Type | Values |
|-------|------|--------|
| `id` | string | UUID |
| `type` | string | `sip_inbound`, `sip_outbound`, `webrtc`, `whatsapp_in`, `whatsapp_out`, `websocket_in`, `websocket_out`, `moq_in`, `livekit_publish`, `livekit_participant` |
| `state` | string | `ringing`, `early_media`, `connected`, `held`, `hung_up` |
| `room_id` | string | Room ID if assigned, empty otherwise |
| `muted` | boolean | `true` if the leg is muted (cannot be heard by others) |
| `deaf` | boolean | `true` if the leg is deaf (cannot hear others) |
| `held` | boolean | `true` if the call is on hold (SIP legs only) |
| `role` | string | Routing role used by the room's audio routing matrix (e.g. `"customer"`, `"agent"`, `"supervisor"`). Omitted/empty means full mesh. |
| `sip_headers` | object | Deprecated — `X-*` headers from the inbound INVITE. Only present on `sip_inbound` legs. Use `headers` for new code. |
| `headers` | object | Custom protocol headers exposed by the leg's transport — `X-`/`P-` headers from a SIP INVITE, WebSocket handshake, or supplied at outbound dial time. |
| `custom_data` | any | Opaque application JSON attached to the leg. Omitted when the leg has none. See [Custom data](#custom-data). |

---

### Custom data

`custom_data` attaches an arbitrary JSON value to a leg. It is echoed on the leg object and
repeated at the **top level of every event published for that leg**, so an application can carry
its own identifiers through the call without maintaining a `leg_id → external state` lookup table.

```jsonc
// POST /v1/legs
{ "type": "sip", "to": "sip:alice@example.com",
  "custom_data": { "order_id": "A-991", "tenant": 42 } }
```

```jsonc
// every subsequent event for that leg
{ "type": "leg.connected", "leg_id": "550e8400-…", "timestamp": "…",
  "custom_data": { "order_id": "A-991", "tenant": 42 } }
```

**Where it can be set**

| Entry point | How |
|---|---|
| `POST /v1/legs` (all leg types) | `custom_data` in the request body |
| VSI `create_leg` | `custom_data` in the command payload |
| `POST /v1/legs/{id}/answer`, `/ring`, `/early-media` | `custom_data` in the request body — the hook for **inbound** calls |
| VSI `answer_leg`, `leg_ring`, `leg_early_media` | `custom_data` in the command payload |
| `POST /v1/webrtc/offer` | `custom_data` in the request body |
| `GET /v1/legs/websocket`, `CONNECT /v1/legs/moq` | `custom_data=<url-encoded JSON>` query parameter |
| `PUT /v1/legs/{id}/custom-data` | replace at any point in the call — see below |
| `DELETE /v1/legs/{id}/custom-data` | clear at any point in the call |
| VSI `set_leg_custom_data`, `delete_leg_custom_data` | same two operations over the WebSocket |

**Semantics**

- Any JSON value is accepted — object, array, string, number or boolean.
- On the answer/ring/early-media endpoints, **omitting** the field leaves any existing value
  untouched, and sending `null` clears it. Sending a value replaces the previous one outright;
  there is no merge.
- The value is stored verbatim, so large integers and key order survive the round trip unchanged.
- Size is capped by `CUSTOM_DATA_MAX_BYTES` (default `1024`, `0` = unlimited). Exceeding it is
  rejected with `400` and the leg is not created.
- It can be changed or cleared at any point in the call via `PUT`/`DELETE /v1/legs/{id}/custom-data`.
  A change takes effect on the **next** event published for that leg; events already emitted are
  unaffected.
- Data lives for the leg's lifetime and is released after `leg.disconnected` is published — that
  final CDR event still carries whatever value was current.
- **Inbound SIP legs:** `leg.ringing` fires before the application has seen the call, so it never
  carries `custom_data`. Attach it on `/ring`, `/early-media` or `/answer`; every event from that
  point on carries it.

---

### POST /v1/legs

Originate an outbound SIP call.

**Request:**

```json
{
  "type": "sip",
  "to": "sip:alice@192.168.1.100:5060",
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
  "room_id": "room-123",
  "custom_data": { "order_id": "A-991", "tenant": 42 },
  "streams": [
    { "direction": "sendonly", "content": "alt", "lang": "es",
      "room_id": "room-translated", "role": "translator" }
  ],
  "amd": {
    "initial_silence_timeout": 2500,
    "greeting_duration": 1500,
    "after_greeting_silence": 800,
    "total_analysis_time": 5000,
    "minimum_word_length": 100,
    "beep_timeout": 10000
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | yes | `"sip"`, `"whatsapp"` (see [WhatsApp Business Calling](#whatsapp-business-calling) below), `"websocket"` (see [WebSocket Legs](#websocket-legs)), or `"livekit_room"` (see [LiveKit Room Legs](#livekit-room-legs)) |
| `to` | string | yes | Destination. For `sip` legs, a SIP URI (e.g. `"sip:alice@example.com"`); the transport comes from the URI — `sips:` or `;transport=tls` dials over TLS, `;transport=tcp` over TCP, otherwise UDP. An `outbound_proxy`, when present, is the hop actually contacted and its transport wins. For `whatsapp` legs, an E.164 phone number (with or without `+`). |
| `uri` | string | no | Deprecated alias for `to` (sip legs only). Kept for backward compat; prefer `to`. |
| `from` | string | no | Caller ID. A bare user-part (e.g. `"+15551234567"`, `"alice"`) sets the user of the SIP From header. A full SIP URI (e.g. `"sip:alice@pbx.example.com"`) sets both the user and the host; otherwise the host comes from the matched trunk's AOR realm, falling back to `SIP_DOMAIN`. |
| `outbound_proxy` | string | no | SIP legs only. Next hop for this INVITE, attached as a loose `Route` header with the Request-URI left unchanged (e.g. `"sip:edge.acme.net:5060;transport=tcp"`). Outranks the matched trunk's `outbound_proxy` and `SIP_OUTBOUND_PROXY`; ignored when `to` resolves to an AOR registered here. See [Routing through an outbound proxy](#routing-through-an-outbound-proxy). |
| `privacy` | string | no | SIP Privacy header value (e.g. `"id"`, `"none"`) |
| `ring_timeout` | integer | no | Seconds to wait for answer; 0 = no timeout |
| `max_duration` | integer | no | Maximum call duration in seconds after connect. The call is automatically hung up when reached. 0 or omitted = no limit. |
| `codecs` | string[] | no | Codec preference order. Supported: `PCMU`, `PCMA`, `G722`, `opus`, `AMR-WB`, `AMR-NB`. Defaults to engine config. |
| `headers` | object | no | Custom SIP headers to include in the outbound INVITE (e.g. `X-Correlation-ID`). Keys are header names, values are header values. |
| `auth` | object | no for sip, **yes for whatsapp** | Digest auth credentials. Contains `username` (string, optional for whatsapp — defaults to `from` with `+` stripped) and `password` (string). For sip legs, retried on 401/407 challenge. |
| `room_id` | string | no | Room ID to auto-add the leg to once media is ready. The leg joins the room on `early_media` (183+SDP) or `connected` (200 OK), whichever comes first. If the room does not exist, it is automatically created. |
| `webhook_url` | string | no | Per-leg webhook URL. Events for this leg are routed exclusively to this URL instead of global webhooks. |
| `webhook_secret` | string | no | HMAC-SHA256 signing secret for the per-leg webhook. |
| `custom_data` | any | no | Opaque application JSON attached to the leg and repeated on every event for it. Capped by `CUSTOM_DATA_MAX_BYTES` (default 1024). See [Custom data](#custom-data). |
| `amd` | object | no | Enable Answering Machine Detection on this outbound call. Disabled by default — omit the field entirely to skip AMD. Include the object to enable; all inner fields are optional and default to sensible values when omitted or zero. See **AMD Parameters** below. |
| `speech_detection` | bool | no | Emit `speaking.started` / `speaking.stopped` events for this leg. Omit to use the server default (`SPEECH_DETECTION_ENABLED` env var, default `false`). |
| `rtt` | bool | no | Offer Real-Time Text (T.140 / RFC 4103) on the outbound INVITE. The peer may accept or ignore the `m=text` section; audio negotiation is unaffected either way. Default: `false`. |
| `streams` | object[] | no | **SIP only.** Extra `m=audio` sections to offer alongside the call's primary bidirectional audio, so a multi-stream call is established by the **first INVITE** instead of a follow-up re-INVITE. Each entry takes `direction` (`sendrecv`/`sendonly`/`recvonly`/`inactive`, default `sendrecv`), `lang` (BCP 47, emitted as `a=lang`), `content` (`main`/`alt`/…, emitted as `a=content`), `label` (emitted as `a=label`), and `room_id` + `role` to mix that stream into its own room once connected. See [Per-leg audio streams](#per-leg-audio-streams-multiple-maudio-lines). |

**AMD Parameters** (all optional — `"amd": {}` enables AMD with all defaults):

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `initial_silence_timeout` | integer | 2500 | Max milliseconds of silence before declaring `no_speech`. |
| `greeting_duration` | integer | 1500 | Speech duration threshold (ms). Continuous/cumulative speech exceeding this value classifies the answerer as `machine`. |
| `after_greeting_silence` | integer | 800 | Silence duration (ms) after initial speech to declare `human`. |
| `total_analysis_time` | integer | 5000 | Hard analysis deadline (ms). If no determination is made within this window, the result is `not_sure`. When `beep_timeout` is non-zero and the media stream stalls, the terminal event can be delayed by up to `beep_timeout` beyond this deadline: a single timer covers both the analysis and beep windows. |
| `minimum_word_length` | integer | 100 | Minimum speech burst duration (ms) to count as a word. Shorter bursts are treated as noise. |
| `beep_timeout` | integer | 0 | After detecting `machine`, continue listening up to this many ms for the voicemail beep tone (800–1200 Hz). `0` = beep detection disabled. |

**Constraints** — `total_analysis_time` bounds every verdict: a threshold longer than the analysis window can never be reached, so that verdict never fires. Setting one threshold past the window is allowed, and is how you suppress a single verdict — a large `greeting_duration`, for example, disables `machine` while `no_speech` and `human` keep working. A window shorter than *all* of `initial_silence_timeout`, `greeting_duration` and `after_greeting_silence` is rejected with `400`, because such a call could only ever end `not_sure`.

Analysis advances in 20 ms frames and the deadline is checked before a verdict is emitted, so a window exactly equal to a threshold does not reach it. Leave at least one frame of headroom above the thresholds you rely on.

Omitted or `0` values fall back to the defaults above (for `beep_timeout`, `0` means beep detection is disabled); a negative value on any field is rejected with `400`.

Examples:

```json
"amd": {}                                          // all defaults
"amd": { "beep_timeout": 10000 }                   // defaults + beep detection
"amd": { "greeting_duration": 2000, "beep_timeout": 8000 }  // custom thresholds
```

**Jitter Buffer:** The SIP ingress jitter buffer absorbs variation in RTP packet arrival times. When enabled, packets are reordered by sequence number and released to the decoder at a fixed 20 ms cadence; late packets that miss their slot are replaced with silence. The buffer adds latency equal to its target depth, so it is **off by default** — turn it on only when network jitter is a real concern (PSTN trunks, mobile carriers, satellite links), not for latency-sensitive voice-agent legs.

Configured server-wide:

- `SIP_JITTER_BUFFER_MS` — target delay in ms, applied to every SIP leg. `0` = disabled passthrough (default).
- `SIP_JITTER_BUFFER_MAX_MS` — queue cap in ms (default `300`). Frames beyond this are dropped oldest-first to catch up after a stall.

WebRTC legs are unaffected — pion/webrtc provides its own jitter buffer.

**WebSocket playout buffer:** WebSocket legs and room WebSocket participants get a separate, simpler mechanism. There is no reordering to do — a WebSocket delivers in order — so the buffer is only a playout lead: inbound PCM is withheld until the lead is buffered, and a producer that runs a few milliseconds late against the mixer's 20 ms tick spends the lead instead of leaving a gap in the mix. The lead is rebuilt after every underrun. Like the SIP buffer it costs latency equal to its depth and is off by default; `40`–`60` ms is typical for an external agent pacing PCM of its own accord. It needs no separate cap — the WebSocket ingress buffer is already bounded at 1000 ms.

Configured server-wide:

- `WS_JITTER_BUFFER_MS` — playout lead in ms, applied to every WebSocket leg and room WebSocket participant. `0` = disabled passthrough (default). `40`–`80` suits a healthy link.

When the lead is enabled the buffer also compensates for clock drift. The producer's clock and the mixer's 20 ms tick never run at exactly the same rate, so over time one frame has to be given back or taken away — a producer only 2–3% slow leaves a 20 ms hole in the mix roughly every second, which is audible as a click. The buffer makes that correction during a pause instead: whenever its level drifts off target and the audio at the head is below the speech floor, it duplicates or drops one frame of silence, which nothing can hear. Speech is never dropped. If the buffer does run dry mid-word it emits a short faded repeat rather than digital silence, bounded to 40 ms, after which it waits for real audio so a genuinely dead producer still shows up as a gap.

**Sizing the lead.** Drift and jitter are separate problems and only one of them needs buffer. Drift is corrected during pauses, so it is absorbed at any lead down to a single frame — it does not need to be *stored*. Jitter does: a transport that goes quiet for N ms needs a lead of at least N ms, because there is nothing else to play. So size `WS_JITTER_BUFFER_MS` to the worst-case stall you expect from the transport and nothing more; the lead is added one-way latency on every call, which matters far more for a voice agent than for a recording.

**Comfort noise:** `COMFORT_NOISE_ENABLED` (default `true`) injects low-level noise (~−75 dBFS) into otherwise silent mixer frames, so a quiet room does not sound like a dead line. Set it to `false` when downstream processing needs digital silence to stay digital.

**Response:** `201 Created` — Leg object (initially in `ringing` state)

**Early Media:** When the remote sends a 183 Session Progress response with SDP, the leg automatically transitions to `early_media` state and a `leg.early_media` webhook event is emitted. The RTP media pipeline starts immediately, allowing the leg to be added to a room so other participants can hear the remote's early media (custom ringback, IVR prompts, etc.). When the remote answers (200 OK), the leg transitions to `connected` as normal.

**Errors:**
- `400` — Invalid JSON, bad SIP URI, unknown codec, unsupported type, or an invalid `streams[].direction`
- `404` — A `streams[].room_id` names a room that does not exist

---

### WhatsApp Business Calling

VoiceBlender terminates calls to and from WhatsApp's SIP calling service. The signalling layer is SIP over TLS with HTTP Digest auth; the media layer is Opus over ICE + DTLS-SRTP (pion). Meta mandates both and does **not** support `re-INVITE`, so these operations return **409** for WhatsApp legs: `hold`, `unhold`, `transfer`.

**Server prerequisites** (see README env var table):
- `SIP_TLS_PORT=5061`
- `SIP_TLS_CERT` / `SIP_TLS_KEY` pointing at a **CA-signed** certificate (Meta rejects self-signed) whose SAN matches the public FQDN you registered with Meta.
- Operator-side: the SIP endpoint must be registered via Meta's Graph API (`POST /{phone-number-id}/settings`). VoiceBlender does not perform this registration itself.

#### Outbound: POST /v1/legs (type=whatsapp)

Originate a call to a WhatsApp user.

**Request:**

```json
{
  "type": "whatsapp",
  "to": "+15557654321",
  "from": "+15551234567",
  "auth": {
    "password": "meta-issued-digest-password"
  },
  "room_id": "room-123",
  "app_id": "myapp"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | yes | `"whatsapp"` |
| `to` | string | yes | Destination phone number (E.164, with or without `+`). |
| `from` | string | yes | Business phone number, E.164 (with or without `+`). Used as the From URI user-part and, by default, as the digest auth username. |
| `auth.password` | string | yes | Meta-issued digest password for the business number. |
| `auth.username` | string | no | Override the digest auth username. Defaults to `from` with `+` stripped, per Meta's spec. |
| `room_id` | string | no | Room ID to auto-add the leg to once connected. Created on the fly if it doesn't exist. |
| `app_id` | string | no | Application identifier for event stream filtering. |

The handler is **asynchronous**: it returns the leg view as soon as PCMedia setup succeeds and the leg is registered. ICE gathering, the INVITE round-trip (including the digest 401/407 retry), and the SDP answer apply happen in the background. Progress is signalled via webhook events:

- `leg.ringing` (`type: "whatsapp_out"`) — fires immediately after the leg is created. The HTTP response is sent at this moment.
- `leg.connected` — fires once Meta returns 200 OK and the SDP answer has been applied.
- `leg.disconnected` — fires if the INVITE fails (`reason: "invite_failed"`), the answer is rejected (`bad_answer`), or the dialog ends (`remote_bye`).

**Response:** `201 Created` — Leg object in `ringing` state with `type: "whatsapp_out"`. Subscribe to `leg.connected` / `leg.disconnected` (webhook or `/v1/vsi`) to track progress.

**Errors (synchronous, before the leg is created):**
- `400` — missing `to` / `from` / `password`.
- `503` — `SIP_TLS_PORT` not configured on this instance.
- `500` — local PCMedia or SDP setup failed.

**Async errors (delivered via `leg.disconnected` event after `201`):**
- `invite_failed` — Meta rejected the INVITE (e.g. 403 / 404 / digest auth failed) or the request timed out.
- `bad_answer` — Meta's 200 OK contained an SDP answer that pion couldn't apply.
- `remote_bye` — call ended normally or Meta hung up.

#### Inbound

INVITEs whose From-URI host is `meta.vc` (or any subdomain, e.g. `wa.meta.vc`) **and** whose SDP offer still carries DTLS-SRTP (`a=fingerprint`) are routed to the WhatsApp handler automatically. If a fronting SIP proxy has already terminated ICE+DTLS into plain RTP/AVP, the Meta From host alone is not enough — the call takes the classic SIP inbound path instead. A proxy that re-encrypts with SDES-SRTP instead (every active audio section on an `RTP/SAVP`-style profile) is rejected with **488 Not Acceptable Here**: no media path here decrypts SRTP, so answering would yield silent audio. When the WhatsApp path is taken, the leg is created in `ringing` state with `type: "whatsapp_in"`, a `leg.ringing` webhook event is emitted, and the call remains in this state until `POST /v1/legs/{id}/answer` is invoked. At that point a 200 OK with the pre-gathered SDP answer is sent and the leg transitions to `connected`.

The standard `/answer`, `/mute`, `/deaf`, `/dtmf`, `/play`, `/record`, `/stt`, `/tts`, and `/agent/*` endpoints all apply. The following explicitly return **409 Conflict**:

- `POST /v1/legs/{id}/hold`
- `DELETE /v1/legs/{id}/hold`
- `POST /v1/legs/{id}/transfer`

---

### WebSocket Legs

A **websocket leg** carries PCM audio over a single WebSocket connection. Both directions are supported:

- **Outbound** (`websocket_out`) — VoiceBlender dials a remote WebSocket. Created via `POST /v1/legs` with `type: "websocket"`.
- **Inbound** (`websocket_in`) — an external client connects to VoiceBlender. Created by upgrading `GET /v1/legs/websocket`.

Both directions go straight to `connected` (no ringing/answer flow). Audio is signed 16-bit little-endian PCM, mono, at the configured sample rate (8000/16000/24000/48000 Hz — the room mixer resamples automatically). Hold and DTMF send are not supported on websocket legs. The bidirectional text-message channel is enabled with `rtt: true` (outbound) or `?rtt=true` (inbound) and ties into the same `/v1/legs/{id}/rtt` REST endpoint and `rtt.received` event stream that SIP RTT uses.

#### Wire format

`wire_format=binary` (default): each WebSocket binary frame is one 20ms PCM frame at the configured sample rate. Most efficient; matches the framing used by Deepgram / VAPI-style providers.

`wire_format=json_base64`: PCM frames are wrapped as JSON text frames `{"type":"audio","audio":"<base64-pcm>"}`. Browser-friendly; matches the existing `/v1/rooms/{id}/ws` shape.

Text and control messages always use JSON text frames regardless of wire format:

```
{"type":"text","text":"hello"}            // bidi text (rtt.received event + /rtt REST)
{"type":"ping","event_id":42}             // server-initiated heartbeat
{"type":"pong","event_id":42}             // reply to a server ping
{"type":"hangup"}                          // peer-initiated termination
```

Inbound text triggers a `rtt.received` event; outbound text is sent via `POST /v1/legs/{id}/rtt` or by the WebSocket peer writing the JSON frame above.

#### Outbound: POST /v1/legs (type=websocket)

```json
{
  "type": "websocket",
  "url": "wss://agent.example.com/voice",
  "sample_rate": 16000,
  "wire_format": "binary",
  "headers": {
    "Authorization": "Bearer abc-123",
    "X-Correlation-ID": "call-456"
  },
  "room_id": "room-789",
  "rtt": true
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | yes | `"websocket"` |
| `url` | string | yes | `ws://` or `wss://` target URL. |
| `sample_rate` | int | no | 8000/16000/24000/48000. Default 16000. |
| `wire_format` | string | no | `binary` (default) or `json_base64`. |
| `sample_format` | string | no | `s16le` (only option in v1). |
| `headers` | object | no | Headers sent on the upgrade request (e.g. `Authorization`, `X-*`, `P-*`). |
| `room_id` | string | no | Room to auto-add the leg to once connected. |
| `rtt` | boolean | no | Enable bidi text channel. Default false. |
| `ring_timeout` | int | no | Seconds to wait for the WS handshake to complete. Default unbounded. |
| `app_id`, `webhook_url`, `webhook_secret`, `max_duration`, `accept_dtmf`, `speech_detection` | — | no | Same semantics as SIP legs. |

**Response:** `201 Created` — Leg object in `ringing` state with `type: "websocket_out"`. The dial completes asynchronously: `leg.connected` (success) or `leg.disconnected` (one of `ring_timeout`, `service_unavailable`, `unauthorized`, `forbidden`, `not_found`, `ws_dial_failed`).

#### Inbound: GET /v1/legs/websocket (HTTP upgrade)

```
GET /v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=room-789&rtt=true
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: ...
X-Tenant: tenant-a
P-Asserted-Identity: alice@example.com
```

Query parameters: `sample_rate`, `wire_format`, `sample_format`, `room_id`, `app_id`, `rtt`, `webhook_url`, `webhook_secret`. `X-*` and `P-*` request headers (plus `Authorization`) are captured into the leg's `headers` map and exposed on `leg.ringing` (as `sip_headers` for back-compat) and in `LegView.headers`.

Both `leg.ringing` and `leg.connected` are emitted back-to-back on upgrade. `leg.disconnected` fires when the WS closes — reasons: `hangup`, `timeout`, `connection_reset`, `peer_slow`, `ws_error`.

---

### MoQ Legs (experimental, PoC)

A **MoQ leg** (`moq_in`) carries **bidirectional** Opus audio over a single Media-over-QUIC session inside a WebTransport/HTTP/3 connection. Only the connection direction is fixed (client-initiated, hence `moq_in`); media flows both ways. The leg goes straight to `connected` (no ringing/answer flow), and event parity is limited to `leg.connected` / `leg.disconnected` — no DTMF, no RTT, no hold/transfer.

Reachable only on the HTTP/3 MoQ listener (`MOQ_LISTEN_ADDR`, default `:8443`), not on the regular HTTP/1.1 listener. Requires `MOQ_ENABLED=true` plus `MOQ_TLS_CERT_FILE` and `MOQ_TLS_KEY_FILE`. Speaks IETF draft-11 of moq-transport (via `mengelbart/moqtransport`); browser interop with draft-16 clients (moqtail, moq.dev) is not expected to work.

#### Wire format

One MoQ session per leg, with two fixed tracks:

- **Downlink** (server → client): server publishes namespace `mix`, track `audio` — the room mix sent to the leg.
- **Uplink** (client → server): server subscribes to namespace `mic`, track `audio` — the leg's mic into the room.

Audio is Opus, one frame per MoQ Object (LOC-style), at 48 kHz mono with a 20 ms frame size. The encoder/decoder are hard-wired to 48 kHz — the `sample_rate` query param exists but only `48000` is accepted; the room mixer handles any rate conversion. The Opus bitrate is server-controlled via the `MOQ_OPUS_BITRATE` env var (default 24000 bps).

#### Inbound: CONNECT /v1/legs/moq (WebTransport extended-CONNECT)

```
CONNECT /v1/legs/moq?sample_rate=48000&room_id=room-789 HTTP/3
:protocol: webtransport
X-Tenant: tenant-a
P-Asserted-Identity: alice@example.com
```

Use a WebTransport-capable HTTP/3 client. Standard HTTP/1.1 clients (e.g. `curl -X POST`) cannot reach this endpoint.

| Query param | Type | Required | Description |
|-------|------|----------|-------------|
| `sample_rate` | int | no | `48000` only (encoder/decoder are 48 kHz). Default 48000. |
| `room_id` | string | no | Room to auto-add the leg to once connected. Created on demand if it does not exist. |
| `app_id` | string | no | Tag the leg for event filtering. |
| `webhook_url` | string | no | Per-leg webhook URL. |
| `webhook_secret` | string | no | HMAC secret for per-leg webhook signing. |

`X-*` and `P-*` request headers (plus `Authorization`) are captured into the leg's `headers` map and surfaced on `LegView`.

**Response:**
- `200 OK` — WebTransport extended-CONNECT accepted; MoQ session established. No JSON body — the response is the upgraded WebTransport session.
- `400` — Invalid query parameters or config.
- `500` — Room create failure.
- `503` — MoQ endpoint is not enabled (`MOQ_ENABLED=false`).

`leg.connected` fires on session establishment. `leg.disconnected` fires when the MoQ session closes — reasons: `hangup`, `moq_error`.

> **OpenAPI note:** OpenAPI 3.1 does not define `connect` as a path-item method, so this operation is documented in `openapi.yaml` under `post` with an `x-actual-method: CONNECT` extension. The actual wire method is HTTP/3 extended-CONNECT.

---

### LiveKit Room Legs

VoiceBlender bridges SIP and LiveKit by joining an external LiveKit room as a participant, then mapping the LiveKit room's other participants onto VoiceBlender legs **one-to-one**. Each remote LK participant becomes its own `livekit_participant` leg in the same VoiceBlender room. The VB room mixer drives audio for everyone; there is no bespoke sum-mixer. No LiveKit SDK is used — the signaling protocol is spoken directly over WebSocket against pion's WebRTC stack.

**Participant model (Model B).** A `POST /v1/legs type=livekit_room` call creates one umbrella **publish leg** (`type: "livekit_publish"`) that owns the outbound audio direction. As remote LK participants are discovered, each becomes a **participant leg** (`type: "livekit_participant"`) registered in `LegMgr` and added to the publish leg's VoiceBlender room. Per-LK-participant operations — recording, role routing, mute/deaf, AI agents, STT, `DELETE /v1/legs/{id}` — all work natively because each LK participant *is* a real VB leg.

**Why two leg types instead of one:**

- `livekit_publish` represents what VoiceBlender publishes **to** LiveKit (mixed audio of all non-LK participants in the same VB room). Its `AudioReader` is empty (no upstream of its own); its `AudioWriter` is fed by the VB room mixer's mixed-minus-self output.
- `livekit_participant` represents one remote LK participant's audio coming **from** LiveKit. Its `AudioReader` yields decoded PCM from the LK participant's audio track; its `AudioWriter` is a discard sink (the LK side already handles outbound mixing for that participant).

**Audio feedback prevention.** The publish leg's mixer whitelist (`Hears`) is automatically maintained to include every leg in its room **except** participant legs (role `livekit_listen`). This means LK participants' audio never gets re-published to LiveKit. The whitelist is recomputed on every `leg.joined_room` / `leg.left_room` event for the publish leg's room.

**Server prerequisites:**
- `LIVEKIT_ENABLED=true`
- `LIVEKIT_URL=wss://your.livekit.server` (overridable per-request)
- *(optional)* `LIVEKIT_TOKEN_SIGNING_ENABLED=true` + `LIVEKIT_API_KEY` + `LIVEKIT_API_SECRET` to let VoiceBlender mint JWTs. **Security note:** enabling minting puts a high-privilege secret (the LiveKit API secret can mint tokens for any room/identity) inside VoiceBlender. Default is OFF — callers pass a pre-signed JWT.

#### POST /v1/legs (type=livekit_room)

Two token modes, mutually exclusive per-request. If both are present, the explicit `token` wins.

**Mode 1: caller-supplied JWT (default, no VB-side secrets):**

```json
{
  "type": "livekit_room",
  "livekit": {
    "url": "wss://lk.example.com",
    "token": "eyJhbGciOiJIUzI1NiIs..."
  },
  "room_id": "vb-room-7",
  "webhook_url": "https://app.example.com/lk-hook"
}
```

**Mode 2: VoiceBlender mints (requires `LIVEKIT_TOKEN_SIGNING_ENABLED=true`):**

```json
{
  "type": "livekit_room",
  "livekit": {
    "room": "support-call-123",
    "identity": "voiceblender-bridge",
    "participant_name": "VoiceBlender",
    "token_ttl": "30m",
    "permissions": {
      "can_publish": true,
      "can_subscribe": true
    }
  },
  "room_id": "vb-room-7"
}
```

**Top-level fields:** same `room_id`, `webhook_url`, `webhook_secret`, `app_id`, `headers` semantics as other leg types. `app_id` is inherited by every auto-created participant leg; `webhook_url` is set on the publish leg only.

**`livekit` parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `url` | string | no | LiveKit server endpoint (`wss://...`). Overrides `LIVEKIT_URL`. |
| `token` | string | yes (mode 1) | Pre-signed LiveKit JWT. When set, all other minting fields are ignored. |
| `room` | string | yes (mode 2) | LiveKit room name. Required when minting. |
| `identity` | string | yes (mode 2) | LiveKit participant identity. Required when minting. |
| `participant_name` | string | no | Display name surfaced in LK Room UIs. |
| `permissions` | object | no | LK grant flags. See below. Defaults: publish=true, subscribe=true, data=false, admin=false. |
| `token_ttl` | string | no | Go duration string (e.g. `"30m"`, `"6h"`). Used only when minting. Defaults to `LIVEKIT_DEFAULT_TOKEN_TTL` (6h). |
| `opus_bitrate` | int | no | Override `LIVEKIT_OPUS_BITRATE` for this leg. Must be 6000..510000. |

**`permissions` fields** (each optional; nil → default):

| Field | Default | Description |
|-------|---------|-------------|
| `can_publish` | `true` | Publishing local audio. |
| `can_subscribe` | `true` | Subscribing to remote tracks. |
| `can_publish_data` | `false` | Data channel. Not used by the audio bridge. |
| `room_admin` | `false` | Required for server-side admin actions on the LK room. |

**Response:** `201 Created` — Leg object for the **publish leg** (`type: "livekit_publish"`, state `connected`). Connect completes (or fails) before the HTTP response is sent. Use `GET /v1/legs?type=livekit_participant` or filter by `room_id` to enumerate the participant legs that get auto-created as LK participants join.

**Headers surfaced** in the publish leg's `LegView.headers`:
- `livekit_identity` — participant identity reported by `JoinResponse`
- `livekit_room` — LK room name

Each `livekit_participant` leg's `LegView.headers` carries:
- `livekit_identity` — the remote LK participant's identity
- `livekit_track_sid` — the LiveKit track SID that backs this leg's audio

**Lifecycle.**
- The umbrella connection is created synchronously inside `createLiveKitRoomLeg`.
- As LK ParticipantUpdate + OnTrack events arrive, the API layer auto-creates a `livekit_participant` leg per audio track, registers it in `LegMgr`, emits `leg.connected`, and adds it to the publish leg's VB room (which fires `leg.joined_room`).
- When a track is unpublished (or the LK participant disconnects), the matching participant leg is cleaned up; `leg.disconnected` fires with reason `livekit_participant_left`.
- When the umbrella signaling closes (LK leave, server shutdown, network error), participant legs are torn down first, then the publish leg; the publish leg's `leg.disconnected.reason` maps from the LiveKit `DisconnectReason` (e.g. `livekit_kicked`, `livekit_server_shutdown`, `livekit_token_expired`, `livekit_room_deleted`).

**Mute / Deaf semantics (per leg).**
- `POST /v1/legs/{publish_id}/mute` — VB stops contributing to the LK publish track. LK participants stop hearing the rest of the VB room.
- `POST /v1/legs/{participant_id}/mute` — that LK participant's audio is excluded from the VB room mix. Other VB participants stop hearing them; the LK participant itself continues to receive audio normally.
- `DELETE /v1/legs/{participant_id}` — drops the VB-side leg only. The LK participant remains in the LiveKit room (VoiceBlender just stops representing them).
- `DELETE /v1/legs/{publish_id}` — tears down the entire umbrella: every participant leg is removed first, then the publish leg, then the LK signaling Leave is sent.

**Mid-call JWT expiry.** When VoiceBlender mints, default TTL is 6h; tune via `token_ttl`. When the caller supplies the JWT, the call ends when the JWT expires (no auto-refresh in v1). Long calls should use long-lived tokens.

**Errors:**
- `400` — missing `livekit` block, missing token + signing disabled, missing room/identity in mint mode, invalid `token_ttl`, missing LiveKit URL.
- `502` — LiveKit signaling failed (bad token, server unreachable, protocol error). No events emitted; no leg registered.
- `503` — `LIVEKIT_ENABLED=false`.

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

### POST /v1/legs/{id}/ring

**Asynchronous.** Queue a SIP **180 Ringing** provisional response (no SDP) on a ringing inbound SIP leg. Use this when `SIP_AUTO_RINGING=false` (the default) and you want to indicate alerting before deciding whether to early-media or answer.

The endpoint is **idempotent in spirit** — each call emits another 180 on the wire. Receivers tolerate re-sends, and SIP retransmission rules already handle reliability of provisionals, so multiple `/ring` calls are fine.

> **Auto-ringing default:** Starting with this version, VoiceBlender does **not** send 180 Ringing automatically on inbound INVITE — only 100 Trying. The API caller drives ringing via `/ring`, `/early-media`, or `/answer`. Set `SIP_AUTO_RINGING=true` to restore the legacy "auto-180-on-INVITE" behavior.

**Request:** Optional body

```json
{
  "custom_data": { "crm": "case-7" }
}
```

| Field | Type | Description |
|---|---|---|
| `custom_data` | any (optional) | Attach opaque application JSON to this inbound leg. Omit to leave any existing value untouched; send `null` to clear it. See [Custom data](#custom-data). |

**Response:** `202 Accepted`

```json
{ "status": "ringing" }
```

SIP-level send failures surface as `leg.command_failed` with `command="ring"`.

**Errors:**
- `400` — Not a SIP inbound leg, or `custom_data` over `CUSTOM_DATA_MAX_BYTES`
- `404` — Leg not found
- `409` — Leg is not in `ringing` state (already early-media, connected, or hung up)

---

### POST /v1/legs/{id}/challenge

**Asynchronous.** Send a SIP **401 Unauthorized** carrying a digest `WWW-Authenticate` challenge on a ringing (or early-media) inbound SIP leg, instead of answering or rejecting it. Use this to authenticate inbound INVITEs on a per-call basis — for example, challenge calls from unknown source addresses while accepting trusted peers without a challenge.

Inbound calls are **not** challenged by default; a VSI/REST client decides per call (the `leg.ringing` event carries `source_address` to inform that decision). Challenging is optional and additive — existing accept/reject behavior is unchanged.

Flow:
1. You call `/challenge` on a ringing leg. VoiceBlender sends `401` with a freshly generated `nonce` and tears the current leg down (a `leg.disconnected` with `reason="challenged"` is published).
2. The UAC retries the INVITE with an `Authorization` header (same `Call-ID`). VoiceBlender verifies the digest response against the credential you supplied.
3. On success a **new** inbound leg is surfaced via `leg.ringing` with `authenticated: true` and `auth_username` set — answer/reject it as usual. On failure VoiceBlender replies `403 Forbidden` and the call is not surfaced.

VoiceBlender holds the supplied credential only in memory for the challenge's lifetime (`SIP_INBOUND_AUTH_NONCE_TTL_SECONDS`, default 60s) and never persists or returns it.

**Request:**

```json
{
  "realm": "vb.example",
  "username": "alice",
  "password": "s3cret",
  "algorithm": "MD5",
  "qop": ["auth"]
}
```

| Field | Type | Description |
|---|---|---|
| `realm` | string (required) | Digest realm advertised in the challenge. |
| `username` | string (optional) | Expected username. When set, a verified response whose username differs is rejected. When omitted, any username that verifies is accepted. |
| `password` | string | Plaintext secret. Provide this **or** `ha1`. |
| `ha1` | string | Precomputed `MD5(username:realm:password)` (hex), to avoid sending a plaintext secret. Provide this **or** `password`. |
| `algorithm` | string (optional) | `MD5` (default), `SHA-256`, or `SHA-512-256`. |
| `qop` | array (optional) | Advertised quality-of-protection, e.g. `["auth"]`. Omit to leave `qop` out of the challenge. |

**Response:** `202 Accepted`

```json
{ "status": "challenging" }
```

**Errors:**
- `400` — Not a SIP inbound leg, or missing `realm`/credential
- `404` — Leg not found
- `409` — Leg is not in `ringing` or `early_media` state

---

### POST /v1/legs/{id}/answer

**Asynchronous.** Queue the SIP 200 OK on a ringing or early-media inbound SIP leg. If the leg is in `early_media` state, the existing media pipeline and SDP are reused; if in `ringing` state, a new RTP session and codec negotiation are performed when the goroutine sends the 200 OK. Successful connection is observed via `leg.connected`.

**Request:** Optional body

```json
{
  "speech_detection": true,
  "codec": "PCMA",
  "custom_data": { "crm": "case-7" },
  "streams": [
    { "room_id": "room-translated", "role": "translator" }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `speech_detection` | bool (optional) | Override the server default for `speaking.started` / `speaking.stopped` events on this leg. Omit to use `SPEECH_DETECTION_ENABLED` (default `false`). |
| `codec` | string (optional) | Force a specific codec for the answer SDP. One of `PCMU`, `PCMA`, `G722`, `opus`, `AMR-WB`, `AMR-NB`. Must appear in the remote offer's `offered_codecs` list (see `leg.ringing`). When omitted, the server picks the first codec present in both the remote offer and the engine's supported set. Ignored when the leg is already in `early_media` state — the codec is locked in at 183. |
| `custom_data` | any (optional) | Attach opaque application JSON to this inbound leg. Omit to leave any existing value untouched; send `null` to clear it. See [Custom data](#custom-data). |
| `streams` | object[] (optional) | **SIP only.** Rooms for the caller's additional audio streams, applied once the answer is negotiated. Each entry takes `room_id` and `role`. Entries are **positional over the accepted streams beyond the primary**, in m-line order — the caller's offer decides how many exist, so an entry with no matching stream is ignored. See [Choosing a room per stream](#choosing-a-room-per-stream). |

**Response:** `202 Accepted`

```json
{ "status": "answering" }
```

**Errors:**
- `400` — Not a SIP inbound leg, invalid request body, unknown codec name, codec not in remote offer, or `custom_data` over `CUSTOM_DATA_MAX_BYTES`
- `404` — Leg not found, or a `streams[].room_id` names a room that does not exist
- `409` — Leg is not in `ringing` or `early_media` state

---

### POST /v1/legs/{id}/early-media

**Asynchronous.** Queue early-media setup on a ringing inbound SIP leg. The goroutine sends SIP 183 Session Progress with SDP, sets up the RTP session and media pipeline, and transitions the leg to `early_media` state. Once in that state, audio can be played to the caller (e.g. custom ringback tones, announcements) and the leg can be added to a room — all before answering the call. Successful transition is observed via `leg.early_media`; setup failures surface as `leg.command_failed` with `command="early_media"`.

**Request:** Optional body

```json
{
  "codec": "opus",
  "custom_data": { "crm": "case-7" }
}
```

| Field | Type | Description |
|---|---|---|
| `custom_data` | any (optional) | Attach opaque application JSON to this inbound leg. Omit to leave any existing value untouched; send `null` to clear it. See [Custom data](#custom-data). |
| `codec` | string (optional) | Force a specific codec for the 183 Session Progress SDP. One of `PCMU`, `PCMA`, `G722`, `opus`, `AMR-WB`, `AMR-NB`. Must appear in the remote offer's `offered_codecs` list. The codec chosen here is locked in for the subsequent `/answer`. When omitted, the server picks the first codec present in both the remote offer and the engine's supported set. |

**Response:** `202 Accepted`

```json
{ "status": "early_media" }
```

**Errors:**
- `400` — Not a SIP inbound leg, unknown codec name, codec not in remote offer, or `custom_data` over `CUSTOM_DATA_MAX_BYTES`
- `404` — Leg not found
- `409` — Leg is not in `ringing` state

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

### POST /v1/legs/{id}/deaf

Deafen a leg. A deaf leg does not receive mixed audio from the room — the participant cannot hear other participants. The leg can still speak (its audio is still mixed for others).

**Request:** Empty body

**Response:** `200 OK`

```json
{ "status": "deaf" }
```

**Errors:** `404` — Leg not found

---

### DELETE /v1/legs/{id}/deaf

Undeafen a leg. Restores the participant's ability to hear other participants.

**Response:** `200 OK`

```json
{ "status": "undeaf" }
```

**Errors:** `404` — Leg not found

---

### POST /v1/legs/{id}/hold

**Asynchronous.** Queue a re-INVITE with `sendonly` SDP direction. The RTP timeout is paused while held, and a 2-hour auto-hangup timer starts. Successful hold is observed via `leg.hold`; failures surface as `leg.command_failed` with `command="hold"`.

**Response:** `202 Accepted`

```json
{ "status": "holding" }
```

**Errors:**
- `404` — Leg not found
- `400` — Not a SIP leg
- `409` — Hold not supported for this leg type (e.g. WhatsApp), or leg is neither connected nor held

---

### DELETE /v1/legs/{id}/hold

**Asynchronous.** Queue a re-INVITE with `sendrecv` SDP direction. Successful resume is observed via `leg.unhold`; failures surface as `leg.command_failed` with `command="unhold"`.

**Response:** `202 Accepted`

```json
{ "status": "unholding" }
```

**Errors:**
- `404` — Leg not found
- `400` — Not a SIP leg
- `409` — Hold not supported for this leg type (e.g. WhatsApp), or leg is neither connected nor held

---

### DELETE /v1/legs/{id}

**Asynchronous.** Queue a hangup. Sends SIP BYE (or closes the WebRTC connection) on a goroutine and tears down the leg. Final disconnect is observed via `leg.disconnected`.

**Request:** Optional body

```json
{ "reason": "busy" }
```

| Field | Type | Description |
|---|---|---|
| `reason` | string (optional) | Disconnect reason. Honored only for **unanswered SIP inbound legs** (state `ringing` or `early_media`); on connected legs the body is ignored and the leg is hung up with the legacy `api_hangup` reason. The reason value flows through to `leg.disconnected`'s `cdr.reason` and selects the SIP final response sent to the caller. |

#### Reason → SIP final response (unanswered inbound only)

| `reason` | SIP response |
|---|---|
| `busy` | 486 Busy Here |
| `declined` / `rejected` | 603 Decline |
| `unavailable` | 480 Temporarily Unavailable |
| `not_found` | 404 Not Found |
| `forbidden` | 403 Forbidden |
| `server_error` | 500 Server Internal Error |

Without a body, behavior is unchanged: BYE on connected legs (`cdr.reason: "api_hangup"`), or dialog cancel on unanswered inbound legs (`cdr.reason: "caller_cancel"`).

**Response:** `202 Accepted`

```json
{ "status": "hanging_up" }
```

**Errors:**
- `400` — Unknown `reason` value
- `404` — Leg not found

---

### POST /v1/legs/{id}/transfer

Transfer a SIP leg to a third party using SIP REFER (RFC 3515). Supports both **blind** and **attended** flavours. The leg must be in `connected` state.

- **Blind transfer** — `{"target": "sip:..."}`. We send REFER inside the leg's existing dialog; the peer dials the target. Progress is reported back to us via NOTIFY sipfrag and surfaces as `leg.transfer_progress` events. On terminal 2xx (`leg.transfer_completed`) the leg is hung up automatically.
- **Attended transfer** — `{"target": "sip:...", "replaces_leg_id": "<uuid>"}`. The named leg must already be in `connected` state. Its dialog identity is embedded as a `Replaces` parameter on the Refer-To URI (RFC 3891) so the peer's INVITE replaces that dialog instead of creating a fresh one. Both legs are hung up on completion.

**Request:**

```json
{
  "target": "sip:bob@example.com",
  "replaces_leg_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `target` | string | yes | SIP URI of the third party |
| `replaces_leg_id` | string | no | Existing connected SIP leg whose dialog should be replaced (attended transfer). Omit for blind. |

**Response:** `202 Accepted`

```json
{ "status": "transfer_initiated" }
```

The REST call returns immediately after validating the request. The REFER is sent in the background and its outcome (accepted, rejected, or network error) surfaces on the event bus.

**Events emitted:** `leg.transfer_initiated` when the peer's 202 Accepted arrives, then `leg.transfer_progress` per NOTIFY sipfrag, then either `leg.transfer_completed` or `leg.transfer_failed`. If the peer rejects the REFER outright (e.g. 603 Decline), only `leg.transfer_failed` fires.

**Errors:**
- `400` — Missing or invalid `target` (including URIs without a host such as `sip:`)
- `404` — Leg not found
- `409` — Leg not connected, not a SIP leg, or `replaces_leg_id` is not a connected SIP leg

---

### Receiving a transfer (inbound REFER)

When a peer sends **us** a REFER (asks us to transfer its call), the handling depends on `SIP_REFER_AUTO_DIAL`:

- **`SIP_REFER_AUTO_DIAL=true`** — the server accepts (`202`) and **dials the target itself**, driving the NOTIFY sipfrag progress. The app is notified via `leg.transfer_requested` but does nothing.
- **`SIP_REFER_AUTO_DIAL=false` (default)** — the REFER is **parked** and surfaced as `leg.transfer_requested` (a *decision request*). The app drives the outcome with the commands below, keyed by the **referrer leg** (`{id}` = the leg that received the REFER). If no decision arrives within `SIP_REFER_CONSULT_TIMEOUT_MS` (default 2000 ms), the REFER auto-declines with `603` (fail-closed) and `leg.transfer_failed` fires.

The typical app flow: on `leg.transfer_requested`, call **accept**, perform the re-bridge (route the other party to the target), optionally report **progress**, then **complete**. If the app can't perform the transfer, call **decline** instead.

**Identity on the auto-dialled INVITE.** With `SIP_REFER_AUTO_DIAL=true`, the INVITE the server places toward the target is originated on the referrer leg's behalf:

- If the referrer leg arrived on — or was dialled over — a **registered SIP trunk**, the transfer goes out over that same trunk: `From` and `P-Asserted-Identity` carry the trunk's AOR, and the INVITE picks up the trunk's digest credentials and `Route`. The upstream that delivered the call is the one that can route the target, and it only accepts an identity it authenticated; the transferor's own caller ID would usually match no AOR and be rejected. `Referred-By`, when the referrer sent one, still identifies who asked for the transfer.
- Otherwise the leg's own identity is reused — the caller's `From` for an inbound leg, the `from` the leg was created with for an outbound one.

The resulting leg's `leg.ringing` reports both, as `from` (a full SIP URI when the host is known) and `trunk_id`.

All four return `202 Accepted` on success, or `404` when there is no matching parked/accepted transfer for the leg. The same actions are available over VSI as `accept_transfer`, `progress_transfer`, `complete_transfer`, and `decline_transfer` (payload `{ "id": "<referrer_leg_id>", ... }`).

#### POST /v1/legs/{id}/transfer/accept

Accepts the parked REFER: replies `202 Accepted` to the referrer and sends `NOTIFY 100 Trying`, keeping the refer subscription open. No body.

```json
{ "status": "accepting" }
```

#### POST /v1/legs/{id}/transfer/progress

Sends an interim sipfrag `NOTIFY` (e.g. `180 Ringing`) on an accepted transfer so the referrer's UA reflects real progress.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `status_code` | int | yes | SIP status to relay (100–699) |
| `reason` | string | no | Reason phrase; a sensible default is used when omitted |

Errors: `400` (status_code out of range), `404` (no accepted transfer for the leg).

#### POST /v1/legs/{id}/transfer/complete

Terminates an accepted transfer with a final sipfrag `NOTIFY` and emits `leg.transfer_completed` (2xx) or `leg.transfer_failed`. The referrer leg is left for the app to hang up.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `success` | bool | no | `true` sends `200 OK`; otherwise a failure NOTIFY is sent |
| `status_code` | int | no | Explicit terminal status (defaults: `200` on success, `500` on failure) |
| `reason` | string | no | Reason phrase; defaulted from `status_code` when omitted |

```json
{ "success": true }
```

#### POST /v1/legs/{id}/transfer/decline

Rejects a parked (not-yet-accepted) REFER, replying to the referrer with a non-2xx and emitting `leg.transfer_failed`.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `code` | int | no | SIP reject code (default `603`) |
| `reason` | string | no | Reason phrase (default `Decline`) |

Once a REFER has been **accepted**, use `complete` with `success:false` to signal failure — `decline` only applies before acceptance (`404` otherwise).

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

### DTMF broadcast

When a leg in a room receives DTMF (e.g. the SIP peer presses a key), VoiceBlender forwards
that digit to every other leg in the same room that has DTMF reception enabled. The originating
leg always emits its `dtmf.received` event regardless.

WebRTC legs are skipped as recipients (DTMF send over WebRTC is not yet implemented). The sending
leg is excluded from the broadcast.

DTMF reception is **on by default** for every leg. To control it:

- At originate: set `accept_dtmf: false` in the `POST /v1/legs` body.
- When adding to a room: set `accept_dtmf: false` in the `POST /v1/rooms/{id}/legs` body.
- At runtime: `POST /v1/legs/{id}/dtmf/reject` (disable) and `POST /v1/legs/{id}/dtmf/accept` (re-enable).

The current state is exposed as `accept_dtmf` on the leg view returned by `GET /v1/legs/{id}`.

---

### POST /v1/legs/{id}/dtmf/accept

Allow this leg to receive DTMF digits forwarded from other legs in the same room. Default state for new legs.

**Response:** `200 OK`

```json
{ "status": "dtmf_accepting" }
```

**Errors:**
- `404` — Leg not found

---

### POST /v1/legs/{id}/dtmf/reject

Block this leg from receiving DTMF digits forwarded from other legs in the same room. The leg's own DTMF (received from its far end) still emits a `dtmf.received` event.

**Response:** `200 OK`

```json
{ "status": "dtmf_rejecting" }
```

**Example:**

```bash
curl -X POST http://localhost:8080/v1/legs/abc-123/dtmf/reject
```

**Errors:**
- `404` — Leg not found

---

### Real-Time Text (RTT, ITU-T T.140 / RFC 4103)

VoiceBlender can negotiate an `m=text` media line alongside `m=audio` on SIP legs and exchange UTF-8 text in real time using the RFC 4103 RTP payload with RFC 2198 redundancy. Useful for accessibility (deaf / hard-of-hearing callers) and totally-conversational compliance scenarios.

- **Inbound calls** automatically accept any `m=text` section the caller offers — no configuration needed.
- **Outbound calls** offer RTT only when the originate request sets `"rtt": true` (see `POST /v1/legs`). Peers that don't speak RFC 4103 simply ignore or reject the section, and audio still negotiates normally.

WebRTC legs do not currently bridge RTT (browsers use RFC 8865 over data channels rather than RFC 4103 over RTP).

---

### POST /v1/legs/{id}/rtt

Send a chunk of UTF-8 text on the leg's RTT stream. Requires that the SDP exchange agreed on `m=text`.

**Request:**

```json
{ "text": "hello\n" }
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `text` | string | yes | UTF-8 text. May include T.140 control codes such as backspace (``) or CR/LF. |

**Response:** `200 OK`

```json
{ "status": "sent" }
```

**Example:**

```bash
curl -X POST http://localhost:8080/v1/legs/abc-123/rtt \
  -H 'Content-Type: application/json' \
  -d '{"text":"hello"}'
```

**Errors:**
- `400` — Invalid JSON or empty text
- `404` — Leg not found
- `409` — RTT was not negotiated for this leg (peer didn't include `m=text`, or `RTT_ENABLED=false`)
- `500` — Send failed

---

### POST /v1/legs/{id}/rtt/accept

Allow this leg to receive RTT text broadcast from other legs in the same room and to emit `rtt.received` events. Default for new legs.

**Response:** `200 OK { "status": "rtt_accepting" }`

---

### POST /v1/legs/{id}/rtt/reject

Block this leg from receiving RTT text broadcast from other legs and suppress `rtt.received` events for it.

**Response:** `200 OK { "status": "rtt_rejecting" }`

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

### PATCH /v1/legs/{id}/play/{playbackID}

Change the volume of an active leg playback. Takes effect immediately on the next audio frame. The new level persists for the lifetime of the playback, including across loop iterations.

**Request:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `volume` | integer | yes | Volume adjustment (-8 to 8, ~3dB per step, 0 = unchanged) |

**Response:** `200 OK`

```json
{ "status": "ok" }
```

**Errors:**
- `400` — Invalid JSON or volume out of range
- `404` — Playback not found

---

### POST /v1/legs/{id}/tts

Synthesize speech and play it on a leg.

Transient upstream failures (429, 500/502/503/504, transport timeouts) are retried up to 3 times with jittered backoff, bounded to 5 seconds in total, before `tts.error` is published. Auth failures, rejected input, and unclassifiable errors are not retried.

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

**Request (Deepgram TTS):**

```json
{
  "text": "Hello, how can I help you?",
  "voice": "aura-2-asteria-en",
  "provider": "deepgram",
  "volume": 0
}
```

**Request (Azure TTS):**

```json
{
  "text": "Hello, how can I help you?",
  "voice": "en-US-JennyNeural",
  "provider": "azure",
  "volume": 0
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `text` | string | yes | Text to synthesize |
| `voice` | string | yes | Provider-specific voice identifier. ElevenLabs: voice name or ID. AWS Polly: voice ID (e.g. `Joanna`, `Matthew`). Google Cloud: voice name — either full format (e.g. `en-US-Neural2-F`) or short name for Gemini models (e.g. `Achernar`, `Kore`). Deepgram: model name (e.g. `aura-2-asteria-en`). Azure: voice name (e.g. `en-US-JennyNeural`, `pl-PL-MarekNeural`). |
| `provider` | string | no | TTS provider: `"elevenlabs"` (default), `"aws"`, `"google"`, `"deepgram"`, or `"azure"` |
| `model_id` | string | no | Provider-specific model/engine. ElevenLabs: model ID. AWS Polly: engine (`standard`, `neural`, `long-form`, `generative`; default `neural`). Google Cloud: model name (e.g. `gemini-2.5-pro-tts`, `chirp3-hd`). Not used for Deepgram or Azure (voice selects the model). |
| `language` | string | no | Language code (e.g. `"en-US"`, `"pl-pl"`). Required for Google Gemini TTS voices that use short names (e.g. `Achernar`). Auto-extracted from full voice names like `en-US-Neural2-F` or `en-US-JennyNeural`. |
| `prompt` | string | no | Style/tone instruction for promptable voice models (Google Gemini TTS only). E.g. `"Read aloud in a warm, welcoming tone."` |
| `volume` | integer | no | Volume adjustment in dB (`-8` to `8`, default `0`) |
| `api_key` | string | no | ElevenLabs: API key override (falls back to `ELEVENLABS_API_KEY` env var). AWS: optional `ACCESS_KEY:SECRET_KEY` override (falls back to default AWS credential chain). Google Cloud: optional API key override (falls back to Application Default Credentials). Deepgram: API key override (falls back to `DEEPGRAM_API_KEY` env var). Azure: subscription key override (falls back to `AZURE_SPEECH_KEY` env var). |

**Providers:**
- `elevenlabs` — ElevenLabs streaming TTS API (default). Requires an API key.
- `aws` — Amazon Polly. Uses the default AWS credential chain (env vars, IAM role, shared credentials file). No API key required unless overriding credentials per-request.
- `google` — Google Cloud Text-to-Speech. Uses Application Default Credentials (ADC). No API key required unless overriding per-request. Supports all voice types: Standard, WaveNet, Neural2, Studio, Chirp 3 HD, and Gemini TTS. For Gemini models (e.g. `gemini-2.5-pro-tts`), set `model_id` and `language` explicitly; use `prompt` for style instructions.
- `deepgram` — Deepgram TTS API. Requires an API key. The `voice` field selects the model (e.g. `aura-2-asteria-en`).
- `azure` — Azure Cognitive Speech Services. Requires a subscription key (`AZURE_SPEECH_KEY`). Voice names follow the `{lang}-{region}-{Name}` pattern (e.g. `en-US-JennyNeural`). Language is auto-extracted from the voice name.

**Response:** `200 OK`

```json
{ "tts_id": "tts-a1b2c3d4", "status": "playing" }
```

Events `tts.started` and `tts.finished` are emitted.

**Caching:** When `TTS_CACHE_ENABLED=true`, identical requests (same text, voice, model, language, and prompt) are served from the disk cache stored in `TTS_CACHE_DIR`, skipping the external provider call. The cache persists across restarts; to clear it, delete the files in that directory. Set `TTS_CACHE_INCLUDE_API_KEY=true` to scope the cache per API key (needed when different keys access different voice clones).

**Errors:**
- `400` — Invalid JSON, missing text/voice, volume out of range
- `404` — Leg not found
- `409` — Leg has no audio writer
- `503` — No API key provided for the selected provider

---

### Preflight TTS (speculative synthesis)

`POST /v1/legs/{id}/tts` starts playing as soon as the provider returns audio,
so the synthesis round-trip sits on the critical path between "the caller
finished speaking" and "the agent starts speaking". Preflight moves that cost
off the critical path: the utterance is synthesized and **buffered in memory
without playing**, and a later commit starts playback immediately.

It exists for the turn-taking loop of a voice agent, and pairs with the
`eager_end_of_turn` signal from [conversational turn
detection](#conversational-turn-detection):

```
stt.turn event=eager_end_of_turn   ->  generate a draft reply, POST .../tts/preflight
stt.turn event=turn_resumed        ->  DELETE .../tts/{ttsID}          (caller kept talking)
stt.turn event=end_of_turn         ->  POST .../tts/{ttsID}/commit     (play the draft now)
```

Preflight is **leg-scoped**. A room mix has several speakers and no single turn
to speculate on, so `POST /v1/rooms/{id}/tts` has no preflight equivalent — use
it directly for room announcements.

#### POST /v1/legs/{id}/tts/preflight

Same request body as `POST /v1/legs/{id}/tts`, including `provider`, `voice`,
`model_id`, `language`, `prompt`, `volume` and `api_key`.

**Response:** `200 OK`

```json
{ "tts_id": "tts-a1b2c3d4", "status": "staged" }
```

The response returns before synthesis completes. A `tts.staged` event reports
when the audio is buffered and committing it will be instant:

```json
{ "type": "tts.staged", "leg_id": "550e8400-...", "tts_id": "tts-a1b2c3d4",
  "bytes": 32000, "duration_ms": 1000 }
```

A synthesis failure is reported on `tts.error` with the usual `category`, and
the staged id is dropped.

**Errors:**
- `400` — Invalid JSON, missing text/voice, volume out of range
- `404` — Leg not found
- `409` — Leg has no audio writer, or `TTS_PREFLIGHT_MAX_PER_LEG` staged
  utterances already exist on this leg. The cap **refuses rather than evicts**:
  dropping the oldest could silently destroy an utterance you were about to
  commit.
- `503` — No API key provided for the selected provider

#### POST /v1/legs/{id}/tts/{ttsID}/commit

Play a staged utterance. No request body.

**Response:** `200 OK`

```json
{ "tts_id": "tts-a1b2c3d4", "status": "committed" }
```

Commit **never blocks and never answers "not ready yet"** — committing before
`tts.staged` has arrived is legal, and playback begins the moment the audio
lands. This makes commit a drop-in for `POST /v1/legs/{id}/tts`, which likewise
returns before any audio exists and reports failure asynchronously. From here
the normal `tts.started` / `tts.finished` / `tts.error` lifecycle applies.

Once committed, stop playback with `DELETE /v1/legs/{id}/play/{playbackID}`
using the same `tts_id` — a committed utterance is an ordinary playback.

**Errors:**
- `404` — No staged TTS with that id (never staged, already discarded, or expired)
- `409` — Already committed

#### DELETE /v1/legs/{id}/tts/{ttsID}

Drop a staged utterance without playing it, aborting synthesis if it is still in
flight so you stop paying for audio nobody will hear.

**Response:** `200 OK`

```json
{ "status": "discarded" }
```

A `tts.discarded` event follows with `reason`:

| `reason` | Meaning |
|----------|---------|
| `app` | Explicitly discarded through this endpoint |
| `expired` | `TTS_PREFLIGHT_TTL` elapsed while staged |
| `leg_gone` | The leg ended while the utterance was staged |

**Errors:**
- `404` — No staged TTS with that id
- `409` — Already committed. Use `DELETE /v1/legs/{id}/play/{playbackID}` with
  the same `tts_id` to stop it instead.

#### Worked example

```bash
LEG_ID=550e8400-...

# 1. Start Flux STT with eager end-of-turn enabled.
curl -X POST http://localhost:8080/v1/legs/$LEG_ID/stt \
  -H 'Content-Type: application/json' \
  -d '{"provider":"deepgram_flux","eager_eot_threshold":0.4}'

# 2. On stt.turn event=eager_end_of_turn, stage the draft reply.
TTS_ID=$(curl -sX POST http://localhost:8080/v1/legs/$LEG_ID/tts/preflight \
  -H 'Content-Type: application/json' \
  -d '{"text":"Sure, I can reset that for you.","voice":"Rachel"}' | jq -r .tts_id)

# 3a. On stt.turn event=turn_resumed — the caller kept talking, throw it away.
curl -X DELETE http://localhost:8080/v1/legs/$LEG_ID/tts/$TTS_ID

# 3b. On stt.turn event=end_of_turn — speak now, with no synthesis wait.
curl -X POST http://localhost:8080/v1/legs/$LEG_ID/tts/$TTS_ID/commit
```

**Configuration:** `TTS_PREFLIGHT_TTL` (default `30s`),
`TTS_PREFLIGHT_MAX_PER_LEG` (default `3`), `TTS_PREFLIGHT_MAX_BYTES` (default
4 MiB). The TTL is a leak backstop, not a policy knob — a speculative reply is
normally committed or discarded within a second. Audio over the byte cap is
rejected with `tts.error` and `category: "permanent_input"`.

---

### POST /v1/legs/{id}/record

Start recording a leg to a WAV file.

For SIP legs, recording is **stereo** (16-bit PCM at the codec's native sample rate):
- **Left channel** — incoming audio (what the remote party says)
- **Right channel** — outgoing audio (what we send, including agent TTS)

For legs in a room, recording is stereo at 16kHz:
- **Left channel** — participant's incoming audio (before mix)
- **Right channel** — mixed-minus-self (what the participant hears)

Both channels are written on the recorder's own 20 ms clock, so the file always
advances in real time and the two channels stay sample-aligned. A stretch where
one side sends nothing — a call on hold, an outbound DTMF burst, a deafened room
participant, or plain packet loss — appears as silence on that channel for
exactly as long as it lasted, and never shortens the recording or shifts the
other channel.

Leg types with a single audio stream record mono, on the same clock and with the
same guarantee.

**Request:**

```json
{
  "storage": "s3",
  "filename": "c3ef9e71-6c9c-43a0-9a55-c51b3894a03c",
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
| `storage` | string | no | `"file"` (default) — local disk, `"s3"` — upload to S3 after recording stops, `"gcs"` — upload to Google Cloud Storage via the native GCS API |
| `filename` | string | no | Optional output basename for the WAV file. A `.wav` suffix is added when missing. Must be a single path segment (no directories). Dots inside the name are preserved — only a trailing `.wav` is treated as the extension (e.g. `call.v2` → `call.v2.wav`). When omitted, a timestamped name is generated. |
| `s3_bucket` | string | no | S3 bucket name. Overrides `S3_BUCKET` env var. Required if env var is not set. |
| `s3_region` | string | no | AWS region. Overrides `S3_REGION` env var. Default `us-east-1`. |
| `s3_endpoint` | string | no | Custom S3 endpoint (MinIO, etc.). Overrides `S3_ENDPOINT` env var. |
| `s3_prefix` | string | no | Key prefix (e.g. `recordings/`). Overrides `S3_PREFIX` env var. |
| `s3_access_key` | string | no | AWS access key ID. Overrides default credential chain. |
| `s3_secret_key` | string | no | AWS secret access key. Must be set together with `s3_access_key`. |
| `gcs_bucket` | string | no | GCS bucket name. Overrides `GCS_BUCKET` env var. Required if env var is not set when `storage=gcs`. |
| `gcs_object_name_prefix` | string | no | Object name prefix (e.g. `recordings` or `recordings/`). Overrides `GCS_OBJECT_NAME_PREFIX` env var. A trailing slash is added automatically when missing. |

When `s3_bucket` / `gcs_bucket` is provided, a per-request backend is created using the supplied config. Otherwise the matching server-level backend (from env vars) is used. GCS credentials come from Application Default Credentials / Workload Identity (same chain as Google Cloud TTS).

Creating a per-request S3 backend probes the bucket with a bounded `HeadBucket` call, so a bucket that does not exist returns `400` here instead of failing later at upload. There is no equivalent probe for `gcs_bucket`: a GCS bucket that does not exist surfaces at upload, in the log and in `recording.finished` keeping the local path. A probe that cannot get a verdict (no `s3:ListBucket` permission, a `5xx`, an unreachable endpoint, an expired budget) is only logged, and recording starts normally. An `http://` `s3_endpoint` on a non-local host returns `400` unless the server runs with `S3_ALLOW_INSECURE_ENDPOINT=true`; loopback and private endpoints need no opt-in.

**Response:** `200 OK`

```json
{
  "status": "recording",
  "file": "/tmp/recordings/c3ef9e71-6c9c-43a0-9a55-c51b3894a03c.wav"
}
```

Recording runs asynchronously. Events `recording.started` and `recording.finished` are emitted. When `storage=s3`, the `file` field in the stop response and the `recording.finished` event will contain an `s3://bucket/key` URI. When `storage=gcs`, it will contain a `gs://bucket/object` URI.

The `file` path above does **not** exist while the recording is in progress: the recording is written to a staging file and only appears at this path once it stops, so it is never observed half-written. Read it after `recording.finished`, not during the call.

Custom `filename` values are exclusive: starting a recording when that path already exists on disk, or while another recording has reserved the same name, returns `409` rather than overwriting the earlier file.

**Example — name the WAV after a call id:**

```bash
curl -X POST http://localhost:8080/v1/legs/$LEG_ID/record \
  -H 'Content-Type: application/json' \
  -d '{"filename":"c3ef9e71-6c9c-43a0-9a55-c51b3894a03c"}'
```

**Errors:**
- `400` — Invalid storage type, S3/GCS not configured, invalid credentials, or invalid `filename`
- `404` — Leg not found
- `409` — Leg has no audio reader, or `filename` already exists / in use
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

| Field | Type | Description |
|-------|------|-------------|
| `file` | string | Path/URI of the recording. Empty when the capture was discarded and no file was written — see below. |

A capture that fails mid-write, or that never captures a frame, is discarded:
nothing is written and no file exists. The stop still succeeds with
`status: stopped` — the recording did stop — but `file` comes back as `""`, as
it does in the `recording.finished` event. Treat an empty `file` as "there is no
recording to fetch", not as a path: it is the one case where a `200` produces no
artefact. A non-empty `file` always names something that exists.

**Errors:** `404` — No recording in progress

---

### POST /v1/legs/{id}/record/pause

Pause the active recording for a leg. While paused, the WAV continues to advance in real time but the audio is replaced with silence, so the file preserves the full session duration with a clearly silent gap where sensitive data was excluded (e.g. credit-card capture, PII exchange). Both sides of a stereo recording are silenced together.

Idempotent: calling while already paused returns `status: already_paused`.

**Response:** `200 OK`

```json
{"status": "paused"}
```

or, if already paused:

```json
{"status": "already_paused"}
```

Emits a `recording.paused` event.

**Errors:** `404` — No recording in progress

---

### POST /v1/legs/{id}/record/resume

Resume a previously paused leg recording. Idempotent: calling while not paused returns `status: not_paused`.

**Response:** `200 OK`

```json
{"status": "resumed"}
```

Emits a `recording.resumed` event.

**Errors:** `404` — No recording in progress

**Example — pause around sensitive data:**

```bash
# Start recording
curl -X POST http://localhost:8080/v1/legs/$LEG_ID/record

# ... agent collects call details ...

# Pause before asking for credit card
curl -X POST http://localhost:8080/v1/legs/$LEG_ID/record/pause

# ... agent collects card number + CVV ...

# Resume for the rest of the call
curl -X POST http://localhost:8080/v1/legs/$LEG_ID/record/resume

# Stop when done
curl -X DELETE http://localhost:8080/v1/legs/$LEG_ID/record
```

---

### POST /v1/legs/{id}/stt

Start real-time speech-to-text transcription on a leg.

**Request:**

```json
{
  "language": "en",
  "partial": true,
  "provider": "elevenlabs"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `language` | string | no | Language code (e.g. `"en"`, `"es"`) |
| `partial` | boolean | no | Emit partial (non-final) transcripts |
| `provider` | string | no | STT provider: `"elevenlabs"` (default), `"deepgram"`, `"deepgram_flux"`, `"azure"`, or `"speechmatics"` |
| `api_key` | string | no | API key override (falls back to `ELEVENLABS_API_KEY`, `DEEPGRAM_API_KEY`, `AZURE_SPEECH_KEY`, or `SPEECHMATICS_API_KEY` env var depending on provider) |
| `model` | string | no | Provider-specific model. Deepgram: default `"nova-3"`. Deepgram Flux: `"flux-general-en"` (default, or `"flux-general-multi"` when `language_hints` is given) or `"flux-general-multi"`. Speechmatics: `"standard"` (default) or `"enhanced"`. |
| `keyterms` | string[] | no | Terms to boost recognition of (Deepgram, Deepgram Flux, and Speechmatics — where they become `additional_vocab`) |
| `endpointing` | integer | no | **Deepgram:** milliseconds of silence before a segment is finalized; `0` disables endpointing. **Speechmatics:** maps to `max_delay`, clamped to 700–4000 ms; `0` leaves the provider default. |
| `utterance_end_ms` | integer | no | **Deepgram:** milliseconds of silence after which an `stt.turn` event with `event: "utterance_end"` is emitted. **Speechmatics:** milliseconds of silence that close a turn and emit an `stt.turn` event with `event: "end_of_turn"` — default `600`, capped at `2000`, and `0` disables turn detection. |
| `eager_eot_threshold` | number | no | **Deepgram Flux only.** End-of-turn confidence (0.3–0.9) that fires an `eager_end_of_turn` `stt.turn` event. **When unset, no `eager_end_of_turn` or `turn_resumed` events are emitted at all.** |
| `eot_threshold` | number | no | **Deepgram Flux only.** End-of-turn confidence required to close a turn (0–1). Deepgram default `0.7`. |
| `eot_timeout_ms` | integer | no | **Deepgram Flux only.** Milliseconds of silence after which a turn closes regardless of confidence. Deepgram default `5000`. |
| `language_hints` | string[] | no | **Deepgram Flux only.** Candidate language codes. Selects `"flux-general-multi"` when `model` is not given, since that is the only model Deepgram accepts them on. Ignored when `model` names a different one. |

Fields that do not apply to the selected provider are ignored, so switching
providers never turns a previously valid request into an error.

**Providers:**
- `elevenlabs` — ElevenLabs real-time STT via WebSocket (default). Uses Scribe v2 model.
- `deepgram` — Deepgram real-time STT (`/v1/listen`) via WebSocket. Uses Nova-3 model. Audio is sent as raw binary PCM frames.
- `deepgram_flux` — Deepgram Flux (`/v2/listen`), a conversational model that reports **turn boundaries** rather than plain segments. Emits `stt.turn` events (see [Conversational turn detection](#conversational-turn-detection)) alongside one `stt.text` final per turn. Shares `DEEPGRAM_API_KEY` with `deepgram`. Does not support `POST /v1/legs/{id}/stt/finalize` (`501`) — Flux reports turn ends itself.
- `azure` — Azure Cognitive Speech Services real-time STT via WebSocket. Requires a subscription key (`AZURE_SPEECH_KEY`) and region (`AZURE_SPEECH_REGION`). Language defaults to `"en-US"`.
- `speechmatics` — Speechmatics Realtime v2 via WebSocket. Requires `SPEECHMATICS_API_KEY`; the endpoint comes from `SPEECHMATICS_URL`, so another region or a self-hosted realtime container needs no code change. Reports **turn boundaries** on `stt.turn` (see [Conversational turn detection](#conversational-turn-detection)) and is, alongside `deepgram`, one of the two providers that support `POST /v1/legs/{id}/stt/finalize`.

**Response:** `200 OK`

```json
{ "status": "stt_started", "leg_id": "550e8400-..." }
```

Transcripts are delivered via `stt.text` webhook events (and over `/v1/vsi`).

A leg whose m-line 0 is another party's audio rather than the call — a SIPREC
recording session — is refused with `409`: transcribe its room instead, which
starts one transcriber per stream. Reading such a leg's audio directly would take
frames away from the mixer that is already carrying them.

**Errors:**
- `404` — Leg not found
- `409` — Leg not connected, STT already running, or leg has no audio reader
- `503` — No API key provided for the selected provider

---

### DELETE /v1/legs/{id}/stt

Stop speech-to-text on a leg.

**Response:** `200 OK`

```json
{ "status": "stt_stopped" }
```

**Errors:** `404` — No STT in progress

---

### POST /v1/legs/{id}/stt/finalize

Flush the STT buffer on a leg and force a final transcript for the audio spoken
so far. **STT keeps running** — the provider session is not closed and no new
`POST /v1/legs/{id}/stt` is needed afterwards. Use it when the caller already
knows the speaker has finished (a barge-in, a push-to-talk release, an agent
turn boundary) and does not want to wait for the provider's own endpointing.

No request body.

**Response:** `200 OK`

```json
{ "status": "stt_finalized" }
```

**Provider support:** `deepgram` and `speechmatics`. VoiceBlender's ElevenLabs
integration commits on its own voice-activity detection and its Azure
integration has no mid-stream flush, so both answer `501`. `deepgram_flux` also
answers `501`: `/v2/listen` has no flush message, and Flux already reports turn
ends on `stt.turn`.

**Notes:**
- The flushed transcript arrives on the usual `stt.text` event with
  `is_final: true`. A segment containing **no speech produces no event at
  all** — a `200` here is an acknowledgement that the flush was requested, not
  a promise that a transcript follows. Do not block waiting for one.
- On `speechmatics` the flush is a forced end of utterance, so it also closes
  the turn: an `stt.turn` event with `event: "end_of_turn"` follows the final,
  and the next `turn_index` starts a fresh utterance.
- Applies to leg-scoped STT started with `POST /v1/legs/{id}/stt`. A leg being
  transcribed as part of a room STT session is not tracked per leg and returns
  `404`. There is no room-level finalize.

**Errors:**
- `404` — No STT in progress on this leg
- `409` — The STT session is not connected, or the flush could not be written
- `501` — The active STT provider does not support finalize

---

### Conversational turn detection

`stt.text` answers "what was said". It does not answer "has the caller finished
speaking?" — `is_final` means a *segment* will not change again, which is not
the same thing. The `stt.turn` event carries that second signal.

Three providers report it, at different levels of detail.

#### Deepgram Flux (`provider: "deepgram_flux"`)

Flux models the conversation as a turn state machine and reports every
transition:

```
                 start_of_turn            eager_end_of_turn
      (initial) ---------------> (ongoing) ----------------> (awaiting end)
          ^                          ^                          |
          |                          |    turn_resumed          |
          |                          +--------------------------+
          |                                                     |
          +-------------------- end_of_turn --------------------+
```

| `event` | Meaning |
|---------|---------|
| `start_of_turn` | Speech detected; always carries a non-empty transcript. A good barge-in trigger. |
| `update` | Transcript progress, roughly every 250 ms. Only emitted when `partial: true`. |
| `eager_end_of_turn` | The caller has *probably* finished. Revocable. Only emitted when `eager_eot_threshold` is set. |
| `turn_resumed` | The caller kept talking after an `eager_end_of_turn` — discard anything drafted from it. |
| `end_of_turn` | The turn is closed. Its transcript always matches the immediately preceding `eager_end_of_turn`. |

```json
{
  "type": "stt.turn",
  "leg_id": "550e8400-...",
  "event": "eager_end_of_turn",
  "turn_index": 2,
  "text": "how do I reset my password",
  "end_of_turn_confidence": 0.82,
  "audio_window_start_ms": 0,
  "audio_window_end_ms": 1500,
  "words": [{ "word": "how", "confidence": 0.99, "start_ms": 100, "end_ms": 400 }]
}
```

**`eager_eot_threshold` is opt-in.** Without it Flux emits no
`eager_end_of_turn` and no `turn_resumed`, so there is nothing to speculate on
and [preflight TTS](#preflight-tts-speculative-synthesis) has no head start to
work with. Valid values are 0.3–0.9: lower fires earlier and more often (faster
replies, more wasted drafts), higher fires later and less often.

**Compatibility with `stt.text`.** Flux keeps emitting `stt.text`, so an
existing app can switch to `provider: "deepgram_flux"` without changing how it
consumes transcripts. The mapping is:

| Flux event | `stt.turn` | `stt.text` |
|------------|-----------|------------|
| `start_of_turn` | always | interim, only if `partial: true` |
| `update` | only if `partial: true` | interim, only if `partial: true` |
| `eager_end_of_turn` | always | interim, only if `partial: true` |
| `turn_resumed` | always | *(none)* |
| `end_of_turn` | always | **`is_final: true`, `speech_final: true`** |

An `eager_end_of_turn` **never** produces `is_final: true`, because
`turn_resumed` can revoke it — an app that accumulates on `is_final` would
otherwise corrupt its transcript. Net effect: exactly one final `stt.text` per
turn, instead of one per segment.

#### Deepgram (`provider: "deepgram"`)

The `/v1/listen` model has no turn state machine, but reports two narrower
signals:

- **`speech_final`** on `stt.text` — the speaker stopped, as distinct from
  `is_final`'s "this segment is settled". Tune it with `endpointing`.
- **`utterance_end`** on `stt.turn`, when `utterance_end_ms` is set. It carries
  no transcript, only `last_word_end_ms`. Use it as the reliable
  speaker-stopped signal for callers who never pause long enough for
  `speech_final` to fire.

```json
{ "type": "stt.turn", "leg_id": "550e8400-...", "event": "utterance_end", "last_word_end_ms": 2395 }
```

Deepgram requires interim results to emit `UtteranceEnd`, so setting
`utterance_end_ms` turns them on at the provider. VoiceBlender still suppresses
them locally unless you also set `partial: true` — adding `utterance_end_ms`
never starts sending you partial transcripts you did not ask for.

#### Speechmatics (`provider: "speechmatics"`)

Speechmatics detects the end of an utterance server-side from word timings: a
countdown starts at each recognized word and restarts on the next one, so a
turn closes only after a real pause. VoiceBlender enables it by default with a
**600 ms** trigger and reports it as a single `stt.turn` event:

| `event` | Meaning |
|---------|---------|
| `end_of_turn` | The caller stopped speaking. Carries the whole utterance, its audio window, and word-level timings. |

```json
{
  "type": "stt.turn",
  "leg_id": "550e8400-...",
  "event": "end_of_turn",
  "turn_index": 0,
  "text": "How do I reset my password?",
  "audio_window_start_ms": 0,
  "audio_window_end_ms": 2400,
  "words": [{ "word": "How", "confidence": 0.95, "start_ms": 100, "end_ms": 300 }]
}
```

Tune it with `utterance_end_ms`: 500–800 ms suits most voice agents, 800–1200 ms
suits dictation, and `0` turns turn detection off entirely. Values above
`2000` are clamped.

**Compatibility with `stt.text`.** Speechmatics finalizes a *segment* every
`max_delay` (tuned with `endpointing`), so one turn usually spans several
`stt.text` finals. The `end_of_turn` event carries their concatenation:

| Speechmatics message | `stt.turn` | `stt.text` |
|----------------------|-----------|------------|
| `AddPartialTranscript` | *(none)* | interim, only if `partial: true` |
| `AddTranscript` | *(none)* | **`is_final: true`** |
| `EndOfUtterance` | `end_of_turn` | *(none)* |

`speech_final` is always `false` on this provider. Speechmatics sends the final
segment *before* the end-of-utterance signal, so at the moment a transcript is
emitted it is not yet known whether the speaker has stopped — use `stt.turn`
for that. `POST /v1/legs/{id}/stt/finalize` forces the same `end_of_turn`
early.

**Example:**

```bash
curl -X POST localhost:8080/v1/legs/$LEG_ID/stt \
  -H 'Content-Type: application/json' \
  -d '{
        "provider": "speechmatics",
        "language": "en",
        "partial": true,
        "model": "enhanced",
        "utterance_end_ms": 600,
        "endpointing": 1000,
        "keyterms": ["VoiceBlender", "SIPREC"]
      }'
```

Neither ElevenLabs nor Azure reports turn boundaries: they emit no `stt.turn`
events, and `speech_final` on their `stt.text` events is always `false`.

#### Where in the audio it was said

`stt.text` carries `audio_start_ms` and `audio_end_ms`: the span the transcript
covers, in milliseconds from the first audio the transcriber was given. Both are
absent when the provider reports no timing (ElevenLabs, Azure).

Use these rather than the event's arrival time to line a transcript up against a
recording. A provider with a turn detector reports a turn when the turn *ends*,
so arrival time places a sentence after the words rather than on them, and the
further out the longer the speaker went on.

```json
{ "type": "stt.text", "leg_id": "550e8400-...", "text": "hello there",
  "is_final": true, "speech_final": true, "audio_start_ms": 7200, "audio_end_ms": 8100 }
```

For Deepgram Flux the span is the first and last word's own timings where the
turn has words, and its audio window otherwise — the window opens before the
speaker does, so seeking to it lands on silence.

For Speechmatics the span is the segment's own window, so a turn split across
several finals gives each final its own slice rather than the whole utterance.

---

### POST /v1/legs/{id}/agent/elevenlabs

Attach an ElevenLabs ConvAI agent to a leg.

**Request:**

```json
{
  "agent_id": "abc123",
  "first_message": "Hello!",
  "language": "en",
  "dynamic_variables": { "name": "Alice" },
  "api_key": "xi-..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agent_id` | string | yes | ElevenLabs agent ID |
| `first_message` | string | no | Override the agent's first message |
| `language` | string | no | Language code (e.g. `"en"`, `"es"`) |
| `dynamic_variables` | object | no | Key-value pairs passed to the agent as dynamic variables |
| `api_key` | string | no | API key override (falls back to `ELEVENLABS_API_KEY` env var) |

**Response:** `200 OK` — `{ "status": "agent_started", "leg_id": "..." }`

**Errors:** `400` — Invalid JSON or missing agent_id · `404` — Leg not found · `409` — Leg not connected, agent already attached, or no audio reader/writer · `503` — No API key

---

### POST /v1/legs/{id}/agent/vapi

Attach a VAPI agent to a leg.

**Request:**

```json
{
  "assistant_id": "asst_xyz",
  "first_message": "Hello!",
  "variable_values": { "name": "Alice" },
  "api_key": "vapi-..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `assistant_id` | string | yes | VAPI assistant ID |
| `first_message` | string | no | Override the agent's first message |
| `variable_values` | object | no | Key-value pairs passed as VAPI variable values |
| `api_key` | string | no | API key override (falls back to `VAPI_API_KEY` env var) |

**Response:** `200 OK` — `{ "status": "agent_started", "leg_id": "..." }`

**Errors:** `400` — Invalid JSON or missing assistant_id · `404` — Leg not found · `409` — Leg not connected, agent already attached, or no audio reader/writer · `503` — No API key

---

### POST /v1/legs/{id}/agent/pipecat

Attach a self-hosted Pipecat bot to a leg. Audio is exchanged as protobuf-encoded binary frames (16kHz 16-bit PCM mono). No API key required.

**Request:**

```json
{
  "websocket_url": "ws://my-bot:8765"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `websocket_url` | string | yes | WebSocket URL of the Pipecat bot |

**Response:** `200 OK` — `{ "status": "agent_started", "leg_id": "..." }`

**Errors:** `400` — Invalid JSON or missing websocket_url · `404` — Leg not found · `409` — Leg not connected, agent already attached, or no audio reader/writer

---

### POST /v1/legs/{id}/agent/deepgram

Attach a Deepgram Voice Agent to a leg. Audio is exchanged as raw binary PCM frames (16kHz 16-bit PCM mono).

**Request:**

```json
{
  "settings": {
    "agent": {
      "listen": { "provider": { "type": "deepgram", "model": "nova-3" } },
      "think": { "provider": { "type": "open_ai", "model": "gpt-4o-mini" } },
      "speak": { "provider": { "type": "deepgram", "model": "aura-2-asteria-en" } }
    }
  },
  "greeting": "Hello!",
  "language": "en",
  "api_key": "dg-..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `settings` | object | no | Full Deepgram agent settings object. When omitted, sensible defaults are used (nova-3 STT, gpt-4o-mini LLM, aura-2-asteria-en TTS). |
| `greeting` | string | no | Agent greeting message |
| `language` | string | no | Language code (e.g. `"en"`, `"es"`) |
| `api_key` | string | no | API key override (falls back to `DEEPGRAM_API_KEY` env var) |

**Response:** `200 OK` — `{ "status": "agent_started", "leg_id": "..." }`

**Errors:** `400` — Invalid JSON · `404` — Leg not found · `409` — Leg not connected, agent already attached, or no audio reader/writer · `503` — No API key

---

**Agent notes (all providers):**
- **Standalone leg:** Agent reads/writes audio directly with resampling to 16kHz.
- **Leg in a room:** Agent hears only that leg (via mixer tap) and speaks to everyone (via playback source).
- Agent events (`agent.connected`, `agent.disconnected`, `agent.user_transcript`, `agent.agent_response`) are delivered via webhooks.

---

### POST /v1/legs/{id}/agent/message

Inject a context message or instruction into a running agent session on a leg. This is provider-agnostic — the session routes the message using the appropriate provider mechanism.

**Supported providers:**
- **Deepgram** — sends `InjectAgentMessage` via WebSocket
- **Pipecat** — sends a protobuf `TextFrame` via WebSocket
- **VAPI** — sends `add-message` via HTTP control URL
- **ElevenLabs** — not supported (returns `501`)

**Request:**

```json
{
  "message": "The customer's name is John and their order number is 12345."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `message` | string | yes | Context or instruction to inject into the running agent session |

**Response:** `200 OK`

```json
{ "status": "message_sent" }
```

**Errors:** `400` — Invalid JSON or missing message · `404` — No agent attached to this leg · `409` — Agent session not running · `501` — Provider does not support message injection

---

### DELETE /v1/legs/{id}/agent

Detach the agent from a leg (provider-agnostic).

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
  "sample_rate": 16000,
  "participants": [
    { "id": "leg-uuid", "type": "sip_inbound", "state": "connected", "room_id": "room-123" }
  ]
}
```

---

### POST /v1/rooms

Create a room.

**Request:**

```json
{ "id": "my-custom-room-id", "sample_rate": 48000 }
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string | no | Custom room ID. Auto-generated UUID if omitted. |
| `sample_rate` | integer | no | Mixer sample rate in Hz. Allowed values: `8000`, `16000`, `48000`. Default: `16000`. Higher rates preserve more audio fidelity but use proportionally more CPU and memory. |
| `webhook_url` | string | no | Per-room webhook URL. Events for this room are routed exclusively to this URL instead of global webhooks. |
| `webhook_secret` | string | no | HMAC-SHA256 signing secret for the per-room webhook. |

**Response:** `201 Created` — Room object (empty participants)

**Errors:**
- `400` — Invalid sample rate
- `409` — Room ID already exists

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

Add a leg to a room, or move it from another room. The leg must be in `connected` or `early_media` state. If the leg is a ringing inbound SIP leg, it is automatically answered before being added. If the room does not exist, it is automatically created.

If the leg is already in a different room, it is atomically moved — detached from the source mixer and immediately added to the target mixer with minimal audio gap. If the target room does not exist, it is auto-created.

**Request:**

```json
{ "leg_id": "550e8400-e29b-41d4-a716-446655440000" }
```

Join already muted / deaf:

```json
{
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "mute": true,
  "deaf": false
}
```

Join with one of the leg's additional audio streams:

```json
{
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "streams": [
    { "stream_id": "1", "role": "translator" }
  ]
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `leg_id` | string | yes | ID of the leg to add |
| `mute` | bool | no | Apply this mute state to the leg atomically before it joins the mixer — prevents the race where one frame of un-muted audio leaks into the mix between add and `/mute`. Omit to leave current state untouched (useful on move). |
| `deaf` | bool | no | Apply this deaf state to the leg atomically before it joins the mixer. Omit to leave current state untouched. |
| `role` | string | no | Apply a routing role atomically before the leg enters the mixer. The room's routing matrix (see `/v1/rooms/{id}/routing`) decides who hears whom based on roles, so passing `role` on join guarantees no audio bleed between the leg appearing in the mix and the matrix being applied. Pass an empty string to clear the role (full mesh). Omit to leave the current role untouched. |
| `streams` | object[] | no | **SIP only.** Additional audio streams of the same leg to mix into **this** room, each with its own routing role. Each entry takes `stream_id` (from `GET /v1/legs/{id}/streams`) and `role`. Omit to add only the leg's primary stream, which is the classic behaviour. A stream currently mixed elsewhere is moved here. Because this endpoint is scoped to one room it can only place streams *here* — to fan a leg's streams across different rooms, use [`POST /v1/legs/{id}/streams/{streamId}/room`](#post-v1legsidstreamsstreamidroom). |

**Response (added):** `200 OK`

```json
{ "status": "added" }
```

**Response (moved from another room):** `200 OK`

```json
{ "status": "moved", "from": "room-123", "to": "room-456" }
```

Events `leg.left_room` and `leg.joined_room` are emitted on move.

**Errors:**
- `400` — Invalid JSON, leg not found, leg not connected, or leg already in this room

---

### DELETE /v1/rooms/{id}/legs/{legID}

Remove a leg from a room (without hanging it up).

**Response:** `200 OK`

```json
{ "status": "removed" }
```

**Errors:** `400` — Room or leg not found

---

## Room Bridges

A **bridge** joins two rooms' mixers so audio flows between them, without
merging their participant sets. Both rooms must already exist and use the
**same sample rate** (no resampling is performed on the bridge). Mixed-minus-self
in each mixer prevents the other room's audio from echoing back across the
bridge.

`direction` is always **relative to the room in the request path** (`{id}`):

| `direction` | Path room sends | Path room receives |
|---|---|---|
| `bidirectional` (default) | yes | yes |
| `send` | yes | no |
| `receive` | no | yes |
| `none` | no | no |

A room may hold several bridges (e.g. A↔B and A↔C). The mixer of a bridged
room is kept running even when it has no legs, so a one-way `receive`/`send`
bridge into an otherwise empty room works (e.g. a recorder/agent room).

> **Cycle warning:** bridging rooms into a cycle (A→B→C→A) with feedback-enabled
> directions causes audio feedback. Use one-way directions to break loops.

> **Audio only:** a bridge relays mixed PCM audio between the two rooms. It does
> **not** relay DTMF (RFC 4733 telephone-events) or RTT (T.140) — those are
> broadcast only among the legs within a single room, so digits/text entered in
> one bridged room are not delivered to participants of the other.

The `room.bridged` / `room.bridge_updated` / `room.unbridged` webhook events
report `room_a_id` (the room the bridge was created from) and `room_b_id`, and
their `direction` is **canonical relative to `room_a_id`** (`bidirectional`,
`a_to_b`, `b_to_a`, or `none`) — independent of which room you call the REST
endpoint from.

### POST /v1/rooms/{id}/bridges

Bridge the room in the path to another room.

**Request:**

```json
{ "room_id": "room-b", "direction": "bidirectional" }
```

| Field | Description |
|---|---|
| `id` | Optional custom bridge ID (auto-generated UUID if omitted) |
| `room_id` | The other room to join (required) |
| `direction` | `bidirectional` (default), `send`, `receive`, or `none` |

```bash
curl -X POST http://localhost:8080/v1/rooms/room-a/bridges \
  -H 'Content-Type: application/json' \
  -d '{"room_id":"room-b","direction":"bidirectional"}'
```

**Response:** `201 Created`

```json
{ "id": "b1f2…", "room_id": "room-b", "direction": "bidirectional", "sample_rate": 16000 }
```

**Errors:** `400` — invalid JSON, self-bridge, sample-rate mismatch, or invalid
direction · `404` — path room or `room_id` not found · `409` — a bridge between
these rooms already exists

### GET /v1/rooms/{id}/bridges

List every bridge involving this room. `direction` and `room_id` in each entry
are relative to the room in the path.

**Response:** `200 OK` — array of bridge objects. **Errors:** `404` — room not found

### GET /v1/rooms/{id}/bridges/{bridgeID}

**Response:** `200 OK` — bridge object. **Errors:** `404` — bridge not found for this room

### PATCH /v1/rooms/{id}/bridges/{bridgeID}

Change the bridge's audio flow live (no audio interruption, no participant churn).

**Request:**

```json
{ "direction": "send" }
```

```bash
curl -X PATCH http://localhost:8080/v1/rooms/room-a/bridges/b1f2 \
  -H 'Content-Type: application/json' -d '{"direction":"send"}'
```

**Response:** `200 OK` — updated bridge object. **Errors:** `400` — invalid or
missing direction · `404` — bridge not found for this room

### DELETE /v1/rooms/{id}/bridges/{bridgeID}

Tear the bridge down. Deleting either bridged room also tears down its bridges
automatically (emitting `room.unbridged` with `reason: "room_deleted"`).

**Response:** `200 OK`

```json
{ "status": "deleted" }
```

**Errors:** `404` — bridge not found for this room

---

## Audio routing matrix

Inside a single room, the **audio routing matrix** controls which participants
hear which other participants. The default is full mesh — every leg hears every
other leg. The matrix lets you express asymmetric audio (one-way listens,
whisper, supervisor monitor) without spinning up extra bridges.

The matrix is keyed by **role**, an operator-supplied string set on each leg
(`"customer"`, `"agent"`, `"supervisor"`, or any other free-form value). Each
row of the matrix lists the source roles a listener role is allowed to hear:

```json
{
  "matrix": {
    "customer":   ["agent"],
    "agent":      ["customer", "supervisor"],
    "supervisor": ["customer", "agent"]
  }
}
```

This is the **barge-in / whisper** pattern:

- Customer and agent hear each other.
- Supervisor hears both customer and agent.
- Agent also hears the supervisor (whisper / coaching).
- **Customer does NOT hear the supervisor.**

**Semantics**

For a listener `L` with role `R_L` and a source `S` with role `R_S` (and `L != S`):

- `R_L == ""` (unroled listener) → hears everyone (full mesh).
- `R_S == ""` (unroled source) → not heard by any matrix-routed listener.
- `matrix[R_L]` unset → listener defaults to full mesh.
- `matrix[R_L] == []` → listener hears nothing (isolated).
- Otherwise → listener hears `S` iff `R_S` is in `matrix[R_L]`.

`mute` (source contributes silence) and `deaf` (listener gets no output) still
apply on top of the matrix.

**Atomicity (no bleed)**

To guarantee that a supervisor joining mid-call cannot momentarily be heard
by the customer, pass `"role"` on `POST /v1/rooms/{id}/legs` — the role is
set and the matrix is recomputed under the same mutex acquisition that adds
the participant to the mixer, so the very first `mixTick` that sees the new
leg already has the correct routing. Mid-call role changes via
`PATCH /v1/legs/{id}/role` take effect on the next `mixTick` (≤ 20 ms) and
are also atomic: either every leg's allow-set reflects the change or none of
them do.

---

### GET /v1/rooms/{id}/routing

Return the current routing matrix.

**Response:** `200 OK`

```json
{
  "matrix": {
    "customer":   ["agent"],
    "agent":      ["customer", "supervisor"],
    "supervisor": ["customer", "agent"]
  }
}
```

Roles absent from `matrix` default to full mesh.

**Errors:** `404` — room not found

---

### PUT /v1/rooms/{id}/routing

Replace the full routing matrix. Recomputes every leg's per-listener source
whitelist in one mixer-mutex acquisition; the next mix tick (≤ 20 ms)
reflects the new routing.

**Request:**

```json
{
  "matrix": {
    "customer":   ["agent"],
    "agent":      ["customer", "supervisor"],
    "supervisor": ["customer", "agent"]
  }
}
```

**Response:** `200 OK` — returns the updated matrix in the same shape as `GET`.

Emits `room.routing_changed` with `reason: "set"`.

**Errors:** `400` — invalid JSON; `404` — room not found

---

### PATCH /v1/rooms/{id}/routing

Replace selected rows of the matrix. Useful for adjusting a single role's
allow-list without restating the whole matrix. Pass `"sources": null` on an
update to clear that row back to full mesh.

**Request:**

```json
{
  "updates": [
    { "listener_role": "supervisor", "sources": ["customer", "agent", "trainee"] },
    { "listener_role": "trainee",    "sources": null }
  ]
}
```

**Response:** `200 OK` — returns the updated matrix.

Emits `room.routing_changed` with `reason: "update"`.

**Errors:** `400` — invalid JSON; `404` — room not found

---

### PATCH /v1/legs/{id}/role

Change a leg's routing role. If the leg is currently in a room, the room's
matrix-derived allow-sets are recomputed atomically and `room.routing_changed`
fires with `reason: "leg_role_changed"`. `leg.role_changed` is always emitted.

**Request:**

```json
{ "role": "supervisor" }
```

Pass an empty string to clear the role (the leg falls back to full mesh).

**Response:** `200 OK` — returns the updated `LegView`.

**Errors:** `400` — invalid JSON; `404` — leg not found

---

### PUT /v1/legs/{id}/custom-data

Replace the leg's [`custom_data`](#custom-data) at any point in the call. The new
value is carried on every event published for the leg from the **next** event
onwards; events already emitted are unaffected.

**Request:**

```json
{ "custom_data": { "order_id": "A-991", "tenant": 42 } }
```

| Field | Type | Description |
|---|---|---|
| `custom_data` | any | **Required.** Any JSON value — object, array, string, number or boolean. Replaces the existing value **outright; there is no merge**. Pass `null` to clear it, exactly as `DELETE` does. Omitting the field is rejected with `400` rather than treated as a clear. |

**Response:** `200 OK` — returns the updated `LegView`.

```bash
curl -sX PUT localhost:8080/v1/legs/$LEG/custom-data \
  -H 'Content-Type: application/json' \
  -d '{"custom_data":{"order_id":"A-991","tenant":42}}'
```

**Errors:**
- `400` — invalid JSON, `custom_data` missing, or the value exceeds `CUSTOM_DATA_MAX_BYTES`
- `404` — leg not found

---

### DELETE /v1/legs/{id}/custom-data

Clear the leg's `custom_data`. Subsequent events for the leg omit the field
entirely. Idempotent — clearing a leg that has none succeeds.

**Request:** Empty body

**Response:** `200 OK` — returns the updated `LegView` (with no `custom_data` key).

**Errors:** `404` — leg not found

---

## Per-leg audio streams (multiple m=audio lines)

A SIP dialog normally carries one `m=audio` section, but it may carry several
(RFC 3264 §5.1), each with its own RTP port, direction, language and mixer
routing. Extra sections are negotiated only when one side offers them, so an
ordinary call is unaffected. The motivating case is live translation: `m=audio` #0 carries the
original bidirectional audio while a second, `sendonly` stream carries the
translated feed, mixed into a different room.

The wire profile follows SIPREC (RFC 7866), the deployed multi-`m=audio` shape
SBCs already interoperate with: one section per stream, `a=mid` (RFC 5888) for
stable identity, `a=label` (RFC 4574), `a=content:main|alt` (RFC 4796) and
`a=lang` (RFC 8866).

```
m=audio 40000 RTP/AVP 0 101      ; original
a=sendrecv
a=mid:0
a=content:main
a=lang:en

m=audio 40002 RTP/AVP 0 101      ; translated feed
a=sendonly
a=mid:1
a=content:alt
a=lang:es
```

Rules that follow from RFC 3264 and are visible through this API:

- An answer always carries the **same number of `m=` sections, in the same
  order**, as the offer. Sections we do not accept come back with port 0.
- The m-line count **never decreases** for the life of a dialog. Removing a
  stream leaves a tombstone, so a later added stream takes a new position.
- A peer that rejects an extra stream with port 0 leaves the call running on
  its remaining streams — the extra stream is simply never established.
- A leg never hears its own other streams, whatever the room's routing matrix
  says. Without that rule the original stream would hear the translated one and
  echo it straight back to the caller.

### Establishing a multi-stream call

There are three ways a call ends up with more than one audio stream:

**Outbound, from the first INVITE** — pass `streams` to `POST /v1/legs`. Each
entry is an extra `m=audio` section offered alongside the call's primary
bidirectional audio, so both are negotiated by the initial offer/answer with no
follow-up re-INVITE:

```bash
curl -X POST localhost:8080/v1/legs -d '{
  "type": "sip",
  "to": "sip:bob@example.com",
  "codecs": ["PCMU"],
  "room_id": "room-original",
  "streams": [
    {
      "direction": "sendonly",
      "content": "alt",
      "lang": "es",
      "room_id": "room-translated",
      "role": "translator"
    }
  ]
}'
```

The leg's own `room_id` governs the primary stream; each entry's `room_id`
governs that stream, and the two may differ — that is what lets the original and
translated audio be mixed separately. Streams are attached to their rooms once
the call connects. A stream the peer refuses is simply never established; the
call runs on whatever was negotiated.

**Outbound, after the call is up** — `POST /v1/legs/{id}/streams` triggers a
re-INVITE (see below).

**Inbound** — the sections are accepted automatically when a peer offers several
`m=audio`. To route them at the same time you answer, pass `streams` to
`POST /v1/legs/{id}/answer`:

```bash
curl -X POST localhost:8080/v1/legs/$LEG_ID/answer -d '{
  "streams": [
    {"room_id": "room-translated", "role": "translator"}
  ]
}'
```

Entries are **positional over the accepted secondary streams**, in m-line order:
entry 0 is the first stream after the primary. The caller's offer decides how
many exist, so an entry with no matching stream is ignored rather than failing
the answer. Placement is applied once the answer is negotiated.

### Choosing a room per stream

Every stream can sit in its own room, and a stream's room need not be its leg's.
Four ways to set it, all equivalent in effect:

| When | How |
|---|---|
| Outbound, at create | `streams[].room_id` on `POST /v1/legs` |
| Inbound, at answer | `streams[].room_id` on `POST /v1/legs/{id}/answer` |
| Joining a room | `streams[]` on `POST /v1/rooms/{id}/legs` — puts the named streams in **that** room alongside the leg |
| Any time after | `POST /v1/legs/{id}/streams/{streamId}/room` |

A stream's **role** within its room is changed in place with
[`PATCH /v1/legs/{id}/streams/{streamId}`](#patch-v1legsidstreamsstreamid) —
no detach/re-attach, so it never drops out of the mix.

`POST /v1/rooms/{id}/legs` adds only the leg's primary stream unless `streams`
names others:

```bash
curl -X POST localhost:8080/v1/rooms/room-shared/legs -d '{
  "leg_id": "'$LEG_ID'",
  "streams": [{"stream_id": "1", "role": "translator"}]
}'
```

Because that endpoint is scoped to one room, it can only place streams *there*.
To fan a leg's streams across different rooms, use the per-stream endpoint or the
create/answer forms above. A stream already mixed elsewhere is moved.

Whichever route you use, a leg never hears its own other streams, so the
original audio is never fed the translated feed.

---

### GET /v1/legs/{id}/streams

List a leg's negotiated audio streams, in m-line order.

**Response:** `200 OK`

```json
[
  {
    "id": "0",
    "mid": "0",
    "index": 0,
    "primary": true,
    "state": "active",
    "direction": "sendrecv",
    "codec": "PCMU",
    "sample_rate": 8000,
    "local_port": 40000,
    "remote_addr": "198.51.100.7:40000",
    "content": "main",
    "lang": "en",
    "room_id": "room-original"
  },
  {
    "id": "1",
    "mid": "1",
    "index": 1,
    "primary": false,
    "state": "active",
    "direction": "sendonly",
    "desired_direction": "sendonly",
    "codec": "PCMU",
    "sample_rate": 8000,
    "local_port": 40002,
    "content": "alt",
    "lang": "es",
    "room_id": "room-translated",
    "role": "translator"
  }
]
```

**Errors:** `400` — not a SIP leg; `404` — leg not found

---

### POST /v1/legs/{id}/streams

Negotiate an additional `m=audio` section on a live call via re-INVITE. The new
section is appended below the existing ones and binds its own RTP port.

**Request:**

```json
{
  "direction": "sendonly",
  "content": "alt",
  "lang": "es",
  "room_id": "room-translated",
  "role": "translator"
}
```

`direction` defaults to `sendrecv`. When `room_id` is set the stream is attached
to that room as soon as it is negotiated.

**Response:** `201 Created` — returns the new `LegStreamView`.

Emits `leg.stream_added`, plus `leg.stream_room_changed` when `room_id` was set.
A peer that refuses the section emits `leg.stream_rejected`.

**Errors:** `400` — invalid JSON or not a SIP leg; `404` — leg or room not
found; `409` — leg has no negotiated media yet, or the peer rejected the stream

---

### GET /v1/legs/{id}/streams/{streamId}

Get one stream. **Response:** `200 OK` — a `LegStreamView`.

**Errors:** `400` — not a SIP leg; `404` — leg or stream not found

---

### PATCH /v1/legs/{id}/streams/{streamId}

Change a stream's routing role in place.

**Request:**

```json
{ "role": "translator" }
```

Pass an empty string to clear the role (full mesh). When the stream is mixed
into a room, that room's matrix-derived allow-sets are recomputed atomically —
a single mixer-mutex acquisition, so no audio bleeds through mid-change. Emits
`leg.stream_role_changed` and `room.routing_changed` with
`reason: "leg_stream_role_changed"`.

Only the role is mutable here. The SDP-level attributes — `direction`, `lang`,
`content`, `label` — are fixed when the stream is negotiated; to change them,
remove the stream and add a new one.

**Response:** `200 OK` — the updated `LegStreamView`.

**Errors:** `400` — invalid JSON, not a SIP leg, or the primary stream (its role
follows the leg, so use [`PATCH /v1/legs/{id}/role`](#patch-v1legsidrole));
`404` — leg or stream not found

---

### DELETE /v1/legs/{id}/streams/{streamId}

Disable a stream with a re-INVITE carrying port 0 for its section and release its
RTP port. The m-line slot survives as a tombstone. The primary stream carries the
call and cannot be removed.

**Response:** `204 No Content`. Emits `leg.stream_removed`.

**Errors:** `400` — not a SIP leg; `404` — leg or stream not found; `409` —
primary stream, or the re-INVITE failed

---

### POST /v1/legs/{id}/streams/{streamId}/room

Mix a secondary stream into a room. The room may differ from the leg's own — that
is what lets the original and translated audio be mixed separately.

**Request:**

```json
{ "room_id": "room-translated", "role": "translator" }
```

**Response:** `200 OK` — the updated `LegStreamView`. Emits
`leg.stream_room_changed`.

**Errors:** `400` — invalid JSON, missing `room_id`, not a SIP leg, or the
primary stream (which follows its leg via `/v1/rooms/{id}/legs`); `404` — leg,
stream or room not found; `409` — the stream carries no audio in either direction

---

### DELETE /v1/legs/{id}/streams/{streamId}/room

Remove a stream from whichever room mixes it.

**Response:** `200 OK` — the updated `LegStreamView`. Emits
`leg.stream_room_changed` with an empty `room_id`.

**Errors:** `400` — not a SIP leg; `404` — leg or stream not found

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

### PATCH /v1/rooms/{id}/play/{playbackID}

Change the volume of an active room playback. Takes effect immediately on the next audio frame. The new level persists for the lifetime of the playback, including across loop iterations.

**Request:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `volume` | integer | yes | Volume adjustment (-8 to 8, ~3dB per step, 0 = unchanged) |

**Response:** `200 OK`

```json
{ "status": "ok" }
```

**Errors:**
- `400` — Invalid JSON or volume out of range
- `404` — Playback not found

---

### POST /v1/rooms/{id}/tts

Synthesize speech and play it into a room.

Transient upstream failures (429, 500/502/503/504, transport timeouts) are retried up to 3 times with jittered backoff, bounded to 5 seconds in total, before `tts.error` is published. Auth failures, rejected input, and unclassifiable errors are not retried.

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
| `voice` | string | yes | Provider-specific voice identifier. ElevenLabs: voice name or ID. AWS Polly: voice ID (e.g. `Joanna`, `Matthew`). Google Cloud: voice name — either full format (e.g. `en-US-Neural2-F`) or short name for Gemini models (e.g. `Achernar`, `Kore`). Azure: voice name (e.g. `en-US-JennyNeural`). |
| `provider` | string | no | TTS provider: `"elevenlabs"` (default), `"aws"`, `"google"`, `"deepgram"`, or `"azure"` |
| `model_id` | string | no | Provider-specific model/engine. ElevenLabs: model ID. AWS Polly: engine (`standard`, `neural`, `long-form`, `generative`; default `neural`). Google Cloud: model name (e.g. `gemini-2.5-pro-tts`, `chirp3-hd`). Not used for Deepgram or Azure. |
| `language` | string | no | Language code (e.g. `"en-US"`, `"pl-pl"`). Required for Google Gemini TTS voices that use short names. Auto-extracted from full voice names. |
| `prompt` | string | no | Style/tone instruction for promptable voice models (Google Gemini TTS only). |
| `volume` | integer | no | Volume adjustment in dB (`-8` to `8`, default `0`) |
| `api_key` | string | no | ElevenLabs: API key override (falls back to `ELEVENLABS_API_KEY` env var). AWS: optional `ACCESS_KEY:SECRET_KEY` override (falls back to default AWS credential chain). Google Cloud: optional API key override (falls back to Application Default Credentials). Deepgram: API key override (falls back to `DEEPGRAM_API_KEY` env var). Azure: subscription key override (falls back to `AZURE_SPEECH_KEY` env var). |

**Response:** `200 OK`

```json
{ "tts_id": "tts-a1b2c3d4", "status": "playing" }
```

Events `tts.started` and `tts.finished` are emitted.

**Caching:** When `TTS_CACHE_ENABLED=true`, identical requests (same text, voice, model, language, and prompt) are served from the disk cache stored in `TTS_CACHE_DIR`, skipping the external provider call. The cache persists across restarts; to clear it, delete the files in that directory. Set `TTS_CACHE_INCLUDE_API_KEY=true` to scope the cache per API key (needed when different keys access different voice clones).

**Errors:**
- `400` — Invalid JSON, missing text/voice, volume out of range
- `404` — Room not found
- `409` — Room has no participants
- `503` — No API key provided for the selected provider

---

### POST /v1/rooms/{id}/record

Start recording the full room mix to a WAV file (16-bit, mono, at the room's configured sample rate).

**Request:**

```json
{
  "storage": "s3",
  "filename": "room-mix-call-42",
  "multi_channel": true,
  "s3_bucket": "my-recordings",
  "s3_region": "eu-west-1",
  "s3_access_key": "AKIA...",
  "s3_secret_key": "wJalr..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `storage` | string | no | `"file"` (default) — local disk, `"s3"` — upload to S3 after recording stops, `"gcs"` — upload to Google Cloud Storage via the native GCS API |
| `filename` | string | no | Optional output basename for the room-mix WAV. Same rules as `POST /v1/legs/{id}/record` (`filename`): single path segment, `.wav` appended when missing, dots preserved, `409` on collision. When omitted, a timestamped name is generated. Does not rename the optional multi-channel merge file. |
| `multi_channel` | boolean | no | When `true`, produce a single multi-channel WAV file with one track per participant (time-aligned with silence padding), in addition to the full mix. Covers leg participants and attached streams alike. Default `false`. |
| `s3_bucket` | string | no | S3 bucket name. Overrides `S3_BUCKET` env var. Required if env var is not set. |
| `s3_region` | string | no | AWS region. Overrides `S3_REGION` env var. Default `us-east-1`. |
| `s3_endpoint` | string | no | Custom S3 endpoint (MinIO, etc.). Overrides `S3_ENDPOINT` env var. |
| `s3_prefix` | string | no | Key prefix (e.g. `recordings/`). Overrides `S3_PREFIX` env var. |
| `s3_access_key` | string | no | AWS access key ID. Overrides default credential chain. |
| `s3_secret_key` | string | no | AWS secret access key. Must be set together with `s3_access_key`. |
| `gcs_bucket` | string | no | GCS bucket name. Overrides `GCS_BUCKET` env var. Required if env var is not set when `storage=gcs`. |
| `gcs_object_name_prefix` | string | no | Object name prefix (e.g. `recordings` or `recordings/`). Overrides `GCS_OBJECT_NAME_PREFIX` env var. A trailing slash is added automatically when missing. |

When `s3_bucket` / `gcs_bucket` is provided, a per-request backend is created. Otherwise the matching server-level backend (from env vars) is used.

Creating a per-request S3 backend probes the bucket with a bounded `HeadBucket` call, so a bucket that does not exist returns `400` here instead of failing later at upload. There is no equivalent probe for `gcs_bucket`: a GCS bucket that does not exist surfaces at upload, in the log and in `recording.finished` keeping the local path. A probe that cannot get a verdict (no `s3:ListBucket` permission, a `5xx`, an unreachable endpoint, an expired budget) is only logged, and recording starts normally. An `http://` `s3_endpoint` on a non-local host returns `400` unless the server runs with `S3_ALLOW_INSECURE_ENDPOINT=true`; loopback and private endpoints need no opt-in.

**Response:** `200 OK`

```json
{
  "status": "recording",
  "file": "/tmp/recordings/room-mix-call-42.wav"
}
```

When `storage=s3`, the `file` field in the stop response and the `recording.finished` event will contain an `s3://bucket/key` URI. When `storage=gcs`, it will contain a `gs://bucket/object` URI.

Custom `filename` values are exclusive for the room mix: an existing file or an in-flight reservation of the same name returns `409` rather than overwriting.

#### Automatic Stop

A room recording does not have to be stopped explicitly. Whenever the room is left with **no participants**, the recording is finalized (file closed, uploaded to S3 if configured) and `recording.finished` is published. That applies to every path that can empty a room:

- the last participant disconnects or is hung up (`DELETE /v1/legs/{id}`),
- the last participant is removed from the room (`DELETE /v1/rooms/{id}/legs/{legID}`),
- the last participant is moved to another room (`POST /v1/rooms/{otherID}/legs`),
- the room is deleted (`DELETE /v1/rooms/{id}`),
- the last participant's audio path faults and the server tears the leg down.

After that, `DELETE /v1/rooms/{id}/record` returns `404 — No recording in progress`. A client waiting for the recording to complete should therefore wait on `recording.finished` rather than assume the stop call is what produces it.

#### Multi-Channel Recording

When `multi_channel: true` is set, a single multi-channel WAV file is produced alongside the full mix. Each participant gets their own channel (track) within this file, with silence padding so all tracks are time-aligned to the recording start. Participants that join mid-recording get a new channel; participants that leave have silence written for the remainder.

This gives you one file ready for post-production — each speaker on a clean isolated channel for independent editing, noise reduction, and level adjustment.

The per-participant audio capture uses a dedicated mixer tap that is independent of STT/agent taps, so multi-channel recording and STT can run simultaneously without conflict.

A room holds two kinds of audio source and both are recorded: ordinary **leg**
participants, and a leg's individual **streams** attached with
[`POST /v1/legs/{id}/streams/{streamId}/room`](#post-v1legsidstreamsstreamidroom).
A SIPREC recording session is the second kind and only the second kind — its m=
sections are other parties' audio, so the room holds one stream participant per
recorded party and no leg participant at all. `channels` is keyed by the mixer
participant ID, which for a stream is `"<legID>#<streamID>"`; resolve it to a
person through [`GET /v1/legs/{id}/siprec`](#get-v1legsidsiprec).

Recording a room is therefore how a controller records *some* of a session's
parties: which streams are in the room decides who is captured, and a stream in
no room is never read. Recording the whole session with
[`POST /v1/legs/{id}/record`](#post-v1legsidrecord) captures every party instead.

**Errors:**
- `400` — Invalid storage type, S3 not configured, invalid S3 credentials, or invalid `filename`
- `404` — Room not found
- `409` — Room has no audio sources (no leg participants and no attached streams), or `filename` already exists / in use
- `500` — Failed to create recording file

---

### DELETE /v1/rooms/{id}/record

Stop room recording.

**Response:** `200 OK`

Standard (mono) recording:
```json
{
  "status": "stopped",
  "file": "/tmp/recordings/20260301_110500_a1b2c3d4.wav"
}
```

Multi-channel recording — includes a single multi-channel WAV with channel metadata:
```json
{
  "status": "stopped",
  "file": "/tmp/recordings/20260301_110500_a1b2c3d4.wav",
  "multi_channel_file": "/tmp/recordings/20260301_110500_multichannel_e5f6a7b8.wav",
  "channels": {
    "550e8400-e29b-41d4-a716-446655440000": { "channel": 0, "start_ms": 0, "end_ms": 45000 },
    "660f9500-f3ac-52e5-b827-557766551111": { "channel": 1, "start_ms": 1200, "end_ms": 45000 }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `file` | string | Path/URI of the full mix recording (mono). Empty when the full-mix capture was discarded and no file was written. |
| `multi_channel_file` | string | Path/URI of the multi-channel WAV file. Only present when `multi_channel: true` was used. |
| `channels` | object | Map of leg ID to channel metadata. Only present when `multi_channel: true` was used. |
| `channels[].channel` | integer | Zero-based channel index in the multi-channel WAV |
| `channels[].start_ms` | integer | Milliseconds from recording start when this participant joined |
| `channels[].end_ms` | integer | Milliseconds from recording start when this participant's audio ends |
| `omitted_legs` | array | Leg IDs that took part but are absent from `multi_channel_file`, because capturing them failed. Omitted entirely when the recording is complete. |

A participant whose capture fails is left out of the merge rather than failing
the whole room: the other participants' audio is still produced, and the legs
that were lost are named in `omitted_legs`. Those leg IDs have no entry in
`channels`, and the remaining channel indices are contiguous — a channel index
is only meaningful via `channels`, never by position. If **every** participant's
capture fails there is nothing to merge, and the response carries neither
`multi_channel_file` nor `channels` (the failure is logged server-side; the stop
itself still succeeds).

The full mix is captured independently of the per-participant ones, so it can be
discarded on its own: a capture that fails mid-write, or that never captures a
frame, writes no file. When that happens `file` comes back as `""`, as it does
in the `recording.finished` event, while `multi_channel_file` and `channels` may
still be present and usable. The stop still reports `status: stopped`. An empty
`file` means there is no full mix to fetch, not a path; a non-empty `file`
always names something real. In the worst case both the full mix and every
per-participant capture are discarded, and the stop succeeds with an empty
`file` and neither `multi_channel_file` nor `channels`.

Whether a partial recording is acceptable is the caller's decision, so check
`omitted_legs` before treating `multi_channel_file` as a complete record of the
room. Treat a missing `multi_channel_file` on a `multi_channel: true` recording
the same way — as a failure to produce one, not as an empty room.

**Errors:** `404` — No recording in progress

---

### POST /v1/rooms/{id}/record/pause

Pause the active room recording. The room-mix WAV is silenced. If `multi_channel: true` was used when starting the recording, every per-participant track is paused too — including tracks for participants that join the room **while the recording is paused**, so sensitive data can't leak in via a new leg.

Idempotent: returns `status: already_paused` if already paused.

**Response:** `200 OK`

```json
{"status": "paused"}
```

Emits a `recording.paused` event.

**Errors:** `404` — No recording in progress

---

### POST /v1/rooms/{id}/record/resume

Resume a previously paused room recording. Resumes every per-participant track if multi-channel recording is active. Idempotent: returns `status: not_paused` if not paused.

**Response:** `200 OK`

```json
{"status": "resumed"}
```

Emits a `recording.resumed` event.

**Errors:** `404` — No recording in progress

---

### POST /v1/rooms/{id}/stt

Start real-time speech-to-text on every audio source in a room — one transcriber
each, so every speaker is transcribed separately.

A room holds two kinds of source and both are covered: ordinary **leg**
participants, and a leg's individual **streams** attached with
[`POST /v1/legs/{id}/streams/{streamId}/room`](#post-v1legsidstreamsstreamidroom).
A SIPREC recording session is the second kind and only the second kind: its m=
sections are other parties' audio, so the room holds one stream participant per
recorded party and no leg participant at all. This is the endpoint to use for a
recording session — `POST /v1/legs/{id}/stt` answers `409` on such a leg, because
its m-line 0 is one party's audio rather than "the call".

Each transcript carries `stream_id` (see `stt.text` / `stt.turn`), empty for a leg
participant and set for a stream. Resolve it to a participant through
[`GET /v1/legs/{id}/siprec`](#get-v1legsidsiprec), whose `streams[].leg_stream_id`
carries the same value alongside `participant_aor`.

**Request:**

```json
{
  "language": "en",
  "partial": true,
  "provider": "elevenlabs"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `language` | string | no | Language code |
| `partial` | boolean | no | Emit partial (non-final) transcripts |
| `provider` | string | no | STT provider: `"elevenlabs"` (default), `"deepgram"`, `"deepgram_flux"`, `"azure"`, or `"speechmatics"` |
| `api_key` | string | no | API key override (falls back to `ELEVENLABS_API_KEY`, `DEEPGRAM_API_KEY`, `AZURE_SPEECH_KEY`, or `SPEECHMATICS_API_KEY` env var depending on provider) |

The provider tuning fields documented for [`POST /v1/legs/{id}/stt`](#post-v1legsidstt)
(`model`, `keyterms`, `endpointing`, `utterance_end_ms`, `eager_eot_threshold`,
`eot_threshold`, `eot_timeout_ms`, `language_hints`) apply here too and are
used for every participant in the room.

**Response:** `200 OK`

```json
{ "status": "stt_started", "room_id": "room-123", "leg_ids": ["leg-1", "leg-2"] }
```

`leg_ids` lists one entry per transcriber. A stream source appears as its mixer
participant ID, `"<legID>#<streamID>"` — e.g. a recording session yields
`["<legID>#0", "<legID>#1"]`.

Transcripts are delivered via `stt.text` webhook events, each carrying the
`leg_id` of the participant who spoke. Providers that report turn boundaries
also emit per-participant [`stt.turn`](#conversational-turn-detection) events.

**Errors:**
- `404` — Room not found
- `409` — STT already running on this room, or room has no participants
- `503` — No API key provided for the selected provider

---

### DELETE /v1/rooms/{id}/stt

Stop speech-to-text on a room.

**Response:** `200 OK`

```json
{ "status": "stt_stopped" }
```

**Errors:** `404` — No STT in progress

---

### POST /v1/rooms/{id}/agent/elevenlabs

Attach an ElevenLabs ConvAI agent to a room. The agent joins as a virtual participant, hearing all participants (mixed-minus-self) and speaking to everyone.

**Request:**

```json
{
  "agent_id": "abc123",
  "first_message": "Hello everyone!",
  "language": "en",
  "dynamic_variables": { "topic": "meeting" },
  "api_key": "xi-..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agent_id` | string | yes | ElevenLabs agent ID |
| `first_message` | string | no | Override the agent's first message |
| `language` | string | no | Language code (e.g. `"en"`, `"es"`) |
| `dynamic_variables` | object | no | Key-value pairs passed to the agent as dynamic variables |
| `api_key` | string | no | API key override (falls back to `ELEVENLABS_API_KEY` env var) |

**Response:** `200 OK` — `{ "status": "agent_started", "room_id": "room-123" }`

**Errors:** `400` — Invalid JSON or missing agent_id · `404` — Room not found · `409` — Agent already attached · `503` — No API key

---

### POST /v1/rooms/{id}/agent/vapi

Attach a VAPI agent to a room as a virtual participant.

**Request:**

```json
{
  "assistant_id": "asst_xyz",
  "first_message": "Hello everyone!",
  "variable_values": { "topic": "meeting" },
  "api_key": "vapi-..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `assistant_id` | string | yes | VAPI assistant ID |
| `first_message` | string | no | Override the agent's first message |
| `variable_values` | object | no | Key-value pairs passed as VAPI variable values |
| `api_key` | string | no | API key override (falls back to `VAPI_API_KEY` env var) |

**Response:** `200 OK` — `{ "status": "agent_started", "room_id": "room-123" }`

**Errors:** `400` — Invalid JSON or missing assistant_id · `404` — Room not found · `409` — Agent already attached · `503` — No API key

---

### POST /v1/rooms/{id}/agent/pipecat

Attach a self-hosted Pipecat bot to a room as a virtual participant.

**Request:**

```json
{
  "websocket_url": "ws://my-bot:8765"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `websocket_url` | string | yes | WebSocket URL of the Pipecat bot |

**Response:** `200 OK` — `{ "status": "agent_started", "room_id": "room-123" }`

**Errors:** `400` — Invalid JSON or missing websocket_url · `404` — Room not found · `409` — Agent already attached

---

### POST /v1/rooms/{id}/agent/deepgram

Attach a Deepgram Voice Agent to a room as a virtual participant.

**Request:**

```json
{
  "settings": {
    "agent": {
      "listen": { "provider": { "type": "deepgram", "model": "nova-3" } },
      "think": { "provider": { "type": "open_ai", "model": "gpt-4o-mini" } },
      "speak": { "provider": { "type": "deepgram", "model": "aura-2-asteria-en" } }
    }
  },
  "greeting": "Hello everyone!",
  "language": "en",
  "api_key": "dg-..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `settings` | object | no | Full Deepgram agent settings object. When omitted, sensible defaults are used. |
| `greeting` | string | no | Agent greeting message |
| `language` | string | no | Language code (e.g. `"en"`, `"es"`) |
| `api_key` | string | no | API key override (falls back to `DEEPGRAM_API_KEY` env var) |

**Response:** `200 OK` — `{ "status": "agent_started", "room_id": "room-123" }`

**Errors:** `400` — Invalid JSON · `404` — Room not found · `409` — Agent already attached · `503` — No API key

---

### POST /v1/rooms/{id}/agent/message

Inject a context message or instruction into a running agent session on a room. This is provider-agnostic — the session routes the message using the appropriate provider mechanism.

**Supported providers:**
- **Deepgram** — sends `InjectAgentMessage` via WebSocket
- **Pipecat** — sends a protobuf `TextFrame` via WebSocket
- **VAPI** — sends `add-message` via HTTP control URL
- **ElevenLabs** — not supported (returns `501`)

**Request:**

```json
{
  "message": "The customer's name is John and their order number is 12345."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `message` | string | yes | Context or instruction to inject into the running agent session |

**Response:** `200 OK`

```json
{ "status": "message_sent" }
```

**Errors:** `400` — Invalid JSON or missing message · `404` — No agent attached to this room · `409` — Agent session not running · `501` — Provider does not support message injection

---

### DELETE /v1/rooms/{id}/agent

Detach the agent from a room (provider-agnostic).

**Response:** `200 OK`

```json
{ "status": "agent_stopped" }
```

**Errors:** `404` — No agent attached to this room

---

### GET /v1/rooms/{id}/ws

Upgrade to a WebSocket connection and join the room as a bidirectional audio participant. The client sends and receives 16kHz 16-bit signed little-endian PCM audio (mono), base64-encoded in JSON text frames. Each audio frame is 640 bytes (20ms).

This endpoint shares its WebSocket transport (`internal/wsmedia`) and wire protocol with `GET /v1/legs/websocket` when the leg endpoint is invoked with `wire_format=json_base64`. The two endpoints differ only in semantics: this one attaches a raw mixer participant (no leg lifecycle, no `/v1/legs/{id}/...` operations, no leg events), while `/v1/legs/websocket` creates a real leg.

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

### GET /v1/vsi (VSI)

Upgrade to a WebSocket connection and receive all events in real-time as JSON text frames. The JSON shape is identical to webhook payloads (same `Event.MarshalJSON` format).

The full machine-readable contract for the VSI WebSocket — every command, every event, every lifecycle frame — lives in [`asyncapi.yaml`](./asyncapi.yaml) (AsyncAPI 3.0). The tables below are a quick reference; the YAML is authoritative and is generated from `internal/api/vsi_meta.go` via `make asyncapi`.

**Upgrade:** Standard HTTP → WebSocket upgrade. No request body.

**Query Parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `app_id` | string (regex) | If set, only events whose `app_id` matches the regex are forwarded. Omit to receive all events. |

Set `app_id` on legs via `POST /v1/legs` body, `POST /v1/webrtc/offer` body (WebRTC legs), or the `X-App-ID` SIP header on inbound calls. Set on rooms via `POST /v1/rooms` body. Auto-created rooms inherit `app_id` from the originating leg.

Events from untagged legs carry an empty `app_id` and are dropped by any non-empty filter — tag every leg an app cares about, or it will silently miss its own events.

**Example with filter:**

```bash
websocat "ws://localhost:8080/v1/vsi?app_id=^billing$"
```

#### Message Format

**Server → Client (on connect):**

```json
{"type": "connected"}
```

**Server → Client (event):**

```json
{"type": "leg.connected", "timestamp": "2026-04-15T12:00:00Z", "event_id": "8f14e45f-ceea-467a-9575-9b0ba1f0e3a1", "instance_id": "i-abc", "leg_id": "550e8400-...", "leg_type": "sip_outbound"}
```

Events use the same flattened JSON envelope as webhook POSTs — including `event_id`, the same per-event idempotency key webhook receivers see in the body and the `X-Event-Id` header. Clients already parsing webhook payloads can reuse the same deserializer.

**Server → Client (keepalive ping):**

```json
{"type": "ping", "seq": 1, "event_id": 1}
```

`seq` is a monotonic per-connection counter for the keepalive itself, starting at 1 and resetting on reconnect. It is unrelated to the `event_id` on streamed events — the ping is not an event.

`event_id` on the ping frame is a **deprecated alias** for `seq`, kept so existing clients keep working; it carries the same integer counter, not the UUID that streamed events carry. New clients should read `seq`. The alias will be removed in a future release.

**Client → Server (keepalive pong):**

```json
{"type": "pong"}
```

**Client → Server (disconnect):**

```json
{"type": "stop"}
```

**Client → Server (commands):**

The WebSocket accepts bidirectional commands using the same naming as the REST API. All commands support an optional `request_id` echoed back in the response.

```json
// Client sends:
{"type": "mute_leg", "request_id": "req-1", "payload": {"id": "abc-123"}}

// Server responds (success):
{"type": "mute_leg.result", "request_id": "req-1", "data": {"status": "muted"}}

// Server responds (error):
{"type": "error", "request_id": "req-1", "data": {"code": 404, "message": "leg not found"}}
```

#### Available commands

| Command | Payload | Description |
|---------|---------|-------------|
| `list_legs` | *(none)* | List all legs |
| `get_leg` | `{"id":"..."}` | Get a single leg |
| `create_leg` | `CreateLegRequest` | Originate an outbound leg; returns the leg view. All types are supported (`sip`, `websocket`, `whatsapp`, `livekit_room`). For `livekit_room`, custom headers come from the request's `headers` map. |
| `delete_leg` | `{"id":"..."}` | Hang up and delete a leg |
| `answer_leg` | `{"id":"..."}` | Answer a ringing inbound leg |
| `mute_leg` | `{"id":"..."}` | Mute a leg |
| `unmute_leg` | `{"id":"..."}` | Unmute a leg |
| `deaf_leg` | `{"id":"..."}` | Deafen a leg |
| `undeaf_leg` | `{"id":"..."}` | Undeafen a leg |
| `hold_leg` | `{"id":"..."}` | Put a SIP leg on hold |
| `unhold_leg` | `{"id":"..."}` | Resume a held SIP leg |
| `send_leg_dtmf` | `{"id":"...","digits":"123"}` | Send DTMF digits on a leg |
| `accept_leg_dtmf` | `{"id":"..."}` | Enable DTMF reception |
| `reject_leg_dtmf` | `{"id":"..."}` | Disable DTMF reception |
| `send_leg_rtt` | `{"id":"...","text":"hello"}` | Send Real-Time Text (T.140) on a SIP leg with negotiated `m=text` |
| `accept_leg_rtt` | `{"id":"..."}` | Enable RTT reception (default) |
| `reject_leg_rtt` | `{"id":"..."}` | Disable RTT reception |
| `webrtc_offer` | `{"sdp":"..."}` | Establish a WebRTC leg via SDP offer/answer; returns `{leg_id, sdp}` |
| `webrtc_add_candidate` | `{"id":"...","candidate":{"candidate":"...","sdpMid":"0","sdpMLineIndex":0}}` | Add a remote ICE candidate to a WebRTC leg |
| `webrtc_get_candidates` | `{"id":"..."}` | Drain server-gathered ICE candidates; returns `{candidates, done}` |
| `list_rooms` | *(none)* | List all rooms |
| `get_room` | `{"id":"..."}` | Get a single room |
| `create_room` | `CreateRoomRequest` | Create a room |
| `delete_room` | `{"id":"..."}` | Delete a room |
| `add_leg_to_room` | `{"room_id":"...","leg_id":"..."}` | Add or move leg to room (supports `mute`, `deaf`, `accept_dtmf`) |
| `remove_leg_from_room` | `{"room_id":"...","leg_id":"..."}` | Remove leg from room |

The commands below mirror the corresponding REST endpoints and use **resource-first** naming (`leg_*`, `room_*`). All payloads merge the URL identifier with the REST request body fields.

| Command | Payload | Description |
|---------|---------|-------------|
| `leg_ring` | `{"id":"..."}` | Send 180 Ringing on a SIP inbound leg |
| `leg_early_media` | `{"id":"...","codec":"PCMU"}` | Enable 183 Session Progress with media on a SIP inbound leg |
| `leg_amd_start` | `{"id":"...","initial_silence_timeout":2500,...}` | Start AMD on a connected SIP leg (all `AMDParams` fields are optional) |
| `leg_transfer` | `{"id":"...","target":"sip:bob@example.com","replaces_leg_id":""}` | Initiate SIP REFER transfer (blind or attended) |
| `leg_record_start` | `{"id":"...","storage":"file",...}` | Start recording a leg (stereo when in a room or SIP, mono otherwise) |
| `leg_record_stop` | `{"id":"..."}` | Stop a leg recording; returns `{status, file}` |
| `leg_record_pause` | `{"id":"..."}` | Pause a leg recording |
| `leg_record_resume` | `{"id":"..."}` | Resume a paused leg recording |
| `room_record_start` | `{"id":"...","multi_channel":true,...}` | Start recording a room mix |
| `room_record_stop` | `{"id":"..."}` | Stop a room recording |
| `room_record_pause` | `{"id":"..."}` | Pause a room recording |
| `room_record_resume` | `{"id":"..."}` | Resume a paused room recording |
| `leg_play_start` | `{"id":"...","url":"https://...","volume":0}` | Start audio playback on a leg; returns `{playback_id, status}` |
| `leg_play_stop` | `{"id":"...","playback_id":"pb-..."}` | Stop a leg playback |
| `leg_play_volume` | `{"id":"...","playback_id":"pb-...","volume":-3}` | Adjust active playback volume (-8..8) |
| `room_play_start` | `{"id":"...","tone":"us_ringback"}` | Start audio playback into a room mix |
| `room_play_stop` | `{"id":"...","playback_id":"pb-..."}` | Stop a room playback |
| `room_play_volume` | `{"id":"...","playback_id":"pb-...","volume":2}` | Adjust active room playback volume |
| `leg_stt_start` | `{"id":"...","provider":"deepgram","language":"en"}` | Start speech-to-text on a leg |
| `leg_stt_stop` | `{"id":"..."}` | Stop STT on a leg |
| `leg_stt_finalize` | `{"id":"..."}` | Flush the STT buffer on a leg and emit a final transcript without stopping STT (Deepgram only) |
| `room_stt_start` | `{"id":"...","provider":"elevenlabs"}` | Start STT on every participant of a room (auto-extends to legs that join later) |
| `room_stt_stop` | `{"id":"..."}` | Stop room STT |
| `leg_tts` | `{"id":"...","text":"Hello","voice":"Joanna","provider":"aws"}` | Synthesize and play TTS on a leg; returns `{tts_id, status}` |
| `leg_tts_preflight` | `{"id":"...","text":"Hello","voice":"Rachel"}` | Synthesize and hold for a later commit; returns `{tts_id, status:"staged"}` |
| `leg_tts_commit` | `{"id":"...","tts_id":"tts-a1b2c3d4"}` | Play a staged utterance; returns `{tts_id, status:"committed"}` |
| `leg_tts_discard` | `{"id":"...","tts_id":"tts-a1b2c3d4"}` | Drop a staged utterance without playing it |
| `room_tts` | `{"id":"...","text":"...","voice":"..."}` | Synthesize and play TTS into a room mix |
| `leg_agent_elevenlabs` | `{"id":"...","agent_id":"..."}` | Attach an ElevenLabs Conversational AI agent to a leg |
| `leg_agent_vapi` | `{"id":"...","assistant_id":"..."}` | Attach a VAPI agent to a leg |
| `leg_agent_pipecat` | `{"id":"...","websocket_url":"ws://..."}` | Attach a Pipecat bot to a leg |
| `leg_agent_deepgram` | `{"id":"...","greeting":"...","settings":{...}}` | Attach a Deepgram Voice Agent to a leg |
| `leg_agent_message` | `{"id":"...","message":"..."}` | Inject a text message into a running leg agent session |
| `leg_agent_stop` | `{"id":"..."}` | Detach the agent from a leg |
| `room_agent_elevenlabs` | `{"id":"...","agent_id":"..."}` | Attach ElevenLabs agent to a room |
| `room_agent_vapi` | `{"id":"...","assistant_id":"..."}` | Attach VAPI agent to a room |
| `room_agent_pipecat` | `{"id":"...","websocket_url":"ws://..."}` | Attach Pipecat bot to a room |
| `room_agent_deepgram` | `{"id":"...","greeting":"..."}` | Attach Deepgram agent to a room |
| `room_agent_message` | `{"id":"...","message":"..."}` | Inject a text message into a running room agent session |
| `room_agent_stop` | `{"id":"..."}` | Detach the agent from a room |

The server sends application-level pings every 30 seconds. If a client reads too slowly, events are buffered per-connection. When the buffer is full, **new events are dropped** and the server sends a notification before the next successfully delivered event:

```json
{"type": "events_dropped", "count": 12}
```

On receiving this, the client should resync state via REST (e.g. `GET /v1/legs`, `GET /v1/rooms`) since it may have missed transitions.

The per-connection buffer size defaults to **256 events** and is configurable via the `VSI_EVENT_BUFFER_SIZE` environment variable (clamped to `[16, 1_000_000]`). Operators see a warn log (`vsi: event buffer full, dropping event`) on the first drop in a burst and on each 10× escalation, so sustained drops are visible without flooding the log.

**Tuning the buffer.** Larger buffers absorb longer back-pressure spikes but trade off:
- **Memory:** ~1 KB per slot at typical event sizes; e.g. 256 → ~256 KB per client, 10_000 → ~10 MB per client. Multiply by your concurrent VSI client count.
- **Latency:** when a slow client catches up, every event in the buffer is delivered before any new one — a 10_000-deep buffer means the client may see events that are tens of seconds old. The 30s ping is unaffected (sent on a separate goroutine), but the application's view of "now" can lag.
- **Failure radius:** with a small buffer you drop fast and resync fast; with a large buffer the client stays "almost caught up" for longer before giving up.

The default of 256 is sized for healthy clients on a normal event stream (one inbound call generates ~10 events). Increase only when you have a legitimate slow-consumer scenario you can't fix at the client.

**Write deadline.** Buffering only covers a client that reads slowly. A client that stops reading altogether eventually fills the socket send buffer, and every server → client frame is bounded by a **5 second write deadline**: a frame that cannot be written within that window fails the write and the server closes the connection, logging the disconnect with `reason=write_timeout`. Previously such a write blocked indefinitely; a client that kept *sending* while it had stopped reading held its connection, its send loop and its event subscription open for the lifetime of the process, because the inbound traffic kept refreshing the idle read deadline that would otherwise have torn it down. Note the `events_dropped` notice is written before the event that triggered it, so a connection wedged badly enough may be closed while reporting drops — the socket was already stuck; the notice is the messenger. Clients should reconnect and resync via REST.

**Example:**

```bash
websocat ws://localhost:8080/v1/vsi
```

---

## SIPREC session recording

VoiceBlender can act as a **Session Recording Server (SRS)**: an SBC or PBX acting
as a Session Recording Client (SRC) forks a call's media to it as a *recording
session* (RFC 7866), and a metadata document (RFC 7865) says whose audio arrives
on which stream.

A recording session is **not a call**. It arrives as an INVITE whose body is
`multipart/mixed` — the SDP plus an `application/rs-metadata+xml` part — with one
`sendonly m=audio` section per recorded party. VoiceBlender answers every section
`recvonly` and never transmits.

Disabled by default. Enable with:

```bash
SIP_TCP_ENABLED=true    # required: a SIPREC INVITE is too large to send over UDP
SIPREC_ENABLED=true
```

> **SIP over TCP is mandatory for inbound SIPREC.** RFC 3261 §18.1.1 requires a
> request larger than 1300 bytes to be sent over a congestion-controlled
> transport when the path MTU is unknown. This is a SIP rule, not a UDP one —
> a UDP datagram may be far larger and IP will fragment it — but sipgo enforces
> the rule by refusing to send an oversized request over UDP at all, rather than
> fragmenting or switching transport itself. A SIPREC INVITE carries the whole
> metadata document alongside the SDP and always exceeds 1300 bytes, so with
> `SIP_TCP_ENABLED=false` the INVITE never arrives.
>
> `SIP_TCP_ENABLED` is a general inbound transport flag, not a SIPREC one — with
> it on, any SIP peer can reach this server over TCP, and ordinary calls work
> over TCP too. It is only *required* by SIPREC because SIPREC is the one thing
> here that never fits in a datagram. Acting as a recording **client** does not
> need it: dialling a `;transport=tcp` recording server opens an outbound
> connection whether or not this server listens on TCP.

### What arrives on the wire

```
INVITE sip:srs@voiceblender.example SIP/2.0
Require: siprec
Contact: <sip:sbc@10.0.0.9:5060>;+sip.src
Content-Type: multipart/mixed;boundary=unique-boundary-1

--unique-boundary-1
Content-Type: application/sdp
Content-Disposition: session;handling=required

m=audio 40000 RTP/AVP 0        ; Alice
a=sendonly
a=label:1
m=audio 40002 RTP/AVP 0        ; Bob
a=sendonly
a=label:2

--unique-boundary-1
Content-Type: application/rs-metadata+xml
Content-Disposition: recording-session

<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>complete</datamode>
  <participant participant_id="pa"><nameID aor="sip:alice@example.com"/></participant>
  <stream stream_id="ta"><label>1</label></stream>
  <participantstreamassoc participant_id="pa"><send>ta</send></participantstreamassoc>
</recording>
--unique-boundary-1--
```

The binding chain is `a=label` → `<stream label=…  stream_id=…>` →
`<participantstreamassoc send=stream_id>` → `<participant><nameID aor=…>`. That is
what lets a recorded m= section be named after a real person.

An INVITE is treated as SIPREC when **any** of these holds, because SBC
conformance varies: `Require`/`Proxy-Require` contains `siprec`, the `Contact`
carries the `+sip.src` feature tag, or the body carries an rs-metadata part.

### Rejection cases

| Condition | Response |
|---|---|
| `SIPREC_ENABLED=false` and `Require: siprec` present | `420 Bad Extension` + `Unsupported: siprec` |
| `SIPREC_ENABLED=false`, SIPREC only hinted at (`+sip.src` or a stray metadata part, no `Require`) | Not claimed — handled as an ordinary call, exactly as before SIPREC existed |
| Claims SIPREC but carries no rs-metadata part | `400 Bad Request` |
| rs-metadata is not parseable | `400 Bad Request` |
| rs-metadata exceeds `SIPREC_METADATA_MAX_BYTES` | `413 Request Entity Too Large` |
| More `m=audio` sections than `SIPREC_MAX_STREAMS` | `486 Busy Here` |

### The resulting leg

The session becomes a leg of type `siprec_in`. It is answered without any
`POST /v1/legs/{id}/answer` unless `SIPREC_AUTO_ANSWER=false`.

Unlike an ordinary SIP leg it has **no privileged primary stream**: `m=audio` #0
is simply the first recorded party. Every stream — including `"0"` — is attached
to a room with `POST /v1/legs/{id}/streams/{streamId}/room` and takes its own
routing role. A recording session is receive-only, so DTMF, playback, TTS, hold
and transfer do not apply to it.

### GET /v1/legs/{id}/siprec

Returns the session's participants, streams, and the binding between them.

```bash
curl localhost:8080/v1/legs/$LEG_ID/siprec
```

```json
{
  "instance_id": "vb-1",
  "leg_id": "9f1c...",
  "session_id": "s1",
  "data_mode": "complete",
  "room_id": "siprec-9f1c...",
  "participants": [
    {"participant_id": "pa", "aor": "sip:alice@example.com", "name": "Alice Smith"},
    {"participant_id": "pb", "aor": "sip:bob@example.com",   "name": "Bob Jones"}
  ],
  "streams": [
    {
      "leg_stream_id": "0", "mid": "1", "label": "1",
      "direction": "recvonly", "codec": "PCMU",
      "room_id": "siprec-9f1c...", "role": "Alice Smith",
      "participant_id": "pa",
      "participant_aor": "sip:alice@example.com",
      "participant_name": "Alice Smith"
    },
    {
      "leg_stream_id": "1", "mid": "2", "label": "2",
      "direction": "recvonly", "codec": "PCMU",
      "room_id": "siprec-9f1c...", "role": "Bob Jones",
      "participant_id": "pb",
      "participant_aor": "sip:bob@example.com",
      "participant_name": "Bob Jones"
    }
  ],
  "metadata": "<?xml version=\"1.0\"...?>"
}
```

The VSI equivalent is the `siprec_get` command with `{"id": "<leg id>"}`.

#### `warnings`

The metadata is checked against the SDP it arrived with, and anything that can
be disproved is reported in `warnings` (absent when there is nothing to report):

```json
"warnings": [
  "participant_mismatch (label 0): offer says alice sends on it, metadata assigns it to bob (pb)"
]
```

This exists because the binding between the two — `a=label` in the SDP,
`<label>` in the metadata — is the only thing that says which recorded stream
carries which party, and a recording client that gets it backwards sends a
document that is schema-valid, internally consistent and completely wrong. The
session establishes, both streams arrive, and every word is attributed to the
other participant. No status code reveals it.

The kinds are `participant_mismatch` (the section's `a=ssrc cname` names a
different party than the metadata binds to that label — only the user part is
compared, since the cname is written by whatever anchored the media),
`ambiguous_sender` (two participants claim to send on one section, so it belongs
to neither), `duplicate_label`, `unknown_label` (the metadata labels a stream the
offer does not carry) and `unclaimed_label` (the metadata declares a stream no
participant sends on).

A section the metadata says nothing about is **not** reported. It is not a
contradiction, and it is what a departure leaves behind: a party's association is
closed with a `disassociate-time` and its stream drops out of the session while
the `m=` section stays in the offer. Reporting it would flag every call somebody
hangs up early on.

An empty or absent `warnings` is **not** an assertion that the mapping is
correct — only that nothing could be disproved. An offer that carries no
`a=label` values, or no `a=ssrc cname`, gives nothing to check the metadata
against, and produces no warnings however wrong it is.

The session is answered and recorded either way: which party is on which label
is the recording client's statement to make, an SRS cannot always disprove it,
and dropping a recording is worse than keeping one that is flagged. Warnings are
also logged at `warn`.

### Using the recorded audio

Because the streams are ordinary leg streams, everything that already works on a
room applies to a passively recorded call.

**Mix each party into a room automatically** — set `SIPREC_ROOM_MODE=per_session`
and every session gets a room named `siprec-<legID>` with one participant per
recorded party, each roled by identity. Then point the existing endpoints at it:

```bash
curl -X POST localhost:8080/v1/rooms/siprec-$LEG_ID/stt -d '{"provider":"deepgram"}'
```

**Record one channel per party** with the ordinary recording endpoint — no new
API. On a `siprec_in` leg, `/record` captures every stream separately and merges
them into a single multi-channel WAV, one channel per recorded party:

```bash
curl -X POST   localhost:8080/v1/legs/$LEG_ID/record
curl -X DELETE localhost:8080/v1/legs/$LEG_ID/record
```

`recording.finished` then carries `multi_channel_file` and a `channels` map
**keyed by participant identity** — their AOR, display name or participant ID —
rather than by leg ID as a room recording is:

```json
{
  "type": "recording.finished",
  "leg_id": "9f1c...",
  "file": "/recordings/20260807_124420_multichannel_d4989c5a.wav",
  "multi_channel_file": "/recordings/20260807_124420_multichannel_d4989c5a.wav",
  "channels": {
    "sip:alice@example.com": {"channel": 0, "start_ms": 0, "end_ms": 505},
    "sip:bob@example.com":   {"channel": 1, "start_ms": 0, "end_ms": 505}
  }
}
```

Streams on one session can negotiate different codecs, so every capture is
resampled to `DEFAULT_SAMPLE_RATE` before the merge — all channels share one
rate. A party whose capture produced nothing is listed in `omitted_legs` rather
than failing the merge. `/record/pause` and `/record/resume` apply to every
channel at once, so they stay time-aligned. Set `SIPREC_AUTO_RECORD=true` to
start this automatically when a session arrives.

Each capture runs on the recorder's own 20 ms clock, so a stream that stops
sending RTP — a party on hold, or loss — is silence-filled for exactly that
long. The audio that follows keeps its real offset instead of sliding forward
over the gap.

**Or place streams yourself** with `SIPREC_ROOM_MODE=none` (the default):

```bash
curl -X POST localhost:8080/v1/legs/$LEG_ID/streams/0/room \
  -d '{"room_id": "analysis", "role": "customer"}'
curl -X POST localhost:8080/v1/legs/$LEG_ID/streams/1/room \
  -d '{"room_id": "analysis", "role": "agent"}'
```

A leg never hears its own sibling streams, and every recording stream is
receive-only, so the recorded parties cannot bleed into each other's mixes.

### Events

| Event | When |
|---|---|
| `siprec.session_started` | A recording session was accepted. Carries the participants and the stream→participant bindings — this is the event a controller listens for to decide what to do with the audio. |
| `siprec.session_ended` | The recording session ended. |
| `siprec.metadata_updated` | The metadata document was updated on a re-INVITE or UPDATE. Carries the joined/left participant IDs, added/removed stream IDs, and the refreshed stream bindings. |
| `siprec.participant_joined` | A party joined the call being recorded, with the stream now carrying them. |
| `siprec.participant_left` | A party left the call being recorded, with the stream that had carried them. |

`leg.ringing`, `leg.connected` and `leg.disconnected` also fire, with
`leg_type: "siprec_in"`.

### Mid-session changes

An SRC signals a party joining or leaving with a re-INVITE carrying an updated
metadata document, usually `datamode=partial`.

- **A party joins** — the re-offer adds an `m=audio` section and the metadata
  names the participant and binds it to the new `a=label`. VoiceBlender
  negotiates the section `recvonly`, binds it, publishes
  `siprec.participant_joined`, and — under `SIPREC_ROOM_MODE=per_session` —
  attaches the new stream to the session room with the participant's role.
- **A party leaves** — the metadata closes the association with a
  `disassociate-time`. The participant and the stream it was sending on are
  dropped and `siprec.participant_left` fires. A re-offer that also disables the
  section with port 0 tears the stream down and returns its RTP port.
- **Metadata-only re-INVITE** — legal and common for a rename or a hold. There
  is no SDP part; the update applies and the negotiated media is untouched.

`datamode` semantics follow RFC 7865 §6.1: a `complete` document replaces the
server's whole view, so anything absent from it is gone; a `partial` one is
merged and **never** deletes on absence — removal is always signalled positively
with a `disassociate-time`.

### POST /v1/rooms/{id}/siprec — recording *client*

The other direction: VoiceBlender as the **Session Recording Client**, forking a
room it already hosts to an external recording server. Each participant's *own*
audio goes on its own `sendonly m=audio` section — not the room mix — and the
metadata names them.

Requires `SIPREC_SRC_ENABLED=true`. Off by default: it lets an API caller stream
a room's audio to an arbitrary SIP destination.

```bash
curl -X POST localhost:8080/v1/rooms/conf-1/siprec -d '{
  "srs_uri": "sip:srs@recorder.example.com:5060;transport=tcp",
  "session_id": "conf-1-rec"
}'
```

| Field | Meaning |
|---|---|
| `srs_uri` | SIP URI of the recording server. Use `;transport=tcp` — the INVITE carries the metadata document and exceeds the 1300-byte limit RFC 3261 §18.1.1 puts on UDP requests. `sips:` / `;transport=tls` selects TLS. |
| `leg_ids` | Which participants to record. Omit to record everything in the room. |
| `session_id` | Communication session ID in the metadata. Defaults to the room ID. |
| `auth_username` / `auth_password` | Digest credentials, when the recording server challenges. |
| `headers` | Extra SIP headers. `Require: siprec` is always sent. |
| `app_id` | Tagged onto the resulting leg and its events. |

Returns the resulting `siprec_out` leg. **Delete that leg to end the session** —
`DELETE /v1/legs/{id}` sends the BYE and tears down the taps.

#### Choosing what to record

By default every participant of the room is forked. `leg_ids` narrows that, and
addresses **streams as well as legs**:

```bash
curl -X POST localhost:8080/v1/rooms/conf-1/siprec -d '{
  "srs_uri": "sip:srs@recorder.example.com:5060;transport=tcp",
  "leg_ids": ["leg-abc", "leg-def#1"]
}'
```

`"leg-abc"` is that leg's own audio; `"leg-def#1"` is one of `leg-def`'s
secondary streams, such as a translated feed.

An entry is either a leg ID or `<legID>#<streamID>` — the same participant IDs
the mixer and the routing matrix use. A secondary stream may belong to a leg that
is not itself in the room, which is the point of a cross-room stream, and it is
still addressable. An entry that names nothing in the room is a `404`.

Selection is by identity, not by position: whatever order you list them in, the
parties are assigned to m-lines in a stable sorted order, so the same selection
always produces the same layout.

#### A single call, without a room

`POST /v1/legs/{id}/siprec` forks one call directly as a two-section session —
what the far end says, and what this server sends them — with no room involved:

```bash
curl -X POST localhost:8080/v1/legs/$LEG_ID/siprec -d '{
  "srs_uri": "sip:srs@recorder.example.com:5060;transport=tcp"
}'
```

`leg_ids` is ignored here; the two sections are fixed. This path taps the leg
directly rather than the mixer, using tap slots of its own, so an ordinary
`/record` on the same leg keeps working alongside it. A leg that is itself a
recording session cannot be forked onward.

A participant's address of record in the metadata comes from the leg's
`X-SIPREC-AOR` header when it has one, otherwise a synthetic
`sip:<leg-id>@voiceblender.local`. Recording-session legs are never themselves
recorded, so pointing two SRC sessions at one room does not nest.

The VSI equivalent is `room_siprec_start`.

---

## WebRTC

### POST /v1/webrtc/offer

Establish a WebRTC leg via SDP offer/answer exchange. The browser sends an SDP offer and receives an SDP answer plus a leg ID. The answer is returned immediately without waiting for ICE gathering to complete — use the trickle ICE endpoints below to exchange candidates incrementally.

**Request:**

```json
{
  "sdp": "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n...",
  "app_id": "myapp"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `sdp` | string | yes | SDP offer from the browser. |
| `app_id` | string | no | Tag the leg for event filtering. Carried on every event this leg emits, and matched against the VSI `app_id` filter. Omit it and the leg's events carry an empty `app_id`, so any non-empty filter drops them. |

**Response:** `200 OK`

```json
{
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "sdp": "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n..."
}
```

The returned `leg_id` can be used with all `/v1/legs` and `/v1/rooms` endpoints.

The same request body is accepted by the VSI `webrtc_offer` command.

**`leg.connected` event:** fires only once the underlying peer connection reaches the `Connected` state (post-ICE/DTLS). Wait for it before pushing media into the leg.

**NAT/firewall deployments:** when VoiceBlender runs behind NAT (e.g. Docker, a VPC NAT gateway), set `WEBRTC_EXTERNAL_IPS` to the host's public IPv4/IPv6 address(es) — pion will substitute them into host ICE candidates, allowing remote peers that only emit private host candidates of their own to still reach VB.

**Errors:**
- `400` — Invalid JSON or invalid SDP offer
- `500` — Peer connection, track creation, or answer generation failed

**Audio codec:** PCMU (G.711 u-law), 8kHz, mono.

---

### POST /v1/legs/{id}/amd

Start answering machine detection on an already-connected SIP leg. This is an alternative to including the `amd` object in `POST /v1/legs` — use this endpoint when AMD was not enabled at call creation time.

All AMD parameters are optional. An empty request body `{}` enables AMD with all defaults. See **AMD Parameters** above for the full parameter reference, including the `total_analysis_time` constraints that reject self-defeating params with `400`.

**Request:**

```json
{
  "beep_timeout": 10000
}
```

**Response:** `200 OK`

```json
{
  "status": "started"
}
```

**Errors:**
- `400` — Invalid AMD params (a negative value on any field, or a `total_analysis_time` too short to reach any verdict) or leg is not a SIP leg
- `404` — Leg not found
- `409` — Leg is not in `connected` state (AMD can only start on answered calls)

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

### WebRTC over VSI

The same offer/answer/trickle-ICE flow is also available over the `/v1/vsi` WebSocket — useful when a client is already connected to receive events and wants to avoid an extra HTTP round trip per ICE candidate. Three commands mirror the REST endpoints:

| Command | Payload | Result |
|---------|---------|--------|
| `webrtc_offer` | `{"sdp":"..."}` | `{"leg_id":"...","sdp":"..."}` |
| `webrtc_add_candidate` | `{"id":"...","candidate":{...}}` | `{"status":"added"}` |
| `webrtc_get_candidates` | `{"id":"..."}` | `{"candidates":[...],"done":true}` |

**Example exchange:**

```json
// Client → server
{"type":"webrtc_offer","request_id":"r1","payload":{"sdp":"v=0\r\no=- ..."}}

// Server → client
{"type":"webrtc_offer.result","request_id":"r1","data":{"leg_id":"550e8400-...","sdp":"v=0\r\no=- ..."}}

// Client → server (one frame per browser-side candidate)
{"type":"webrtc_add_candidate","request_id":"r2","payload":{"id":"550e8400-...","candidate":{"candidate":"candidate:...","sdpMid":"0","sdpMLineIndex":0}}}

// Server → client
{"type":"webrtc_add_candidate.result","request_id":"r2","data":{"status":"added"}}

// Client polls until done=true
{"type":"webrtc_get_candidates","request_id":"r3","payload":{"id":"550e8400-..."}}
{"type":"webrtc_get_candidates.result","request_id":"r3","data":{"candidates":[{"candidate":"candidate:...","sdpMid":"0","sdpMLineIndex":0}],"done":false}}
```

The returned `leg_id` is interchangeable with REST: subsequent `mute_leg`, `add_leg_to_room`, `delete_leg`, etc. all accept it. Errors follow the standard VSI error envelope (`{"type":"error","request_id":"...","data":{"code":...,"message":"..."}}`).

---

## Webhooks

Webhooks deliver real-time event notifications via HTTP POST. There are three ways to configure webhooks:

1. **Global webhook** — set via `WEBHOOK_URL` and `WEBHOOK_SECRET` environment variables. Receives all events that don't have a more specific webhook.
2. **Per-leg webhook** — set via `webhook_url` / `webhook_secret` in the create leg request body, or via `X-Webhook-URL` / `X-Webhook-Secret` SIP headers on inbound calls.
3. **Per-room webhook** — set via `webhook_url` / `webhook_secret` in the create room request body.

### Routing Priority

When an event is emitted, webhooks are resolved in this order (highest to lowest):

1. **Leg's webhook** — used when the event carries a `leg_id` and that leg has a `webhook_url` set.
2. **Room's webhook** — used when the event has a `room_id` (but no matching leg webhook) and that room has a `webhook_url` set.
3. **Global webhook** — used for all other events (configured via `WEBHOOK_URL` env var).

For events that carry both `leg_id` and `room_id` (e.g. `speaking.started`, `stt.text`), the leg's webhook takes precedence over the room's webhook.

For inbound SIP calls, the `X-Webhook-URL` and `X-Webhook-Secret` SIP headers in the INVITE can set per-leg webhooks on a call-by-call basis, overriding the `WEBHOOK_URL` environment variable.

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

Delivery is **best-effort, not guaranteed**. An event dropped because the queue was full, or abandoned after all 3 attempts failed, is never redelivered. Both cases are counted — see `voiceblender_webhook_dropped_total` and `voiceblender_webhook_deliveries_total` under [GET /metrics](#get-metrics).

### Signature Verification

When a `secret` is configured, a `X-Signature-256` header is included:

```
X-Signature-256: sha256=<hex-encoded-hmac-sha256>
```

The signature is computed over the raw JSON request body using HMAC-SHA256 with the webhook secret as the key.

### Deduplication

Every delivery carries an `X-Event-Id` header equal to the `event_id` field in the body:

```
X-Event-Id: 550e8400-e29b-41d4-a716-446655440000
```

`event_id` is a UUID assigned once when the event is published, so it is **stable across all 3 delivery attempts** of the same event and **identical for every subscriber** that receives it — the webhook POST and the VSI WebSocket frame for one event share an id.

Because a retried attempt looks exactly like a fresh delivery to your endpoint (same body, same signature), treat `event_id` as an idempotency key: record it and ignore an event whose id you have already processed. Distinct events never share an id.

Example receiver:

```python
seen = set()  # use a TTL cache or your database in production

@app.post("/webhooks/voiceblender")
def handle(request):
    event_id = request.headers["X-Event-Id"]
    if event_id in seen:
        return "", 200          # already processed — ack and drop
    seen.add(event_id)
    process(request.json())
    return "", 200
```

### Event Envelope

Event data fields are flattened into the top-level JSON object alongside the envelope fields — there is no `"data"` wrapper.

```json
{
  "type": "leg.ringing",
  "timestamp": "2026-03-01T11:05:00.123Z",
  "event_id": "8f14e45f-ceea-467a-9575-9b0ba1f0e3a1",
  "instance_id": "550e8400-e29b-41d4-a716-446655440000",
  "custom_data": { "order_id": "A-991", "tenant": 42 },
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "leg_type": "sip_inbound",
  "from": "sip:alice@example.com",
  "to": "sip:bob@example.com",
  "offered_codecs": [
    { "name": "opus", "payload_type": 111, "clock_rate": 48000, "priority": 1 },
    { "name": "PCMU", "payload_type": 0,   "clock_rate": 8000,  "priority": 2 },
    { "name": "PCMA", "payload_type": 8,   "clock_rate": 8000,  "priority": 3 }
  ]
}
```

**`offered_codecs`** (inbound SIP only) lists the audio codecs from the remote INVITE's offer SDP, in offer order. `priority` is 1-based and matches the order — lower value = higher priority. Use any `name` from this list as the `codec` field on `POST /v1/legs/{id}/early-media` or `POST /v1/legs/{id}/answer` to force that codec for the answer SDP.

**`source_address`** (inbound SIP only) is the transport-layer `host:port` the INVITE arrived on. Use it to decide whether to challenge the call via `POST /v1/legs/{id}/challenge`.

**`authenticated`** / **`auth_username`** (inbound SIP only) are present and set when the INVITE carried digest credentials that VoiceBlender verified against a prior `/challenge` — i.e. the credentialed retry surfaced as a new, authenticated leg. They are omitted for un-challenged calls.

**`custom_data`** is the opaque JSON attached to the event's leg (see [Custom data](#custom-data)). It appears on every leg-scoped event, from the point it was attached until after `leg.disconnected`, and is omitted entirely when the leg has none. Room-scoped events never carry it.

All events include `event_id` and `instance_id` alongside the event-specific fields.

### Event Types

All event data uses typed structs with consistent field names. Events scoped to a leg include `leg_id`, events scoped to a room include `room_id`, and events that can target either include both (with the unused field omitted).

| Event | Description | Data Fields |
|-------|-------------|-------------|
| `leg.ringing` | SIP or WhatsApp call ringing | `leg_id`, `leg_type` (`sip_inbound`/`sip_outbound`/`whatsapp_in`), `from`, `to` (inbound); `leg_id`, `leg_type`, `uri`, `from` (outbound). `sip_headers` included when `X-*` headers are present. `offered_codecs` included on inbound SIP — array of `{name, payload_type, clock_rate, priority}` from the remote SDP offer, in priority order. |
| `leg.early_media` | Outbound leg received 183 Session Progress with SDP; media pipeline active | `leg_id`, `leg_type` |
| `leg.connected` | Leg answered/connected | `leg_id`, `leg_type` |
| `leg.disconnected` | Leg hung up | `leg_id`, `cdr`, `quality` (see CDR-style structure below) |
| `leg.joined_room` | Leg added to room | `leg_id`, `room_id` |
| `leg.left_room` | Leg removed from room | `leg_id`, `room_id` |
| `leg.muted` | Leg muted | `leg_id` |
| `leg.unmuted` | Leg unmuted | `leg_id` |
| `leg.deaf` | Leg deafened | `leg_id` |
| `leg.undeaf` | Leg undeafened | `leg_id` |
| `leg.hold` | Leg put on hold (local or remote) | `leg_id`, `leg_type` |
| `leg.unhold` | Leg taken off hold (local or remote) | `leg_id`, `leg_type` |
| `leg.command_failed` | An asynchronous leg command failed after the HTTP 202 was returned | `leg_id`, `command` (e.g. `ring`, `early_media`, `hold`, `unhold`, `add_to_room`), `error` |
| `leg.transfer_initiated` | We sent a SIP REFER for this leg | `leg_id`, `kind` (`blind`/`attended`), `target`, `replaces_leg_id` |
| `leg.transfer_requested` | A peer sent us a SIP REFER targeting this leg. In the default app-driven model it is a decision request — respond via `accept_transfer`/`decline_transfer` (see [Receiving a transfer](#receiving-a-transfer-inbound-refer)). `declined` is vestigial (always false) | `leg_id`, `kind`, `target`, `replaces_call_id`, `declined` |
| `leg.transfer_progress` | NOTIFY sipfrag for an in-flight transfer | `leg_id`, `status_code`, `reason` |
| `leg.transfer_completed` | Transfer reached terminal 2xx; leg is hung up | `leg_id`, `status_code`, `reason` |
| `leg.transfer_failed` | Transfer ended in non-2xx or local error | `leg_id`, `status_code`, `reason`, `error` |
| `dtmf.received` | DTMF digit received | `leg_id`, `digit`, `seq` |
| `rtt.received` | RTT (T.140 / RFC 4103) text chunk received | `leg_id`, `text`, `seq`, `loss_marker` |
| `speaking.started` | Participant started speaking | `leg_id`, `room_id` (if in a room) |
| `speaking.stopped` | Participant stopped speaking | `leg_id`, `room_id` (if in a room) |

> **Note:** `speaking.started` and `speaking.stopped` events fire for any connected leg, whether standalone or in a room. When the leg is in a room, the event includes `room_id`; standalone legs omit it.
>
> **Opt-in:** Speech detection is **disabled by default**. Enable it globally by setting `SPEECH_DETECTION_ENABLED=true`, or per call by setting `"speech_detection": true` on `POST /v1/legs` (outbound) or `POST /v1/legs/{id}/answer` (inbound). Per-call values override the global default.

| `playback.started` | Playback began | `leg_id` or `room_id`, `playback_id` |
| `playback.finished` | Playback ended | `leg_id` or `room_id`, `playback_id`, `reason`, `played_ms` |
| `playback.error` | Playback failed | `leg_id` or `room_id`, `playback_id`, `error` |
| `tts.started` | TTS synthesis began playing | `leg_id` or `room_id`, `tts_id` |
| `tts.finished` | TTS synthesis finished playing | `leg_id` or `room_id`, `tts_id`, `reason`, `played_ms` |
| `tts.error` | TTS synthesis or playback failed | `leg_id` or `room_id`, `tts_id`, `error`, `category` |
| `tts.staged` | Preflight TTS finished synthesizing and is ready to commit | `leg_id`, `tts_id`, `bytes`, `duration_ms` |
| `tts.discarded` | Staged TTS was dropped without being played | `leg_id`, `tts_id`, `reason` (`app`, `expired`, `leg_gone`) |
> **Note:** `playback.finished` and `tts.finished` carry `reason` and `played_ms` so you can tell whether a prompt was heard in full or was cut short, and by how much.
>
> `reason` is `completed` when the audio played through to its end, and `stopped` when it did **not** reach the end — **for any reason**. That includes an app-initiated stop, a barge-in, and a leg hanging up: all three cancel the same playback, and they cannot be told apart from `reason` alone. To distinguish a hangup from a deliberate stop, look for a co-emitted `leg.disconnected` event. Tone playback never ends on its own, so it always reports `stopped`.
>
> `played_ms` is how much audio was actually written to the leg or room, in milliseconds. It counts audio played, **not** the source file's duration: a `repeat`ed playback accumulates across every iteration, so `played_ms` can exceed the length of the file.

| `recording.started` | Recording began | `leg_id` or `room_id`, `file` (does not exist yet — the path only appears when the recording stops) |
| `recording.finished` | Recording ended — including when a room recording is [stopped automatically](#automatic-stop) because the room ran out of participants | `leg_id` or `room_id`, `file`, `multi_channel_file`, `channels`, `omitted_legs` (multi-channel only; `omitted_legs` only when a participant's capture failed) |
| `recording.paused` | Recording paused (audio replaced with silence) | `leg_id` or `room_id` |
| `recording.resumed` | Recording resumed from a paused state | `leg_id` or `room_id` |
| `stt.text` | Speech-to-text transcript | `leg_id`, `room_id` (if room STT), `stream_id` (which of the leg's streams; empty when the leg's audio is the call), `text`, `is_final`, `speech_final`, `audio_start_ms`, `audio_end_ms` |
| `stt.turn` | Speech-to-text turn boundary | `leg_id`, `room_id` (if room STT), `stream_id`, `event`, `turn_index`, `text`, `end_of_turn_confidence`, `audio_window_start_ms`, `audio_window_end_ms`, `last_word_end_ms`, `words`, `languages` |
| `agent.connected` | Agent connected to provider | `leg_id` or `room_id`, `conversation_id` |
| `agent.disconnected` | Agent session ended | `leg_id` or `room_id` |
| `agent.user_transcript` | User speech transcribed by agent | `leg_id` or `room_id`, `text` |
| `agent.agent_response` | Agent generated a response | `leg_id` or `room_id`, `text` |
| `room.created` | Room created | `room_id` |
| `room.deleted` | Room deleted | `room_id` |
| `room.bridged` | Two rooms' mixers joined | `bridge_id`, `room_a_id`, `room_b_id`, `direction` |
| `room.bridge_updated` | Bridge direction changed | `bridge_id`, `room_a_id`, `room_b_id`, `direction` |
| `room.unbridged` | Bridge torn down | `bridge_id`, `room_a_id`, `room_b_id`, `reason` |
| `amd.result` | Answering machine detection completed | `leg_id`, `result`, `initial_silence_ms`, `greeting_duration_ms`, `total_analysis_ms` |
| `amd.beep` | Voicemail beep tone detected | `leg_id`, `beep_ms` |
| `sip.registration_attempt` | An inbound REGISTER (that would create/remove a binding) is awaiting a challenge/accept/reject decision; after `SIP_INBOUND_AUTH_CONSULT_TIMEOUT_MS` the `SIP_INBOUND_REGISTER_DEFAULT` fallback applies (reject by default) | `attempt_id`, `aor`, `contact`, `source_address`, `transport`, `user_agent`, `call_id`, `has_authorization` |
| `sip.registration_active` | A SIP AOR binding was created or refreshed | `aor`, `contact`, `socket`, `transport`, `user_agent`, `call_id`, `granted_expires_seconds`, `expires_at` |
| `sip.registration_expired` | A SIP AOR binding was removed | `aor`, `contact`, `socket`, `reason` (`ttl` / `unregistered` / `forced` / `replaced`) |
> **LiveKit participants.** Remote LK participants do not get their own special event types. Each appears as a regular `leg.connected` / `leg.disconnected` for a `livekit_participant` leg (Model B). `speaking.started` / `speaking.stopped` apply per-leg as usual. The `leg.disconnected.reason` for an LK participant leg is `livekit_participant_left`.

#### `amd.result` — Answering Machine Detection

Emitted when AMD analysis completes on an outbound call. The `result` field is one of:

- `human` — Short greeting followed by silence (likely a person).
- `machine` — Long greeting (likely voicemail or IVR).
- `no_speech` — No speech detected within the initial silence timeout.
- `not_sure` — Analysis timed out without a confident determination.

```json
{
  "type": "amd.result",
  "timestamp": "2026-04-01T12:00:00Z",
  "instance_id": "abc-123",
  "leg_id": "leg-456",
  "result": "machine",
  "initial_silence_ms": 120,
  "greeting_duration_ms": 1680,
  "total_analysis_ms": 1800
}
```

When `beep_timeout` is set and the result is `machine`, the `amd.result` event is sent immediately, then the analyzer continues listening for the voicemail beep tone (800–1200 Hz). If detected, a separate `amd.beep` event is emitted:

```json
{
  "type": "amd.beep",
  "timestamp": "2026-04-01T12:00:03Z",
  "instance_id": "abc-123",
  "leg_id": "leg-456",
  "beep_ms": 1400
}
```

The `beep_ms` field is the time from machine detection to beep detection. Use this event to know exactly when to start playing your voicemail message.

#### `leg.disconnected` — CDR-Style Structure

The `leg.disconnected` event uses a `cdr` object for disconnect reason and timing, plus an optional `quality` object for RTP metrics.

**Answered call with quality metrics:**

```json
{
  "type": "leg.disconnected",
  "timestamp": "2026-03-24T14:30:00.123Z",
  "instance_id": "inst-abc",
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "cdr": {
    "reason": "remote_bye",
    "duration_total": 125.43,
    "duration_answered": 120.10
  },
  "quality": {
    "mos_score": 4.21,
    "rtp_packets_received": 6025,
    "rtp_packets_lost": 12,
    "rtp_jitter_ms": 3.45
  }
}
```

**Unanswered call (no quality):**

```json
{
  "type": "leg.disconnected",
  "timestamp": "2026-03-24T14:30:08.650Z",
  "instance_id": "inst-abc",
  "leg_id": "550e8400-e29b-41d4-a716-446655440000",
  "cdr": {
    "reason": "caller_cancel",
    "duration_total": 8.52,
    "duration_answered": 0
  }
}
```

#### `cdr` Object

| Field | Type | Description |
|-------|------|-------------|
| `reason` | string | See **Disconnect Reasons** below |
| `duration_total` | float | Seconds from leg creation (INVITE sent/received) to disconnect |
| `duration_answered` | float | Seconds from answer (200 OK) to disconnect. `0` if the leg was never answered. |

#### `quality` Object (omitted when no media was received)

| Field | Type | Description |
|-------|------|-------------|
| `mos_score` | float | Mean Opinion Score (1.0–5.0) estimated via simplified E-model (ITU-T G.107) from packet loss and jitter |
| `rtp_packets_received` | integer | Total inbound RTP audio packets received |
| `rtp_packets_lost` | integer | Estimated lost packets based on sequence number gaps |
| `rtp_jitter_ms` | float | Inter-arrival jitter in milliseconds (RFC 3550 §A.8) |

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
| `room_deleted` | Leg was in a room that was deleted via `DELETE /v1/rooms/{id}` |
| `transfer_completed` | Leg ended because a transfer it initiated reached terminal 2xx |
| `rejected` | Inbound leg rejected by API via `DELETE /v1/legs/{id}` with `reason` (also see other reason values from the rejection mapping table) |
| `mixer_panic` | The leg's audio path failed inside the mixer and the leg was torn down. The leg had already left its room (`leg.left_room`); it is deaf and mute at this point, so it is hung up rather than left connected. Its room, and any other legs in it, are unaffected |

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
| `INSTANCE_ID` | _(auto-generated UUID)_ | Instance identifier, included in all API response bodies and webhook events |
| `HTTP_ADDR` | `:8080` | REST API listen address |
| `ALLOWED_IPS` | _(empty = allow all)_ | Comma-separated allowlist of IPs and CIDR ranges (IPv4 and IPv6, in any mix) gating every HTTP endpoint, including the `/v1/vsi` event WebSocket, `/v1/legs/websocket`, the `/v1/legs/moq` WebTransport endpoint, `/metrics`, and pprof. Bare addresses are treated as host routes (`/32` or `/128`); malformed entries fail server startup. Only `X-Forwarded-For` is consulted as a proxy header (see `TRUST_PROXY_HEADERS`). Examples: `127.0.0.1`, `10.0.0.0/8,192.168.0.0/16`, `2001:db8::/32,::1`. |
| `TRUST_PROXY_HEADERS` | `false` | When `true`, the client IP used for the `ALLOWED_IPS` check is taken from the leftmost entry in `X-Forwarded-For` (falling back to the socket peer when the header is absent). Default `false` ignores `X-Forwarded-For`. Enable only behind a trusted reverse proxy that unconditionally overwrites the header. |
| `SIP_BIND_IP` | `127.0.0.1` | IP advertised in SDP/Contact/Via headers (auto-detected if `0.0.0.0`) |
| `SIP_LISTEN_IP` | _(same as SIP_BIND_IP)_ | IP to bind the UDP socket on |
| `SIP_PORT` | `5060` | SIP listen port |
| `SIP_HOST` | `voiceblender` | SIP User-Agent name |
| `ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs (comma-separated) |
| `RECORDING_DIR` | `/tmp/recordings` | Recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`). Verbatim transcript text, DTMF digits and full event payloads are logged only at `debug`. |
| `WEBHOOK_URL` | _(none)_ | Global webhook URL. Events without a per-leg or per-room webhook are delivered here. |
| `WEBHOOK_SECRET` | _(none)_ | HMAC-SHA256 signing secret for the global webhook. |
| `ELEVENLABS_API_KEY` | _(none)_ | Default ElevenLabs API key for TTS, STT, and Agent features (can be overridden per-request via `api_key` in the request body) |
| `VAPI_API_KEY` | _(none)_ | Default VAPI API key for Agent features when `provider=vapi` (can be overridden per-request via `api_key` in the request body) |
| `DEEPGRAM_API_KEY` | _(none)_ | Default Deepgram API key for STT, TTS, and Agent features when `provider=deepgram` (can be overridden per-request via `api_key` in the request body) |
| `SPEECHMATICS_API_KEY` | _(none)_ | Default Speechmatics API key for STT when `provider=speechmatics` (can be overridden per-request via `api_key` in the request body) |
| `SPEECHMATICS_URL` | `wss://eu2.rt.speechmatics.com/v2` | Speechmatics realtime WebSocket endpoint. Point it at another region (`eu`, `us`, `global`) for data residency, or at a self-hosted realtime container. |
| `S3_BUCKET` | _(none)_ | S3 bucket name (required for `storage=s3` recordings) |
| `S3_REGION` | `us-east-1` | AWS region for S3 |
| `S3_ENDPOINT` | _(none)_ | Custom S3 endpoint for S3-compatible stores (MinIO, etc.) |
| `S3_PREFIX` | _(none)_ | Key prefix for S3 objects (e.g. `recordings/`) |
| `GCS_BUCKET` | _(none)_ | GCS bucket name (required for `storage=gcs` recordings). Uses Application Default Credentials / Workload Identity. |
| `GCS_OBJECT_NAME_PREFIX` | _(none)_ | Object name prefix for GCS uploads (e.g. `recordings` or bare workspace id). Trailing slash added if missing. |
| `TTS_CACHE_ENABLED` | `false` | Enable disk-backed TTS audio cache. Cached audio is stored on disk and persists across restarts. |
| `TTS_CACHE_DIR` | `/tmp/tts_cache` | Directory for cached TTS audio files. |
| `TTS_CACHE_INCLUDE_API_KEY` | `false` | Include API key in TTS cache key (set `true` if different keys map to different voice clones) |

Verbatim transcript text, DTMF digits and full event payloads appear only at
`LOG_LEVEL=debug`. Debug output is therefore PII-bearing and should not be
shipped to a general-purpose log sink.

---

## SIP Session Timers (RFC 4028)

- Accepts session timers requested by the remote UA (inbound and outbound)
- Minimum session interval: 90 seconds (`Min-SE`)
- Supports both `refresher=uac` and `refresher=uas` roles
- Re-INVITEs (including hold/unhold) reset the session timer
- Expired sessions disconnect with reason `session_expired`

---

## SIP Registrations (AOR)

VoiceBlender accepts inbound SIP `REGISTER` requests on UDP, TCP, and TLS
(the latter on `SIP_TLS_PORT`). All REGISTERs are **auto-approved** —
authentication is expected to be performed by a SIP proxy in front of
VoiceBlender. The registrar maintains an in-memory map of canonicalised
AORs to one or more bound contacts, each keyed by the exact transport
socket the REGISTER arrived on. Bindings expire automatically when their
TTL elapses (`Expires` header / `;expires=` Contact param, clamped to
the configured maximum).

### Registration lifecycle

| Step | What happens | Event emitted |
|------|--------------|---------------|
| `REGISTER` with non-zero expires | Binding added/refreshed; 200 OK is returned with the granted expiry | `sip.registration_active` |
| `REGISTER` with `Contact: *` and `Expires: 0` | Every contact under the AOR is removed | `sip.registration_expired` (`reason: unregistered`) |
| `REGISTER` with `expires=0` on a specific Contact | That single contact is removed | `sip.registration_expired` (`reason: unregistered`) |
| TTL elapses | Sweeper removes the binding | `sip.registration_expired` (`reason: ttl`) |
| `DELETE /v1/sip/registrations/{aor}` | Operator force-unbinds the AOR (or a single contact via `?contact=`) | `sip.registration_expired` (`reason: forced`) |
| New REGISTER while `SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS=false` | Prior Contacts under the AOR are displaced | `sip.registration_expired` (`reason: replaced`) |

When `SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS=true` (default), the same
AOR can be registered from multiple Contacts simultaneously (e.g. a user
running a softphone on desktop and a SIP client on mobile). Each
Contact's binding has its own socket and expires_at.

### Dialing a registered AOR

`POST /v1/legs` looks up the value of the `to` (or legacy `uri`) field
in the registrar. When a match is found, the outbound INVITE is sent to
the binding's transport socket — bypassing the URI's host:port — and
reusing the persistent TCP/TLS connection from the original REGISTER
where applicable. No change to the API call shape:

```bash
curl -X POST http://vb.local:8080/v1/legs \
  -H "Content-Type: application/json" \
  -d '{"type":"sip","to":"sip:alice@vb.example","from":"support"}'
```

When the AOR has **multiple bound contacts**, the INVITE is
**parallel-forked**: every Contact rings simultaneously, the first to
answer (2xx) wins, and the other branches are CANCELled (RFC 3261 §16).
The leg's lifecycle events report the winning branch only.

When the `to` value does not match any AOR, the leg is routed via the
URI's host:port exactly as before.

### GET /v1/sip/registrations

List every currently bound AOR contact.

```bash
curl http://vb.local:8080/v1/sip/registrations
```

```json
{
  "instance_id": "abc-123",
  "bindings": [
    {
      "aor": "sip:alice@vb.example",
      "contact": "sip:alice@10.0.0.5:5060",
      "socket": "203.0.113.7:51020",
      "transport": "udp",
      "user_agent": "PJSUA/2.13",
      "call_id": "c8f6...",
      "created_at": "2026-06-01T11:30:00Z",
      "last_refresh": "2026-06-01T11:45:00Z",
      "expires_at": "2026-06-01T12:15:00Z",
      "granted_expires_seconds": 1800
    }
  ]
}
```

### DELETE /v1/sip/registrations/{aor}

Force-unbind every contact under an AOR. The AOR must be URL-encoded in
the path.

```bash
curl -X DELETE "http://vb.local:8080/v1/sip/registrations/sip%3Aalice%40vb.example"
```

Add `?contact=<contact-uri>` to remove only one Contact (the contact URI
must also be URL-encoded).

Responses: `204 No Content` on success; `404 Not Found` when the AOR (or
the specified contact) does not exist.

### VSI commands

The same listing is available over the VSI WebSocket:

```json
{"type": "list_sip_registrations", "request_id": "1"}
```

Server replies:

```json
{"type": "list_sip_registrations.result", "request_id": "1",
 "data": {"bindings": [...]}}
```

Force-unbind is available as `delete_sip_registration`. The AOR is sent plain
(no URL-encoding, unlike the REST path); add `contact` to remove just one
Contact:

```json
{"type": "delete_sip_registration", "request_id": "2",
 "payload": {"aor": "sip:alice@vb.example"}}
```

Server replies `{"type": "delete_sip_registration.result", "request_id": "2",
"data": {"status": "unbound"}}`, or an `error` frame with code `404` when the
AOR (or contact) does not exist.

The `sip.registration_active` / `sip.registration_expired` events flow
through the standard webhook and VSI channels — see Event Types above.

### Inbound REGISTER authentication (digest challenge)

Inbound REGISTER auth is handled **symmetrically with inbound INVITE**: just as
an INVITE always surfaces `leg.ringing` and waits for the client to decide,
every inbound REGISTER that creates or removes a binding is surfaced for a
decision and may be challenged.

Flow:

1. An inbound REGISTER (with a Contact) arrives. VoiceBlender publishes a
   `sip.registration_attempt` event (carrying `attempt_id`, `aor`,
   `source_address`, `transport`, etc.) and **parks** the REGISTER awaiting a
   decision for up to `SIP_INBOUND_AUTH_CONSULT_TIMEOUT_MS` (default 2000 ms).
2. The client responds by `attempt_id`:
   - **challenge** — VoiceBlender replies `401` with a `WWW-Authenticate`
     digest challenge.
   - **accept** — bind and reply `200 OK`.
   - **reject** — reply `403` (or a custom code/reason).
3. If challenged, the UA retries the REGISTER with an `Authorization` header
   (same `Call-ID`). VoiceBlender verifies the digest against the supplied
   credential: on success it binds and replies `200 OK`; on failure it replies
   `403 Forbidden`.

Unlike an INVITE — whose leg can ring indefinitely — a REGISTER is request/response
and cannot be parked forever, so the consult is bounded by the timeout above. If
no decision arrives within that window the **`SIP_INBOUND_REGISTER_DEFAULT`**
fallback applies: **`reject` (403) by default** (fail-closed — an unanswered
REGISTER is denied), or `accept` to bind it (the legacy fail-open behaviour,
appropriate when a front proxy enforces auth).

```
POST /v1/sip/registrations/attempts/{id}/challenge   # body: ChallengeRequest (see POST /v1/legs/{id}/challenge)
POST /v1/sip/registrations/attempts/{id}/accept       # optional body: { "max_expires": 30 }
POST /v1/sip/registrations/attempts/{id}/reject       # optional body: { "code": 403, "reason": "Forbidden" }
```

All three return `202 Accepted` on success, or `404` when the attempt is
unknown or already decided (e.g. the consult timeout already elapsed). The same
actions are available over VSI as `challenge_registration`,
`accept_registration`, and `reject_registration` (payload `{ "id": "<attempt_id>", ... }`).

The `ChallengeRequest` body matches `POST /v1/legs/{id}/challenge`: `realm`
(required), one of `password` / `ha1`, and optional `username`, `algorithm`,
`qop`. Supplied credentials are held only in memory for the challenge's lifetime
(`SIP_INBOUND_AUTH_NONCE_TTL_SECONDS`) and never persisted or returned.

**Capping the granted TTL (`max_expires`).** Both the challenge and accept
bodies accept an optional `max_expires` (seconds). It caps the binding lifetime
granted for that REGISTER: the effective grant is
`min(clamped_requested_expires, max_expires)`, so a UA asking for less still
wins and the value never exceeds `SIP_REGISTRATION_MAX_EXPIRES_SECONDS`. The
registrar's **60 s floor still applies** — a `max_expires` below 60 grants 60.
On a **challenge** the cap is remembered and applied when the credentialed
re-REGISTER binds. Omit it (or send `0`) to leave the registrar's normal clamp
in force; a negative value is rejected with `400`. This lets you force
short-lived registrations — and, when challenging, frequent re-authentication —
without lowering the global clamp.

```jsonc
// Challenge this REGISTER and, once authenticated, grant only a 90 s binding
POST /v1/sip/registrations/attempts/{attempt_id}/challenge
{ "realm": "vb.example", "username": "alice", "password": "s3cret", "max_expires": 90 }

// Accept immediately but cap the binding at 60 s (the minimum)
POST /v1/sip/registrations/attempts/{attempt_id}/accept
{ "max_expires": 60 }
```

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SIP_REGISTRATION_DEFAULT_EXPIRES_SECONDS` | `3600` | Used when the REGISTER carries no `Expires` value |
| `SIP_REGISTRATION_MAX_EXPIRES_SECONDS` | `7200` | Upper clamp on the granted expiry |
| `SIP_REGISTRATION_SWEEP_INTERVAL_MS` | `1000` | How often the expiry sweeper runs |
| `SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS` | `true` | When `false`, every REGISTER replaces any prior Contacts for the AOR |
| `SIP_INBOUND_AUTH_CONSULT_TIMEOUT_MS` | `2000` | How long an inbound REGISTER is parked awaiting a challenge/accept/reject decision before the fallback applies |
| `SIP_INBOUND_REGISTER_DEFAULT` | `reject` | Fallback for an undecided inbound REGISTER: `reject` (403, fail-closed default) or `accept` (bind, legacy fail-open) |
| `SIP_INBOUND_AUTH_NONCE_TTL_SECONDS` | `60` | Lifetime of an issued inbound-auth challenge nonce |

---

## SIP Trunks (Outbound Registrations)

VoiceBlender acts as a SIP UAC and REGISTERs to an upstream SIP
registrar/PBX so the registrar can deliver inbound calls to it and so that
VoiceBlender's outbound calls traverse the registrar's proxy under the
registered identity. Trunks are a typed resource: only the `sip_register`
type is implemented in this release; `ip_ip` (static-IP peering) is reserved
in the API schema and returns `501 Not Implemented` when requested.

### Lifecycle summary

| Action | Trigger | Event published |
|---|---|---|
| Create trunk → first REGISTER succeeds | `POST /v1/sip/trunks` | `sip.outbound_registration_active` |
| Periodic refresh succeeds | timer fires at `granted_expires * SIP_OUTBOUND_REGISTRATION_REFRESH_RATIO` | `sip.outbound_registration_active` (re-emitted for liveness) |
| Transport error or non-2xx response | REGISTER attempt fails (after digest retry) | `sip.outbound_registration_failed` |
| Upstream binding lapsed while still failing | granted lifetime expires and refresh has not recovered | `sip.outbound_registration_expired` (`reason: refresh_failed`) — emitted once per outage; resets on the next successful REGISTER |
| `DELETE /v1/sip/trunks/{id}` | operator removes the trunk | `sip.outbound_registration_expired` (`reason: unregistered`) |
| Server shutdown | every trunk is unregistered in parallel | `sip.outbound_registration_expired` (`reason: unregistered` or `shutdown`) |

### Implicit call wiring

- **Outbound**: `POST /v1/legs` with `from` equal to a registered trunk's
  AOR (full URI like `sip:alice@pbx.example`) or just the user-part
  (`alice`) auto-attaches the trunk's digest credentials and adds a
  loose-route `Route: <trunk's registrar URI;lr>` header. Caller-supplied
  `auth` always wins. The resulting leg's `leg.ringing` event carries
  `trunk_id`.
- **Inbound**: any INVITE whose source socket matches a trunk's upstream
  registrar peer (full host:port, or host-only as a fallback for ephemeral
  source ports) is tagged with `trunk_id` on the `leg.ringing` event.
  No filtering — calls from unknown peers still ring as before.

### POST /v1/sip/trunks

Create and start a trunk. Synchronously validates; REGISTER runs
asynchronously. Returns **202 Accepted** with `{id, type, status}`.

```bash
curl -X POST http://vb.local:8080/v1/sip/trunks \
  -H "Content-Type: application/json" \
  -d '{
    "type": "sip_register",
    "app_id": "acme",
    "sip_register": {
      "registrar_uri":   "sip:pbx.example.com:5060",
      "aor":             "sip:alice@pbx.example.com",
      "username":        "alice",
      "password":        "supersecret",
      "contact_user":    "alice",
      "expires_seconds": 600
    }
  }'
```

Response:

```json
{
  "instance_id": "abc-123",
  "id": "7f5d39c6-2987-4643-9822-5c7ced9080e7",
  "type": "sip_register",
  "status": "registering"
}
```

Field reference (`sip_register` block):

| Field | Required | Description |
|---|---|---|
| `registrar_uri` | yes | Upstream registrar SIP URI. `sips:` / `;transport=tls` switches to TLS — and applies to calls placed over the trunk, not only to the REGISTER. Note that `transport` is a URI *parameter* (`;transport=tls`), not a URI *header*: `?transport=tls` is not a transport selector (RFC 3261 §19.1) and leaves the trunk on UDP. |
| `outbound_proxy` | no | Next-hop proxy for this trunk's REGISTER and for outbound INVITEs from its AOR. See [Routing through an outbound proxy](#routing-through-an-outbound-proxy). Defaults to `SIP_OUTBOUND_PROXY`. |
| `aor` | yes | Address-of-record. Becomes the `From` URI and the implicit-match key for outbound calls. |
| `username` | no | Digest username. Defaults to the AOR user-part. |
| `password` | yes | Digest password. **Never returned in any response.** |
| `contact_user` | no | Override the `Contact` user-part. Defaults to the AOR user-part. |
| `expires_seconds` | no | Requested expiry. Clamped to `[SIP_OUTBOUND_REGISTRATION_MIN_EXPIRES_SECONDS, SIP_OUTBOUND_REGISTRATION_MAX_EXPIRES_SECONDS]`. |
| `tls_insecure_skip_verify` | no | Accept this trunk's TLS next hop certificate without verifying it — self-signed, privately signed, or SAN-less (`x509: certificate relies on legacy Common Name field`). Scoped to that peer; every other TLS peer is still verified, unlike the server-wide `SIP_TLS_INSECURE_SKIP_VERIFY`. Ignored with a logged warning when the next hop is not TLS or is named by IP literal. See [TLS proxies](#tls-proxies). |

Errors: `400` for invalid JSON, missing fields, or invalid URIs. `501` when
`type == "ip_ip"` (not yet implemented). `400` for unknown types.

### Routing through an outbound proxy

By default every request goes straight to the host in its Request-URI — the
registrar for a REGISTER, the dialed URI for an INVITE. When signalling must
egress through an SBC or edge proxy instead, name it and VoiceBlender attaches a
loose `Route` header (RFC 3261 §16.12.1) while leaving the Request-URI alone.

```bash
curl -X POST http://vb.local:8080/v1/sip/trunks \
  -H "Content-Type: application/json" \
  -d '{
    "type": "sip_register",
    "sip_register": {
      "registrar_uri":  "sip:pbx.example.com:5060",
      "outbound_proxy": "sip:edge.acme.net:5060;transport=tcp",
      "aor":            "sip:alice@pbx.example.com",
      "password":       "supersecret"
    }
  }'
```

The REGISTER then goes on the wire as:

```
REGISTER sip:pbx.example.com:5060 SIP/2.0
Route: <sip:edge.acme.net:5060;transport=tcp;lr>
From: <sip:alice@pbx.example.com>;tag=...
To: <sip:alice@pbx.example.com>
```

Note what does **not** change: the Request-URI still names the registrar, and
digest authentication still computes its `uri` against the registrar (RFC 3261
§22.4). Only the next hop moves. The same `Route` is attached to every INVITE
placed `from` that trunk's AOR.

A `407 Proxy Authentication Required` from the proxy is answered with the
trunk's credentials in a `Proxy-Authorization` header, exactly as a `401` from
the registrar is answered with `Authorization`.

#### TLS proxies

Both spellings work and are equivalent — `sips:edge.acme.net:5061` and
`sip:edge.acme.net:5061;transport=tls`. The port defaults to `5061` for `sips:`
and `5060` otherwise.

**Certificates.** VoiceBlender does not present a client certificate, so
`SIP_TLS_CERT` / `SIP_TLS_KEY` are *not* required to dial a TLS proxy — they are
required only when `SIP_TLS_PORT` is set, and the server refuses to start if the
port is set without them. What is mandatory is the other direction: by default
the **proxy's** certificate must chain to a root the host trusts and must carry
a SAN matching the URI host (also used as SNI). Configuring `SIP_TLS_CERT` does
*not* make an otherwise-untrusted proxy cert acceptable — the two are unrelated.

An SBC using a private CA or a self-signed cert therefore fails the handshake
with `x509: certificate signed by unknown authority`, and one whose name lives
only in the certificate's Common Name fails with `x509: certificate relies on
legacy Common Name field, use SANs instead`. Either way the trunk goes to
`failed` with that text in `last_error`.

Three settings relax this, from narrowest to widest:

```bash
# 1. Per trunk — accept this one peer's certificate, verify everyone else's.
curl -X POST http://localhost:8080/v1/sip/trunks -d '{
  "type": "sip_register",
  "sip_register": {
    "registrar_uri": "sips:sip.carrier.example:5061",
    "aor": "sip:3005113@sip.carrier.example",
    "password": "s3cret",
    "tls_insecure_skip_verify": true
  }
}'

# 2. Server-wide — trust an extra CA (or pin a peer's self-signed cert) on top
#    of the system trust store. The name in the certificate is still checked.
SIP_TLS_CA_FILE=/etc/voiceblender/internal-ca.pem

# 3. Server-wide — accept whatever any peer presents. Last resort.
SIP_TLS_INSECURE_SKIP_VERIFY=true
```

`tls_insecure_skip_verify` is the one to reach for when a single carrier's
certificate is the problem, including the legacy Common Name case, which
`SIP_TLS_CA_FILE` cannot fix on its own — trusting the certificate as a root
does not give it the SAN it lacks. The exemption is echoed back in
`GET /v1/sip/trunks/{id}` and logged as a warning when the trunk is created.

Its scope is the peer's **hostname** — the trunk's next hop, so the outbound
proxy when one is configured, otherwise the registrar — because the SNI a
handshake carries is the only per-connection identity available. Consequences
worth knowing:

- A next hop named by **IP literal** cannot be exempted this way: such a dial
  sends no SNI. The flag is ignored with a logged warning; use
  `SIP_TLS_CA_FILE` or `SIP_TLS_INSECURE_SKIP_VERIFY` for that peer. For the
  same reason an IP-dialed peer's certificate is chain-verified but not
  name-verified.
- Two trunks pointing at the same hostname share the exemption, and connections
  are pooled per remote socket — a second trunk to a peer an exempted trunk has
  already connected to rides that connection unverified.
- The exemption lives as long as the trunk: `DELETE /v1/sip/trunks/{id}`
  revokes it after the final unregister.

All of these affect **outbound** dials only — REGISTERs, INVITEs, and TLS
proxies alike — and never loosen the inbound TLS listener, which keeps
presenting `SIP_TLS_CERT` unchanged. A private CA can equally be trusted at the
host level instead, with `SSL_CERT_FILE=/etc/voiceblender/internal-ca.pem`, or
in a container by `COPY internal-ca.pem /usr/local/share/ca-certificates/` plus
`update-ca-certificates` in the image build.

The proxy's transport is independent of the registrar's: a `sips:` proxy in
front of a `sip:` registrar is a normal configuration, and the Request-URI still
reads `REGISTER sip:pbx.example.com:5060`.

> **Set `SIP_TLS_PORT` when using a TLS proxy.** Outbound TLS works without it,
> but the `Contact` in the REGISTER can then only advertise the plaintext UDP
> socket — so the upstream sends calls *back* to you unencrypted, and the trunk
> still reports `active` with nothing obviously wrong. `POST /v1/sip/trunks`
> logs a warning when it sees this combination. With `SIP_TLS_PORT` set, the
> Contact goes out as `sips:alice@vb.example:5061;transport=tls`.

#### Precedence

A single call can name its own proxy, which needs no trunk at all:

```bash
curl -X POST http://vb.local:8080/v1/legs \
  -H "Content-Type: application/json" \
  -d '{
    "type": "sip",
    "to": "sip:bob@example.com",
    "outbound_proxy": "sip:edge-eu.acme.net:5060"
  }'
```

Resolution order, most specific first:

| # | Source | Applies to |
|---|---|---|
| 1 | `to` resolves to an AOR registered here | Delivered to the bound contact; any proxy is ignored (and logged). Local delivery is not an egress. |
| 2 | `outbound_proxy` on `POST /v1/legs` | That one INVITE. |
| 3 | `sip_register.outbound_proxy` on the matched trunk | That trunk's REGISTER and its INVITEs. |
| 4 | `SIP_OUTBOUND_PROXY` | Everything not covered above. |
| 5 | The matched trunk's `registrar_uri` | Trunk-matched INVITEs, unchanged from earlier releases. |
| 6 | The Request-URI host | Everything else. |

`SIP_OUTBOUND_PROXY` deliberately does **not** displace rule 5: setting it
cannot silently redirect calls on trunks that already work. A trunk that wants a
proxy names one, and a trunk created while the env var is set adopts it at
creation time — so `GET /v1/sip/trunks/{id}` always reports the hop in effect
rather than leaving you to infer it from the environment.

Not applied to SIPREC SRC legs or WhatsApp legs, which address their own
endpoints. A malformed `outbound_proxy` is a `400`; a malformed
`SIP_OUTBOUND_PROXY` fails startup.

> **Inbound tagging caveat.** Inbound INVITEs are matched back to a trunk by the
> peer socket they arrive on, which is the proxy once one is configured. Several
> trunks behind the same proxy therefore cannot be told apart, and `trunk_id` on
> `leg.ringing` may name any one of them. The tag is informational and gates
> nothing.

### GET /v1/sip/trunks

List every configured trunk.

```bash
curl http://vb.local:8080/v1/sip/trunks
```

```json
{
  "instance_id": "abc-123",
  "trunks": [
    {
      "id": "7f5d39c6-2987-4643-9822-5c7ced9080e7",
      "type": "sip_register",
      "app_id": "acme",
      "status": "active",
      "created_at": "2026-06-24T12:00:00Z",
      "sip_register": {
        "registrar_uri": "sip:pbx.example.com:5060",
        "outbound_proxy": "sip:edge.acme.net:5060;transport=tcp",
        "aor": "sip:alice@pbx.example.com",
        "username": "alice",
        "contact_uri": "sip:alice@vb.example:5060",
        "requested_expires_seconds": 600,
        "granted_expires_seconds": 300,
        "last_registered_at": "2026-06-24T12:00:01Z",
        "next_refresh_at": "2026-06-24T12:02:31Z",
        "call_id": "f7c1...@vb.example",
        "cseq": 4
      }
    }
  ]
}
```

`outbound_proxy` is omitted entirely when the trunk routes at its registrar.
When present it is the hop actually in effect, whether it came from the trunk's
own `outbound_proxy` or from `SIP_OUTBOUND_PROXY`.

### GET /v1/sip/trunks/{id}

Return the same view shape for a single trunk. `404 Not Found` if the id
is unknown.

### DELETE /v1/sip/trunks/{id}

Returns **202 Accepted** immediately. In the background: cancels the refresh
timer, sends one final REGISTER with `Expires: 0` (digest-authed if
challenged), removes the trunk from the manager, and emits
`sip.outbound_registration_expired` with `reason: unregistered`.

```bash
curl -X DELETE http://vb.local:8080/v1/sip/trunks/7f5d39c6-2987-4643-9822-5c7ced9080e7
```

### Events

| Event | When |
|---|---|
| `sip.outbound_registration_active` | REGISTER (initial or refresh) returned 2xx. Carries `trunk_id`, `aor`, `registrar`, `contact`, `granted_expires_seconds`, `expires_at`, `call_id`. |
| `sip.outbound_registration_failed` | REGISTER attempt failed (transport error, non-2xx after digest retry). Carries `trunk_id`, `aor`, `registrar`, `status_code`, `reason`. The trunk stays in the manager and retries with exponential backoff. |
| `sip.outbound_registration_expired` | Trunk removed (DELETE or shutdown), or refresh failed past granted lifetime. `reason` is one of `unregistered`, `shutdown`, `refresh_failed`. The `refresh_failed` variant fires once per outage and resets on the next successful REGISTER. |

### VSI commands

The same four operations are available on the `/v1/vsi` WebSocket:
`create_sip_trunk`, `list_sip_trunks`, `get_sip_trunk`, `delete_sip_trunk`.
Payloads and result shapes mirror the REST endpoints above.

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SIP_OUTBOUND_REGISTRATION_DEFAULT_EXPIRES_SECONDS` | `3600` | Default requested expiry when `expires_seconds` is omitted on create |
| `SIP_OUTBOUND_REGISTRATION_MIN_EXPIRES_SECONDS` | `60` | Lower clamp on requested expiry |
| `SIP_OUTBOUND_REGISTRATION_MAX_EXPIRES_SECONDS` | `7200` | Upper clamp on requested expiry |
| `SIP_OUTBOUND_REGISTRATION_REFRESH_RATIO` | `0.5` | Fraction of granted expiry at which the trunk refreshes |
| `SIP_OUTBOUND_REGISTRATION_FAILURE_BACKOFF_MAX_MS` | `300000` | Upper cap on the failure-retry exponential backoff |
| `SIP_OUTBOUND_PROXY` | _(empty)_ | Default next hop for outbound REGISTERs and INVITEs. Overridden per-trunk by `outbound_proxy` and per-call by `outbound_proxy` on `POST /v1/legs`. A malformed value fails startup. |

---

## Typical Workflow

```
1. Configure global webhook via WEBHOOK_URL env var, or per-leg via request/SIP headers

2. Receive inbound call -> webhook: leg.ringing {leg_id, leg_type: "sip_inbound", from, to}

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

---

## Metrics

### GET /metrics

Returns Prometheus-format metrics for the VoiceBlender instance. No request body or authentication is required.

**Response:** `200 OK` — Prometheus text exposition format (`text/plain; version=0.0.4`)

#### Exported Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `voiceblender_active_legs` | Gauge | — | Number of legs currently in any state (`ringing`, `early_media`, `connected`, `held`) |
| `voiceblender_active_rooms` | Gauge | — | Number of rooms currently open |
| `voiceblender_legs_total` | Counter | `type`, `state` | Total leg lifecycle transitions. `type`: `sip_inbound`, `sip_outbound`. `state`: `ringing`, `connected`, `disconnected` |
| `voiceblender_disconnect_reasons_total` | Counter | `type`, `reason` | Total disconnected legs by type and reason (e.g. `remote_bye`, `api_hangup`, `rtp_timeout`) |
| `voiceblender_call_duration_seconds` | Histogram | `type` | Answered call duration (time from answer to hangup). Use `rate(sum)/rate(count)` for ACD |
| `voiceblender_call_total_duration_seconds` | Histogram | `type` | Total leg lifetime including ringing time (time from leg creation to hangup) |
| `voiceblender_recovered_panics_total` | Counter | `component`, `site` | Panics recovered and contained instead of crashing the process. `component`: `mixer`, `room`. `site`: `readLoop`, `writeLoop`, `mixTick`, `panicTeardown`, `deleteHangup` |
| `voiceblender_webhook_enqueued_total` | Counter | — | Total events accepted onto the webhook delivery queue. Denominator for the drop ratio — see the PromQL below |
| `voiceblender_webhook_dropped_total` | Counter | — | Total events dropped because the webhook delivery queue was full (backpressure from slow endpoints) |
| `voiceblender_webhook_deliveries_total` | Counter | `outcome` | Total terminal webhook delivery outcomes. `outcome`: `success`, `exhausted` (all 3 attempts failed), `marshal_error`, `request_error` (malformed webhook URL). Closed set, so cardinality is fixed at 4 |
| `voiceblender_vsi_events_dropped_total` | Counter | — | Total events dropped because a VSI WebSocket client's buffer was full (slow consumer). Tune with `VSI_EVENT_BUFFER_SIZE` |
| Go runtime metrics | — | — | Standard `go_*` and `process_*` metrics from the Prometheus Go client |

Every delivery that reaches a terminal exit increments exactly one `voiceblender_webhook_deliveries_total{outcome}`. This is not a global `enqueued == sum(outcomes)` identity: jobs still queued at shutdown are abandoned without an outcome, so a small, shutdown-only skew is expected.

#### PromQL Examples

Compute the Average Call Duration (ACD) over a 5-minute window:

```promql
rate(voiceblender_call_duration_seconds_sum[5m])
  / rate(voiceblender_call_duration_seconds_count[5m])
```

Alert on audio-path panics being contained (each one drops a participant or a frame):

```promql
sum by (component, site) (rate(voiceblender_recovered_panics_total[5m])) > 0
```

Webhook drop ratio — the fraction of events that never made it onto the delivery queue:

```promql
rate(voiceblender_webhook_dropped_total[5m])
  / (rate(voiceblender_webhook_dropped_total[5m]) + rate(voiceblender_webhook_enqueued_total[5m]))
```

Alert on webhooks that reached your endpoint but never succeeded:

```promql
rate(voiceblender_webhook_deliveries_total{outcome="exhausted"}[5m]) > 0
```

Alert on VSI consumers falling behind (raise `VSI_EVENT_BUFFER_SIZE` or fix the slow client):

```promql
rate(voiceblender_vsi_events_dropped_total[5m]) > 0
```

### Profiling (pprof)

Only available when the binary is built with the `pprof` build tag:

```
go build -tags pprof ./...
```

| Endpoint | Description |
|----------|-------------|
| `GET /debug/pprof/` | Index of available profiles |
| `GET /debug/pprof/profile` | 30-second CPU profile |
| `GET /debug/pprof/heap` | Heap memory snapshot |
| `GET /debug/pprof/goroutine` | All goroutine stack traces |
| `GET /debug/pprof/trace` | Execution trace |
| `GET /debug/pprof/cmdline` | Process command line |

**Do not enable in production without access controls** — these endpoints expose internal runtime state.

```
go tool pprof http://localhost:8080/debug/pprof/profile
```
