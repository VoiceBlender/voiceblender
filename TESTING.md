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
```

### Run with verbose output

```bash
go test -v ./internal/...
```

### Run a single test by name

```bash
go test -v -run TestMixer_TapRecording ./internal/mixer/
go test -v -run TestS3Backend_Upload ./internal/storage/
```

### What each package tests

| Package | Tests | Description |
|---------|-------|-------------|
| `internal/amd` | 21 | AMD state machine (human/machine/no\_speech/not\_sure), beep detection (Goertzel), parameter validation |
| `internal/mixer` | 15 | Audio mixing, configurable sample rate (8/16/48 kHz), resampling, playback sources, tap recording, per-listener routing whitelist (full mesh / supervisor-whisper / isolated / mute+deaf interaction) |
| `internal/bridge` | 9 | Duplex conduit: pair wiring, blocking Read, EOF/idempotent Close, drop-oldest backpressure, leftover handling, buffer-copy |
| `internal/room` (bridge) | 12 | Direction mapping/validation, `CreateBridge` matrix (self/missing/rate/duplicate), live direction flip, delete teardown, mixer keepalive with zero legs |
| `internal/room` (routing) | 6 | Role-based routing matrix: supervisor-whisper no-bleed at join, mid-call role change recomputes allow-sets, unroled legs default to full mesh, removing a leg prunes others' whitelists, `UpdateRoutingRow` with null clears row, matrix round-trip via `RoutingMatrix()` |
| `internal/speaking` | 7 | Voice activity detection, debouncing, mute handling, 8kHz/16kHz sample rates |
| `internal/codec` | 17 | G.722 encode/decode, silence/tone round-trips, up/downsample, AMR-WB and AMR-NB type registration + factory + RFC 4867 round-trip (both payload formats) |
| `goamr-wb` (external module) | 98 | AMR-WB (G.722.2) pure-Go DSP: ITU fixed-point basic ops, LPC/ISF, pitch, algebraic codebook, gain quant, synthesis/HF band, RFC 4867 payload (de)pack (octet-aligned + bandwidth-efficient), MIME sort/unsort. Lives in its own published module (`github.com/VoiceBlender/goamr-wb`, pinned in `go.mod`); its tests run from a local clone of that repo. |
| `internal/playback` | 22 | WAV/MP3 parsing, format detection, streaming, resampling, repeat, cancellation |
| `internal/storage` | 3 | FileBackend (no-op), S3Backend upload (with httptest fake), error handling |
| `internal/comfortnoise` | 5 | Comfort noise generation, amplitude clamping, mix-in |
| `internal/jitter` | 10 | Fixed-delay reorder buffer: warm-up, reorder, duplicate drop, late-arrival drop, underrun silence, uint16 wraparound, max-depth eviction, reset |
| `internal/sip` (refer) | 5 | Refer-To parsing (blind / attended with Replaces / no angles), Replaces.String() formatting, sipfrag status-line parsing |
| `internal/sip` (whatsapp) | 12 | `IsWhatsAppInvite` host matching (exact/subdomain/lookalike/case-insensitive), `WhatsAppRecipientURI` E.164 normalisation, `InviteWhatsApp` precondition checks (TLS configured, required fields) |
| `internal/sip` (tls) | 4 | `EngineConfig` TLS validation, concurrent UDP+TLS listener startup, self-signed cert handshake loopback |
| `internal/sip` (inbound auth) | 11 | `recordChallenge` WWW-Authenticate shape + nonce uniqueness; `VerifyInboundAuth` valid digest (MD5/SHA-256, password and HA1), wrong password/username → invalid, no/unknown/expired challenge → none, single-use nonce consumption |
| `internal/api` (inbound auth) | 8 | `HandleRegisterAttempt` always publishes the `sip.registration_attempt` event, timeout-accepts when undecided, propagates a challenge decision; `ChallengeRequest` validation (realm + password/ha1 required) |
| `internal/sip` (dtmf) | 8 | RFC 4733 packet generation (7-packet sequence, marker bit, duration units at 8 kHz vs AMR-WB 16 kHz), `TelephoneEventClockRate` per codec (incl. G.722's 8 kHz RTP clock despite 16 kHz sampling), offer/answer/re-INVITE advertise telephone-event at the codec's clock rate (16 kHz for AMR-WB, 8 kHz for G.722), `ParseSDP`/`DTMFPTForRate` capture the remote telephone-event PT and rate |
| `internal/leg` (pcmedia) | 6 | Codec-driven PeerConnection construction, SampleRate wiring, idempotent `Start`, ICE candidate drain, two-peer ICE+DTLS-SRTP loopback with PCM round-trip |
| `internal/leg` (whatsapp) | 6 | Outbound starts `connected`, inbound starts `ringing`, `RequestAnswer` rejects outbound and is idempotent, `Hangup` is idempotent, `SIPHeaders` propagation, Leg interface compliance |
| `internal/leg` (websocket) | 4 | Outbound lifecycle (ringing → connected via `AttachTransport`, audio + text round-trip, ClaimDisconnect single-flight, Hangup); inbound auto-connect with header capture (X-/P- filter); SendText returns `ErrRTTNotNegotiated` when RTT is disabled; SendDTMF returns "not supported" |
| `internal/wsmedia` | 11 | Framing (binary s16le and json_base64 round-trips), streamBuffer drop-on-overflow + paced read + Close, Transport echo loopback for both wire formats, text round-trip, hangup frame closes peer, context cancel exits loops, ingress overflow drops increment counters, SendStructured for vendor control messages, write deadline trips after `WriteTimeout` |

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
| `TestOutboundInbound_RingTimeout` | Ring timeout expires, call fails |
| `TestRecording_StandaloneSIPLeg` | Stereo recording of standalone SIP leg (left=in, right=out) |
| `TestRecording_InRoomLeg` | Stereo recording of leg in a room (left=participant, right=mix) |
| `TestRecording_Room` | Mono room mix recording |
| `TestRecording_StopsOnDisconnect` | Recording auto-stops when leg hangs up |
| `TestRecording_RoomNoParticipants` | Recording empty room returns 409 |
| `TestRecording_StopWithNoRecording` | Stop without active recording returns 404 |
| `TestRecording_StorageFileExplicit` | Explicit `storage=file` works |
| `TestRecording_StorageS3NotConfigured` | `storage=s3` without config returns 400 |
| `TestRecording_PauseResume_Leg` | Pause/resume endpoints, events, idempotency, 404 after stop |
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
| `TestAMD_InvalidParams` | Invalid AMD parameters are rejected with 400 |
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
| `TestVSI_WebRTC_FullFlow` | VSI `webrtc_offer` / `webrtc_get_candidates` / `webrtc_add_candidate` round-trip with a real pion client; leg appears in `list_legs` and emits `leg.connected` |
| `TestVSI_WebRTC_OfferInvalidSDP` | VSI `webrtc_offer` with malformed SDP returns a 400 error frame |
| `TestVSI_WebRTC_AddCandidateNotFound` | VSI `webrtc_add_candidate` for an unknown leg returns a 404 error frame |
| `TestWSEvents_ConnectedAndEvents` | Connect to `/v1/vsi`, originate a call, verify `leg.ringing` event arrives |
| `TestWSEvents_UnknownCommand` | Send unknown command with `request_id`, verify error response echoes it |
| `TestWSEvents_StopCommand` | Send `stop`, verify server closes the connection |
| `TestWSCommands_RoomLifecycle` | Create, get, list, delete room via WS commands; error on deleted room |
| `TestWSCommands_MuteLeg` | Mute/get_leg via WS; error on missing leg; error on unknown command |
| `TestWSEvents_AppIDFilter` | Two WS clients (filtered + unfiltered), two legs with different `app_id`; filtered client only sees matching events |
| `TestTransfer_Blind_Outbound` | A↔B, REFER on B's leg dials C, completion event fires, original hung up |
| `TestTransfer_Inbound_DeclinedByDefault` | With default `SIP_REFER_AUTO_DIAL=false` the peer's REFER gets 603 and an audit event |
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
| `TestSIPRegister_Basic` | Raw sipgo client REGISTERs; expects 200 OK echoing `Expires`, a `sip.registration_active` event, and the binding visible via `GET /v1/sip/registrations` with the client's actual source socket |
| `TestSIPRegister_Refresh` | Same Contact re-registers from a different ephemeral source port; binding's `Socket` updates in-place; only one binding remains for the (AOR, Contact) pair |
| `TestSIPRegister_MultiContact` | Same AOR registers from two distinct Contacts; both bindings are listed; per-contact unregister and the `SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS=false` displacement path are also exercised |
| `TestSIPRegister_Expiry` | Short-expires REGISTER → TTL sweeper removes the binding and emits `sip.registration_expired` with `reason:ttl` |
| `TestSIPRegister_Unregister` | REGISTER with `Contact: *` and `Expires: 0` removes all bindings with `reason:unregistered` |
| `TestSIPRegister_ForceDelete` | `DELETE /v1/sip/registrations/{aor}` force-unbinds with `reason:forced` |
| `TestSIPRegister_DialAOR` | After REGISTER, `POST /v1/legs {"type":"sip","to":"sip:alice@..."}` routes the outbound INVITE to the bound socket rather than the URI host |
| `TestSIPRegister_Fork` | AOR registered from two raw clients; `POST /v1/legs` parallel-forks (both clients receive an INVITE); the second client answers 200 OK, the first receives CANCEL and its INVITE transaction terminates (after Timer I = 5 s for UDP) |
| `TestSIPInboundAuth_RegisterChallengeSuccess` | Consult enabled (webhook set); REGISTER parks → `sip.registration_attempt` event → `POST /v1/sip/registrations/attempts/{id}/challenge` → `401`; credentialed re-REGISTER (same Call-ID) verifies → `200 OK` and a live binding |
| `TestSIPInboundAuth_RegisterChallengeWrongPassword` | Same challenge flow but the retry signs with a wrong password → `403 Forbidden` and no binding is created |
| `TestSIPInboundAuth_RegisterTimeoutAutoAccepts` | Symmetric with inbound INVITE: the REGISTER is always surfaced (`sip.registration_attempt` fires); with no client decision it auto-accepts with `200 OK` after the consult window and binds |
| `TestSIPInboundAuth_InviteChallengeSuccess` | Inbound INVITE → `POST /v1/legs/{id}/challenge` → `401`; credentialed re-INVITE surfaced as a new `leg.ringing` with `authenticated=true` + `auth_username`; answered → `200 OK` |
| `TestTrunk_SIPRegister_HappyPath` | `POST /v1/sip/trunks {"type":"sip_register",...}` → 202; the fake registrar receives one REGISTER, returns 200 with `Expires=120`; trunk status flips to `active`; `sip.outbound_registration_active` event emitted; no response body leaks `password` |
| `TestTrunk_SIPRegister_DigestAuth` | Fake registrar challenges the initial REGISTER with `401 Unauthorized` + `WWW-Authenticate`; the trunk computes the digest response and the second REGISTER carries an `Authorization` header that the registrar accepts |
| `TestTrunk_SIPRegister_Refresh` | Registrar grants `Expires=2`; the trunk refreshes at `granted * SIP_OUTBOUND_REGISTRATION_REFRESH_RATIO` (default 0.5) so a second REGISTER hits the registrar within ~1 s |
| `TestTrunk_SIPRegister_Unregister` | `DELETE /v1/sip/trunks/{id}` returns 202; a final REGISTER with `Expires: 0` lands at the registrar; `sip.outbound_registration_expired` event with `reason: unregistered`; subsequent `GET /v1/sip/trunks/{id}` returns 404 |
| `TestTrunk_SIPRegister_RefreshFailedEmitsExpired` | First REGISTER succeeds with `Expires=1`, then the registrar rejects every subsequent REGISTER with 503. After the granted lifetime lapses, exactly one `sip.outbound_registration_expired` event with `reason: refresh_failed` is emitted; the trunk keeps retrying in the background |
| `TestTrunk_SIPRegister_OutboundCallUsesTrunk` | After REGISTER, `POST /v1/legs {"type":"sip","to":"sip:bob@<registrar>","from":"alice"}` auto-attaches the trunk's credentials and adds a `Route: <registrar_uri;lr>` header on the outbound INVITE (verified at the fake registrar) |
| `TestTrunk_TypeIPIP_NotImplemented` | `POST /v1/sip/trunks {"type":"ip_ip",...}` returns 501 Not Implemented (the type is reserved in the schema but not yet wired) |
| `TestTrunk_TypeUnknown_BadRequest` | `POST /v1/sip/trunks {"type":"bogus"}` returns 400 with an "unknown trunk type" error |
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

**Tests hang or timeout:** Integration tests use loopback UDP for SIP. If the system firewall blocks localhost UDP, tests will timeout waiting for SIP responses. Increase timeout with `-timeout 120s` on slower machines.

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
