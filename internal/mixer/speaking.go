package mixer

import "math"

const (
	// speakingThreshold is the RMS energy level (16kHz int16 samples) above
	// which a frame is considered voiced. Typical speech RMS is 1000–10000;
	// background noise sits below 200.
	speakingThreshold = 300

	// speakingOnFrames is the number of consecutive voiced frames required
	// before emitting a speaking-started event (3 × 20ms = 60ms).
	speakingOnFrames = 3

	// speakingOffFrames is the number of consecutive silent frames required
	// before emitting a speaking-stopped event (15 × 20ms = 300ms).
	// A longer release avoids flicker during natural speech pauses.
	speakingOffFrames = 15
)

// SpeakingEvent is emitted when a participant starts or stops speaking.
type SpeakingEvent struct {
	ParticipantID string
	Speaking      bool
}

// speakingState tracks per-participant voice activity with debouncing.
type speakingState struct {
	speaking     bool
	activeFrames int // consecutive frames above threshold
	silentFrames int // consecutive frames below threshold
}

// update feeds a new frame's RMS energy into the state machine.
// Returns true if the speaking state changed.
func (s *speakingState) update(rms float64) bool {
	if rms >= speakingThreshold {
		s.activeFrames++
		s.silentFrames = 0
	} else {
		s.silentFrames++
		s.activeFrames = 0
	}

	prev := s.speaking
	if !s.speaking && s.activeFrames >= speakingOnFrames {
		s.speaking = true
	} else if s.speaking && s.silentFrames >= speakingOffFrames {
		s.speaking = false
	}
	return s.speaking != prev
}

// computeRMS returns the root-mean-square of int16 samples.
func computeRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}
