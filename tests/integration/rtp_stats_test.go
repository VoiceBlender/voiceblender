//go:build integration

package integration

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
)

// TestRTPStats_InboundAccounting sends a raw PCMU stream that crosses a
// sequence wrap, loses one packet, interleaves STUN, changes SSRC, and then
// restarts its sequence and timestamps under the same SSRC. The leg must count
// the RTP alone, report the one loss, and keep the jitter at loopback level.
func TestRTPStats_InboundAccounting(t *testing.T) {
	inst := newTestInstance(t, "rtp-stats")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	offer := srtpOffer(conn.LocalAddr().(*net.UDPAddr).Port, "RTP/AVP")
	dialog, inbound := answerInboundCall(t, ctx, inst,
		sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, offer)
	if err := dialog.Ack(ctx); err != nil {
		t.Fatal(err)
	}
	answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
	if err != nil {
		t.Fatal(err)
	}
	ra := answer.PrimaryStream()
	dst := &net.UDPAddr{IP: net.ParseIP(ra.RemoteIP), Port: ra.RemotePort}

	rawLeg, ok := inst.legMgr.Get(inbound.ID)
	if !ok {
		t.Fatalf("inbound leg %s not found", inbound.ID)
	}
	sipLeg, ok := rawLeg.(*leg.SIPLeg)
	if !ok {
		t.Fatalf("inbound leg is %T, want *leg.SIPLeg", rawLeg)
	}

	stun := []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xa4, 0x42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	payload := bytes.Repeat([]byte{0xff}, 160)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	send := func(ssrc uint32, seq uint16, ts uint32) {
		<-tick.C
		raw, err := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 0, SSRC: ssrc, SequenceNumber: seq, Timestamp: ts},
			Payload: payload,
		}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.WriteToUDP(raw, dst); err != nil {
			t.Fatal(err)
		}
	}

	// SSRC 1: 100 packets across the wrap, one lost, STUN every tenth.
	for i := 0; i < 100; i++ {
		if i%10 == 5 {
			if _, err := conn.WriteToUDP(stun, dst); err != nil {
				t.Fatal(err)
			}
		}
		if i == 50 {
			<-tick.C
			continue
		}
		send(1, uint16(65_500+i), uint32(i)*160)
	}
	// SSRC 2: unrelated sequence and timestamps.
	for i := 0; i < 50; i++ {
		send(2, uint16(1000+i), 1<<30+uint32(i)*160)
	}
	// SSRC 2 restarts: its first packet waits for confirmation and is not counted.
	for i := 0; i < 25; i++ {
		send(2, uint16(30_000+i), 3<<29+uint32(i)*160)
	}

	const wantReceived = 99 + 50 + 24
	deadline := time.Now().Add(3 * time.Second)
	var st leg.RTPStats
	for {
		st = sipLeg.RTPStats()
		if st.PacketsReceived >= wantReceived || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("stats %+v", st)
	if st.PacketsReceived != wantReceived {
		t.Errorf("packets received = %d, want %d", st.PacketsReceived, wantReceived)
	}
	if st.PacketsLost != 1 {
		t.Errorf("packets lost = %d, want 1", st.PacketsLost)
	}
	if st.JitterMs > 30 {
		t.Errorf("jitter %.1f ms on loopback; a sender change leaked into it", st.JitterMs)
	}
	hangup(t, inst, inbound.ID)
}
