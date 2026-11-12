package room

import (
	"io"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// playoutMockLeg is a mockLeg that also implements playoutLeg.
type playoutMockLeg struct {
	*mockLeg
	conceals bool
	mu       sync.Mutex
	queued   func() int
	set      int
}

func (l *playoutMockLeg) SetPlayoutQueued(queued func() int) {
	l.mu.Lock()
	l.queued = queued
	l.set++
	l.mu.Unlock()
}

func (l *playoutMockLeg) ConcealsLoss() bool { return l.conceals }

func (l *playoutMockLeg) state() (func() int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.queued, l.set
}

// A leg that conceals loss needs to see the mixer's queue while it is in a
// room, and must lose sight of it on the way out: the queue belongs to a
// participant that no longer plays the leg.
func TestPlayoutQueueFollowsTheLegInAndOutOfARoom(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-playout", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := &playoutMockLeg{mockLeg: newMockLeg("concealer"), conceals: true}
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)
	queued, _ := l.state()
	if queued == nil {
		t.Fatal("joining a room should let the leg see the mixer's queue")
	}
	if n := queued(); n < 0 || n > 3 {
		t.Errorf("queued = %d, outside the mixer's queue of 3", n)
	}

	r.RemoveLeg(l.ID())
	if queued, _ := l.state(); queued != nil {
		t.Error("leaving a room must take the queue away")
	}
}

func TestPlayoutQueueIsNotGivenToALegThatDoesNotConceal(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-playout-g722", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := &playoutMockLeg{mockLeg: newMockLeg("g722")}
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)
	queued, set := l.state()
	if queued != nil {
		t.Error("a leg whose codec is not concealed was given the queue")
	}
	if set == 0 {
		t.Error("the queue was not cleared: a leg that renegotiated away from a concealed codec would keep its old one")
	}
}
