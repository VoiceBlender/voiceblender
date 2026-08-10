# VoiceBlender voice-agent concurrency benchmark

How many concurrent telephony voice-agent sessions does one VoiceBlender
process carry, what does one session cost, and how fast does it answer?

This document is the methodology and the results. It deliberately mirrors the
methodology jambonz published in *"jambonz handles 10× LiveKit's call volume"*
(August 2026) so the numbers can be read against theirs, and it uses that
paper's open harness rather than a VoiceBlender-specific one.

> **Status: local shakedown.** Measured with the load generator, the mock vendor
> host and VoiceBlender all on one 24-core desktop. These validate the pipeline
> and show the shape of the curve; they are not a capacity claim — no knee was
> reached, so they bound VoiceBlender from below. The definitive ladders run on
> AWS c7g with the rig on a separate host (§9).

## Summary

| | |
|---|---|
| Turn latency | **p50 1,477 ms**, p95 1,630 ms — unchanged from N=25 to N=1,200 |
| CPU per concurrent session | **0.0044–0.0047 cores**, flat across a 48× range |
| Sessions per core consumed | **~227** measured, **~100–125** Graviton-equivalent — see §6.0 and §6.3.1 |
| Sustainable concurrent sessions | **≥ 1,200**, rig-valid throughout (§6.4) |
| Capacity knee | **not found** — neither threshold clause approached at any step |
| Memory | ~65 MB base + **0.17 MB/session** (270 MB at 1,200 sessions) |
| Failures | 2 in 7,875 calls, both at one step, not load-correlated |
| VSI events dropped | 0 at every step, despite ~5,000 turn events/second at N=1,200 |
| With preflight | p50 **1,237 ms** (§6.1) |

Of the 1,478 ms turn latency, roughly 1,450 ms is injected mock-vendor delay
plus the model's turn decision; **the media path contributes ~27 ms**. The
remaining reducible term is the LLM token stream (§7).

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
and 900 ms. They are the point of the workload: they are what separates a turn
*model* from a silence timer. A timer must be set beyond 900 ms to ride them
out, and then pays that same delay at the end of every normal turn; a model
treats them as what they are and does not end the turn at all.

## 3. Deployment under test

```
driver (SIP/RTP) ──► VoiceBlender ──► mock STT  (Deepgram Flux, WS /v2/listen)
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
2. `leg.connected` → `leg_stt_start` (provider `deepgram_flux`), then the greeting
3. `stt.turn` `end_of_turn` → call the LLM → play the reply
4. `leg.disconnected` → drop state

Vendor endpoints are redirected to the mock host with `DEEPGRAM_FLUX_URL`,
`DEEPGRAM_STT_URL` and `DEEPGRAM_TTS_URL`. No traffic reaches a real vendor and
no vendor charges are incurred; the vendor names in configuration select a
*protocol dialect*.

Mock latencies, identical to the published paper: STT interim results every
250 ms; Flux `EndOfTurn` 350 ms after end of speech with `EagerEndOfTurn`
200 ms earlier; TTS first audio 150 ms after request; LLM first token 400 ms
then 60 tokens/second.

### 3.1 Turn detection — the controlled variable

Turn detection is the one pipeline component platforms implement differently,
and it is the dominant term in response time, so it is varied deliberately.

| Rung | Mechanism | Boundary signal | Decision latency |
|---|---|---|---|
| **`flux`** (default) | Deepgram Flux `/v2/listen` turn model | `stt.turn` `end_of_turn` | **~350 ms** |
| **`flux` + preflight** | as above, plus speculative generation | `eager_end_of_turn` → stage, `end_of_turn` → commit, `turn_resumed` → discard | ~350 ms, minus a ~200 ms head start |
| `stt` (comparison) | Deepgram v1 `/v1/listen` silence timer | `stt.text` with `speech_final` | `endpointing`, set to **1000 ms** |

**Flux** is the configuration this benchmark reports. The turn boundary is a
model decision, so a mid-utterance hesitation produces `eager_end_of_turn`
followed by `turn_resumed` rather than a premature turn end. This is also the
configuration the published jambonz figures used, which makes the two directly
comparable.

**Preflight** exploits the fact that `eager_end_of_turn` arrives ~200 ms before
`end_of_turn`. The controller generates the reply and stages the audio with
`leg_tts_preflight` on the eager signal, then `leg_tts_commit` on `end_of_turn`
— so committing starts playback with no synthesis on the critical path. A
withdrawn guess is discarded on `turn_resumed`.

**The `stt` rung is kept only as a comparison.** A fixed timer must exceed the
script's longest hesitation (900 ms) or turns 3 and 6 split mid-utterance, and
that second is then added to every normal turn. It is what a deployment without
a turn model is reduced to, and it is included to size that cost.

Two implementation notes that turned out to matter:

- Flux emits eager events **only when `eager_eot_threshold` is set**, matching
  the real API. The controller sends it on the preflight rung only.
- The eager lead (~200 ms) is far shorter than generation (~1 s), so
  `end_of_turn` almost always arrives with the speculation still in flight. The
  controller *waits* for it rather than abandoning it and starting over. An
  earlier version regenerated instead, which committed 1 speculation in 28 and
  made latency worse than not speculating at all.

## 4. Instrumentation

`cmd/hostsampler` samples every 2 s: whole-machine CPU by mode, load average,
memory, network packet rates, and per-process CPU (as a percentage of total
machine capacity), PSS, RSS and fd counts for VoiceBlender, the controller, the
mock host and the driver.

Per step, `/metrics` is captured from both VoiceBlender and the mock host.
Two counters invalidate a run rather than degrade it:

- `voiceblender_vsi_events_dropped_total` — a dropped event is a missed turn.
  `VSI_EVENT_BUFFER_SIZE` is raised to 65536 because one controller receives
  every event from every call. This matters more on Flux than on the silence
  timer: `update` fires ~4×/second per active turn, against ~11 events per
  whole call.
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

## 6. Results

Codec PCMU, mixer 16 kHz, so every session pays 8k↔16k resampling in both
directions.

### 6.0 The host, and what "cores" means here

| | This host | c7g.2xlarge (the paper's instance) |
|---|---|---|
| CPU | AMD Ryzen 9 7900 (Zen 4) | AWS Graviton3 |
| Physical cores | 12 | 8 |
| Threads | 24 (SMT) | 8 — **no SMT**, 1 vCPU = 1 core |
| Clock under load | ~4.94 GHz measured | ~2.6 GHz |
| Placement | driver, mocks, controller and SUT co-resident | SUT alone, rig on its own host |

`hostsampler` reports per-process CPU as a fraction of whole-machine capacity
over 24 logical CPUs, so **every "cores/session" figure below is
logical-CPU-seconds per session-second** — hyperthread-seconds, not
physical-core-seconds. Two consequences.

**SMT.** Linux fills idle physical cores before doubling up on siblings, so
below ~50% box utilisation a logical-CPU-second is effectively a
physical-core-second. The rig-valid range (§6.4) sits entirely in that region:

| N | Box busy | Threads busy | Sibling contention |
|---|---|---|---|
| 25–400 | 8–23% | ~2–6 | none |
| 800 | 31–38% | ~8–9 | none |
| 1000 | 44% | ~11 | starting |
| 1200 | 53% | ~13 | mild |

This also explains a drift previously attributed to concurrency: on the
silence-timer rung cores/session rose 0.0050 → 0.0056 exactly as the box crossed
into sibling territory. The per-session *work* is likely flat; the unit
degraded.

**Clock.** This is the larger distortion by far, and it does not favour
VoiceBlender's numbers. Against Graviton3 there are two independent gaps —
nearly 2× the clock, plus Zen 4's higher IPC than Neoverse V1 — call it
**1.8–2.2× per core**, an estimate rather than a measurement. Converting the
Flux rung's measured 0.0044:

| Basis | cores/session | sessions per consumed core |
|---|---|---|
| Measured (Zen 4 @ 4.9 GHz, hyperthread-seconds) | 0.0044 | 227 |
| Estimated Graviton3-equivalent | ~0.008–0.010 | ~100–125 |
| Estimated x86 EC2 vCPU-equivalent (also a hyperthread, ~3.2 GHz) | ~0.006–0.007 | ~145–165 |

**Read the ratio to other platforms on the Graviton row, not the measured one.**
Against jambonz's 0.014 that is roughly **1.4–1.75× cheaper per session**, not
the 3× the raw figure implies. The comparison is otherwise unusually clean now:
jambonz's 250-session result also used Flux off-box, so both platforms do the
same work per session, differing only in where the LLM call is made — worth
0.061 cores across 800 sessions here, i.e. nothing.

The AWS run (§9) replaces all of this estimation with measurement.

### 6.1 Turn latency

| Configuration | N | Calls | p50 | p95 |
|---|---|---|---|---|
| **`flux`** | 25 | 75 | **1478 ms** | 1630 ms |
| **`flux` + preflight** | 2 | 2 | **1237 ms** | 1391 ms |
| `stt` (comparison) | 25 | 200 | 2077 ms | 2230 ms |

The per-turn budget decomposes cleanly against the injected latencies:

| Component | `flux` | + preflight | `stt` |
|---|---|---|---|
| Turn decision | 350 | 150 (eager) | 1000 |
| LLM first token | 400 | 400 | 400 |
| LLM token stream (~30 tokens @ 60/s) | ~500 | ~500 | ~500 |
| TTS first audio | 150 | 0 (pre-staged) | 150 |
| VoiceBlender media path + driver floor | ~27 | ~27 | ~27 |
| **Total** | **~1427** | **~1077** | **~2077** |
| **Measured** | **1478** | **1237** | **2077** |

Nothing unaccounted for sits in the media path.

For reference, the published jambonz median on its Flux configuration was
**1460 ms**. VoiceBlender's `flux` rung lands at 1478 — within noise of it. Once
turn detection is held constant the two platforms answer at the same speed, and
preflight takes VoiceBlender below that.

Preflight is not free: it generated **28 replies for 16 turns** (1.75× the LLM
calls), of which 14 were committed and 12 discarded on withdrawn guesses, with
0 regenerated. Against mocked vendors that costs only CPU; against real ones it
is 1.75× the token spend, which belongs in any cost model built on these
figures.

### 6.2 Capacity ladder

Three waves of calls per step (`calls = 3N`), 10 s ramp, ladder run to the
harness's 25%-failure safety stop. Unloaded baseline p95 = 1,630 ms, so the
+25% clause puts the latency ceiling at **2,038 ms**.

| N | Completed | Failed | Fail % | p50 ms | p95 ms | p95 vs baseline | Verdict |
|---|---|---|---|---|---|---|---|
| 25 | 75 | 0 | 0.00 | 1478 | 1630 | — | pass |
| 200 | 600 | 0 | 0.00 | 1476 | 1629 | −0.0% | pass |
| 400 | 1198 | 2 | 0.17 | 1458 | 1611 | −1.2% | pass |
| 800 | 2400 | 0 | 0.00 | 1477 | 1611 | −1.2% | pass |
| 1200 | 3600 | 0 | 0.00 | 1477 | 1630 | −0.0% | pass |

**No knee was found.** p95 does not rise anywhere in the ladder — it dips
slightly mid-range and returns to baseline at the top. Neither clause of the
threshold is approached at any tested step, and unlike the earlier silence-timer
ladders the rig stayed inside its own validity bar the whole way (§6.4). The
ladder ended because the *host* ran out of capacity to generate load, not
because VoiceBlender degraded, so **≥ 1,200 is a floor, not a ceiling**.

Two calls failed out of 7,875, both at N=400 (0.17%, an order of magnitude under
the 0.5% clause) and none at the two larger steps — so not load-correlated. The
same signature appeared on the silence-timer ladders: `caller_cancel` after an
`ACK missed` warning, a lost loopback UDP packet on the co-resident rig.

**The event plane held.** This was the main open risk for Flux: `update` fires
roughly four times a second per active turn, so N=1,200 puts on the order of
5,000 events/second through a single controller WebSocket, against ~11 events
per *whole call* on the silence-timer rung. `voiceblender_vsi_events_dropped_total`
was **0 at every step**, with `VSI_EVENT_BUFFER_SIZE=65536`.

### 6.3 Cost per session

Per-process CPU as cores, sampled every 2 s over each step's steady state
(first 60 s skipped). Read §6.0 first on what the unit is.

| N | VB cores | **VB cores/session** | VB PSS MB | Controller cores | Mock cores | Driver cores | Box busy % |
|---|---|---|---|---|---|---|---|
| 25 | 0.110 | **0.0044** | 69 | 0.000 | 0.023 | 0.065 | 9.4 |
| 200 | 0.872 | **0.0044** | 105 | 0.018 | 0.152 | 0.377 | 13.5 |
| 400 | 1.896 | **0.0047** | 141 | 0.036 | 0.328 | 0.809 | 20.7 |
| 800 | 3.515 | **0.0044** | 202 | 0.061 | 0.640 | 1.495 | 31.2 |
| 1200 | 5.366 | **0.0045** | 270 | 0.096 | 0.973 | 2.135 | 41.3 |

**One session costs 0.0044–0.0047 CPU cores, with no trend across a 48× range
of concurrency.** Cost scales with load; it does not compound. This is flatter
than the silence-timer rung managed (0.0049 → 0.0056 over the same span), and
§6.0 explains why: that rung pushed the box past 50% and into sibling threads,
while Flux never exceeded 41%.

Flux is also **~12% cheaper per session** than the silence timer at matched
concurrency. It sends 80 ms frames rather than 20 ms — four times fewer
WebSocket writes for the same audio — which more than pays for the extra turn
events even at 1,200 sessions. The mock host shows the same effect more
strongly (0.973 against 2.350 cores at N=1,200).

Memory is a ~65 MB base plus roughly **0.17 MB per concurrent session** — 270 MB
of PSS at 1,200 sessions, with no swapping.

The controller tier is cheap (0.096 cores at N=1,200) because it does one HTTP
request per turn and touches no audio. Worth stating explicitly: the published
paper found its own customer-code tier became the ceiling before the platform
did, twice. Ours did not.

### 6.3.1 Sessions per vCPU — which denominator

"Sessions per vCPU" names two different quantities, and the difference is large
enough that mixing them produces nonsense.

**Sessions per core *consumed*** is the inverse of the per-session cost above: a
property of the code, measurable at any load, and what this run establishes.

| | cores/session | sessions per consumed core |
|---|---|---|
| VoiceBlender, Flux (N=25) | 0.0044 | **227** |
| VoiceBlender, silence timer (fit over the ladder) | 0.0056 | 180 |
| jambonz media process | 0.014 | 71 |
| LiveKit agent process | 0.110 | 9 |

Per step, VoiceBlender's silence-timer figure ranges 172–206 with no trend
against concurrency. The jambonz and LiveKit rows are the mechanism figures from
the published paper, inverted onto the same basis.

**Sessions per vCPU *allocated*** is capacity at the knee divided by the vCPUs
the deployment was given — the paper's headline 31.3 for jambonz and 3.1 for
LiveKit. **This run cannot state it**, because no step reached a knee: there is
no numerator.

The two are not interchangeable. In the paper's own data jambonz consumed 0.014
cores/session (71 per consumed core) yet reported 31.3 per allocated vCPU, a
factor of ~2.3, because its single node hit its limit at roughly 52% mean box
CPU — it ran out of headroom to inter-tier contention rather than running out of
cores. Allocated vCPU is what you pay for; consumed cores is what the code
costs, and the gap between them is a property of the deployment, not the
software.

Applying that same ~52% packing fraction to the Graviton-equivalent figure in
§6.0 would suggest something near 50–65 sessions per allocated vCPU. That is a projection stacked on an assumption
borrowed from a different platform, recorded here only to show the size of the
gap — it is not a result and should not be quoted as one.

Do not read the measured row as a platform ranking: it is Zen 4 at 4.9 GHz
against Graviton3 at 2.6 GHz, and §6.0 gives the normalised figures to compare
on. What the numbers do establish is the *regime* —
VoiceBlender's per-session cost is that of a shared media plane, flat in
concurrency, not that of a process per call.

### 6.4 Where the local rig stops being valid

The published paper's validity bar is that the load generator and mock host
never exceed roughly 14% CPU, so the measured ceiling belongs to the system
under test. Here they are co-resident with it, so the same fraction is computed
against the whole box:

| N | Rig (driver + mocks) as % of box |
|---|---|
| 25 | 0.4 |
| 200 | 2.2 |
| 400 | 4.7 |
| 800 | 8.9 |
| 1200 | **12.9** |

**The Flux ladder stays inside the bar for its whole length**, ending at 12.9%
at N=1,200 — so ≥ 1,200 sessions is supportable under the paper's own criterion,
with no caveat. (The silence-timer ladders crossed it between 800 and 1,000;
Flux's cheaper mock host is most of the difference.)

Note what this section is *not* saying. The bar is a caution about attribution,
and every mechanism it could plausibly hide points the wrong way: a rig stealing
CPU from the system under test would inflate latency and failures, and neither
moved.

### 6.5 Work actually performed

Flat latency is not the pipeline quietly skipping work. Across the Flux ladder:
7,875 calls carrying **69,345 turns**, with `tts_unknown_text_total` **0** and
`voiceblender_vsi_events_dropped_total` **0** at every step.

`tts_unknown_text_total` is the strongest single check in the run: the mock
serves a filler clip and increments that counter for any text it cannot resolve
to a scripted fixture, so zero means every synthesized reply was the right words
in the right order, at every concurrency.

The turn state machine is visible in the mock's counters. For two calls with
preflight: 16 `StartOfTurn`, 30 `EagerEndOfTurn`, 14 `TurnResumed`,
16 `EndOfTurn` — two eager guesses per turn, one withdrawn at each scripted
hesitation.

Turns run to ~8.8 per call rather than 8. Flux detects the caller's final
utterance more reliably than a silence timer does, so it produces a reply the
caller has already hung up on; that reply's `leg_tts` then fails with 404. The
controller counted 6,361 such errors across 7,875 calls (~0.8/call), consistent
with one per call at hangup — but the flux rung's controller log was truncated
when the stack restarted for the next rung, so that attribution is inferred from
the rate rather than read from the log. `up.sh` now appends rather than
truncates, so the next run can confirm it directly.

## 7. Limitations

Stated up front because they bound what these numbers mean.

1. **The LLM call is made by the controller, not the platform.** jambonz's
   `agent` verb makes it inside feature-server. This moves one HTTP request per
   turn (no audio) out of the system under test and into the app tier. The app
   tier's own CPU is sampled and reported for exactly this reason.
2. **No incremental LLM → TTS handoff.** `leg_tts` takes complete text, so
   synthesis cannot start until the LLM has finished. Preflight moves synthesis
   off the critical path but does not make it incremental; the ~500 ms token
   stream is still paid in full.
3. **Turn-detection accuracy is not measured.** The rungs in §3.1 are compared
   on latency and cost only. A silence timer and a turn model do different work,
   and a scripted hesitation is a gentler test than real speech: callers trail
   off mid-thought and end sentences ambiguously. Nothing here measures false
   interruptions, so none of these figures argue that one mechanism is better at
   turn-taking — only that they cost different amounts of time.
4. **Resampling is in the path.** PCMU at 8 kHz against a 16 kHz mixer and
   16 kHz STT/TTS; a real cost, reported rather than tuned away.
5. **Mocked vendors.** Absolute latencies are not representative of production;
   only differences between configurations are meaningful.
6. **Co-resident rig (local runs only).** The driver and mocks contend with the
   system under test on one box, so local ladders characterise the pipeline,
   not the hardware ceiling (§6.4).
7. **One workload.** Single-caller inbound telephony, one codec, one script.
   Nothing here speaks to rooms, bridging, recording, video, outbound campaigns,
   or WebSocket/WebRTC legs.

## 8. Reproducing

The harness is a fork of
`github.com/jambonz/jambonz-livekit-load-testing-benchmarks` with
`apps/voiceblender/`, `scenarios/vb-*.yaml` and
`scripts/local-voiceblender/` added.

```bash
# bring up mocks + VoiceBlender + controller.
# TURN_DETECTION=flux|stt selects the rung; TTS_PREFLIGHT=true adds
# speculative generation (flux only).
VB_SRC=../VoiceBlender TURN_DETECTION=flux scripts/local-voiceblender/up.sh

# validate the chain
go run ./cmd/calibrate
go run ./cmd/echoresponder -port 5070 &
go run ./cmd/driver -scenario scenarios/floor-echo.yaml

# smoke, then ladder
go run ./cmd/driver -scenario scenarios/vb-local-smoke.yaml
scripts/local-voiceblender/ladder.sh runs/vb-ladder-1 "25 200 400 800 1200"
python3 scripts/local-voiceblender/report.py runs/vb-ladder-1

# or ladder several rungs back to back, restarting the stack between them
scripts/local-voiceblender/ladder-rungs.sh "25 200 400 800 1200" \
  flux flux-preflight stt

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
2. **Find the knee.** No tested step degraded, so the ceiling is unknown. At
   ~0.005 cores/session it lies far above what one desktop can generate load
   for; a dedicated rig host is the prerequisite for looking.
3. **Incremental LLM → TTS.** The last large reducible latency term (§7, item 2).
