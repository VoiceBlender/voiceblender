// Package matrix implements MSC3401 1:1 VoIP signaling on top of
// maunium.net/go/mautrix. VoiceBlender itself runs no SFU; each Matrix peer
// connects as a separate 1:1 WebRTC call and the mixer combines them.
//
// This file declares the narrow EventSender interface used by MatrixLeg
// (keeping mautrix out of internal/leg) and the typed union of inbound m.call.*
// events delivered to subscribers via the dispatcher.
package matrix

import (
	"context"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// CallEventKind identifies the variant of an inbound MatrixCallEvent.
type CallEventKind string

const (
	KindInvite     CallEventKind = "invite"
	KindAnswer     CallEventKind = "answer"
	KindCandidates CallEventKind = "candidates"
	KindHangup     CallEventKind = "hangup"
	KindReject     CallEventKind = "reject"
	KindNegotiate  CallEventKind = "negotiate"
)

// CallEvent is the demux-facing union for inbound m.call.* events. Exactly
// one of the *Content fields is populated based on Kind.
type CallEvent struct {
	Kind   CallEventKind
	RoomID id.RoomID
	Sender id.UserID
	CallID string

	Invite     *event.CallInviteEventContent
	Answer     *event.CallAnswerEventContent
	Candidates *event.CallCandidatesEventContent
	Hangup     *event.CallHangupEventContent
	Reject     *event.CallRejectEventContent
	Negotiate  *event.CallNegotiateEventContent
}

// EventSender is the narrow interface MatrixLeg depends on for both sending
// outbound m.call.* events and consuming inbound ones. Both Client (per-leg)
// and Listener (global) implement it.
type EventSender interface {
	SendAnswer(ctx context.Context, roomID id.RoomID, content *event.CallAnswerEventContent) error
	SendCandidates(ctx context.Context, roomID id.RoomID, content *event.CallCandidatesEventContent) error
	SendHangup(ctx context.Context, roomID id.RoomID, content *event.CallHangupEventContent) error

	// Subscribe returns a buffered channel that receives every inbound
	// m.call.* event tagged with the given (roomID, callID). Caller MUST
	// call Unsubscribe once finished. Buffer is sized so candidate bursts
	// do not block the sync goroutine; oldest candidates are dropped under
	// pressure.
	Subscribe(roomID id.RoomID, callID string) <-chan CallEvent
	Unsubscribe(roomID id.RoomID, callID string)
}

// InboundHandler is invoked by the Listener on every fresh m.call.invite
// addressed to the configured service account. The handler runs on its own
// goroutine; the listener keeps syncing while it runs. The sender argument
// is the Listener itself, supplied so the handler can construct a leg
// without taking a direct dependency on the Listener type.
type InboundHandler func(ctx context.Context, ev *CallEvent, sender EventSender)

// CallEventTypes is the set of m.call.* event types subscribed to by both
// outbound Clients and the global Listener.
var CallEventTypes = []event.Type{
	event.CallInvite,
	event.CallAnswer,
	event.CallCandidates,
	event.CallHangup,
	event.CallReject,
	event.CallNegotiate,
}

// SubscribedEventTypes is the superset CallEventTypes + m.room.encrypted. We
// include encrypted events solely so we can log a WARN when calls happen in
// an E2EE room — v1 has no megolm support, so encrypted m.call.* events
// arrive wrapped and we cannot read them.
var SubscribedEventTypes = func() []event.Type {
	out := make([]event.Type, 0, len(CallEventTypes)+1)
	out = append(out, CallEventTypes...)
	out = append(out, event.EventEncrypted)
	return out
}()
