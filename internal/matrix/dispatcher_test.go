package matrix

import (
	"testing"
	"time"

	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestDispatcherSubscribeUnsubscribe(t *testing.T) {
	d := newDispatcher()
	room := id.RoomID("!a:test")
	ch := d.subscribe(room, "call-1")
	// Resubscribing yields the same channel.
	if ch2 := d.subscribe(room, "call-1"); ch2 != ch {
		t.Fatal("resubscribe returned different channel")
	}
	d.unsubscribe(room, "call-1")
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel closed after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after unsubscribe")
	}
}

func TestDispatcherKeyedByRoomAndCallID(t *testing.T) {
	d := newDispatcher()
	roomA := id.RoomID("!a:test")
	roomB := id.RoomID("!b:test")
	chA := d.subscribe(roomA, "call-x")
	chB := d.subscribe(roomB, "call-x") // same call-id, different room
	if chA == chB {
		t.Fatal("same call-id across different rooms must not collide")
	}
	if !d.dispatch(CallEvent{Kind: KindHangup, RoomID: roomA, CallID: "call-x", Hangup: &mevent.CallHangupEventContent{}}) {
		t.Fatal("expected dispatch to room A subscriber")
	}
	select {
	case ev := <-chA:
		if ev.RoomID != roomA {
			t.Fatalf("wrong room delivered: %s", ev.RoomID)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered to A")
	}
	select {
	case <-chB:
		t.Fatal("event leaked into B subscriber")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDispatcherDropsOldestCandidates(t *testing.T) {
	d := newDispatcher()
	room := id.RoomID("!a:test")
	ch := d.subscribe(room, "c")
	// Fill the buffer (64).
	for i := 0; i < 64; i++ {
		d.dispatch(CallEvent{Kind: KindCandidates, RoomID: room, CallID: "c", Candidates: &mevent.CallCandidatesEventContent{}})
	}
	// One more should drop oldest and enqueue.
	if !d.dispatch(CallEvent{Kind: KindCandidates, RoomID: room, CallID: "c", Candidates: &mevent.CallCandidatesEventContent{}}) {
		t.Fatal("candidate burst was not absorbed by drop-oldest")
	}
	// Drain to verify still buffered.
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			if count != 64 {
				t.Fatalf("expected 64 candidates buffered, got %d", count)
			}
			return
		}
	}
}
