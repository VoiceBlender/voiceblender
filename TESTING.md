# Testing Guide

## Quick Reference

```bash
# Unit tests only (fast, no external dependencies)
go test ./internal/...

# Integration tests (requires no external services, uses loopback SIP)
go test -tags integration -timeout 60s ./tests/integration/

# Everything
go test ./internal/... && go test -tags integration -timeout 60s ./tests/integration/

# Benchmark (scaling + audio latency)
go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/

# Two-instance cluster (for manual end-to-end / peer-to-peer scenarios)
docker compose -f docker/docker-compose.cluster.yml up --build
```

---

## Unit Tests

Unit tests cover internal packages and run without any network services or external dependencies.

### Run all unit tests

```bash
go test ./internal/...
```

### Run a specific package

```bash
go test ./internal/mixer/
go test ./internal/codec/
go test ./internal/playback/
go test ./internal/storage/
go test ./internal/comfortnoise/
go test ./internal/recording/
```

### Run with verbose output

```bash
go test -v ./internal/...
```

### Run a single test by name

```bash
go test -v -run TestMixer_TapRecording ./internal/mixer/
go test -v -run TestS3Backend_Upload ./internal/storage/
go test -v -run TestS3Backend_Preflight ./internal/storage/
go test -v -run TestGCSBackend ./internal/storage/
go test -v -run TestResolveStorage ./internal/api/
```

### What each package tests

| Package | Tests | Description |
|---------|-------|-------------|
| `internal/amd` | 31 | AMD state machine (human/machine/no\_speech/not\_sure), beep detection (Goertzel), parameter validation; push-mode `Feed`/`OnDeadline`/`FeedBeep` (chunk-boundary independence, accumulator draining), and verdict-reachability guards pinned against the real FSM (per-verdict sub-frame edges, rejection only when no verdict is reachable) |
| `internal/mixer` | 39 | Audio mixing, configurable sample rate (8/16/48 kHz), anti-aliasing polyphase resampler (stopband attenuation, passband flatness, group-delay budget, seam continuity, full-scale clamp, `Close` propagation through the wrapper), playback sources, tap recording, per-listener routing whitelist (full mesh / supervisor-whisper / isolated / mute+deaf interaction), and panic containment: an IO-loop panic removes only its own participant instance (a stale loop cannot evict its successor), the owner hook fires exactly once, the writer is closed to wake a ws/agent owner but spared when the owner closes its own egress, and a `mixTick` panic skips one tick with rate-limited logging while the room keeps running |
| `internal/resampler` | 10 | Vendored pure-Go Speex/Opus polyphase resampler (MIT); upstream tests: same-rate, direct and interpolated up/down-sampling for float32 and float64 |
| `internal/bridge` | 9 | Duplex conduit: pair wiring, blocking Read, EOF/idempotent Close, drop-oldest backpressure, leftover handling, buffer-copy |
| `internal/room` (bridge) | 12 | Direction mapping/validation, `CreateBridge` matrix (self/missing/rate/duplicate), live direction flip, delete teardown, mixer keepalive with zero legs |
| `internal/room` (routing) | 6 | Role-based routing matrix: supervisor-whisper no-bleed at join, mid-call role change recomputes allow-sets, unroled legs default to full mesh, removing a leg prunes others' whitelists, `UpdateRoutingRow` with null clears row, matrix round-trip via `RoutingMatrix()` |
| `internal/room` (panic teardown) | 8 | Mixer panic dispatch by participant class: a leg is removed and hung up with reason `mixer_panic`, a bridge endpoint tears down the whole bridge, a ws/agent source publishes nothing; teardown is elected on the participant instance so a leg that left and returned is spared, and a panicking `Hangup` inside the teardown does not crash the process |
| `internal/metrics` | 11 | Prometheus collector: `/metrics` exposition, active-leg and active-room gauges, disconnect-reason and duration series from bus events, `voiceblender_recovered_panics_total` (registered on every collector, incremented off the panic-recovery sites), and the egress counters (`webhook_enqueued`/`webhook_dropped`/`webhook_deliveries{outcome}`/`vsi_events_dropped`) — registered at zero and incremented through the `events.MetricsObserver` methods |
| `internal/events` | 28 | Event bus (subscribe/publish/unsubscribe, timestamp stamping), flattened `MarshalJSON` envelope, DTMF/RTT per-leg sequence numbers, webhook registry (leg/room/global routing, HMAC signing, `Stop`); the stable `event_id` (unique per publish, identical across subscribers, omitted on unpublished events, constant across all 3 retry attempts and matching the `X-Event-Id` header); and the exactly-one-outcome invariant for `deliver` (`success`, `exhausted`, `marshal_error`, `request_error`) plus full-queue drop counting and nil-observer safety |
| `internal/speaking` | 7 | Voice activity detection, debouncing, mute handling, 8kHz/16kHz sample rates |
| `internal/codec` | 17 | G.722 encode/decode, silence/tone round-trips, up/downsample, AMR-WB and AMR-NB type registration + factory + RFC 4867 round-trip (both payload formats) |
| `goamr-wb` (external module) | 98 | AMR-WB (G.722.2) pure-Go DSP: ITU fixed-point basic ops, LPC/ISF, pitch, algebraic codebook, gain quant, synthesis/HF band, RFC 4867 payload (de)pack (octet-aligned + bandwidth-efficient), MIME sort/unsort. Lives in its own published module (`github.com/VoiceBlender/goamr-wb`, pinned in `go.mod`); its tests run from a local clone of that repo. |
| `internal/playback` | 48 | WAV/MP3 parsing, format detection, streaming, anti-aliasing resampler with per-stream filter lifetime (seam continuity across frames, fresh filter per stream, no cross-playback bleed), repeat, cancellation |
| `internal/storage` | 15 | FileBackend (no-op), S3Backend upload (with httptest fake), error handling; the `HeadBucket` preflight taxonomy (absent bucket is fatal, while a 403 without `s3:ListBucket`, a 5xx, an unreachable endpoint or an expired budget stay inconclusive and leave the backend usable) and the plaintext-endpoint guard, including the loopback/private/single-label exemptions that let a co-located MinIO work without an opt-in. GCS uploads: the `gs://` location and object name (including the trailing slash supplied for a bare `GCS_OBJECT_NAME_PREFIX`), the `audio/wav` content type and payload checked on the wire through the real client against a fake endpoint, a failed upload leaving the local file in place, and client lifetime — a per-request backend releases its client after its one upload (success or failure) while a reusable one keeps it open |
| `internal/recording` | 52 | Recorder lifecycle (start/stop/double-start, WAV header, custom rate, pause/resume, context cancel); custom `filename` sanitization (dots preserved, only trailing `.wav` stripped) and exclusive path reservation so a reuse or in-flight collision fails instead of overwriting; atomic publish — capture goes to a `.rec-*.tmp` staging file and is fsync+chmod+link'd into place (no overwrite), so nothing exists at the advertised path until it completes, a capture that errors mid-write or never wrote a frame is discarded rather than published, and a publish that fails only at the closing directory sync still reports `Finalized`; and the stereo loop on its own 20 ms clock — both channels drained non-blocking into bounded drop-oldest accumulators, silence-fill for a stalled channel, no drift when one channel bursts or closes, a stalled *producer* still advancing the timeline (the hold/DTMF/deafened case), pause zeroing both channels, and the bound holding at 8/16/48 kHz |
| `internal/comfortnoise` | 5 | Comfort noise generation, amplitude clamping, mix-in |
| `internal/jitter` | 10 | Fixed-delay reorder buffer: warm-up, reorder, duplicate drop, late-arrival drop, underrun silence, uint16 wraparound, max-depth eviction, reset |
| `internal/sip` (refer) | 5 | Refer-To parsing (blind / attended with Replaces / no angles), Replaces.String() formatting, sipfrag status-line parsing |
| `internal/sip` (whatsapp) | 12 | `IsWhatsAppInvite` host matching (exact/subdomain/lookalike/case-insensitive), `WhatsAppRecipientURI` E.164 normalisation, `InviteWhatsApp` precondition checks (TLS configured, required fields) |
| `internal/sip` (multipart) | 10 | `ParseMessageBody` / `BuildMultipartMixed`: single-part bodies pass through byte-identically whatever the declared type, multipart SIPREC bodies split into SDP + `rs-metadata` parts (registered and legacy content types), `Content-Disposition` parsing, missing-boundary and malformed-type errors, deterministic boundaries, and `setRequestBody` emitting either a plain `application/sdp` body or a multipart one |
| `internal/sip` (siprec) | 10 | `DetectSIPREC` across all three RFC 7866 signals (`Require`/`Proxy-Require: siprec` with token splitting and case-insensitivity, `Contact` `+sip.src` feature tag, rs-metadata body part), negative cases (ordinary INVITE with `Supported: siprec`, empty call), `HasOptionTag`, `UnsupportedHeader` |
| `internal/sip` (direction) | 3 | `NarrowDirection` table, and `PlanAnswer` with `MaxDirection: recvonly` forcing every accepted section receive-only (`sendrecv`→`recvonly`, `sendonly`→`inactive`) while an unset cap leaves plain RFC 3264 §6.1 mirroring untouched |
| `internal/siprec` | 14 | RFC 7865 metadata: parsing with and without the declared namespace, BOM stripping, malformed/foreign-root errors, `Marshal` round-trip, and the `State` aggregate — `complete` replaces wholesale, `partial` upserts and never deletes on absence, `disassociate-time` removes a participant and its streams, `Apply` is idempotent, `ParticipantForLabel` resolves the label→participant join, deterministic `Snapshot` ordering, nil-safety |
| `internal/leg` (siprec) | 5 | A recording session's answer is `recvonly` on every section with labels echoed and no `writeLoop` started; a `sendrecv` offer is still narrowed to `recvonly`; `streamsIndependent` gives m-line 0 its own room and role while an ordinary leg keeps the primary privileged; a rejected re-offer section closes its stream and releases its RTP port |
| `internal/sip` (tls) | 4 | `EngineConfig` TLS validation, concurrent UDP+TLS listener startup, self-signed cert handshake loopback |
| `internal/sip` (inbound auth) | 12 | `recordChallenge` WWW-Authenticate shape + nonce uniqueness; `VerifyInboundAuth` valid digest (MD5/SHA-256, password and HA1), wrong password/username → invalid, no/unknown/expired challenge → none, single-use nonce consumption, `max_expires` TTL cap round-trips through the pending challenge |
| `internal/api` (inbound auth) | 12 | `HandleRegisterAttempt` always publishes the `sip.registration_attempt` event, applies the configured fallback when undecided (reject by default, accept when configured), propagates a challenge decision (incl. `max_expires`), accept decision carries `max_expires`; `registerConsultFallback` mapping (unset/unknown → reject, `accept` case-insensitive); `ChallengeRequest` validation (realm + password/ha1 required, non-negative `max_expires`) |
| `internal/api` (inbound transfer) | 2 | `pendingReferStore` state machine (accept vs decline/timeout are mutually exclusive; progress/complete require a prior accept; unknown-leg misses); `sipReasonPhrase` default reason phrases |
| `internal/api` (from identity) | 14 | `splitFromIdentity` splitting a `from` into the user and host of the outbound From (full SIP URI, bare user, E.164, `tel:`, `sip:alice@` with no host; port and URI params discarded; host lowercased while the case-sensitive user-part survives verbatim). `referIdentity` choosing what a REFER-originated INVITE goes out as: the referrer's trunk AOR when the leg has a trunk, its own identity when it does not, and the same fallback when the trunk ID is stale |
| `internal/sip` (from identity) | 6 | `Engine.fromIdentity` building the From and matching P-Asserted-Identity (no headers at all without a caller ID, `publicHost` as the default host, `FromHost` override, PAI tracking the From URI, tag present); `OutboundRegistration.FromHost` returning the AOR realm and not the registrar host |
| `internal/api` (amd driver) | 11 | Push-mode `amdDriver`: frames classified inline on the leg's readLoop, the wall-clock `watch` goroutine exits on leg teardown without publishing, exactly one `amd.result` per call, the machine+beep `pending` handshake (deadline firing mid-verdict defers the publish rather than dropping it), and tap-ownership gating so a superseded analysis publishes nothing and cannot clear the live tap |
| `internal/api` (storage resolution) | 8 | `resolveStorage` for a per-request S3 backend: the operator's insecure-endpoint decision is inherited from server config and cannot be overridden per request, an absent bucket fails record-start while an inconclusive probe proceeds, the configured budget and the caller's own deadline each cut an unanswered probe short, and `S3_REQUEST_PREFLIGHT_TIMEOUT=0` skips the probe entirely. A VSI record-start against a black-holed endpoint returns on that budget rather than stalling the connection's recv loop. For GCS: `storage=gcs` resolves to the server-level backend, errors when configured nowhere, prefers a request-supplied `gcs_bucket` over the server-level one, and releases a per-recording backend once its uploads are done while leaving the shared server-level ones open |
| `internal/api` (stt turn events) | 12 | Provider selection (`deepgram_flux` resolves to the Flux transcriber and shares `DEEPGRAM_API_KEY` with `deepgram`); `STTRequest`→`stt.Options` mapping including pointer semantics (`endpointing: 0` is not the same as absent) and the 400s for out-of-range thresholds; `attachSTTSinks` publishing `stt.text` with `speech_final` and `stt.turn` with seconds converted to milliseconds; and the cross-wiring guard — a room shares one `Options` template across its legs, so the callbacks must bind to a per-leg copy or every leg's events would go to whichever started last |
| `internal/api` (tts preflight) | 11 | Stage → `tts.staged` → commit → `tts.started`/`tts.finished`; commit before synthesis finishes returns 200 and plays when the audio lands; discard aborts the in-flight synthesis and publishes `tts.discarded{reason:"app"}`; TTL expiry and leg teardown publish `expired`/`leg_gone`; the per-leg cap refuses with 409 rather than evicting; double commit, commit-after-expiry and discard-after-commit error codes; a synthesis failure publishes `tts.error` with the synthesis category and no `tts.started`; oversize audio is `permanent_input`; a committed utterance is stoppable through `leg_play_stop` with the same `tts_id`; `pcmDurationMs` across rates and container formats |
| `internal/api` (room-scoped cleanup) | 3 | Every path that empties a room finalizes its recording: removing the last leg, moving the last leg to another room, and deleting the room. Each starts a real recorder through `doStartRecordRoom` and asserts it is unregistered and `recording.finished` published, so leaving the recording running fails as loudly as leaking the map entry |
| `internal/api` (vsi write deadline) | 7 | A VSI client that stops draining is disconnected instead of pinning the handler: an end-to-end stalled reader is hung up on and the disconnect logs `reason=write_timeout`, the recv loop's pong reply is deadline-bounded both standalone and interleaved into a fragmented message, and `vsiExitReason` lets only a write *timeout* override the read classification |
| `internal/mixer` (restart safety) | — | The stop channel is captured per goroutine at spawn time under the mixer lock. A remove-then-re-add that stops and restarts the mixer used to have the new `AddParticipant` rewrite `stopCh` while a winding-down `readLoop` still selected on it — a data race the `-race` build catches via `TestRemoveStreamIfParticipant_PointerElection`. |
| `internal/room` (leg streams) | 14 | `StreamParticipantID`/`SplitStreamParticipantID` round-trip and leave every existing participant namespace (UUID, `__bridge:`, `ws-`, `agent-`, `tts-`) unmatched; `AddLegStream` uses the stream's own sample rate rather than the leg's, mutes a sendonly stream as a source and deafens a recvonly one, refuses the primary and inactive streams, and keeps the mixer alive for a stream-only room; `RemoveLegStream` clears the stream's room; `RemoveStreamIfParticipant` elects on the participant pointer so a stale teardown cannot evict a successor; removing a leg takes its streams in that room; **sibling suppression** — a leg never hears its own secondary streams even under full-mesh routing, while unrelated legs stay full mesh; streams carry their own role in the routing matrix; `Manager.SetLegStreamRole` recomputes the matrix in place without the stream leaving the room, errors on an unknown leg or stream, and still records a role for a stream not currently in a room |
| `internal/leg` (media streams) | 8 | Per-stream direction gating (`sends`/`receives` so a recvonly stream runs no writeLoop), DTMF receive PTs default to 100/101 when the peer advertised none and follow the negotiated PTs when it did (incl. reset on renegotiation), primary stream always initialized and registered, `closeStream` removes a secondary stream, tombstones its m-line slot, releases its RTP port, and is a no-op for the primary |
| `internal/leg` (answer negotiation) | 4 | `negotiateInboundAnswer` answers a two-`m=audio` offer with two sections, both accepted (distinct ports, `a=recvonly` mirroring, `a=mid`/`a=content`/`a=lang` echoed); single-stream offers answer exactly as before with no unsolicited `a=mid`; no common codec fails; video is port-0 rejected under `SIP_SDP_STRICT_MLINE_ANSWER` |
| `internal/leg` (re-offer) | 7 | `ApplyRemoteOffer` re-applies the peer's media address on a re-INVITE (the regression guard for one-way audio after an SBC re-anchor), mirrors the offered direction into the answer, no-ops on a bodyless session-timer refresh, still answers a malformed offer; `applyAnswer` re-applies the address from a 200 OK to an offer we sent; `matchRemoteStream` prefers `a=mid` over m-line position; `adoptOutboundStreams` drops and tombstones a stream the peer answered with port 0 while the call continues on the primary |
| `internal/leg` (amd tap) | 1 | `ClearAMDTapIf` only clears the writer it was given, so a superseded AMD analysis cannot rip out the tap a later start installed |
| `internal/sip` (multi-stream SDP) | 21 | `ParseSDP` keeps every `m=audio` section and every `m=` line of any kind (`Audio[]`/`MLines[]`/`PrimaryAudio`), picks the first non-rejected section as primary, errors only when all audio is rejected, parses `a=mid`/`a=label`/`a=content`/`a=lang`/`a=rtcp-mux`, inherits session-level direction and lets media-level override it; legacy scalars stay in sync with the primary stream; `AudioByMID`, `MirrorDirection` (RFC 3264 §6.1 pairing), `HoldDirection` |
| `internal/sip` (SDP goldens) | 10 | Byte-identity of the single-stream offer/answer/re-INVITE SDP across the multi-stream refactor (PCMU/PCMA, Opus, AMR-WB, rejected `m=text`, RTT); multi-section rendering with `a=mid`/`a=label`/`a=content`/`a=lang`; port-0 rejection sections; round-tripping our own multi-stream offer back through `ParseSDP` |
| `internal/sip` (m-line table) | 9 | `MLineTable` append-only slots (count never shrinks, RFC 3264 §8), tombstones persist with port 0 and keep their mid, `ByMID`/`ByStreamID`/`Slot` lookups, `ActiveAudio`/`ActiveAudioCount` excluding tombstones, `MintMID` collision avoidance and `ReserveMID`, `LocalStreams` hold transform without mutating stored intent, `skipMedia` filtering |
| `internal/sip` (answer planning) | 9 | `PlanAnswer` emits one plan per offered `m=` section in order; accepts every offered audio section, echoes peer-disabled sections, rejects `no_common_codec` per section, omits vs. port-0-rejects video depending on `StrictMLines`, skips the RTT section, applies the preferred codec only to the first stream, mirrors offered directions |
| `internal/sip` (dtmf) | 8 | RFC 4733 packet generation (7-packet sequence, marker bit, duration units at 8 kHz vs AMR-WB 16 kHz), `TelephoneEventClockRate` per codec (incl. G.722's 8 kHz RTP clock despite 16 kHz sampling), offer/answer/re-INVITE advertise telephone-event at the codec's clock rate (16 kHz for AMR-WB, 8 kHz for G.722), `ParseSDP`/`DTMFPTForRate` capture the remote telephone-event PT and rate |
| `internal/leg` (pcmedia) | 6 | Codec-driven PeerConnection construction, SampleRate wiring, idempotent `Start`, ICE candidate drain, two-peer ICE+DTLS-SRTP loopback with PCM round-trip |
| `internal/leg` (whatsapp) | 6 | Outbound starts `connected`, inbound starts `ringing`, `RequestAnswer` rejects outbound and is idempotent, `Hangup` is idempotent, `SIPHeaders` propagation, Leg interface compliance |
| `internal/leg` (websocket) | 4 | Outbound lifecycle (ringing → connected via `AttachTransport`, audio + text round-trip, ClaimDisconnect single-flight, Hangup); inbound auto-connect with header capture (X-/P- filter); SendText returns `ErrRTTNotNegotiated` when RTT is disabled; SendDTMF returns "not supported" |
| `internal/wsmedia` | 11 | Framing (binary s16le and json_base64 round-trips), streamBuffer drop-on-overflow + paced read + Close, Transport echo loopback for both wire formats, text round-trip, hangup frame closes peer, context cancel exits loops, ingress overflow drops increment counters, SendStructured for vendor control messages, write deadline trips after `WriteTimeout` |
| `internal/wsutilx` | 13 | Shared WebSocket helpers: read-deadline arming and `WatchCancel`, and `LockedWriter` — every write arms a write deadline (a peer that never drains returns `os.ErrDeadlineExceeded` instead of blocking), concurrent writers never overlap inside `conn.Write` so frames stay intact, the client constructor masks frames while the server constructor does not, the first failure is latched so a poisoned mid-frame stream is never appended to, and a server writer's `onFail` teardown hook runs exactly once on that first failure and never on a healthy write |
| `internal/stt` | 40 | Azure framing/full-flow against a mock WS server, transcript and DTMF log-level redaction, and per-provider WebSocket guards: every send loop returns on a wedged write rather than pinning its goroutine, and every recv loop logs the peer's close code and reason. Turn detection: `emitTranscript` sink selection (the detail callback supersedes the positional one and never doubles up); `buildDeepgramV1URL` byte-identical to the pre-tuning URL when no new option is set, `utterance_end_ms` forcing `interim_results=true` on the wire while interims stay suppressed locally unless `partial`; Deepgram dispatch separating `is_final` from `speech_final` and routing `UtteranceEnd` (whose `channel` is an array, not an object) to a turn event with no transcript; Deepgram Flux `buildFluxURL`, the full `StartOfTurn→Update→EagerEndOfTurn→TurnResumed→EndOfTurn` sequence with and without partials, `EagerEndOfTurn` never surfacing as `is_final`, error frames not killing the recv loop, `*FluxTranscriber` deliberately not implementing `Finalizer`, and the 80 ms send cadence held against a 20 ms-granularity reader (plus the partial tail flush) |
| `internal/agent` | 10 | Per-provider WebSocket guards for Deepgram/ElevenLabs/Pipecat/VAPI agent sessions: send loops unblock on a wedged write (VAPI included, whose write was previously neither serialized nor bounded), and recv loops log the peer's close code and reason |
| `internal/lkmedia` | 60 | LiveKit signaling and transport: token minting, SDP fixups, audio level parsing, config validation, transport lifecycle — plus signal WebSocket guards: `send` is bounded by a write deadline, control-frame replies from the read goroutine go out under `writeMu` as whole frames, close code/reason are logged, and a normal (1000) closure is classified as a clean disconnect rather than an error |

---

### AMR-WB codec tests & benchmarks (sibling `goamr-wb` module)

The AMR-WB codec is a pure-Go port of the Apache-2.0 `opencore-amrwb` (decoder) and
`vo-amrwbenc` (encoder) C reference, maintained as its own published module
(`github.com/VoiceBlender/goamr-wb`) and pinned in VoiceBlender's `go.mod`. Its own tests run
from a local clone of that repo:

```bash
git clone https://github.com/VoiceBlender/goamr-wb && cd goamr-wb

# Unit tests (pure Go; ~98 subtests)
go test .

# Go micro-benchmarks: encode/decode ns/op + an xRT real-time factor, per mode
go test -bench 'BenchmarkEncode|BenchmarkDecode' -benchmem .
```

**Differential + speed-vs-C tests (optional).** Two differential tests validate the port
bit-for-bit, and two speed tests report a per-mode Go-vs-C table (`go ns/frame | c ns/frame |
go/c | ×realtime`). All four **skip** unless pointed at a locally built reference harness (the
C binaries are not vendored and do not run in CI):

```bash
# Encoder: bit-exact + speed vs vo-amrwbenc
AMRWB_ENC=/path/to/vo-amrwbenc-harness go test -run TestEncDiffAgainstCReference .
AMRWB_ENC=/path/to/vo-amrwbenc-harness go test -run TestEncSpeedVsCReference -v .

# Decoder: bit-exact + speed vs opencore-amrwb
AMRWB_DIFF=/path/to/opencore-amr-harness go test -run TestDiffAgainstCReference .
AMRWB_DIFF=/path/to/opencore-amr-harness go test -run TestDecSpeedVsCReference -v .
```

The speed comparison times the C reference across a pipe to a separate process and uses a
two-point (slope) method over `AMRWB_BENCH_FRAMES` frames (default 4000) to cancel process
startup; see the header of `goamr-wb/speed_test.go` for the methodology and its caveats.
Without the env vars all of these skip, so the normal VoiceBlender `go test ./internal/...` run
is unaffected.

---

## Integration Tests

Integration tests spin up two full VoiceBlender instances (SIP + HTTP) on localhost with dynamic ports and make real SIP calls between them. No external services required.

### Run all integration tests

```bash
go test -tags integration -timeout 60s ./tests/integration/
```

The `-tags integration` flag is **required** — without it, the test files are skipped.

### Run with verbose output

```bash
go test -tags integration -v -timeout 60s ./tests/integration/
```

### Run a specific integration test

```bash
go test -tags integration -v -timeout 60s -run TestOutboundInbound_Connect ./tests/integration/
go test -tags integration -v -timeout 60s -run TestRecording ./tests/integration/
go test -tags integration -v -timeout 60s -run TestMute ./tests/integration/
go test -tags integration -v -timeout 60s -run TestDTMFBroadcast ./tests/integration/
go test -tags integration -v -timeout 60s -run TestRTT ./tests/integration/
go test -tags integration -v -timeout 60s -run TestWSEvents ./tests/integration/
go test -tags integration -v -timeout 60s -run TestSIPInboundAuth ./tests/integration/
go test -tags integration -v -timeout 60s -run TestS3Preflight ./tests/integration/
go test -tags integration -v -timeout 60s -run TestGCSRecording ./tests/integration/
```

### Integration test list

| Test | Description |
|------|-------------|
| `TestOutboundInbound_Connect` | Basic SIP call: A dials B, B answers, both connect |
| `TestUseSourceSocket_RoundTripCall` | Smoke test for `SIP_USE_SOURCE_SOCKET=true`: end-to-end call setup, BYE, and disconnect events still complete with the flag enabled. Unit tests in `internal/sip/engine_test.go` (`TestEngine_PinDestinationToSource`) cover the destination-pinning logic itself. |
| `TestCall_IPv6Loopback` | Same as above, but both instances are bound to `[::1]` (IPv6 loopback). Skipped when the host has no IPv6 loopback. |
| `TestCall_DualStackInterop_V4Caller` | A dual-stack callee answers an IPv4-only caller with `IN IP4` SDP — exercises the family-from-offer rule. |
| `TestDisconnect_DurationFields` | Verify `duration_total` and `duration_answered` in disconnect event |
| `TestDisconnect_UnansweredDuration` | Unanswered call has `duration_answered=0` |
| `TestOutboundInbound_CallerCancel` | Caller cancels before answer |
| `TestOutboundInbound_RoomBridge` | Create room, add/remove leg, verify events |
| `TestRoomBridge_AudioCrossesBidirectional` | Bridge two rooms; audio injected into either room reaches the other; `room.bridged` event |
| `TestRoomBridge_DirectionOneWayAndPatch` | `direction:"send"` blocks the reverse path; live `PATCH` to `receive` re-enables it; `room.bridge_updated` event |
| `TestRoomBridge_None` | Parked (`none`) bridge passes no audio but is still listed |
| `TestRoomBridge_Validation` | Self-bridge/bad-direction → 400, missing room → 404, sample-rate mismatch → 400, duplicate pair → 409 |
| `TestRoomBridge_KeepaliveAndRoomDeleteTeardown` | Bridge keeps a leg-less room's mixer alive; deleting a bridged room tears the bridge down with `room.unbridged` (`reason: room_deleted`) |
| `TestRoutingMatrix_HTTP_PutGetPatch` | `PUT /v1/rooms/{id}/routing` replaces the role-based audio routing matrix and emits `room.routing_changed` with `reason: set`; `GET` round-trips the matrix; `PATCH` with `"sources": null` clears a row (full mesh) and emits `reason: update` |
| `TestRoutingMatrix_RoomMissing` | All three routing endpoints (`GET/PUT/PATCH`) return 404 for an unknown room ID |
| `TestMixerPanic_TearsDownOnlyPanickedLeg` | A leg whose audio path panics is hung up and reported as `leg.disconnected` with `cdr.reason: mixer_panic`, and disappears from `GET /v1/legs/{id}` (404). A live SIP call sharing the room is untouched and stays a participant; the panic is counted in `voiceblender_recovered_panics_total{site="writeLoop"}` |
| `TestMixerPanic_BadTapDoesNotStopTheRoom` | A participant tap that panics on every frame — a broken recording/STT consumer — only skips ticks: the mix loop keeps ticking, the call stays up, and the panic is counted with `site="mixTick"` |
| `TestSIPREC_InboundSessionAnsweredRecvOnly` | An SRC sends a multipart `SDP` + `rs-metadata` INVITE with `Require: siprec`, a `+sip.src` Contact and two `sendonly` sections over TCP; the SRS answers both `recvonly` with labels echoed, without any REST `/answer`, publishes `siprec.session_started`, types the leg `siprec_in`, and `GET /v1/legs/{id}/siprec` resolves each label to the right participant AOR |
| `TestSIPREC_RejectedWhenDisabled` | With `SIPREC_ENABLED=false` (the shipped default) an INVITE demanding `Require: siprec` is refused and never surfaces as a ringing leg |
| `TestSIPREC_DisabledIgnoresHintOnlyInvite` | With SIPREC disabled, an INVITE carrying only the `+sip.src` Contact tag and no `Require` still connects as an ordinary `sip_inbound` call — only an option tag obliges a response (RFC 3261 §8.2.2.3), so enabling nothing changes nothing |
| `TestSIPREC_MissingMetadataRejected` | An INVITE claiming `Require: siprec` but carrying a plain SDP body is rejected: without metadata no stream can be bound to a participant |
| `TestSIPREC_StreamsAttachedToSessionRoom` | With `SIPREC_ROOM_MODE=per_session`, every stream — including m-line 0, which on an ordinary leg is the privileged primary — joins room `siprec-<legID>`, each with its own participant-derived role |
| `TestSIPREC_ReInviteAddsParticipant` | A party joins mid-session: the re-offer adds a third `m=audio` section and a `datamode=partial` document binds it to a new participant; the section is negotiated `recvonly`, bound to that participant, published as `siprec.participant_joined`, and pulled into the session room |
| `TestSIPREC_ReInviteRemovesParticipant` | A `datamode=partial` document closing an association with `disassociate-time` drops the participant and unbinds the stream it was sending on, publishing `siprec.participant_left` |
| `TestSIPREC_MetadataOnlyReInviteUpdatesSession` | A re-INVITE with no SDP part is still answered and still applies: a participant rename lands, the negotiated media is untouched, and the participants the partial document does not mention survive |
| `TestSIPREC_RecordsOneChannelPerParticipant` | `POST /v1/legs/{id}/record` on a recording session captures every stream separately; RTP is pushed on both, and the merged file is a 2-channel WAV at `DEFAULT_SAMPLE_RATE` (PCMU at 8 kHz resampled up) whose `recording.finished` `channels` map is keyed by participant AOR with no omitted parties |
| `TestSIPREC_SRCForksRoomToRecordingServer` | Full loop between two instances: one hosts a two-leg room and forks it to the other acting as a recording server. Asserts the `siprec_out` leg type, that the server bound one recvonly stream per room leg, that audio actually reaches the server (recorded as a 2-channel WAV, proving the mixer-tap → pipe → pump → RTP path), and that deleting the SRC leg ends the session on the server |
| `TestSIPREC_SRCSelectsSubsetOfRoom` | `leg_ids` records only the named participants: two of a three-leg room are forked and the third is not |
| `TestSIPREC_SRCSelectsASecondaryStream` | `leg_ids` addresses streams as well as legs — selecting `"<legID>#1"` forks a translated feed mixed into the room without forking the leg's own audio |
| `TestSIPREC_SRCRejectsUnknownLegID` | A `leg_ids` entry naming nothing in the room is a 404 rather than a silently smaller session |
| `TestSIPREC_SRCForksASingleCall` | `POST /v1/legs/{id}/siprec` forks one call as two recvonly sections bound to distinct parties, with no room; an ordinary `/record` on the same leg still works alongside it, proving the dedicated SRC taps do not collide with recording |
| `TestSIPREC_SRCRefusesRecordingALegTwice` | A `siprec_in` leg cannot be forked onward to another recording server |
| `TestSIPREC_SRCDisabledByDefault` | `POST /v1/rooms/{id}/siprec` returns 403 unless `SIPREC_SRC_ENABLED=true` |
| `TestSIPREC_OrdinaryCallUnaffected` | A plain call against a SIPREC-enabled server still negotiates as `sip_inbound` and starts no recording session — regression guard for the Content-Type-aware body handling |
| `TestMultiStream_OfferTwoAudioLines_Answered` | A dials B offering two `m=audio` sections (original `a=content:main a=lang:en`, translated `a=sendonly a=content:alt a=lang:es`) on distinct ports; B accepts both, mirrors the sendonly section to `a=recvonly`, echoes `a=lang`, and materializes two media streams on its leg |
| `TestMultiStream_PlainCallIsSingleStream` | An ordinary call offering one `m=audio` section still yields exactly one stream — multi-stream changes nothing for it |
| `TestMultiStream_AnswerWithStreamRooms` | `POST /v1/legs/{id}/answer` with a `streams` array routes the caller's translated stream into its own room in the same request that answers the call; entries are positional over the accepted secondary streams and the primary is left unattached |
| `TestMultiStream_AddLegToRoomWithStreams` | `POST /v1/rooms/{id}/legs` with a `streams` array puts the leg and a named secondary stream into the same room in one request |
| `TestMultiStream_PatchStreamRole` | `PATCH /v1/legs/{id}/streams/{streamId}` changes a stream's routing role in place — the stream stays in its room throughout (no detach/re-attach) — and rejects the primary stream with 400 since its role follows the leg |
| `TestMultiStream_AddLegToRoomStreamValidation` | That endpoint rejects an unknown `stream_id` (404), a missing one (400), and the primary stream (400 — it joins with the leg itself) |
| `TestMultiStream_RESTCreateWithStreams` | `POST /v1/legs` with a `streams` array establishes a two-`m=audio` call from the **initial INVITE** (no re-INVITE): the primary joins the leg's `room_id` while the `sendonly` translated feed joins its own room with its own role, on a distinct RTP port, and the answering side materializes both |
| `TestMultiStream_RESTCreateWithStreamsValidation` | `POST /v1/legs` rejects a malformed `streams` request up front rather than as a failed INVITE: 400 for an invalid direction, 404 for an unknown `room_id` |
| `TestMultiStream_RESTAddAttachRemove` | Drives the whole per-leg stream API over HTTP: `POST /v1/legs/{id}/streams` adds a `sendonly` translated stream to a live call via re-INVITE (own RTP port, `a=content:alt`, `a=lang:es`), attaches it to its own room with a role, the peer materializes the matching stream, and `DELETE` removes it leaving only the primary |
| `TestMultiStream_TranslationTopology` | A two-stream leg's original audio is mixed in one room while its translated stream is mixed in another; the leg's own `RoomID` is unaffected, the stream's participant is `<legID>#<streamID>`, and tearing the leg down detaches the cross-room stream (which removing the leg from its own room would never reach) |
| `TestOutboundInbound_RingTimeout` | Ring timeout expires, call fails |
| `TestRecording_StandaloneSIPLeg` | Stereo recording of standalone SIP leg (left=in, right=out) |
| `TestRecording_InRoomLeg` | Stereo recording of leg in a room (left=participant, right=mix) |
| `TestRecording_Room` | Mono room mix recording |
| `TestRecording_CustomFilename` | Optional `filename` on leg `/record` publishes a WAV with that basename (dotted stems preserved, `.wav` appended) |
| `TestRecording_CustomFilename_RejectsReuse` | Record → stop → record again with the same `filename` returns 409 and leaves the first WAV untouched |
| `TestRecording_CustomFilename_RejectsInFlightCollision` | A room `/record` with the same `filename` as an in-flight leg recording on the same instance returns 409 |
| `TestRecording_CustomFilename_InvalidRejected` | Path-shaped `filename` values (e.g. `../secret`) return 400 |
| `TestRecording_StopsOnDisconnect` | Recording auto-stops when leg hangs up |
| `TestRoomRecordingFinalize_RemoveLastLeg` | `DELETE /v1/rooms/{id}/legs/{legID}` on the last participant finalizes the room recording: `recording.finished` carries the room's `app_id` and file, a second stop returns 404, and the file stops growing |
| `TestRoomRecordingFinalize_MoveLastLegOut` | Moving a room's last participant into another room finalizes the source room's recording |
| `TestRoomRecordingFinalize_DeleteRoom` | `DELETE /v1/rooms/{id}` finalizes the recording explicitly — the room is gone by then, so the per-leg cleanup cannot reach it and the `app_id` has to be snapshotted first |
| `TestRoomRecordingFinalize_MixerPanicOnLastLeg` | A panicked leg that is the room's last participant finalizes the recording via the panic teardown; the recording is *not* finalized while that leg is still in the room |
| `TestRecording_RoomNoParticipants` | Recording empty room returns 409 |
| `TestRecording_StopWithNoRecording` | Stop without active recording returns 404 |
| `TestRecording_StorageFileExplicit` | Explicit `storage=file` works |
| `TestRecording_StorageS3NotConfigured` | `storage=s3` without config returns 400 |
| `TestGCSRecording_LegUploadsAndReportsGSURI` | A leg recording with `storage=gcs` uploads to the bucket once and reports a `gs://` location in both the stop response and `recording.finished`, with the object name carrying the `GCS_OBJECT_NAME_PREFIX` separator |
| `TestGCSRecording_UnconfiguredRejectsRecordStart` | `storage=gcs` with no bucket configured returns 400 instead of starting a recording with nowhere to go |
| `TestGCSRecording_PerRequestBucketUploadsMultiChannelAndMix` | The per-request `gcs_bucket` path end to end on a `multi_channel` room recording — the one case that uploads twice through a single backend: both the merged per-participant file and the room mix land in the request's bucket |
| `TestGCSRecording_RoomUploadsMix` | A room recording takes its own path to the same backend: the merged mix is uploaded and reported as a `gs://` URI |
| `TestS3Preflight_MissingBucketRejectsRecordStart` | A bucket the store reports absent fails record-start with 400 and never attempts an upload, so the caller finds out before the call is recorded |
| `TestS3Preflight_NoListBucketPermissionStillRecords` | A probe that cannot get a verdict (403 without `s3:ListBucket`, a 5xx, or an expired budget) is only warned about: record-start returns 200 and the recording still uploads, with `recording.finished` naming the `s3://` location |
| `TestS3Preflight_InsecureEndpoint` | A plaintext `http://` `s3_endpoint` on a non-local host returns 400, and 200 once the operator sets `S3_ALLOW_INSECURE_ENDPOINT=true` |
| `TestS3Preflight_UnresponsiveEndpointDoesNotStallRecordStart` | An endpoint that accepts the connection and never answers does not hold record-start beyond `S3_REQUEST_PREFLIGHT_TIMEOUT`, and does not fail it |
| `TestStereoRecording_StaysAlignedAcrossIncomingStall` | A real loopback call stalls incoming RTP mid-recording while outgoing keeps flowing; marker bursts injected on both sides before and after prove the channels' offset did not change, i.e. the stall left no lasting skew |
| `TestStereoRecording_SurvivesOutgoingStall` | The mirror case a producer-paced recorder cannot handle: holding the *recorded* leg stops its outgoing tap writes entirely, and the recording must still span the call in real time with the incoming audio at its true offsets |
| `TestRecording_PauseResume_Leg` | Pause/resume endpoints, events, idempotency, 404 after stop; also asserts the advertised path does not exist while recording is in progress and no `.rec-*.tmp` staging residue is left behind |
| `TestRecording_PauseResume_Room` | Room-level pause/resume with events |
| `TestMute_LegInRoom` | Mute/unmute in room, verify mix excludes muted audio |
| `TestMute_SpeakingEventsSuppressed` | No speaking events for muted legs (with speech detection explicitly enabled) |
| `TestMute_BeforeRoomJoin` | Mute before joining room, verify it persists |
| `TestAddLegToRoom_JoinMutedAndDeaf` | Join a room already muted + deaf via `mute`/`deaf` on `POST /v1/rooms/{id}/legs` (race-free) |
| `TestSpeechDetection_DisabledByDefault` | No speaking detector attached when `SPEECH_DETECTION_ENABLED` is unset |
| `TestSpeechDetection_EnabledGlobally` | `SPEECH_DETECTION_ENABLED=true` attaches the detector on every leg |
| `TestSpeechDetection_PerCallOutboundOverride` | `speech_detection: true` on `POST /v1/legs` overrides a disabled default |
| `TestSpeechDetection_PerCallAnswerOverride` | `speech_detection: false` on `POST /v1/legs/{id}/answer` overrides an enabled default |
| `TestAMD_Human` | AMD classifies short tone burst as `human` |
| `TestAMD_Machine` | AMD classifies continuous tone as `machine` |
| `TestAMD_NoSpeech` | AMD returns `no_speech` when no audio is played |
| `TestAMD_Disabled` | No AMD event when `amd` field is omitted |
| `TestAMD_InvalidParams` | A window too short to reach any verdict and a negative override are rejected with 400; suppressing a single verdict with one oversized threshold stays valid |
| `TestAMD_TeardownMidAnalysis` | Hanging up mid-analysis ends AMD with no `amd.result` event and leaks no goroutine |
| `TestAMD_DefaultParams` | Empty `"amd": {}` uses all defaults |
| `TestDTMFBroadcast_Default` | DTMF received on one leg is forwarded to other legs in the same room |
| `TestDTMFBroadcast_RejectAtRuntime` | `POST /v1/legs/{id}/dtmf/reject` blocks reception; `accept` re-enables it |
| `TestDTMFBroadcast_RejectAtOriginate` | `accept_dtmf:false` in originate body blocks reception from the start |
| `TestDTMFBroadcast_SequenceNumbers` | DTMF events carry monotonically increasing per-leg sequence numbers |
| `TestDTMFBroadcast_SenderExcluded` | Originating leg never receives a forwarded copy of its own DTMF |
| `TestRTT_RoundTrip` | Two RTT-enabled instances exchange T.140 / RFC 4103 text in both directions; `rtt.received` events fire with the sent payload |
| `TestRTT_NotEnabledRejectsSendCleanly` | When the peer omits `m=text`, audio still negotiates; `POST /v1/legs/{id}/rtt` on the un-negotiated side returns 409 |
| `TestVSI_RTT_SendDelivers` | VSI `send_leg_rtt` over the `/v1/vsi` WebSocket delivers text to the remote leg (parity with REST `POST /rtt`) |
| `TestVSI_RTT_AcceptRejectFlags` | VSI `accept_leg_rtt`/`reject_leg_rtt` toggle the receiver's `accept_text` flag; rejected legs suppress `rtt.received` events |
| `TestVSI_RTT_SendOnNonNegotiatedLegReturns409` | VSI `send_leg_rtt` returns an error frame when RTT was never negotiated on the leg |
| `TestVSI_CreateLeg_Originates` | Outbound SIP originate over the `/v1/vsi` WebSocket (`create_leg` type `sip`) returns a `create_leg.result` leg view; the callee receives the INVITE, answers, and both legs reach `connected` (parity with REST `POST /v1/legs`) |
| `TestVSI_CreateLeg_InvalidURI` | VSI `create_leg` with an invalid SIP URI returns an `error` frame with code 400 (not a 501 or silent accept) |
| `TestVSI_CreateLeg_WebSocket` | VSI `create_leg` type `websocket` dials an in-test echo server and the leg reaches `connected` (parity with REST `POST /v1/legs` type websocket) |
| `TestVSI_CreateLeg_WebSocketValidation` | VSI `create_leg` type `websocket` with no `url` returns an `error` frame with code 400 |
| `TestVSI_CreateLeg_WhatsAppValidation` | VSI `create_leg` type `whatsapp` with no `to` returns an `error` frame with code 400 (validation runs over VSI, not "unsupported leg type") |
| `TestVSI_CreateLeg_LiveKitError` | VSI `create_leg` type `livekit_room` is dispatched and returns a real `error` frame (503 when LiveKit disabled — the default — or 400 for missing params), never a 501/unsupported |
| `TestVSI_DeleteRegistration` | Over VSI: a raw client REGISTERs, `list_sip_registrations` shows the binding, `delete_sip_registration` unbinds it, the list goes empty, and an unknown-AOR delete returns an `error` frame with code 404 |
| `TestVSI_WebRTC_FullFlow` | VSI `webrtc_offer` / `webrtc_get_candidates` / `webrtc_add_candidate` round-trip with a real pion client; leg appears in `list_legs` and emits `leg.connected` |
| `TestVSI_WebRTC_OfferInvalidSDP` | VSI `webrtc_offer` with malformed SDP returns a 400 error frame |
| `TestVSI_WebRTC_AddCandidateNotFound` | VSI `webrtc_add_candidate` for an unknown leg returns a 404 error frame |
| `TestWSEvents_ConnectedAndEvents` | Connect to `/v1/vsi`, originate a call, verify `leg.ringing` event arrives |
| `TestWSEvents_UnknownCommand` | Send unknown command with `request_id`, verify error response echoes it |
| `TestWSEvents_StopCommand` | Send `stop`, verify server closes the connection |
| `TestWSCommands_RoomLifecycle` | Create, get, list, delete room via WS commands; error on deleted room |
| `TestWSCommands_MuteLeg` | Mute/get_leg via WS; error on missing leg; error on unknown command |
| `TestWSEvents_AppIDFilter` | Two WS clients (filtered + unfiltered), two legs with different `app_id`; filtered client only sees matching events |
| `TestEventID_WebhookHeaderAndBody` | A delivered webhook POST carries an `X-Event-Id` header equal to the `event_id` in the flattened body; `voiceblender_webhook_enqueued_total` and `voiceblender_webhook_deliveries_total` advance on `/metrics` |
| `TestEventID_SharedAcrossWebhookAndVSI` | One published event reaches a webhook receiver and a `/v1/vsi` subscriber with the same `event_id` — the cross-subscriber guarantee that makes it usable as an idempotency key |
| `TestWebRTC_AppIDFilter` | A WebRTC leg tagged with `app_id` on the offer reaches an `app_id`-filtered VSI subscriber, while an untagged WebRTC leg on the same server is dropped by the filter |
| `TestWebRTC_AppIDInLegEvents` | `app_id` set on `POST /v1/webrtc/offer` is carried on the leg's `leg.connected` event |
| `TestTransfer_Blind_Outbound` | A↔B, REFER on B's leg dials C, completion event fires, original hung up |
| `TestTransfer_Blind_RemoteByeEndsTransferredLeg` | After the transfer completes, C hangs up: B must publish `leg.disconnected` with `remote_bye` for the leg it originated and drop it from the manager (the referrer that would otherwise watch it is already gone) |
| `TestTransfer_Inbound_AutoDeclineOnTimeout` | Auto-dial off: an undecided inbound REFER is parked, surfaced as `leg.transfer_requested` (declined vestigial), then auto-declined 603 after `SIP_REFER_CONSULT_TIMEOUT_MS`; referrer sees `transfer_failed` |
| `TestTransfer_Inbound_AppAcceptsAndCompletes` | App-driven happy path: `transfer/accept` then `transfer/complete{success:true}`; referrer observes 202→NOTIFY 100→NOTIFY 200 as `transfer_completed` |
| `TestTransfer_Inbound_AppDeclines` | App calls `transfer/decline`; referrer sees `transfer_failed` |
| `TestTransfer_NotConnected` | `/transfer` on a ringing leg returns 409 |
| `TestTransfer_BadRequest` | Missing or malformed `target` returns 400 |
| `TestCodecSelect_RingingExposesOffer` | `leg.ringing` payload includes `offered_codecs` array with priority order |
| `TestCodecSelect_AnswerWithExplicitCodec` | `POST /v1/legs/{id}/answer` honors a `codec` field in the request body |
| `TestCodecSelect_AnswerRejectsCodecNotInOffer` | Answer with a codec not in the remote offer returns 400 |
| `TestAMRWB_NegotiateAndConnect` | AMR-WB-only offer exposed in `leg.ringing` (16 kHz clock, dynamic PT), `/answer` with `AMR-WB` connects both legs |
| `TestAMRWB_EndToEndAudio` | AMR-WB call recovers non-silent audio through encode → RTP → decode, for both octet-aligned and bandwidth-efficient framing |
| `TestAMRWB_DTMF` | Out-of-band DTMF (RFC 4733) flows in both directions over the 16 kHz telephone-event negotiated alongside AMR-WB |
| `TestAMRNB_NegotiateAndConnect` | AMR-NB-only offer exposed in `leg.ringing` (8 kHz clock, dynamic PT, rtpmap name "AMR" per RFC 4867 §8.1), `/answer` with `AMR-NB` connects both legs |
| `TestAMRNB_EndToEndAudio` | AMR-NB call recovers non-silent audio through encode → RTP → decode, for both octet-aligned and bandwidth-efficient framing |
| `TestAMRNB_DTMF` | Out-of-band DTMF (RFC 4733) flows in both directions over the 8 kHz telephone-event paired with AMR-NB |
| `TestG722_DTMF` | Out-of-band DTMF (RFC 4733) flows in both directions over a G.722 call (telephone-event stays at G.722's 8 kHz RTP clock despite 16 kHz sampling) |
| `TestRing_ExplicitRingingThenAnswer` | Default `SIP_AUTO_RINGING=false`; multiple `/ring` calls send 180s, then `/answer` connects |
| `TestRing_AutoRingingPreservesLegacyFlow` | `SIP_AUTO_RINGING=true` restores auto-180 behavior; no explicit `/ring` needed |
| `TestRing_RejectsAfterAnswer` | `/ring` on a connected leg returns 409 |
| `TestWSLegInboundAutoConnect` | WebSocket client connects to `/v1/legs/websocket`, joins a room, exchanges audio + text, `headers` map captures X-/P- headers |
| `TestWSLegOutboundDialAndHeaders` | `POST /v1/legs` with `type:"websocket"` dials a remote WS echo server, verifies the echo server received the supplied X-Correlation-ID header |
| `TestWSLegOutboundDialFailure` | Outbound WS dial to a non-listening port produces a `leg.disconnected` event with a mapped reason |
| `TestWSLegAudioFlows` | Egress audio: WS leg joins a room, a tone playback runs into the room, the WS client reads binary PCM frames and asserts RMS is well above the silence floor |
| `TestWSLegAudioFlowsBidirectional` | Ingress + egress audio: two WS legs in the same room; client A streams a 1 kHz sine, client B reads PCM frames and asserts the sine survives the WS→mixer→WS round-trip (RMS above the silence floor) |
| `TestRoomWSCompatibleWithLegWS` | Confirms `/v1/rooms/{id}/ws` and `/v1/legs/websocket` speak the same wire protocol after both endpoints share `wsmedia.Transport`: a leg WS writer and a room WS reader exchange JSON-base64 audio (`{"audio":"<b64>"}` shape) end-to-end, including the welcome `connected` frame and the `{"type":"stop"}` close verb |
| `TestWSLegPing` | Inbound WS leg replies to a `{"type":"ping","event_id":N}` text frame with a matching pong |
| `TestPlaybackCrossSampleRate` | Sweeps leg/room sample-rate combinations (8/16/48 kHz × 8/16/48 kHz) with a tone playback started before the leg joins a room. The captured WS egress must hold the original 425 Hz tone across the inject path even when room rate ≠ producer rate — regression guard for the high-pitched-TTS bug where `legPlaybackWriter` skipped resampling on the room inject path |
| `TestResampleAntiAliasing` | Two 16 kHz WS legs in an 8 kHz room; leg A injects a 6 kHz tone (above the 4 kHz room Nyquist), the mixer routes it to leg B. Asserts the 2 kHz fold-back alias stays ≥20 dB below a genuine in-band reference tone — end-to-end proof the polyphase resampler filters above-Nyquist energy instead of aliasing it, which the old linear/decimation downsampler could not do |
| `TestSIPRegister_Basic` | Raw sipgo client REGISTERs; expects 200 OK echoing `Expires`, a `sip.registration_active` event, and the binding visible via `GET /v1/sip/registrations` with the client's actual source socket |
| `TestSIPRegister_Refresh` | Same Contact re-registers from a different ephemeral source port; binding's `Socket` updates in-place; only one binding remains for the (AOR, Contact) pair |
| `TestSIPRegister_MultiContact` | Same AOR registers from two distinct Contacts; both bindings are listed; per-contact unregister and the `SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS=false` displacement path are also exercised |
| `TestSIPRegister_Expiry` | Short-expires REGISTER → TTL sweeper removes the binding and emits `sip.registration_expired` with `reason:ttl` |
| `TestSIPRegister_Unregister` | REGISTER with `Contact: *` and `Expires: 0` removes all bindings with `reason:unregistered` |
| `TestSIPRegister_ForceDelete` | `DELETE /v1/sip/registrations/{aor}` force-unbinds with `reason:forced` |
| `TestSIPRegister_DialAOR` | After REGISTER, `POST /v1/legs {"type":"sip","to":"sip:alice@..."}` routes the outbound INVITE to the bound socket (via a loose Route) while keeping the dialed AOR as the Request-URI |
| `TestSIPRegister_CancelUnansweredRoutesToContact` | Deleting an unanswered outbound leg dialed to a registered AOR sends CANCEL to the registered contact (not the AOR host / VoiceBlender itself); the CANCEL Request-URI stays the dialed AOR per RFC 3261 §9.1 |
| `TestSIPRegister_Fork` | AOR registered from two raw clients; `POST /v1/legs` parallel-forks (both clients receive an INVITE); the second client answers 200 OK, the first receives CANCEL and its INVITE transaction terminates (after Timer I = 5 s for UDP) |
| `TestSIPInboundAuth_RegisterChallengeSuccess` | Consult enabled (webhook set); REGISTER parks → `sip.registration_attempt` event → `POST /v1/sip/registrations/attempts/{id}/challenge` → `401`; credentialed re-REGISTER (same Call-ID) verifies → `200 OK` and a live binding |
| `TestSIPInboundAuth_RegisterChallengeMaxExpires` | Challenge with `max_expires: 30`; the UA requests 600 s and the credentialed re-REGISTER binds at `granted_expires_seconds == 60` (the cap floored to the 60 s minimum) |
| `TestSIPInboundAuth_RegisterChallengeWrongPassword` | Same challenge flow but the retry signs with a wrong password → `403 Forbidden` and no binding is created |
| `TestSIPInboundAuth_RegisterTimeoutAcceptsWhenConfigured` | With `SIP_INBOUND_REGISTER_DEFAULT=accept`, the REGISTER is surfaced (`sip.registration_attempt` fires) and, with no client decision, the fallback binds it with `200 OK` after the consult window |
| `TestSIPInboundAuth_RegisterTimeoutRejectsByDefault` | Shipped fail-closed default (`SIP_INBOUND_REGISTER_DEFAULT=reject`): an undecided REGISTER is denied `403` and never binds |
| `TestSIPInboundAuth_InviteChallengeSuccess` | Inbound INVITE → `POST /v1/legs/{id}/challenge` → `401`; credentialed re-INVITE surfaced as a new `leg.ringing` with `authenticated=true` + `auth_username`; answered → `200 OK` |
| `TestTrunk_SIPRegister_HappyPath` | `POST /v1/sip/trunks {"type":"sip_register",...}` → 202; the fake registrar receives one REGISTER, returns 200 with `Expires=120`; trunk status flips to `active`; `sip.outbound_registration_active` event emitted; no response body leaks `password` |
| `TestTrunk_SIPRegister_DigestAuth` | Fake registrar challenges the initial REGISTER with `401 Unauthorized` + `WWW-Authenticate`; the trunk computes the digest response and the second REGISTER carries an `Authorization` header that the registrar accepts |
| `TestTrunk_SIPRegister_Refresh` | Registrar grants `Expires=2`; the trunk refreshes at `granted * SIP_OUTBOUND_REGISTRATION_REFRESH_RATIO` (default 0.5) so a second REGISTER hits the registrar within ~1 s |
| `TestTrunk_SIPRegister_Unregister` | `DELETE /v1/sip/trunks/{id}` returns 202; a final REGISTER with `Expires: 0` lands at the registrar; `sip.outbound_registration_expired` event with `reason: unregistered`; subsequent `GET /v1/sip/trunks/{id}` returns 404 |
| `TestTrunk_SIPRegister_RefreshFailedEmitsExpired` | First REGISTER succeeds with `Expires=1`, then the registrar rejects every subsequent REGISTER with 503. After the granted lifetime lapses, exactly one `sip.outbound_registration_expired` event with `reason: refresh_failed` is emitted; the trunk keeps retrying in the background |
| `TestTrunk_SIPRegister_OutboundCallUsesTrunk` | After REGISTER, `POST /v1/legs {"type":"sip","to":"sip:bob@<registrar>","from":"alice"}` auto-attaches the trunk's credentials and adds a `Route: <registrar_uri;lr>` header on the outbound INVITE. The bare `from` carries no host, so the From and P-Asserted-Identity land in the trunk's AOR realm (`sip:alice@vb.test`) rather than the engine's public host (all verified at the fake registrar) |
| `TestTrunk_SIPRegister_OutboundCallFromFullURI` | Same call with `from` given as the full URI `sip:alice@vb.test`: trunk matching still hits (Route present) and the From/P-Asserted-Identity are well-formed. Guards the two-`@` From (`sip:sip:alice@vb.test@<public host>`) produced when the whole URI landed in the user part |
| `TestTrunk_SIPRegister_OutboundCallFromHostNormalized` | `from: "sip:alice@VB.TEST"` — the caller's spelling of the host does not reach the wire: trunk matching still hits and the From goes out as `sip:alice@vb.test`, while the case-sensitive user-part is unchanged |
| `TestTransfer_ReferInheritsTrunkIdentity` | instB holds a registered trunk (AOR `sip:alice@vb.test`) whose registrar is instA, and calls instA over it; instA answers and then REFERs instB to instC. instB's auto-dialled transfer leg goes out under the trunk — its `leg.ringing` carries `from: "sip:alice@vb.test"` and the referrer's `trunk_id` — so the INVITE inherits the trunk's credentials and Route instead of going out unauthenticated |
| `TestTrunk_TypeIPIP_NotImplemented` | `POST /v1/sip/trunks {"type":"ip_ip",...}` returns 501 Not Implemented (the type is reserved in the schema but not yet wired) |
| `TestTrunk_TypeUnknown_BadRequest` | `POST /v1/sip/trunks {"type":"bogus"}` returns 400 with an "unknown trunk type" error |
| `TestVSI_Trunk_Lifecycle` | Full trunk lifecycle over the `/v1/vsi` WebSocket: `create_sip_trunk` → `list_sip_trunks` → `get_sip_trunk` → `delete_sip_trunk`; the trunk REGISTERs at the fake registrar, list/get return it without leaking `password`, and a get after delete returns an error frame |
| `TestVSI_Trunk_CreateValidationError` | `create_sip_trunk` with a missing `sip_register` block returns a VSI `error` frame with code 400 (not silently accepted) |
| `TestSIPUpdate_SessionTimerRefresh` | In-dialog UPDATE (RFC 3311) with no body and a `Session-Expires` header is accepted with 200 OK and the `Session-Expires` value is echoed (RFC 4028 §10) — guards against the legacy 405 Method Not Allowed response for session-timer refreshes |
| `TestSIPUpdate_OutOfDialogRejected` | UPDATE that doesn't match any active dialog gets 481 Call/Transaction Does Not Exist (RFC 3311 §5.2), not 405 |
| `TestLiveKit_PublishLegLifecycle` | Gated on `LIVEKIT_TEST_*` env vars. `POST /v1/legs type=livekit_room` creates a `livekit_publish` leg with the correct `livekit_identity` / `livekit_room` headers; `DELETE` tears it down with `leg.disconnected`. |
| `TestLiveKit_RemoteParticipantBecomesLeg` | Gated on `LIVEKIT_TEST_*` env vars. VB joins the LK room via the API; a second LK client (driven by `lkmedia.NewTransport`) joins the same room; VB auto-registers a `livekit_participant` leg with role `livekit_listen`. Disconnecting the second client triggers `leg.disconnected` for the participant leg. |
| `TestLiveKit_BadTokenReturns502` | Gated on `LIVEKIT_TEST_*` env vars. A JWT signed with the wrong secret causes the LK server to reject the JOIN; VB returns 502 with no leg registered. |
| `TestIPAllowlist_DefaultAllowsLoopback` | Default config (no `ALLOWED_IPS`) — loopback request returns 200 (sanity baseline). |
| `TestIPAllowlist_LoopbackAllowed` | `ALLOWED_IPS=127.0.0.1` — loopback request returns 200. |
| `TestIPAllowlist_LoopbackRejected` | `ALLOWED_IPS=10.0.0.0/8` — loopback request returns 403. |
| `TestIPAllowlist_MixedListWithRanges` | `ALLOWED_IPS` mixing IPv4 CIDR, IPv4 literal, and IPv6 CIDR in one value — loopback still matches the literal entry. |
| `TestIPAllowlist_VSIWebSocketRejected` | `ALLOWED_IPS=10.0.0.0/8` — `ws://host/v1/vsi` handshake from loopback fails (allowlist gates WebSocket upgrades). |
| `TestIPAllowlist_TrustProxyHeadersUsesXFF` | `TRUST_PROXY_HEADERS=true` — leftmost `X-Forwarded-For` entry is used for the allowlist match. |
| `TestIPAllowlist_XFFIgnoredWhenTrustProxyOff` | Default `TRUST_PROXY_HEADERS=false` — `X-Forwarded-For` is ignored; only the socket peer is consulted. |
| `TestIPAllowlist_RejectedBody` | Rejected requests return the standard `{"error":"forbidden"}` envelope with status 403. |
| `TestAgentSession_StuckPeerReleasesSession` | A local WS server completes the agent handshake and then stops draining (small receive buffer, so the client's send buffer fills in milliseconds). The write deadline breaks the send loop, whose exit cancels the recv loop, so `Start` returns and `Running()` goes false in well under a second instead of stranding the session — no external provider or API key needed. |
| `TestLeak_VSIZombieConnection` | A VSI client whose TCP has gone half-open (no FIN, no RST, no traffic) is freed by the idle read deadline: `vsiRecvLoop` wakes, unsubscribes from the bus, and every goroutine it pinned drains back to baseline. |
| `TestLeak_VSIStalledReader` | The write-side sibling: the client stays connected and simply stops draining, which the read deadline cannot detect. The write deadline fires, the server hangs up, and the goroutines drain — the read timeout is left at its 60s default so the test cannot pass by the wrong mechanism. |
| `TestTTSPreflight_CommitAvoidsSynthesisLatency` | The feature's whole justification, measured on a real call: a fixture provider with a 700 ms synthesis delay is driven twice, once through `POST /legs/{id}/tts` and once through preflight + commit. Both latencies are logged; the test fails if commit → first audio is no better than paying the synthesis delay. |
| `TestTTSPreflight_DiscardOverVSI` | The `leg_tts_preflight` / `leg_tts_discard` VSI commands over `/v1/vsi`: staging returns `status:"staged"`, `tts.staged` arrives, discarding returns `status:"discarded"` and publishes `tts.discarded{reason:"app"}`, and the discarded utterance never produces `tts.started`. |
| `TestTTSPreflight_ExpiresAfterTTL` | With `TTS_PREFLIGHT_TTL=300ms`, a staged utterance nobody commits is dropped with `reason:"expired"`, and committing the id afterwards is a 404 rather than a silent no-op. |
| `TestTTSPreflight_LegHangupDiscards` | Hanging up mid-stage drops the buffered audio with `reason:"leg_gone"` instead of holding it for the rest of the TTL. |
| `TestDeepgramFlux_TurnLifecycle` | Live `/v2/listen`: real speech (synthesized through Deepgram TTS, so only `DEEPGRAM_API_KEY` is needed) is streamed at wall-clock speed with `eager_eot_threshold` set. Asserts an `end_of_turn` with a non-empty transcript matching the preceding `eager_end_of_turn`, and exactly one final `stt.text` per turn, marked `speech_final`. **Skipped unless `DEEPGRAM_API_KEY` is set.** |
| `TestDeepgramV1_SpeechFinalAndUtteranceEnd` | The v1 fallback mode against the live API: with `utterance_end_ms=1000` and `partial:false`, an `utterance_end` turn event and a `speech_final` transcript both arrive, and **no interim transcripts leak** despite `interim_results` being forced on the wire. **Skipped unless `DEEPGRAM_API_KEY` is set.** |

---

### LiveKit integration tests — env-var setup

The three `TestLiveKit_*` tests above are skipped unless **all** of these env vars are set:

| Variable | Description |
|----------|-------------|
| `LIVEKIT_TEST_URL` | LiveKit server endpoint (e.g. `ws://localhost:7880` or `wss://lk.example.com`) |
| `LIVEKIT_TEST_KEY` | LiveKit API key used to mint test JWTs |
| `LIVEKIT_TEST_SECRET` | LiveKit API secret used to mint test JWTs |

Quick local setup with `livekit-server` in Docker:

```bash
docker run --rm -p 7880:7880 \
  -e LIVEKIT_KEYS="devkey: secret" \
  livekit/livekit-server --dev

export LIVEKIT_TEST_URL=ws://localhost:7880
export LIVEKIT_TEST_KEY=devkey
export LIVEKIT_TEST_SECRET=secret

go test -tags integration -v -timeout 120s -run TestLiveKit_ ./tests/integration/
```

The tests use `lkmedia.NewTransport` directly to act as a second LK client joining the same room — no separate LK client SDK or browser is required.

## AMD Accuracy Tests

The accuracy tests run the AMD analyzer directly against real audio files (no SIP transport) to measure classification accuracy at scale. They require test data to be downloaded or generated first.

### Test data setup

```bash
# Download voicemail greeting recordings (machine-expected)
make download-greetings

# Generate short human greeting WAV files via ElevenLabs TTS (human-expected)
# Requires ELEVENLABS_API_KEY — generates 46 greetings in 11 languages with 3s trailing silence
ELEVENLABS_API_KEY=sk-... make gen-human-greetings
```

Test data is stored in `tests/data/greetings/` and gitignored. Directory structure:

```
tests/data/greetings/
  frankj-dob/       # 7 MP3 voicemail greetings (expected: machine)
  gavvllaw/         # 34 MP3/WAV voicemail greetings (expected: machine)
  chetaniitbhilai/  # 7 WAV voicemail greetings (expected: machine)
  human/            # 46 WAV short human greetings in 11 languages (expected: human)
```

### Run accuracy tests

```bash
# Voicemail greetings — expected: machine (requires make download-greetings)
go test -tags integration -v -run TestAMD_Accuracy ./tests/integration/

# Human greetings — expected: human (requires make gen-human-greetings)
go test -tags integration -v -run TestAMD_FalsePositives ./tests/integration/

# Combined report — both machine and human sources
go test -tags integration -v -run TestAMD_AccuracyAll ./tests/integration/
```

Tests skip automatically if the required test data is not present.

### Accuracy test list

| Test | Description |
|------|-------------|
| `TestAMD_Accuracy` | Voicemail greetings (48 files, 3 sources) — expected `machine` |
| `TestAMD_FalsePositives` | Human greetings (46 files, 11 languages) — expected `human` |
| `TestAMD_AccuracyAll` | Combined report across all sources |

### Example output

```
=== AMD Accuracy Report ===
Total files:  94
Correct:      93
Accuracy:     98.9%

  frankj-dob           7/7 (100%)   [expected: machine]
  gavvllaw             34/34 (100%) [expected: machine]
  chetaniitbhilai      6/7 (86%)    [expected: machine]
  human                46/46 (100%) [expected: human]

Misclassified files:
  chetaniitbhilai/vm1_output.wav  got=no_speech  expected=machine (greeting=0ms silence=2500ms)
```

---

## Voice-agent concurrency benchmark (external load generator)

The scaling test below runs the caller and the system under test in **one Go
test process**, with no STT/TTS in the path — it characterises the mixer, not a
voice-agent deployment. For sustainable-concurrency numbers under a realistic
conversational workload, driven by an external SIP/RTP load generator against
mocked speech and language-model vendors, see **[BENCHMARK.md](BENCHMARK.md)**:
methodology, the success threshold, ladder results and limitations.

That benchmark lives in a fork of the published jambonz harness
(`github.com/jambonz/jambonz-livekit-load-testing-benchmarks`) with a
VoiceBlender controller added. It needs `DEEPGRAM_STT_URL` / `DEEPGRAM_TTS_URL`
pointed at the harness's mock vendor host:

```bash
# in the harness checkout
VB_SRC=../VoiceBlender scripts/local-voiceblender/up.sh   # mocks + VB + controller
go run ./cmd/calibrate                                     # mock latencies honored?
go run ./cmd/driver -scenario scenarios/vb-local-smoke.yaml
scripts/local-voiceblender/ladder.sh runs/vb-ladder-1 "25 50 100 200 300"
scripts/local-voiceblender/down.sh
```

---

## Benchmark / Scaling Test

The scaling benchmark creates many concurrent rooms with 2 SIP legs each and measures:

- **Setup throughput** — rooms/sec and per-room latency percentiles
- **Resource usage** — goroutines and heap allocation at each stage
- **CPU usage** — process CPU consumed during the steady-state sustain phase, normalized as % of one core, % of all cores, and µs of CPU per room-second (linux/darwin only, via `getrusage`)
- **Call quality** — per-leg MOS score, RTP jitter, and packet loss aggregated from `leg.disconnected` events (min/avg/p50/p95 across all legs, both instances)
- **Audio latency** — end-to-end impulse injection through the full SIP + mixer path, located via cross-correlation against the known impulse waveform (immune to codec pre-ringing)
- **Connection health** — verifies all legs remain connected under load
- **Teardown throughput** — cleanup speed

### Run the full benchmark (default scales: 5, 10, 25, 50, 100 rooms)

```bash
go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/
```

### Custom room counts

Use the `-bench-rooms` flag or `BENCH_ROOMS` env var to specify a comma-separated list of room counts:

```bash
# Single custom scale
go test -tags integration -v -timeout 120s -run TestConcurrentRoomsScale ./tests/integration/ -bench-rooms=200

# Multiple custom scales
go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScale ./tests/integration/ -bench-rooms=50,100,200,500

# Via environment variable
BENCH_ROOMS=500 go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScale ./tests/integration/
```

For large room counts, increase the timeout accordingly (~1s per 10 rooms for setup + 3s sustain + latency measurement per scale).

### Run a single scale from the default set

```bash
go test -tags integration -v -timeout 120s -run 'TestConcurrentRoomsScale/rooms_25$' ./tests/integration/
```

### Opus codec variant

`TestConcurrentRoomsScaleOpus` runs the same workload but negotiates the **Opus** codec on every leg (48 kHz native, gopus encode/decode, 6× resampling to/from the 16 kHz mixer). It accepts the same `-bench-rooms` / `BENCH_ROOMS`, `-bench-latency-rooms`, and `-bench-latency-trials` flags. Compare its log output against `TestConcurrentRoomsScale` (PCMU baseline) to characterize codec-path overhead.

```bash
# Default scales with Opus
go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScaleOpus ./tests/integration/

# Custom scales
go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScaleOpus ./tests/integration/ -bench-rooms=50,100,200

# Side-by-side PCMU vs Opus at one scale
BENCH_ROOMS=50 go test -tags integration -v -timeout 300s -run 'TestConcurrentRoomsScale(Opus)?$' ./tests/integration/
```

### Dedicated Opus latency test

`TestOpusAudioLatency` characterizes the Opus codec round-trip with a single 2-leg room and many trials (default 100), reporting min/avg/p50/p90/p95/p99/max/stddev. Runs without the throughput stress of the scale benchmark, giving a clean distribution.

```bash
# Default 100 trials (~30s)
go test -tags integration -v -timeout 120s -run TestOpusAudioLatency ./tests/integration/

# Custom trial count
OPUS_LATENCY_TRIALS=500 go test -tags integration -v -timeout 300s -run TestOpusAudioLatency ./tests/integration/
```

### Audio latency measurement

The benchmark measures leg-to-leg audio latency by:

1. Injecting a 1kHz sine impulse into one leg via `SIPLeg.AudioWriter()` on instance B
2. Detecting it at the other leg's mixer output tap (`SetParticipantOutTap`) on instance A
3. Measuring the time delta

This covers the full path: `B.writeLoop → RTP → A.readLoop → resample → mixer → participantOutTap`.

By default, up to 10 rooms are sampled, 3 trials each, for up to 30 latency measurements per scale.

Use `-bench-latency-rooms` and `-bench-latency-trials` flags (or `BENCH_LATENCY_ROOMS` / `BENCH_LATENCY_TRIALS` env vars) to customize:

```bash
# Sample all 50 rooms, 5 trials each
go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/ -bench-rooms=50 -bench-latency-rooms=50 -bench-latency-trials=5

# Via environment variables
BENCH_LATENCY_ROOMS=20 BENCH_LATENCY_TRIALS=5 go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/
```

### Example output

```
Phase 1 — Setup: 100 rooms in 3.7s (26.9 rooms/sec)
  call+room setup latency: avg=570ms p50=615ms p95=728ms p99=751ms max=751ms (n=100)
  Goroutines: 1914
  Heap alloc: 19.0 MB (delta: 8.0 MB)
Phase 2 — Sustaining 100 rooms for 3s...
  All 200 calls still connected
Phase 3 — Measuring audio latency...
  audio leg-to-leg latency: avg=20ms p50=10ms p95=62ms p99=64ms max=64ms (n=30)
Phase 4 — Teardown: 100 rooms in 5.6ms (17782 rooms/sec)
```

---

## CI / All Tests

To run everything in one command:

```bash
go test ./internal/... && go test -tags integration -v -timeout 120s ./tests/integration/ -count=1
```

Add `-count=1` to disable test caching.

To include the scaling benchmark (adds ~40s):

```bash
go test ./internal/... && go test -tags integration -v -timeout 300s ./tests/integration/ -count=1
```

---

## Troubleshooting

**Tests hang or timeout:** Integration tests use loopback UDP for SIP, except the SIPREC ones, which dial over TCP because a SIPREC INVITE exceeds the 1300-byte limit RFC 3261 §18.1.1 puts on UDP requests. If the system firewall blocks localhost UDP (or TCP for the SIPREC tests), tests will timeout waiting for SIP responses. Increase timeout with `-timeout 120s` on slower machines.

**Port conflicts:** Each test instance picks random free UDP/TCP ports. Conflicts are unlikely but possible under heavy system load. Re-running usually resolves this.

**`no test files` for integration tests:** You forgot `-tags integration`. The test files have a `//go:build integration` constraint.

---

## Manual RTT (Real-Time Text) Interop

Automated coverage exercises VoiceBlender against itself. To verify wire-level interop with a third-party SIP UA, [Linphone](https://www.linphone.org/) is the most reliable open-source RFC 4103 implementation:

1. Start VoiceBlender with `RTT_ENABLED=true` and a webhook receiver listening for `rtt.received`.
2. In Linphone, enable Real-Time Text in account settings, then call into VoiceBlender's SIP port.
3. Answer the call via the REST API: `POST /v1/legs/{id}/answer`.
4. Type in Linphone's RTT pane — observe `rtt.received` events arriving at your webhook with the typed text and a monotonically increasing `seq`.
5. Send text from VoiceBlender:

   ```bash
   curl -X POST http://localhost:8080/v1/legs/<leg_id>/rtt \
     -H 'Content-Type: application/json' \
     -d '{"text":"hello"}'
   ```

   The text appears in Linphone's RTT pane.
6. To verify RFC 2198 redundancy, force packet loss on loopback before step 4:
   ```bash
   sudo tc qdisc add dev lo root netem loss 10%
   # ... run the test ...
   sudo tc qdisc del dev lo root netem
   ```
   Most characters should still arrive; bursts of loss show up as the U+FFFD replacement character with `loss_marker: true` on the event.
