package sip

import (
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

type fakeServerTx struct {
	done      chan struct{}
	responses []int
}

func (f *fakeServerTx) Respond(res *sip.Response) error {
	f.responses = append(f.responses, res.StatusCode)
	return nil
}
func (f *fakeServerTx) Acks() <-chan *sip.Request          { return nil }
func (f *fakeServerTx) OnCancel(sip.FnTxCancel) bool       { return true }
func (f *fakeServerTx) Terminate()                         {}
func (f *fakeServerTx) OnTerminate(sip.FnTxTerminate) bool { return true }
func (f *fakeServerTx) Done() <-chan struct{}              { return f.done }
func (f *fakeServerTx) Err() error                         { return nil }

func referTestResponse(code int) *sip.Response {
	req := sip.NewRequest(sip.REFER, sip.Uri{Host: "127.0.0.1"})
	return sip.NewResponseFromRequest(req, code, "x", nil)
}

func waitReturns(t *testing.T, r *referTx, limit, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() { r.wait(limit); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("wait did not return within %v", within)
	}
	return time.Since(start)
}

// The REFER handler holds its transaction until the final response, so a
// decision made after the handler returns still goes out inside it.
func TestReferTx_WaitsForFinalResponse(t *testing.T) {
	f := &fakeServerTx{done: make(chan struct{})}
	r := newReferTx(f)
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.Respond(referTestResponse(100))
		time.Sleep(50 * time.Millisecond)
		r.Respond(referTestResponse(202))
	}()
	if d := waitReturns(t, r, 5*time.Second, 2*time.Second); d < 90*time.Millisecond {
		t.Errorf("returned after %v, before the final response", d)
	}
	if len(f.responses) != 2 || f.responses[1] != 202 {
		t.Errorf("responses = %v", f.responses)
	}
}

func TestReferTx_ReturnsWhenTransactionEnds(t *testing.T) {
	f := &fakeServerTx{done: make(chan struct{})}
	r := newReferTx(f)
	go func() { time.Sleep(30 * time.Millisecond); close(f.done) }()
	waitReturns(t, r, 5*time.Second, 2*time.Second)
}

func TestReferTx_GivesUpAtLimit(t *testing.T) {
	r := newReferTx(&fakeServerTx{done: make(chan struct{})})
	waitReturns(t, r, 50*time.Millisecond, 2*time.Second)
}

func TestReferTx_RespondedBeforeWait(t *testing.T) {
	r := newReferTx(&fakeServerTx{done: make(chan struct{})})
	r.Respond(referTestResponse(603))
	r.Respond(referTestResponse(603))
	if d := waitReturns(t, r, 5*time.Second, time.Second); d > 100*time.Millisecond {
		t.Errorf("waited %v after the final response was already sent", d)
	}
}
