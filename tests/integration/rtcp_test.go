//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
)

// rtcpReportWait covers the server's first two reports: the first lands within
// 0.75 of the 5 s interval, the next up to 1.5 intervals later.
const rtcpReportWait = 12 * time.Second

func withMux(offer []byte) []byte {
	return bytes.Replace(offer, []byte("a=sendrecv\r\n"), []byte("a=sendrecv\r\na=rtcp-mux\r\n"), 1)
}

// runPeerMedia reads everything the server sends, so its RTCP is processed,
// and sends 20 ms of PCMU silence every 20 ms until the test ends.
func runPeerMedia(t *testing.T, peer *sipmod.RTPSession) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		var pkt rtp.Packet
		for {
			peer.SetReadDeadline(time.Now().Add(30 * time.Second))
			if err := peer.ReadRTPInto(&pkt); err != nil && !errors.Is(err, sipmod.ErrNotRTP) {
				return
			}
		}
	}()
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		payload := bytes.Repeat([]byte{0xff}, 160)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			_ = peer.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 0, SSRC: 0x5eed, SequenceNumber: uint16(i), Timestamp: uint32(i) * 160},
				Payload: payload,
			})
		}
	}()
}

func outboundQuality(t *testing.T, inst *testInstance, legID string) *events.OutboundQuality {
	t.Helper()
	disc := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
	q := disc.Data.(*events.LegDisconnectedData).Quality
	if q == nil {
		t.Fatal("leg.disconnected carries no quality")
	}
	return q.Outbound
}

// TestRTCP_RawPeer exchanges RTCP with a bare RTP peer that runs its own
// reports. Each side measuring a round-trip time proves the other's reports
// arrived, named the right SSRC and echoed its sender reports; on an SDES call
// that is only possible if both sides sent SRTCP under the negotiated keys.
func TestRTCP_RawPeer(t *testing.T) {
	for _, srtp := range []bool{false, true} {
		name := "plain"
		if srtp {
			name = "srtcp"
		}
		t.Run(name, func(t *testing.T) {
			inst := newTestInstance(t, "rtcp-raw")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			peer, err := sipmod.NewRTPSession()
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			offer := srtpOffer(peer.LocalPort(), "RTP/AVP")
			var peerKey sipmod.SDESCrypto
			if srtp {
				if peerKey, err = sipmod.NewSDESCrypto(1, sipmod.SuiteAES128CMSHA1_80); err != nil {
					t.Fatal(err)
				}
				offer = srtpOffer(peer.LocalPort(), "RTP/SAVP", peerKey)
			}

			dialog, inbound := answerInboundCall(t, ctx, inst,
				sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, withMux(offer))
			if !strings.Contains(string(dialog.InviteResponse.Body()), "a=rtcp-mux\r\n") {
				t.Fatalf("answer dropped a=rtcp-mux:\n%s", dialog.InviteResponse.Body())
			}
			answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
			if err != nil {
				t.Fatal(err)
			}
			if err := dialog.Ack(ctx); err != nil {
				t.Fatal(err)
			}
			ra := answer.PrimaryStream()
			if srtp {
				if err := peer.SetSRTP(&peerKey, &ra.Crypto[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := peer.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
				t.Fatal(err)
			}
			peer.SetRTCPMux(true)
			peer.SetClockRate(8000)
			peer.StartRTCP(200 * time.Millisecond)
			runPeerMedia(t, peer)

			deadline := time.Now().Add(rtcpReportWait)
			for {
				if rep, ok := peer.RTCPReport(); ok && rep.RTT > 0 {
					if rep.PacketsLost > 5 {
						t.Errorf("server reports %d of our packets lost on loopback", rep.PacketsLost)
					}
					break
				}
				if time.Now().After(deadline) {
					rep, _ := peer.RTCPReport()
					t.Fatalf("no server report echoing our SR; last %+v", rep)
				}
				time.Sleep(50 * time.Millisecond)
			}

			// The peer's next receiver reports echo the server's sender report.
			time.Sleep(time.Second)
			hangup(t, inst, inbound.ID)
			o := outboundQuality(t, inst, inbound.ID)
			t.Logf("quality.outbound = %+v", o)
			if o == nil || o.PacketsSent == 0 || o.RTTMs <= 0 {
				t.Fatalf("quality.outbound = %+v", o)
			}
		})
	}
}

// expectNoRTCP watches the peer's RTP port for long enough to have seen the
// server's first report.
func expectNoRTCP(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	buf := make([]byte, 1500)
	var rtpSeen int
	conn.SetReadDeadline(time.Now().Add(4500 * time.Millisecond))
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n >= 2 && buf[1] >= 192 && buf[1] <= 223 {
			t.Fatalf("RTCP (PT %d) on the RTP port", buf[1])
		}
		rtpSeen++
	}
	if rtpSeen == 0 {
		t.Fatal("no RTP either; the check proves nothing")
	}
}

// TestRTCP_NotSent covers the cases where no report may reach the RTP port.
func TestRTCP_NotSent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
		mux    bool
	}{
		{"peer does not multiplex", nil, false},
		{"SIP_RTCP off", func(c *config.Config) { c.SIPRTCP = false }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstanceWithOpts(t, "rtcp-off", tc.mutate)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			offer := srtpOffer(conn.LocalAddr().(*net.UDPAddr).Port, "RTP/AVP")
			if tc.mux {
				offer = withMux(offer)
			}
			dialog, inbound := answerInboundCall(t, ctx, inst,
				sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, offer)
			if got := strings.Contains(string(dialog.InviteResponse.Body()), "a=rtcp-mux"); got != tc.mux {
				t.Fatalf("answer a=rtcp-mux = %v, want %v", got, tc.mux)
			}
			if err := dialog.Ack(ctx); err != nil {
				t.Fatal(err)
			}
			expectNoRTCP(t, conn)
			hangup(t, inst, inbound.ID)
		})
	}
}

// TestRTCP_InstanceToInstance checks that two servers report on each other's
// audio: both leg.disconnected events describe the stream that leg sent.
func TestRTCP_InstanceToInstance(t *testing.T) {
	instA := newTestInstance(t, "rtcp-a")
	instB := newTestInstance(t, "rtcp-b")
	out, in := dialSRTP(t, instA, instB, nil)

	// Each side's first report on the other's audio lands within 0.75 of the
	// interval of the call starting.
	time.Sleep(4 * time.Second)
	hangup(t, instA, out)
	for _, side := range []struct {
		inst *testInstance
		leg  string
	}{{instA, out}, {instB, in}} {
		o := outboundQuality(t, side.inst, side.leg)
		if o == nil || o.PacketsSent == 0 {
			t.Errorf("%s: quality.outbound = %+v", side.leg, o)
		}
	}
}
