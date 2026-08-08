# VoiceBlender voice-agent concurrency benchmark

How many concurrent telephony voice-agent sessions does one VoiceBlender
process carry, and what does one session cost?

This document is the methodology and the results. It deliberately mirrors the
methodology jambonz published in *"jambonz handles 10× LiveKit's call volume"*
(August 2026) so the numbers can be read against theirs, and it uses that
paper's open harness rather than a VoiceBlender-specific one.

> **Status: local shakedown.** The numbers below were measured with the load
> generator, the mock vendor host and VoiceBlender all on one 24-core desktop.
> They validate the pipeline and show the shape of the curve; they are not a
> capacity claim — no knee was reached, so they bound VoiceBlender from below.
> The definitive ladders run on AWS c7g with the rig on a separate host (§8).

## Summary

| | |
|---|---|
| Sustainable concurrent sessions | **≥ 800** (rig-valid); ≥ 1,200 observed clean (§6.4) |
| Capacity knee | **not found** — p95 flat across a 48× concurrency range |
| CPU per concurrent session | **0.0049–0.0058 cores**, near-constant with load |
| Memory | ~65 MB base + **0.16 MB/session** (257 MB at 1,200 sessions) |
| Turn latency | p50 2,077 ms, p95 2,210 ms — unchanged from N=25 to N=1,200 |
| Failures | 3 in 14,025 calls, all lost-loopback-ACK, not load-correlated |
| RTP | 0 packets lost of 17.8M at N=1,000 |

Roughly 2,000 ms of that 2,077 ms turn latency is injected mock-vendor delay
plus VoiceBlender's silence-timer turn detection (§3.1); the media path
contributes ~27 ms. The two largest reducible terms are product gaps, not
benchmark artifacts — see §7, items 2 and 3.

## 1. What is measured

**Sustainable concurrent sessions**, against an explicit threshold:

> The highest tested concurrency at which fewer than **0.5% of calls fail**
> **and** p95 turn latency stays within **25% of VoiceBlender's own unloaded
> baseline**.

A *session* is one inbound SIP call carrying bidirectional RTP through a
complete eight-turn conversation. It counts only if setup succeeds and every
expected agent reply arrives within its timeout.

Alongside capacity: turn latency distribution, CPU cores consumed per
concurrent session, and memory. The shape of the overload region is also of
interest — whether a saturated deployment rejects calls or absorbs the load as
rising latency — but no tested step reached it, so this run has nothing to say
about it.

## 2. Workload

The harness's scripted conversation, unmodified: an eight-turn airline
flight-change dialogue of roughly three minutes, spoken as G.711 over real SIP
and RTP from pre-rendered fixtures, so every call on every run transmits
byte-identical audio.

Two caller utterances contain mid-utterance hesitation pauses of 600, 700, 800
and 900 ms. They are the point of the workload: a fixed silence timer must
exceed 900 ms to ride out all four, and doing so adds that same delay to the end
of every *normal* turn. There is no timeout value that is both patient enough
for turns 3 and 6 and fast enough for the other six.

## 3. Deployment under test

```
driver (SIP/RTP) ──► VoiceBlender ──► mock STT  (Deepgram dialect, WS)
                          │      ──► mock TTS  (Deepgram dialect, REST)
                          │
                     /v1/vsi
                          │
                   benchmark controller ──► mock LLM (OpenAI-compatible)
```

VoiceBlender does not orchestrate STT → LLM → TTS. Its `agent` verb bridges leg
audio to an external agent product; the STT, TTS and event/command primitives
are separate. The comparable pipeline is therefore assembled from those
primitives by a controller (`apps/voiceblender/` in the harness fork) holding a
single `/v1/vsi` WebSocket:

1. `leg.ringing` → `answer_leg`
2. `leg.connected` → `leg_stt_start` (provider `deepgram`), then the greeting
3. `stt.text` with `is_final` → call the LLM → `leg_tts` (provider `deepgram`)
4. `leg.disconnected` → drop state

Vendor endpoints are redirected to the mock host with `DEEPGRAM_STT_URL` and
`DEEPGRAM_TTS_URL`. No traffic reaches a real vendor and no vendor charges are
incurred; the vendor names in configuration select a *protocol dialect*.

Mock latencies, identical to the published paper: STT interim results every
250 ms; TTS first audio 150 ms after request; LLM first token 400 ms then 60
tokens/second.

### 3.1 Turn detection

VoiceBlender has no model-based end-of-turn detector, and
`internal/stt/deepgram.go` acts on Deepgram's `is_final` (it parses
`speech_final` but does not use it). Turn-taking therefore rests entirely on the
STT silence timer, which is set to **1000 ms** so it exceeds the script's 900 ms
hesitation. Every turn in every reported run came back `early: false`, so the
pauses are ridden out correctly — at a cost of roughly one second on every
normal turn.

This is the single largest term in VoiceBlender's turn latency and the main
difference from the published jambonz figures, which used Deepgram Flux's
model-based `EndOfTurn` (350 ms). It is a turn-detection difference, not a
media-path one.

## 4. Instrumentation

`cmd/hostsampler` samples every 2 s: whole-machine CPU by mode, load average,
memory, network packet rates, and per-process CPU (as a percentage of total
machine capacity), PSS, RSS and fd counts for VoiceBlender, the controller, the
mock host and the driver.

Per step, `/metrics` is captured from both VoiceBlender and the mock host.
Two counters invalidate a run rather than degrade it:

- `voiceblender_vsi_events_dropped_total` — a dropped event is a missed turn.
  `VSI_EVENT_BUFFER_SIZE` is raised to 65536 because one controller receives
  every event from every call.
- `tts_unknown_text_total` on the mock — nonzero means a reply did not match
  any scripted text, so the conversation diverged.

## 5. Validation of the measurement chain

Run before any capacity number is believed.

| Check | Expected | Measured |
|---|---|---|
| `cmd/calibrate` — LLM TTFT | 400 ms | 400.8 ms |
| `cmd/calibrate` — TTS TTFB | 150 ms | 150.4 ms |
| `cmd/calibrate` — STT final vs expected | 0 ms | −19 ms |
| `floor-echo` scenario, N=10 | 1400 ms (echo endpointing + reply delay) | p50 1404 ms, 20/20 completed |

The floor-echo run drives a SIP echo responder with programmed reply timing, so
it establishes the driver + network measurement floor independent of
VoiceBlender: 4 ms.

## 6. Results — local shakedown

Host: 24-core x86 desktop, everything co-resident (driver, mocks, controller
and VoiceBlender on one box). Codec PCMU, mixer 16 kHz, so every session pays
8k↔16k resampling in both directions.

### 6.1 Unloaded baseline

| N | Completed | Failed | p50 | p95 |
|---|---|---|---|---|
| 2 | 2 | 0 | 2076 ms | 2210 ms |
| 25 | 200 | 0 | 2077 ms | 2230 ms |

Latency is flat between N=2 and N=25, so **p95 = 2230 ms** is the unloaded
baseline. The 25% clause puts the latency ceiling at **2788 ms**.

The per-turn budget decomposes cleanly against the injected latencies:

| Component | ms |
|---|---|
| STT silence timer (§3.1) | 1000 |
| LLM first token | 400 |
| LLM token stream (~30 tokens @ 60/s) | ~500 |
| TTS first audio | 150 |
| VoiceBlender media path + driver floor | ~27 |
| **Total** | **~2077** |

Nothing unaccounted for sits in the media path.

### 6.2 Capacity ladder

Three waves of calls per step (`calls = 3N`), 10 s ramp, ladder run to the
harness's 25%-failure safety stop.

| N | Completed | Failed | Fail % | p50 ms | p95 ms | p95 vs baseline | Verdict |
|---|---|---|---|---|---|---|---|
| 25 | 75 | 0 | 0.00 | 2077 | 2230 | — | pass |
| 50 | 150 | 0 | 0.00 | 2077 | 2229 | −0.0% | pass |
| 100 | 300 | 0 | 0.00 | 2077 | 2211 | −0.9% | pass |
| 200 | 600 | 0 | 0.00 | 2077 | 2210 | −0.9% | pass |
| 300 | 899 | 1 | 0.11 | 2077 | 2210 | −0.9% | pass |
| 400 | 1200 | 0 | 0.00 | 2077 | 2210 | −0.9% | pass |
| 600 | 1800 | 0 | 0.00 | 2077 | 2210 | −0.9% | pass |
| 800 | 2400 | 0 | 0.00 | 2077 | 2210 | −0.9% | pass |
| 1000 | 2999 | 1 | 0.03 | 2077 | 2210 | −0.9% | pass¹ |
| 1200 | 3599 | 1 | 0.03 | 2077 | 2211 | −0.9% | pass¹ |

¹ passes the quality threshold, but the co-resident rig exceeds its own
validity bar at these steps — see §6.4.

**No knee was found.** p95 does not rise anywhere in the ladder; it falls
slightly from the N=25 baseline and then sits flat across a 48× concurrency
range, from 25 to 1,200 concurrent sessions. Neither clause of the threshold is
approached at any tested step. The ladder ended because the *host* ran out of
capacity to generate load, not because VoiceBlender degraded — so every figure
here is a floor, not a measured ceiling.

Three calls failed out of 14,025. All three were `caller_cancel` following an
`ACK missed` warning in VoiceBlender: a lost loopback UDP packet, at N=300,
N=1000 and N=1200. The rate does not scale with concurrency, so it reads as rig
noise rather than a load-dependent failure mode.

### 6.3 Cost per session

Per-process CPU as cores, sampled every 2 s over each step's steady state
(first 60 s skipped):

| N | VB cores | **VB cores/session** | VB PSS MB | Controller cores | Mock cores | Driver cores | Box busy % |
|---|---|---|---|---|---|---|---|
| 25 | 0.125 | 0.0050 | 69 | 0.000 | 0.046 | 0.062 | 7.6 |
| 50 | 0.243 | 0.0049 | 75 | 0.000 | 0.087 | 0.099 | 8.2 |
| 100 | 0.497 | 0.0050 | 85 | 0.002 | 0.173 | 0.191 | 9.9 |
| 200 | 1.089 | 0.0054 | 103 | 0.016 | 0.397 | 0.390 | 14.5 |
| 300 | 1.741 | 0.0058 | 120 | 0.022 | 0.653 | 0.614 | 18.3 |
| 400 | 2.296 | 0.0057 | 136 | 0.031 | 0.899 | 0.827 | 23.2 |
| 600 | 3.224 | 0.0054 | 163 | 0.039 | 1.233 | 1.196 | 30.4 |
| 800 | 4.254 | 0.0053 | 194 | 0.053 | 1.531 | 1.535 | 37.6 |
| 1000 | 5.525 | 0.0055 | 225 | 0.070 | 1.923 | 1.930 | 44.3 |
| 1200 | 6.769 | 0.0056 | 257 | 0.085 | 2.350 | 2.305 | 52.9 |

**One session costs 0.0049–0.0058 CPU cores**, near-constant across a 48× range
of concurrency. Cost scales with load; it does not compound.

Memory is a ~65 MB base plus roughly **0.16 MB per concurrent session** — 257 MB
of PSS at 1,200 sessions, with no swapping.

The controller tier is cheap here (0.085 cores at N=1,200) because it does one
HTTP request per turn and touches no audio. That is worth stating explicitly:
the published paper found its own customer-code tier became the ceiling before
the platform did, twice. Ours did not.

For orientation, the published paper measured 0.014 cores/session for jambonz's
shared media process and 0.110 for LiveKit's per-call agent subprocess. Do not
read the ratio as a platform comparison: this is x86 desktop silicon rather than
Graviton, and this session genuinely does less work (§7, items 1–3). What the figure
does establish is the *regime* — VoiceBlender's cost is that of a shared media
plane, flat in concurrency, not that of a process per call.

### 6.4 Where the local rig stops being valid

The published paper's validity bar is that the load generator and mock host
never exceed roughly 14% CPU, so the measured ceiling belongs to the system
under test. Here they are co-resident with it, so the same fraction is
computed against the whole box:

| N | Rig (driver + mocks) as % of box |
|---|---|
| 400 | 7.2 |
| 600 | 10.1 |
| 800 | **12.8** |
| 1000 | 16.1 |
| 1200 | 19.4 |

The rig crosses the bar between 800 and 1000. So:

- **≥ 800 sessions** is supportable under the paper's own rig-validity criterion.
- **≥ 1,200 sessions** was observed with no measurable degradation, but at those
  steps the rig is doing more work than the criterion allows, and the numbers
  are reported with that caveat rather than as clean results.

The driver held true concurrency at every step: each step ran a constant 370 s,
and RTP packets received scaled exactly with N (10.7M / 14.3M / 17.8M at
600 / 800 / 1000) with **zero packet loss** across all of it.

Note what this section is *not* saying. The rig bar is a caution about
attribution, and every mechanism it could plausibly hide points the wrong way:
a rig stealing CPU from the system under test would inflate latency and
failures, and neither moved. The honest reading is that 800 is the number this
methodology fully supports, and that the true ceiling is well above it.

### 6.5 Work actually performed

Counters reconcile exactly, so the flat latency is not the pipeline quietly
skipping work:

Cumulative over every step of both ladders:

| Counter | Value |
|---|---|
| Calls connected (VoiceBlender) | 14,022 |
| STT sessions opened (mock) | 14,022 |
| Final transcripts (mock) | 112,176 |
| LLM requests (mock) | 112,178 |
| Controller turns | 112,178 |
| `tts_unknown_text_total` | 0 |
| `voiceblender_vsi_events_dropped_total` | 0 |

14,022 calls × 8 = 112,176, and one STT session per call. The two extra LLM
requests are greetings for calls that connected and then dropped — the two
`caller_cancel` failures that got as far as answering.

`tts_unknown_text_total` is zero, which is the strongest single check in the
run: the mock serves a filler clip and increments that counter for any text it
cannot resolve to a scripted fixture, so zero means all 112,178 synthesized
replies were the right words in the right order. No conversation diverged, and
no turn was skipped, at any concurrency.

## 7. Limitations

Stated up front because they bound what these numbers mean.

1. **The LLM call is made by the controller, not the platform.** jambonz's
   `agent` verb makes it inside feature-server. This moves one HTTP request per
   turn (no audio) out of the system under test and into the app tier. The app
   tier's own CPU is sampled and reported for exactly this reason — the
   published paper found its Node app became the ceiling before jambonz did.
2. **No incremental LLM → TTS handoff.** `leg_tts` takes complete text, so
   synthesis cannot start until the LLM has finished. Platforms that stream the
   LLM into TTS save most of the token-stream time.
3. **Turn detection is a silence timer** (§3.1), which adds ~1 s to every turn
   and is not comparable to a model-based detector on either latency or
   interruption accuracy.
4. **Resampling is in the path.** PCMU at 8 kHz against a 16 kHz mixer and
   16 kHz STT/TTS; a real cost, reported rather than tuned away.
5. **Mocked vendors.** Absolute latencies are not representative of production;
   only differences between configurations are meaningful.
6. **Co-resident rig (local runs only).** The driver and mocks contend with the
   system under test on one box, so local ladders characterise the pipeline,
   not the hardware ceiling. The published paper's validity bar is rig
   utilisation under ~14%; AWS runs are laid out to meet it.
7. **One workload.** Single-caller inbound telephony, one codec, one script.
   Nothing here speaks to rooms, bridging, recording, video, outbound campaigns,
   or WebSocket/WebRTC legs.

## 8. Reproducing

The harness is a fork of
`github.com/jambonz/jambonz-livekit-load-testing-benchmarks` with
`apps/voiceblender/`, `scenarios/vb-*.yaml` and
`scripts/local-voiceblender/` added.

```bash
# bring up mocks + VoiceBlender + controller
VB_SRC=../VoiceBlender scripts/local-voiceblender/up.sh

# validate the chain
go run ./cmd/calibrate
go run ./cmd/echoresponder -port 5070 &
go run ./cmd/driver -scenario scenarios/floor-echo.yaml

# smoke, then ladder
go run ./cmd/driver -scenario scenarios/vb-local-smoke.yaml
scripts/local-voiceblender/ladder.sh runs/vb-ladder-1 "25 50 100 200 300"
python3 scripts/local-voiceblender/report.py runs/vb-ladder-1

scripts/local-voiceblender/down.sh
```

`report.py` derives each step's steady-state window from the driver summaries
and renders both result tables, including the capacity verdict against the
threshold in §1.

Raw run directories are not committed: a ladder produces tens of MB of
`timeline.jsonl` and per-call `summary.json`, and the harness's own `runs/`
convention is manifests rather than raw data.

## 9. Next

1. **AWS c7g run.** c7g.2xlarge system under test with the rig on its own host,
   which is the only layout where sessions-per-vCPU is comparable to the
   published figures and where the rig-validity bar (§6.4) is met by
   construction. The harness's `terraform/shared/` stack provides the VPC,
   placement group and rig hosts; the platform stack needs a VoiceBlender
   equivalent.
2. **Find the knee.** No tested step degraded, so the ceiling is unknown. On
   c7g.2xlarge the 0.0055 cores/session figure suggests it lies far above what
   one desktop can generate load for; a dedicated rig host is the prerequisite
   for looking.
3. **Turn detection.** Items 2 and 3 of §7 are the whole latency story and both
   are product work: surfacing Deepgram's `speech_final` distinctly from
   `is_final`, adding Flux (`/v2/listen`) support, and an incremental LLM → TTS
   path. Each would be measurable on this harness immediately.
