# Configuration

All VoiceBlender configuration is via environment variables. See
[voiceblender.env.example](voiceblender.env.example) for a ready-to-edit file with every
variable and its default.

Full documentation, guides and API reference: <https://voiceblender.org/docs/>

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
| `SIP_TLS_CA_FILE` | *(system trust store only)* | Path to a PEM bundle of extra CA certificates trusted when **dialing** a remote peer over TLS (registrar, outbound proxy, carrier SBC). Added to the system roots, not a replacement. To trust a peer that presents a self-signed certificate, point this at that certificate. The name in the certificate is still checked, so a peer whose certificate has no SAN needs the per-trunk `tls_insecure_skip_verify` instead. Not a client certificate — VoiceBlender never presents one. |
| `SIP_TLS_INSECURE_SKIP_VERIFY` | `false` | When `true`, any certificate a remote peer presents on an outbound TLS dial is accepted without verification — every peer, server-wide. Prefer `SIP_TLS_CA_FILE`, or the per-trunk `sip_register.tls_insecure_skip_verify`, which scopes the exemption to one peer. Never affects the inbound TLS listener. |
| `SIP_DEBUG` | `false` | When `true`, log the full RFC 3261 wire form of every inbound and outbound SIP request and response. Very verbose — use only for troubleshooting. |
| `SIP_DOMAIN` | *(falls back to advertised IP)* | FQDN advertised in From, Contact and Via on outbound SIP signalling (classic trunks and WhatsApp). Two exceptions apply to the From host only: a call matched to a registered SIP trunk uses that trunk's AOR realm, and a `from` given as a full SIP URI uses the host in that URI. Should match the SAN on `SIP_TLS_CERT` and any allowlist your carrier or Meta keeps. |
| `SIP_HOST` | `voiceblender` | SIP User-Agent name |
| `ICE_SERVERS` | `stun:stun.l.google.com:19302` | STUN/TURN URLs (comma-separated) |
| `WEBRTC_EXTERNAL_IPS` | *(empty)* | Comma-separated public IPs advertised as host ICE candidates (pion `SetNAT1To1IPs`). Set this when VoiceBlender runs behind NAT/Docker and the gathered host interface IPs aren't routable from the remote peer — otherwise WebRTC peers behind firewalls won't be able to reach VB. Supports IPv4 and IPv6 literals. The literal value `auto` performs STUN-based public-IP discovery at startup against the first reachable `ICE_SERVERS` entry; discovery failure is non-fatal and logs a warning. |
| `RECORDING_DIR` | `/tmp/recordings` | Local recording output directory |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`). Verbatim transcript text, DTMF digits and full event payloads are logged only at `debug`. |
| `WEBHOOK_URL` | | Default webhook URL for inbound calls |
| `WEBHOOK_SECRET` | | HMAC-SHA256 signing secret for the global webhook. Applied to events delivered to `WEBHOOK_URL`; per-leg/per-room webhooks set via the API can supply their own secret. |
| `CUSTOM_DATA_MAX_BYTES` | `1024` | Maximum size in bytes of a leg's `custom_data` JSON. The value is repeated on every event for that leg, so this caps webhook payload growth. `0` = unlimited. |
| `ELEVENLABS_API_KEY` | | API key for ElevenLabs TTS, STT, and Agent |
| `VAPI_API_KEY` | | API key for VAPI Agent provider |
| `DEEPGRAM_API_KEY` | | API key for Deepgram STT and TTS |
| `AZURE_SPEECH_KEY` | | Subscription key for Azure Cognitive Speech Services (TTS and STT) |
| `AZURE_SPEECH_REGION` | `eastus` | Azure region for Speech Services (e.g. `eastus`, `westeurope`) |
| `SPEECHMATICS_API_KEY` | | API key for Speechmatics STT |
| `SPEECHMATICS_URL` | `wss://eu2.rt.speechmatics.com/v2` | Speechmatics realtime endpoint. Change it for another region (`eu`, `us`, `global`) or a self-hosted realtime container. |
| `S3_BUCKET` | | S3 bucket for recording uploads. See [S3 bucket preflight](#s3-bucket-preflight). |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_ENDPOINT` | | Custom S3 endpoint (MinIO, etc.). Must include an `http://` or `https://` scheme. |
| `S3_PREFIX` | | Key prefix for S3 objects |
| `S3_ALLOW_INSECURE_ENDPOINT` | `false` | Allow a plaintext `http://` `S3_ENDPOINT` on a **non-local** host. Loopback and private addresses never need this. See [S3 bucket preflight](#s3-bucket-preflight). |
| `S3_PREFLIGHT_TIMEOUT` | `10s` | Budget for the startup bucket probe. `0` disables it. |
| `S3_REQUEST_PREFLIGHT_TIMEOUT` | `2s` | Budget for the bucket probe on a per-request S3 backend. `0` disables it. |
| `GCS_BUCKET` | | Google Cloud Storage bucket for recording uploads via the native GCS API. Prefer this over `S3_ENDPOINT=https://storage.googleapis.com` on GKE — Workload Identity / ADC works directly, with no HMAC interop keys. |
| `GCS_OBJECT_NAME_PREFIX` | | Object name prefix for GCS uploads (e.g. `recordings` or a bare workspace id like `dev`). A trailing slash is added automatically when missing. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE` | | AWS credentials for S3 uploads and AWS Polly TTS. Resolved by the AWS SDK's default credential chain (env vars → `~/.aws/credentials` → EC2/ECS/EKS instance role), not by VoiceBlender directly. `AWS_REGION` is honored only when `S3_REGION` is empty. |
| `GOOGLE_APPLICATION_CREDENTIALS` | | Path to a Google Cloud service-account JSON file used by Google Cloud TTS and by GCS recording uploads when no other credential source is available. Resolved by Google's Application Default Credentials chain (env var → `~/.config/gcloud/application_default_credentials.json` → GCE/Cloud Run/GKE metadata / Workload Identity), not by VoiceBlender directly. |
| `TTS_CACHE_ENABLED` | `false` | Enable disk-backed TTS audio cache. Cached audio persists across restarts. |
| `TTS_CACHE_DIR` | `/tmp/tts_cache` | Directory for cached TTS audio files (used when `TTS_CACHE_ENABLED=true`) |
| `TTS_CACHE_INCLUDE_API_KEY` | `false` | Include API key in TTS cache key (set `true` if different keys map to different voice clones) |
| `TTS_PREFLIGHT_TTL` | `30s` | How long a staged (preflight) TTS utterance is held before being discarded |
| `TTS_PREFLIGHT_MAX_PER_LEG` | `3` | Maximum staged TTS utterances per leg; staging past the cap returns 409 |
| `TTS_PREFLIGHT_MAX_BYTES` | `4194304` | Maximum buffered audio per staged TTS utterance, in bytes |
| `RTP_PORT_MIN` | `10000` | Minimum UDP port for RTP/RTCP media |
| `RTP_PORT_MAX` | `20000` | Maximum UDP port for RTP/RTCP media |
| `SIP_JITTER_BUFFER_MS` | `0` | SIP ingress jitter buffer target delay in ms (0 = disabled passthrough). Applies to every SIP leg. |
| `SIP_JITTER_BUFFER_MAX_MS` | `300` | Max depth of the SIP ingress jitter buffer (ms); frames beyond this are dropped oldest-first. |
| `WS_JITTER_BUFFER_MS` | `0` | WebSocket ingress playout lead in ms (0 = disabled passthrough). Applies to every websocket leg and room WS participant. Non-zero also enables clock-drift compensation, which absorbs the producer/mixer rate difference during pauses instead of punching a 20 ms hole in the mix. Size it to the transport's worst-case stall, not to the drift — `40`–`80` suits a healthy link, and every millisecond is added one-way latency. |
| `COMFORT_NOISE_ENABLED` | `true` | Inject low-level comfort noise (~−75 dBFS) into otherwise silent mixer frames. |
| `SIP_SDP_STRICT_MLINE_ANSWER` | `false` | Emit a port-0 placeholder for every offered `m=` section we do not accept, so answers carry the same m-line count and order as the offer (RFC 3264 §6). Gated separately from multi-stream because it changes the SDP **single-stream** calls emit whenever a peer offers a section we don't handle, such as video. |
| `SIP_EXTERNAL_IP` | *(empty)* | Public IPv4 address for NAT/Docker deployments. When set, used in SIP Contact headers and SDP media (c=) lines instead of the auto-detected or bind IP. IPv6 has no equivalent: set `SIP_BIND_IPV6` directly to the address you want advertised. |
| `DEFAULT_SAMPLE_RATE` | `16000` | Default mixer sample rate (Hz) for new rooms when `sample_rate` is not specified. Allowed: `8000`, `16000`, `48000`. |
| `SIP_CODECS` | `PCMU,PCMA` | Comma-separated, preference-ordered list of codecs the SIP engine offers on outbound INVITEs **and** accepts on inbound INVITEs (a codec absent from this list cannot be negotiated in either direction). Recognized names (case-insensitive): `PCMU`, `PCMA`, `G722`, `opus`, `AMR-WB`, `AMR-NB` (the bare token `AMR` also resolves to AMR-NB per RFC 4867 §8.1). Unknown names and duplicates are dropped silently; if the parsed list ends up empty the default is used. Example: `SIP_CODECS=opus,G722,PCMU,PCMA,AMR-WB,AMR-NB` enables every supported codec, ranked Opus-first. |
| `SIP_REFER_AUTO_DIAL` | `false` | When `true`, the server accepts an incoming SIP REFER (202) and **dials the target itself**. When `false` (default), the REFER is parked and surfaced as `leg.transfer_requested` for the app to drive via the transfer commands (`accept`/`progress`/`complete`/`decline`); an undecided REFER auto-declines (603, **default-deny** — toll-fraud risk). Outbound transfers via the REST API are unaffected. |
| `SIP_REFER_CONSULT_TIMEOUT_MS` | `2000` | How long an inbound REFER is parked awaiting an app accept/decline decision before it auto-declines with `603` (fail-closed). Only used when `SIP_REFER_AUTO_DIAL=false`. |
| `SIP_AUTO_RINGING` | `false` | **Behavior change vs prior releases**: previously the server always sent `180 Ringing` after `100 Trying`. The new default sends only `100 Trying`; the API caller drives ringing explicitly via `POST /v1/legs/{id}/ring`, `/early-media`, or `/answer`. Set to `true` to restore the legacy auto-180 behavior. |
| `SIP_TCP_ENABLED` | `false` | Listen for SIP over TCP on `SIP_PORT` alongside the UDP listener. Recommended with SIPREC: a recording session's INVITE carries the metadata document alongside the SDP and is larger than RFC 3261 §18.1.1 allows over UDP. |
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
| `SIP_OUTBOUND_PROXY` | _(empty = route at the registrar / dialed URI)_ | Default next hop for outbound REGISTERs and INVITEs, with the Request-URI left unchanged. INVITEs carry it as a loose `Route: <sip:proxy;lr>` header; REGISTERs are sent straight to the hop with no Route, since a proxy that does not recognise the Route URI as its own forwards the REGISTER back at itself. Overridden per-trunk by `sip_register.outbound_proxy` on `POST /v1/sip/trunks` and per-call by `outbound_proxy` on `POST /v1/legs`; a `to` that resolves to an AOR registered here outranks all three. Digest auth still targets the registrar, not the proxy. Not applied to SIPREC SRC or WhatsApp legs. A malformed value fails startup. **Caveat:** inbound legs are tagged with `trunk_id` by the peer socket they arrive on, so when several trunks share one proxy that tag becomes ambiguous — it is informational, never an authorization gate. |
| `SPEECH_DETECTION_ENABLED` | `false` | Emit `speaking.started` / `speaking.stopped` events for every connected leg by default. Per-call `speech_detection` on `POST /v1/legs` or `POST /v1/legs/{id}/answer` overrides this. |
| `AMRWB_MODE` | `2` | AMR-WB (G.722.2) encoder speech-mode **ceiling** `0..8`: `0`=6.60, `1`=8.85, `2`=12.65, `3`=14.25, `4`=15.85, `5`=18.25, `6`=19.85, `7`=23.05, `8`=23.85 kbit/s. The actual transmit mode is this ceiling clamped to the peer's negotiated `mode-set` (so e.g. `8` yields HD 23.85 only when the peer allows it, falling back automatically). Default `2` (12.65) matches the GSMA IR.92 / VoLTE common rate. Out-of-range values clamp to `0..8`. |
| `AMRWB_OCTET_ALIGNED` | `true` | Offer octet-aligned AMR-WB framing (RFC 4867) in outbound SDP. When `false`, offers bandwidth-efficient framing. On answers, VoiceBlender always echoes the framing the peer negotiated. |
| `AMRNB_MODE` | `7` | AMR-NB (RFC 4867) encoder speech-mode **ceiling** `0..7`: `0`=4.75, `1`=5.15, `2`=5.90, `3`=6.70, `4`=7.40, `5`=7.95, `6`=10.2, `7`=12.2 kbit/s. The actual transmit mode is this ceiling clamped to the peer's negotiated `mode-set`. Default `7` is the GSM-EFR-equivalent 12.2 kbit/s, the highest AMR-NB quality and the rate most enterprise PBXes and mobile networks default to. Out-of-range values clamp to `0..7`. |
| `AMRNB_OCTET_ALIGNED` | `true` | Offer octet-aligned AMR-NB framing (RFC 4867) in outbound SDP. When `false`, offers bandwidth-efficient framing. On answers, VoiceBlender always echoes the framing the peer negotiated. |
| `VSI_EVENT_BUFFER_SIZE` | `256` | Per-client buffer (in events) on the `/v1/vsi` WebSocket. When the client consumes events slower than they're produced, the buffer fills and new events are dropped (with a warn log on the leading edge of each drop burst and at every 10× threshold; the next delivered event also includes an `events_dropped` notification to the client). Clamped to `[16, 1_000_000]`. **Tuning:** larger values absorb longer back-pressure spikes at the cost of higher peak memory per client (roughly the average JSON event size × buffer size, e.g. ~1 KB × 256 ≈ 256 KB per connection at the default) and longer end-to-end latency for buffered events when the client recovers. Increase only if you observe drops on legitimate slow-consumer scenarios you can't fix at the client. |
| `SIPREC_ENABLED` | `false` | Accept inbound SIPREC recording sessions (RFC 7866), where an SBC or PBX forks a call's media to VoiceBlender. When off, an INVITE carrying `Require: siprec` is rejected with `420 Bad Extension` and one that only hints at SIPREC with `488`. Requires `SIP_TCP_ENABLED=true`. |
| `SIPREC_AUTO_ANSWER` | `true` | Answer an inbound recording session immediately instead of parking it until `POST /v1/legs/{id}/answer`. A session recording client does not wait for an application decision, so leaving this on is usually correct; turn it off to gate sessions from a controller. |
| `SIPREC_MAX_STREAMS` | `8` | Maximum number of `m=audio` sections accepted on one recording session. A session offering more is rejected with `486`, bounding the RTP ports and goroutines a single peer can claim. |
| `SIPREC_METADATA_MAX_BYTES` | `65536` | Maximum size of the `rs-metadata` XML document in a SIPREC INVITE. A larger document is rejected with `413` rather than parsed. |
| `SIPREC_SRC_ENABLED` | `false` | Allow `POST /v1/rooms/{id}/siprec` to originate outbound recording sessions, forking a room's participants to an external session recording server. Off by default: it lets an API caller stream a room's audio to an arbitrary SIP destination. |
| `SIPREC_AUTO_RECORD` | `false` | Start multi-channel recording automatically when a SIPREC session is accepted, one channel per recorded participant. When false, recording is driven through the usual `/v1/legs/{id}/record` endpoint. |
| `SIPREC_ROOM_MODE` | `none` | Where a recording session's audio streams are mixed. `none` attaches nothing and leaves placement to the stream API; `per_session` creates a room named `siprec-<legID>` and attaches every stream, which is what makes live STT and agents apply to a recorded call; `fixed` attaches every session's streams into `SIPREC_ROOM_ID`. |
| `SIPREC_ROOM_ID` | _(empty)_ | Room every recording session's streams join when `SIPREC_ROOM_MODE=fixed`. Ignored in the other modes. |
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

Verbatim transcript text, DTMF digits and full event payloads appear only at
`LOG_LEVEL=debug`. Debug output is therefore PII-bearing and should not be
shipped to a general-purpose log sink.

## S3 bucket preflight

Recordings upload after the call ends, so a misconfigured bucket used to surface
only in a log line once the audio had already been captured. VoiceBlender now
probes the bucket with a `HeadBucket` call when it builds an S3 backend — at
startup, and again per request when a request supplies `s3_bucket`.

Only a bucket the store reports as **absent** is treated as fatal: startup exits
`1`, and a per-request backend returns `400`. Anything that leaves the answer
open — a `403` because the credentials have `s3:PutObject` but not
`s3:ListBucket`, a `5xx`, an unreachable endpoint, an expired budget — is logged
as a warning and the recording proceeds, because the upload may still succeed and
a failed probe is not a reason to refuse to record. Least-privilege IAM policies
and a briefly unreachable store therefore keep working as before.

Both probes are bounded (`S3_PREFLIGHT_TIMEOUT`, `S3_REQUEST_PREFLIGHT_TIMEOUT`)
and are also cut short when the caller goes away. Set either to `0` to skip that
probe. The per-request budget is deliberately short: it runs inside record-start,
and over VSI that occupies the connection's command loop.

**Plaintext endpoints.** An `http://` `S3_ENDPOINT` pointing at a non-local host
is refused, rather than shipping recording audio in cleartext — set
`S3_ALLOW_INSECURE_ENDPOINT=true` to override. Loopback, RFC1918/link-local
addresses, single-label hostnames (`http://minio:9000`) and `.internal` / `.local`
names are exempt and need no flag, which covers the usual co-located MinIO. This
is an operator decision and governs per-request `s3_*` uploads too; there is no
per-request override.
