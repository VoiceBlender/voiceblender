package mixer

import (
	"testing"
)

// TestMixer_PanicSparesOwnerClosedEgress drives the production panic path and
// asserts the mixer does NOT close the writer of a participant whose owner
// closes it itself. This is the leg case: Hangup, dispatched by the room's
// identity-gated hook, is what closes the egress, so a synchronous close here
// is redundant when the leg genuinely died and destructive when it moved.
//
// The panic hook fires after recoverParticipant has already made its
// writer-close decision, so waiting on it is a race-free barrier — no timing.
func TestMixer_PanicSparesOwnerClosedEgress(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	panicked := make(chan struct{})
	m.SetOnParticipantPanic(func(p *Participant, loop string) { close(panicked) })

	egress := &closeSpyWriter{}
	p := m.AddParticipant("leg", &panicAfterReader{limit: 1, frame: make([]byte, m.frameSizeBytes)}, egress)
	p.MarkOwnerClosesEgress()

	<-panicked
	if egress.closed.Load() {
		t.Fatal("the mixer closed a leg's egress on panic; its owner closes it via Hangup, and a moved leg shares that pipe with a live successor")
	}
}

// TestMixer_PanicVsMoveKeepsSharedEgressOpen reproduces the panic-vs-move
// interleaving, laid out in the exact order the race produces it, and asserts
// the moved leg's shared egress survives.
//
// A ws/MoQ leg hands the same egress pipe back from AudioWriter on every join,
// so the participant carrying the leg before a MoveLeg and the one after it
// share that writer. When a stale IO loop's recover wins the mixer race, it
// removes its own instance, the move re-adds the leg on a fresh participant
// over the same egress, and the recover then reaches the writer close. Ungated,
// that close silences the successor: it can hear ingress but never send — a
// one-way zombie the room layer never gets told about, because the room's
// identity-gated hook correctly does nothing for the already-moved leg.
//
// Sequencing the mixer's own steps by hand keeps this deterministic. Each maps
// to a point in the real race, called on the same methods recoverParticipant
// runs in the same order.
func TestMixer_PanicVsMoveKeepsSharedEgressOpen(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)

	egress := &closeSpyWriter{}
	frame := make([]byte, m.frameSizeBytes)

	// The leg is in room A on p1.
	p1 := m.AddParticipant("leg", &silenceReader{frame: frame}, egress)
	p1.MarkOwnerClosesEgress()

	// 1. p1's dead IO loop recovers and wins the mixer race: it removes p1.
	if !m.removeParticipantIf(p1) {
		t.Fatal("removeParticipantIf(p1) did not remove the panicked instance")
	}

	// 2. The MoveLeg lands: DetachLeg found p1 already gone (RemoveParticipant
	//    returned false, ignored by removeLegLocked) and AddLeg re-added the leg
	//    to room B on a fresh participant over the SAME egress.
	p2 := m.AddParticipant("leg", &silenceReader{frame: frame}, egress)
	p2.MarkOwnerClosesEgress()
	defer m.removeParticipantIf(p2)

	// 3. p1's recover finally reaches the writer close.
	m.closeWriterForPanic(p1)

	if egress.closed.Load() {
		t.Fatal("a moved leg's shared egress was closed by the panicked instance's recover; its successor can hear but never speak")
	}

	// The successor is still the registered instance and still live.
	m.mu.Lock()
	cur := m.participants["leg"]
	m.mu.Unlock()
	if cur != p2 {
		t.Fatal("the successor is no longer the registered participant")
	}
	select {
	case <-p2.done:
		t.Fatal("the successor's IO loops were stopped")
	default:
	}
}
