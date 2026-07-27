package amd

import (
	"encoding/binary"
	"testing"
	"time"
)

// frameOf builds one 20 ms PCM frame, voiced (well above speechThreshold) or
// silent.
func frameOf(voiced bool) []byte {
	buf := make([]byte, frameSizeBytes)
	if !voiced {
		return buf
	}
	for i := 0; i < samplesPerFrame; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:i*2+2], uint16(8000))
	}
	return buf
}

// drive runs the real FSM over a frame pattern until it reaches a verdict.
// voiced reports whether frame i (0-based) carries speech. step always
// terminates at TotalAnalysisTime, so this cannot spin forever.
func drive(params Params, voiced func(i int) bool) Detection {
	a := New(params)
	for i := 0; ; i++ {
		if det, done := a.Feed(frameOf(voiced(i))); done {
			return det
		}
	}
}

// The fastest path to each verdict. Pure silence can never enter the greeting
// phase, and pure speech never accrues silence, so neither scenario can reach
// a verdict other than its own; only shortestBurst can be pre-empted (by
// machine, when GreetingDuration is shorter than the burst).
func silenceOnly(int) bool { return false }
func speechOnly(int) bool  { return true }

// shortestBurst speaks the fewest frames that still leave a counted greeting,
// then goes silent — the fastest path to human.
func shortestBurst(params Params) func(i int) bool {
	burst := int(analysisFrames(params.MinimumWordLength) / frameDuration)
	if burst < speechOffFrames {
		burst = speechOffFrames
	}
	// currentSpeech keeps accruing through the off-debounce, so the voiced run
	// only needs to cover what the debounce does not.
	voicedFrames := burst - speechOffFrames + speechOnFrames
	if voicedFrames < speechOnFrames {
		voicedFrames = speechOnFrames
	}
	return func(i int) bool { return i < voicedFrames }
}

// minReachableTotal finds, against the real FSM, the smallest
// TotalAnalysisTime at which the scenario actually yields want. It reports
// false when want is unreachable at every total (another verdict pre-empts it).
func minReachableTotal(base Params, want Result, voiced func(i int) bool) (time.Duration, bool) {
	for total := frameDuration; total <= 15*time.Second; total += frameDuration {
		p := base
		p.TotalAnalysisTime = total
		if drive(p, voiced).Result == want {
			return total, true
		}
	}
	return 0, false
}

// TestValidate_WindowGuardsMatchFSM pins Validate's accept boundary to the FSM
// itself. Validate rejects only a window in which *no* verdict can be reached,
// so that boundary must be exactly the smallest of the verdicts' first-reachable
// totals, as measured by driving the real FSM. Accepting below it lets through a
// config that can only ever return not_sure; rejecting at it would refuse a
// window that genuinely yields a verdict, which would be worse than the bug.
func TestValidate_WindowGuardsMatchFSM(t *testing.T) {
	combos := 0
	for _, initial := range []time.Duration{300, 1000, 2500} {
		for _, greeting := range []time.Duration{200, 900, 1500} {
			for _, after := range []time.Duration{110, 800, 1230} {
				for _, minWord := range []time.Duration{40, 100, 250} {
					p := Params{
						InitialSilenceTimeout: initial * time.Millisecond,
						GreetingDuration:      greeting * time.Millisecond,
						AfterGreetingSilence:  after * time.Millisecond,
						MinimumWordLength:     minWord * time.Millisecond,
					}

					noSpeech, ok := minReachableTotal(p, ResultNoSpeech, silenceOnly)
					if !ok {
						t.Fatalf("%+v: no_speech unreachable at any total", p)
					}
					machine, ok := minReachableTotal(p, ResultMachine, speechOnly)
					if !ok {
						t.Fatalf("%+v: machine unreachable at any total", p)
					}
					// human can be unreachable at every total when the greeting
					// threshold is shorter than the shortest qualifying burst, so
					// machine always pre-empts it. No window can fix that, and it
					// does not make the params degenerate — the other two verdicts
					// still fire, so it simply does not constrain the boundary.
					human, humanOK := minReachableTotal(p, ResultHuman, shortestBurst(p))
					combos++

					want := noSpeech
					if machine < want {
						want = machine
					}
					if humanOK && human < want {
						want = human
					}

					below := p
					below.TotalAnalysisTime = want - frameDuration
					if err := below.Validate(); err == nil {
						t.Errorf("init=%v greet=%v after=%v word=%v: total=%v accepted, but the first reachable verdict needs %v (no_speech=%v machine=%v human=%v/%v)",
							p.InitialSilenceTimeout, p.GreetingDuration, p.AfterGreetingSilence, p.MinimumWordLength,
							below.TotalAnalysisTime, want, noSpeech, machine, human, humanOK)
					}

					at := p
					at.TotalAnalysisTime = want
					if err := at.Validate(); err != nil {
						t.Errorf("init=%v greet=%v after=%v word=%v: total=%v rejected (%v), but a verdict is reachable there (no_speech=%v machine=%v human=%v/%v)",
							p.InitialSilenceTimeout, p.GreetingDuration, p.AfterGreetingSilence, p.MinimumWordLength,
							want, err, noSpeech, machine, human, humanOK)
					}
				}
			}
		}
	}
	if combos == 0 {
		t.Fatal("no combinations exercised")
	}
	t.Logf("pinned %d parameter combinations against the FSM", combos)
}

// TestValidate_AcceptsSuppressedVerdict covers the tuning idiom of pushing one
// threshold past the window on purpose — a large greeting_duration is how a
// caller disables the machine verdict. Those params are not degenerate and must
// keep validating, so the guard cannot reject per-verdict.
func TestValidate_AcceptsSuppressedVerdict(t *testing.T) {
	p := Params{
		InitialSilenceTimeout: 2500 * time.Millisecond,
		GreetingDuration:      30 * time.Second,
		AfterGreetingSilence:  800 * time.Millisecond,
		TotalAnalysisTime:     5 * time.Second,
		MinimumWordLength:     100 * time.Millisecond,
	}

	if p.canReachMachine() {
		t.Fatal("this config is meant to have machine suppressed")
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("suppressing one verdict must stay valid: %v", err)
	}

	// The two surviving verdicts must genuinely still fire.
	if det := drive(p, silenceOnly); det.Result != ResultNoSpeech {
		t.Errorf("silence gave %s, want no_speech", det.Result)
	}
	if det := drive(p, shortestBurst(p)); det.Result != ResultHuman {
		t.Errorf("short burst gave %s, want human", det.Result)
	}
}

// TestValidate_RejectsDegenerateEqualWindows covers the reported config: three
// equal windows were accepted, yet continuous speech from t=0 yields not_sure
// because the deadline check runs before the phase switch that emits machine.
func TestValidate_RejectsDegenerateEqualWindows(t *testing.T) {
	p := Params{
		InitialSilenceTimeout: 1500 * time.Millisecond,
		GreetingDuration:      1500 * time.Millisecond,
		AfterGreetingSilence:  1500 * time.Millisecond,
		TotalAnalysisTime:     1500 * time.Millisecond,
		MinimumWordLength:     100 * time.Millisecond,
	}

	// The FSM's own behaviour is what makes this config degenerate.
	det := drive(p, speechOnly)
	if det.Result != ResultNotSure {
		t.Fatalf("expected the FSM to fall out as not_sure, got %s", det.Result)
	}
	if det.GreetingDurationMs != 1460 {
		t.Errorf("greeting_duration_ms=%d, want 1460", det.GreetingDurationMs)
	}

	if err := p.Validate(); err == nil {
		t.Fatal("expected equal windows to be rejected")
	}
}

// TestReachability_SubFrameEdge pins the sub-frame edge of each per-verdict
// reachability predicate. A verdict fires at a frame-aligned elapsed (its
// verdict frame), yet it is reachable at any TotalAnalysisTime strictly greater
// than that frame — not only at the next whole frame. Bounds that rejected the
// whole open interval between the two would refuse usable configs whose window
// merely was not frame-aligned. Each verdict's frame is read straight from the
// FSM (driven with a generous window); the predicate must then report false
// exactly at the frame and true for a non-aligned window 10 ms past it.
//
// This exercises the predicates rather than Validate because Validate rejects
// only when every verdict is unreachable, so one verdict's edge does not move
// its accept boundary. TestValidate_WindowGuardsMatchFSM covers that boundary.
func TestReachability_SubFrameEdge(t *testing.T) {
	const roomy = 15 * time.Second
	cases := []struct {
		name   string
		p      Params
		voiced func(i int) bool
		want   Result
		reach  func(Params) bool
	}{
		// The reported config: initial_silence_timeout=2500 with a 2510 window.
		// no_speech fires at 2500; the deadline at 2510 does not pre-empt it.
		{
			name: "no_speech",
			p: Params{
				InitialSilenceTimeout: 2500 * time.Millisecond,
				GreetingDuration:      200 * time.Millisecond,
				AfterGreetingSilence:  110 * time.Millisecond,
				MinimumWordLength:     40 * time.Millisecond,
			},
			voiced: silenceOnly,
			want:   ResultNoSpeech,
			reach:  Params.canReachNoSpeech,
		},
		{
			name: "machine",
			p: Params{
				InitialSilenceTimeout: 300 * time.Millisecond,
				GreetingDuration:      1230 * time.Millisecond,
				AfterGreetingSilence:  110 * time.Millisecond,
				MinimumWordLength:     40 * time.Millisecond,
			},
			voiced: speechOnly,
			want:   ResultMachine,
			reach:  Params.canReachMachine,
		},
		{
			name: "human",
			p: Params{
				InitialSilenceTimeout: 300 * time.Millisecond,
				GreetingDuration:      900 * time.Millisecond,
				AfterGreetingSilence:  1230 * time.Millisecond,
				MinimumWordLength:     250 * time.Millisecond,
			},
			want:  ResultHuman,
			reach: Params.canReachHuman,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			voiced := tc.voiced
			if voiced == nil {
				voiced = shortestBurst(tc.p)
			}

			// Read the verdict frame from the FSM itself.
			roomyP := tc.p
			roomyP.TotalAnalysisTime = roomy
			det := drive(roomyP, voiced)
			if det.Result != tc.want {
				t.Fatalf("with a roomy window the FSM gave %s, want %s", det.Result, tc.want)
			}
			verdictFrame := time.Duration(det.TotalAnalysisMs) * time.Millisecond

			// At the verdict frame the deadline strikes before the phase switch,
			// so the verdict never emits: reject, and confirm the FSM agrees.
			atFrame := tc.p
			atFrame.TotalAnalysisTime = verdictFrame
			if got := drive(atFrame, voiced); got.Result != ResultNotSure {
				t.Fatalf("at the verdict frame the FSM gave %s, want not_sure", got.Result)
			}
			if tc.reach(atFrame) {
				t.Errorf("total=%v (== verdict frame) reported reachable, want unreachable", verdictFrame)
			}

			// A non-aligned window 10 ms past the frame is genuinely reachable.
			subFrame := tc.p
			subFrame.TotalAnalysisTime = verdictFrame + 10*time.Millisecond
			if got := drive(subFrame, voiced); got.Result != tc.want {
				t.Fatalf("at %v the FSM gave %s, want %s", subFrame.TotalAnalysisTime, got.Result, tc.want)
			}
			if !tc.reach(subFrame) {
				t.Errorf("total=%v (10 ms past verdict frame) reported unreachable, but %s fires there",
					subFrame.TotalAnalysisTime, tc.want)
			}
		})
	}
}

// TestValidate_AcceptsDefaults guards against the bounds tightening so far that
// the shipped defaults stop validating.
func TestValidate_AcceptsDefaults(t *testing.T) {
	if err := DefaultParams().Validate(); err != nil {
		t.Fatalf("default params must stay valid: %v", err)
	}
}
