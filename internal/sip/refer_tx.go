package sip

import (
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

// referAnswerWait caps how long a REFER's transaction is held open for the
// application's decision: 64*T1, after which a UDP referrer has given up.
const referAnswerWait = 32 * time.Second

// referTx reports when the REFER it wraps has had its final response.
type referTx struct {
	sip.ServerTransaction
	once  sync.Once
	final chan struct{}
}

func newReferTx(tx sip.ServerTransaction) *referTx {
	return &referTx{ServerTransaction: tx, final: make(chan struct{})}
}

func (r *referTx) Respond(res *sip.Response) error {
	err := r.ServerTransaction.Respond(res)
	if res.StatusCode >= 200 {
		r.once.Do(func() { close(r.final) })
	}
	return err
}

// wait blocks until the final response is sent, the transaction ends, or
// limit passes.
func (r *referTx) wait(limit time.Duration) {
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-r.final:
	case <-r.Done():
	case <-t.C:
	}
}
