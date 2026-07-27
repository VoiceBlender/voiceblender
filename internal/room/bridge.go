package room

import (
	"errors"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/bridge"
)

// Bridge validation/lookup errors. The API layer maps these to HTTP status
// codes via errors.Is.
var (
	ErrBridgeSelf        = errors.New("cannot bridge a room to itself")
	ErrBridgeRoomMissing = errors.New("room not found")
	ErrBridgeSampleRate  = errors.New("sample rate mismatch")
	ErrBridgeExists      = errors.New("bridge between these rooms already exists")
	ErrBridgeDirection   = errors.New("invalid bridge direction")
	ErrBridgeNotFound    = errors.New("bridge not found")
)

// Direction is the canonical audio flow of a bridge, relative to room A
// (the room the bridge was created from).
type Direction string

const (
	DirectionBidirectional Direction = "bidirectional"
	DirectionAToB          Direction = "a_to_b"
	DirectionBToA          Direction = "b_to_a"
	DirectionNone          Direction = "none"
)

func (d Direction) Valid() bool {
	switch d {
	case DirectionBidirectional, DirectionAToB, DirectionBToA, DirectionNone:
		return true
	}
	return false
}

// flags reports whether room A and room B each emit audio toward the peer.
func (d Direction) flags() (aSends, bSends bool) {
	switch d {
	case DirectionBidirectional:
		return true, true
	case DirectionAToB:
		return true, false
	case DirectionBToA:
		return false, true
	default: // DirectionNone
		return false, false
	}
}

// Bridge is a live link joining the mixers of RoomAID and RoomBID through a
// duplex in-memory conduit. Mixed-minus-self in each mixer prevents the
// other room's audio from echoing back across the conduit.
type Bridge struct {
	ID        string
	RoomAID   string
	RoomBID   string
	Direction Direction

	epA, epB *bridge.Endpoint
	pid      string // synthetic participant id in both mixers
}

// bridgeParticipantPrefix marks a mixer participant as a synthetic bridge
// endpoint rather than a leg. The mixer's participant map is not leg-only, so
// teardown has to be able to tell the classes apart by ID.
const bridgeParticipantPrefix = "__bridge:"

func bridgeParticipantID(bridgeID string) string {
	return bridgeParticipantPrefix + bridgeID
}

// bridgeIDFromParticipant reports the bridge behind a synthetic bridge
// participant ID, and whether participantID is one at all.
func bridgeIDFromParticipant(participantID string) (string, bool) {
	return strings.CutPrefix(participantID, bridgeParticipantPrefix)
}
