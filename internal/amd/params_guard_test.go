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

// TestValidate_WindowGuardsMatchFSM pins the guards to the FSM itself. Validate
// accepts a params set only if every verdict is reachable within the analysis
// window, so its accept boundary must be exactly the largest of the three
// verdicts' first-reachable totals, as measured by driving the real FSM.
// Accepting below that defeats a verdict; rejecting at it would reject a
// genuinely usable config, which would be worse than the bug.
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
					human, ok := minReachableTotal(p, ResultHuman, shortestBurst(p))
					if !ok {
						// The greeting threshold is shorter than the shortest
						// qualifying burst, so machine always pre-empts human.
						// No total_analysis_time can fix that — out of scope
						// for a window guard.
						continue
					}
					combos++

					want := noSpeech
					if machine > want {
						want = machine
					}
					if human > want {
						want = human
					}

					below := p
					below.TotalAnalysisTime = want - frameDuration
					if err := below.Validate(); err == nil {
						t.Errorf("init=%v greet=%v after=%v word=%v: total=%v accepted, but no_speech=%v machine=%v human=%v need %v",
							p.InitialSilenceTimeout, p.GreetingDuration, p.AfterGreetingSilence, p.MinimumWordLength,
							below.TotalAnalysisTime, noSpeech, machine, human, want)
					}

					at := p
					at.TotalAnalysisTime = want
					if err := at.Validate(); err != nil {
						t.Errorf("init=%v greet=%v after=%v word=%v: total=%v rejected (%v), but all verdicts reachable there (no_speech=%v machine=%v human=%v)",
							p.InitialSilenceTimeout, p.GreetingDuration, p.AfterGreetingSilence, p.MinimumWordLength,
							want, err, noSpeech, machine, human)
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

// TestValidate_AcceptsSubFrameReachableTotal pins the sub-frame edge of each
// guard. A verdict fires at a frame-aligned elapsed (its verdict frame), yet it
// is reachable at any TotalAnalysisTime strictly greater than that frame — not
// only at the next whole frame. The earlier bounds rejected the whole open
// interval between the verdict frame and the next frame, 400'ing usable configs
// whose window merely was not frame-aligned. Here each verdict's frame is read
// straight from the FSM (driven with a generous window), then Validate must
// reject exactly at the frame and accept a non-aligned window 10 ms past it.
func TestValidate_AcceptsSubFrameReachableTotal(t *testing.T) {
	const roomy = 15 * time.Second
	cases := []struct {
		name   string
		p      Params
		voiced func(i int) bool
		want   Result
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
		},
		{
			name: "human",
			p: Params{
				InitialSilenceTimeout: 300 * time.Millisecond,
				GreetingDuration:      900 * time.Millisecond,
				AfterGreetingSilence:  1230 * time.Millisecond,
				MinimumWordLength:     250 * time.Millisecond,
			},
			want: ResultHuman,
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
			if err := atFrame.Validate(); err == nil {
				t.Errorf("total=%v (== verdict frame) accepted, want rejected", verdictFrame)
			}

			// A non-aligned window 10 ms past the frame is genuinely reachable.
			subFrame := tc.p
			subFrame.TotalAnalysisTime = verdictFrame + 10*time.Millisecond
			if got := drive(subFrame, voiced); got.Result != tc.want {
				t.Fatalf("at %v the FSM gave %s, want %s", subFrame.TotalAnalysisTime, got.Result, tc.want)
			}
			if err := subFrame.Validate(); err != nil {
				t.Errorf("total=%v (10 ms past verdict frame) rejected (%v), but %s is reachable there",
					subFrame.TotalAnalysisTime, err, tc.want)
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
