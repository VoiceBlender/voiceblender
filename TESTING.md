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
| `internal/mixer` | 9 | Audio mixing, resampling (8kHz/16kHz), playback sources, tap recording |
| `internal/speaking` | 7 | Voice activity detection, debouncing, mute handling, 8kHz/16kHz sample rates |
| `internal/codec` | 9 | G.722 encode/decode, silence/tone round-trips, up/downsample |
| `internal/playback` | 22 | WAV/MP3 parsing, format detection, streaming, resampling, repeat, cancellation |
| `internal/storage` | 3 | FileBackend (no-op), S3Backend upload (with httptest fake), error handling |
| `internal/comfortnoise` | 5 | Comfort noise generation, amplitude clamping, mix-in |

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
```

### Integration test list

| Test | Description |
|------|-------------|
| `TestOutboundInbound_Connect` | Basic SIP call: A dials B, B answers, both connect |
| `TestDisconnect_DurationFields` | Verify `duration_total` and `duration_answered` in disconnect event |
| `TestDisconnect_UnansweredDuration` | Unanswered call has `duration_answered=0` |
| `TestOutboundInbound_CallerCancel` | Caller cancels before answer |
| `TestOutboundInbound_RoomBridge` | Create room, add/remove leg, verify events |
| `TestOutboundInbound_RingTimeout` | Ring timeout expires, call fails |
| `TestRecording_StandaloneSIPLeg` | Stereo recording of standalone SIP leg (left=in, right=out) |
| `TestRecording_InRoomLeg` | Stereo recording of leg in a room (left=participant, right=mix) |
| `TestRecording_Room` | Mono room mix recording |
| `TestRecording_StopsOnDisconnect` | Recording auto-stops when leg hangs up |
| `TestRecording_RoomNoParticipants` | Recording empty room returns 409 |
| `TestRecording_StopWithNoRecording` | Stop without active recording returns 404 |
| `TestRecording_StorageFileExplicit` | Explicit `storage=file` works |
| `TestRecording_StorageS3NotConfigured` | `storage=s3` without config returns 400 |
| `TestMute_LegInRoom` | Mute/unmute in room, verify mix excludes muted audio |
| `TestMute_SpeakingEventsSuppressed` | No speaking events for muted legs |
| `TestMute_BeforeRoomJoin` | Mute before joining room, verify it persists |
| `TestAMD_Human` | AMD classifies short tone burst as `human` |
| `TestAMD_Machine` | AMD classifies continuous tone as `machine` |
| `TestAMD_NoSpeech` | AMD returns `no_speech` when no audio is played |
| `TestAMD_Disabled` | No AMD event when `amd` field is omitted |
| `TestAMD_InvalidParams` | Invalid AMD parameters are rejected with 400 |
| `TestAMD_DefaultParams` | Empty `"amd": {}` uses all defaults |

---

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
- **Audio latency** — end-to-end impulse injection through the full SIP + mixer path
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
