package mixer

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// voidClosePanicWriter mirrors the API layer's egress pipe writer, which an
// agent registers through AddParticipant: its Close reports no error, so it
// does not satisfy io.Closer. Write panics to drive writeLoop into
// recoverParticipant. closed stands in for the read end the owner is parked
// on — closing it is the only wakeup that owner will ever get.
type voidClosePanicWriter struct {
	once   sync.Once
	closed chan struct{}
}

func (w *voidClosePanicWriter) Write([]byte) (int, error) { panic("simulated write panic") }
func (w *voidClosePanicWriter) Close()                    { w.once.Do(func() { close(w.closed) }) }

// parkedReader keeps readLoop blocked instead of spinning, so the write loop is
// the only one that panics.
type parkedReader struct{ release chan struct{} }

func (r *parkedReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

func newClosertestMixer() *Mixer {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultSampleRate)
}

// pushOutgoing hands one frame to the participant's write loop, which is what
// makes it call Write and panic.
func pushOutgoing(t *testing.T, m *Mixer, id string) {
	t.Helper()
	m.mu.Lock()
	p, ok := m.participants[id]
	m.mu.Unlock()
	if !ok {
		t.Fatalf("participant %q not registered", id)
	}
	select {
	case p.outgoing <- make([]byte, m.frameSizeBytes):
	case <-time.After(time.Second):
		t.Fatal("outgoing channel never drained")
	}
}

func awaitClose(t *testing.T, closed <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("owner never woken: %s was not closed after its write loop panicked", what)
	}
}

// An agent's writer closes with no error return. The panic path must still
// close it, or the agent's owner parks on the pipe forever.
func TestCloseWriterForPanicClosesVoidCloseWriter(t *testing.T) {
	m := newClosertestMixer()
	rd := &parkedReader{release: make(chan struct{})}
	t.Cleanup(func() { close(rd.release) })
	w := &voidClosePanicWriter{closed: make(chan struct{})}

	m.AddParticipant("agent-1", rd, w)
	pushOutgoing(t, m, "agent-1")

	awaitClose(t, w.closed, "a writer whose Close returns no error")
}
