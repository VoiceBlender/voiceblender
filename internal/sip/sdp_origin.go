package sip

import (
	"math/rand/v2"
	"sync"

	pionsdp "github.com/pion/sdp/v3"
)

// SDPOrigin keeps the o= line of every SDP one side sends in a session
// consistent (RFC 3264 §8): the session id never changes, and the version
// goes up by one exactly when the rest of the SDP changes.
type SDPOrigin struct {
	mu      sync.Mutex
	id      uint64
	version uint64
	last    string
}

// NewSDPOrigin starts the origin of a new session.
func NewSDPOrigin() *SDPOrigin {
	return &SDPOrigin{id: uint64(rand.Int64N(1<<31 - 1))}
}

// stamp fills sd's origin and marshals it. A nil origin keeps the old
// behaviour of a fresh session id per SDP.
func (o *SDPOrigin) stamp(sd *pionsdp.SessionDescription) []byte {
	if o == nil {
		b, _ := sd.Marshal()
		return b
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	sd.Origin.SessionID = o.id
	sd.Origin.SessionVersion = 0
	key, _ := sd.Marshal()
	if o.last != "" && string(key) != o.last {
		o.version++
	}
	o.last = string(key)
	sd.Origin.SessionVersion = o.version
	b, _ := sd.Marshal()
	return b
}
