package matrix

import (
	"sync"

	"maunium.net/go/mautrix/id"
)

// subKey scopes a subscription to (roomID, callID). call_id is room-scoped
// per MSC2746, so a global key would collide across rooms.
type subKey struct {
	room   id.RoomID
	callID string
}

// dispatcher is the inbound-event multiplexer shared by Client and Listener.
// Subscribe registers a per-call channel; Dispatch routes incoming events to
// the matching channel.
type dispatcher struct {
	mu   sync.RWMutex
	subs map[subKey]chan CallEvent
}

func newDispatcher() *dispatcher {
	return &dispatcher{subs: make(map[subKey]chan CallEvent)}
}

// subscribe returns a buffered channel for events tagged with (roomID, callID).
// Re-subscribing for the same key returns the same channel.
func (d *dispatcher) subscribe(roomID id.RoomID, callID string) <-chan CallEvent {
	k := subKey{roomID, callID}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.subs[k]; ok {
		return ch
	}
	// Buffer 64: candidate bursts can come 10+ per sync. Hangup and answer
	// are blocking-sent below, so this only matters for candidates.
	ch := make(chan CallEvent, 64)
	d.subs[k] = ch
	return ch
}

func (d *dispatcher) unsubscribe(roomID id.RoomID, callID string) {
	k := subKey{roomID, callID}
	d.mu.Lock()
	ch, ok := d.subs[k]
	if ok {
		delete(d.subs, k)
	}
	d.mu.Unlock()
	if ok {
		close(ch)
	}
}

// dispatch routes ev to its subscriber. Returns true if delivered.
// Candidates are lossy under back-pressure (drop oldest); other kinds drop
// the new event instead of blocking the sync loop.
func (d *dispatcher) dispatch(ev CallEvent) bool {
	k := subKey{ev.RoomID, ev.CallID}
	d.mu.RLock()
	ch, ok := d.subs[k]
	d.mu.RUnlock()
	if !ok {
		return false
	}
	if ev.Kind == KindCandidates {
		select {
		case ch <- ev:
			return true
		default:
			// Drop oldest then enqueue.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
				return true
			default:
				return false
			}
		}
	}
	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}

// closeAll closes every subscription (used on Client/Listener shutdown).
func (d *dispatcher) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, ch := range d.subs {
		close(ch)
		delete(d.subs, k)
	}
}
