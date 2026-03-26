package sip

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"
)

// DTMFEvent represents an RFC 4733 telephone-event payload.
type DTMFEvent struct {
	Event      uint8  // 0-9, 10=*, 11=#, 12-15=A-D
	EndOfEvent bool
	Volume     uint8  // 0-63
	Duration   uint16 // in timestamp units
}

// EncodeDTMFEvent encodes a DTMFEvent into a 4-byte RFC 4733 payload.
func EncodeDTMFEvent(ev DTMFEvent) []byte {
	buf := make([]byte, 4)
	buf[0] = ev.Event
	if ev.EndOfEvent {
		buf[1] = 0x80 | (ev.Volume & 0x3F)
	} else {
		buf[1] = ev.Volume & 0x3F
	}
	binary.BigEndian.PutUint16(buf[2:], ev.Duration)
	return buf
}

// DecodeDTMFEvent decodes a 4-byte RFC 4733 payload into a DTMFEvent.
func DecodeDTMFEvent(payload []byte) (DTMFEvent, error) {
	if len(payload) < 4 {
		return DTMFEvent{}, fmt.Errorf("DTMF payload too short: %d bytes", len(payload))
	}
	return DTMFEvent{
		Event:      payload[0],
		EndOfEvent: payload[1]&0x80 != 0,
		Volume:     payload[1] & 0x3F,
		Duration:   binary.BigEndian.Uint16(payload[2:]),
	}, nil
}

// DTMFDigitToEvent converts a DTMF digit character to its RFC 4733 event code.
func DTMFDigitToEvent(digit rune) (uint8, bool) {
	switch {
	case digit >= '0' && digit <= '9':
		return uint8(digit - '0'), true
	case digit == '*':
		return 10, true
	case digit == '#':
		return 11, true
	case digit >= 'A' && digit <= 'D':
		return uint8(digit-'A') + 12, true
	case digit >= 'a' && digit <= 'd':
		return uint8(digit-'a') + 12, true
	default:
		return 0, false
	}
}

// DTMFEventToDigit converts an RFC 4733 event code to a digit character.
func DTMFEventToDigit(event uint8) (rune, bool) {
	switch {
	case event <= 9:
		return rune('0' + event), true
	case event == 10:
		return '*', true
	case event == 11:
		return '#', true
	case event >= 12 && event <= 15:
		return rune('A' + event - 12), true
	default:
		return 0, false
	}
}

// GenerateDTMFPackets builds the sequence of RTP packets for one DTMF digit.
// Returns 7 packets: 4 normal (80ms) + 3 end-of-event, each 20ms apart.
// The timestamp remains fixed for the entire event per RFC 4733.
func GenerateDTMFPackets(digit rune, pt uint8, ssrc uint32, baseSeq uint16, baseTS uint32) []*rtp.Packet {
	event, ok := DTMFDigitToEvent(digit)
	if !ok {
		return nil
	}

	const (
		volume        = 10
		samplesPerPkt = 160 // 20ms at 8kHz clock rate
	)

	pkts := make([]*rtp.Packet, 0, 7)
	seq := baseSeq

	// 4 normal event packets (duration increases by 160 each)
	for i := 0; i < 4; i++ {
		duration := uint16((i + 1) * samplesPerPkt)
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: seq,
				Timestamp:      baseTS,
				SSRC:           ssrc,
				Marker:         i == 0, // Marker on first packet
			},
			Payload: EncodeDTMFEvent(DTMFEvent{
				Event:    event,
				Volume:   volume,
				Duration: duration,
			}),
		})
		seq++
	}

	// 3 end-of-event packets (same duration, end flag set)
	finalDuration := uint16(4 * samplesPerPkt)
	for i := 0; i < 3; i++ {
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: seq,
				Timestamp:      baseTS,
				SSRC:           ssrc,
			},
			Payload: EncodeDTMFEvent(DTMFEvent{
				Event:      event,
				EndOfEvent: true,
				Volume:     volume,
				Duration:   finalDuration,
			}),
		})
		seq++
	}

	return pkts
}
