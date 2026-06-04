package lkmedia

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v4"
)

func TestMergeICEServers_OperatorOverridesServer(t *testing.T) {
	op := []webrtc.ICEServer{{URLs: []string{"stun:operator.example:3478"}}}
	srv := []*livekit.ICEServer{{Urls: []string{"stun:server.example:3478"}}}
	got := mergeICEServers(op, srv)
	if !reflect.DeepEqual(got, op) {
		t.Errorf("got %+v, want operator override %+v", got, op)
	}
}

func TestMergeICEServers_FallsBackToServer(t *testing.T) {
	srv := []*livekit.ICEServer{
		{Urls: []string{"stun:s1.example:3478"}},
		{Urls: []string{"turn:s2.example:3478"}, Username: "u", Credential: "p"},
	}
	got := mergeICEServers(nil, srv)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[1].Username != "u" || got[1].Credential != "p" {
		t.Errorf("creds not propagated: %+v", got[1])
	}
}

func TestMergeICEServers_BothEmpty(t *testing.T) {
	got := mergeICEServers(nil, nil)
	if len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}

func TestInt16BytesRoundTrip(t *testing.T) {
	src := []int16{0, 1, -1, 32767, -32768, 12345, -12345}
	enc := int16ToBytes(src)
	if len(enc) != len(src)*2 {
		t.Fatalf("byte length = %d, want %d", len(enc), len(src)*2)
	}
	dec := bytesToInt16(enc)
	if !reflect.DeepEqual(dec, src) {
		t.Errorf("round trip mismatch:\n  in=%v\n out=%v", src, dec)
	}
	// Spot-check little-endian byte order for an unambiguous sample.
	enc2 := int16ToBytes([]int16{0x1234})
	if got := binary.LittleEndian.Uint16(enc2); got != 0x1234 {
		t.Errorf("byte order wrong: %02x", enc2)
	}
}

func TestStreamBuffer_TryReadNonBlocking(t *testing.T) {
	sb := newStreamBuffer(1024, 20)
	defer sb.Close()

	// Empty buffer → tryRead returns (0, nil) without blocking.
	out := make([]byte, 32)
	n, err := sb.tryRead(out)
	if n != 0 || err != nil {
		t.Errorf("empty tryRead = (%d, %v), want (0, nil)", n, err)
	}

	// Partial fill (fewer bytes than requested) → tryRead returns (0, nil).
	_, _ = sb.Write([]byte{1, 2, 3})
	n, err = sb.tryRead(out)
	if n != 0 || err != nil {
		t.Errorf("partial tryRead = (%d, %v), want (0, nil)", n, err)
	}

	// Pad to a full frame → tryRead returns the frame.
	_, _ = sb.Write(bytes.Repeat([]byte{0xAB}, 32-3))
	n, err = sb.tryRead(out)
	if n != 32 || err != nil {
		t.Errorf("full tryRead = (%d, %v), want (32, nil)", n, err)
	}
}

func TestStreamBuffer_TryReadClosedReturnsEOF(t *testing.T) {
	sb := newStreamBuffer(64, 20)
	sb.Close()
	out := make([]byte, 16)
	n, err := sb.tryRead(out)
	if n != 0 || err != io.EOF {
		t.Errorf("closed tryRead = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestNewTransport_RequiresValidConfig(t *testing.T) {
	// Missing Log → Validate fails before any signaling work.
	_, err := NewTransport(context.Background(), Config{},
		SignalConfig{URL: "wss://x", Token: "t"}, PeerConfig{}, Callbacks{})
	if err == nil {
		t.Fatal("expected error for missing Log")
	}
}

func TestNewTransport_RejectsLegacyPublisherPrimary(t *testing.T) {
	join := canonicalJoin()
	join.SubscriberPrimary = false
	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		// Block until the client closes.
		_, _ = readClientSignal(conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := NewTransport(ctx, Config{Log: slog.Default()},
		SignalConfig{URL: srv.wsURL(), Token: "tok"}, PeerConfig{}, Callbacks{})
	if err == nil || !strings.Contains(err.Error(), "publisher-primary") {
		t.Fatalf("expected publisher-primary rejection, got %v", err)
	}
}

func TestNewTransport_SubscriberPrimarySendsAddTrack(t *testing.T) {
	// Full negotiation needs a real LiveKit server (PR 6). Here we only
	// verify that NewTransport gets to the AddTrack request — i.e., the
	// PCs were created without error and the publish flow started.
	join := canonicalJoin()
	join.SubscriberPrimary = true

	gotAddTrack := make(chan *livekit.AddTrackRequest, 1)
	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		for {
			req, err := readClientSignal(conn)
			if err != nil {
				return
			}
			if at := req.GetAddTrack(); at != nil {
				select {
				case gotAddTrack <- at:
				default:
				}
			}
			if req.GetLeave() != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tr, err := NewTransport(ctx, Config{Log: slog.Default()},
		SignalConfig{URL: srv.wsURL(), Token: "tok"}, PeerConfig{}, Callbacks{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close(livekit.DisconnectReason_CLIENT_INITIATED) })

	select {
	case at := <-gotAddTrack:
		if at.GetType() != livekit.TrackType_AUDIO {
			t.Errorf("track type = %v, want AUDIO", at.GetType())
		}
		if at.GetCid() == "" {
			t.Error("CID empty")
		}
		if at.GetName() != "voice" {
			t.Errorf("track name = %q, want %q", at.GetName(), "voice")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AddTrack request not seen")
	}
}

func TestTransport_AudioWriterAcceptsAndDoesNotBlock(t *testing.T) {
	// Verify AudioWriter is usable: write a frame, confirm no error.
	// We do this by constructing a Transport whose Connect succeeded.
	join := canonicalJoin()
	join.SubscriberPrimary = true

	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		_, _ = readClientSignal(conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tr, err := NewTransport(ctx, Config{Log: slog.Default()},
		SignalConfig{URL: srv.wsURL(), Token: "tok"}, PeerConfig{}, Callbacks{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close(livekit.DisconnectReason_CLIENT_INITIATED) })

	frame := make([]byte, tr.cfg.FrameBytesPCM())
	n, err := tr.AudioWriter().Write(frame)
	if err != nil || n != len(frame) {
		t.Errorf("Write = (%d, %v), want (%d, nil)", n, err, len(frame))
	}
}

func TestTransport_CloseIsIdempotent(t *testing.T) {
	join := canonicalJoin()
	join.SubscriberPrimary = true

	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		_, _ = readClientSignal(conn)
	})

	tr, err := NewTransport(context.Background(), Config{Log: slog.Default()},
		SignalConfig{URL: srv.wsURL(), Token: "tok"}, PeerConfig{}, Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(livekit.DisconnectReason_CLIENT_INITIATED); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := tr.Close(livekit.DisconnectReason_CLIENT_INITIATED); err != nil {
		t.Errorf("second Close: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after Close")
	}
}
