//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/thesyncim/gopus"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// opusBitrateCase is what a peer advertises and what VoiceBlender must send.
type opusBitrateCase struct {
	name    string
	mutate  func(*config.Config)
	fmtp    string // extra Opus fmtp parameters the peer advertises
	minKbps float64
	maxKbps float64
	maxBW   gopus.Bandwidth // widest bandwidth VB may use
	reachBW bool            // VB must also use maxBW
}

func opusBitrateCases(setBitrate func(*config.Config, int)) []opusBitrateCase {
	return []opusBitrateCase{
		{name: "default", minKbps: 16, maxKbps: 32, maxBW: gopus.BandwidthFullband, reachBW: true},
		{name: "configured", mutate: func(c *config.Config) { setBitrate(c, 12000) },
			minKbps: 8, maxKbps: 15, maxBW: gopus.BandwidthFullband},
		{name: "peer_maxaveragebitrate", fmtp: "maxaveragebitrate=10000",
			minKbps: 6, maxKbps: 13, maxBW: gopus.BandwidthFullband},
		{name: "peer_maxplaybackrate", fmtp: "maxplaybackrate=8000",
			minKbps: 16, maxKbps: 32, maxBW: gopus.BandwidthNarrowband, reachBW: true},
		{name: "peer_caps_both", mutate: func(c *config.Config) { setBitrate(c, 32000) },
			fmtp:    "maxaveragebitrate=16000;maxplaybackrate=16000",
			minKbps: 11, maxKbps: 20, maxBW: gopus.BandwidthWideband, reachBW: true},
	}
}

// feedRichTone writes 48 kHz PCM with energy up to 14 kHz into w in 20 ms
// frames, so the encoder has a reason to use its whole bitrate and bandwidth.
func feedRichTone(ctx context.Context, w io.Writer) {
	frame := make([]byte, 960*2)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for n := 0; ; {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for i := 0; i < 960; i, n = i+1, n+1 {
			x := float64(n) / 48000
			v := 4000*math.Sin(2*math.Pi*300*x) + 3000*math.Sin(2*math.Pi*3100*x) +
				2000*math.Sin(2*math.Pi*7000*x) + 2000*math.Sin(2*math.Pi*14000*x)
			binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(v)))
		}
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
}

// opusCollector gathers the Opus payloads VB sends once the tone is flowing.
type opusCollector struct {
	mu       sync.Mutex
	start    time.Time
	payloads [][]byte
}

func (c *opusCollector) add(payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Skip the encoder's ramp-up and the silence frames sent before the tone.
	if c.start.IsZero() || time.Since(c.start) < 300*time.Millisecond || len(payload) <= 8 {
		return
	}
	c.payloads = append(c.payloads, append([]byte(nil), payload...))
}

func (c *opusCollector) begin() {
	c.mu.Lock()
	c.start = time.Now()
	c.mu.Unlock()
}

func (c *opusCollector) check(t *testing.T, tc opusBitrateCase) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloads) < 50 {
		t.Fatalf("received %d voiced Opus packets, want at least 50", len(c.payloads))
	}
	total := 0
	var widest gopus.Bandwidth
	for _, p := range c.payloads {
		total += len(p)
		if bw := gopus.ParseTOC(p[0]).Bandwidth; bw > widest {
			widest = bw
		}
	}
	kbps := float64(total*8) / (float64(len(c.payloads)) * 0.02) / 1000
	t.Logf("%d packets, %.1f kbps, widest bandwidth %v", len(c.payloads), kbps, widest)
	if kbps < tc.minKbps || kbps > tc.maxKbps {
		t.Errorf("VB sent %.1f kbps, want %.0f..%.0f", kbps, tc.minKbps, tc.maxKbps)
	}
	if widest > tc.maxBW || (tc.reachBW && widest != tc.maxBW) {
		t.Errorf("widest bandwidth VB used = %v, want %v", widest, tc.maxBW)
	}
}

// TestOpusBitrate_SIP calls an instance from a bare SIP peer offering Opus and
// measures what the leg sends: SIP_OPUS_BITRATE, capped by the peer's
// maxaveragebitrate, and no wider than its maxplaybackrate.
func TestOpusBitrate_SIP(t *testing.T) {
	for _, tc := range opusBitrateCases(func(c *config.Config, bps int) { c.SIPOpusBitrate = bps }) {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstanceFull(t, "opus-sip-"+tc.name, tc.mutate, []codec.CodecType{codec.CodecOpus})

			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			rtpmap := "111 opus/48000/2"
			if tc.fmtp != "" {
				rtpmap += "\r\na=fmtp:111 " + tc.fmtp
			}
			recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}
			offer := sdpOffer("127.0.0.1", conn.LocalAddr().(*net.UDPAddr).Port, "111", rtpmap)
			dialog, inbound := answerInboundCall(t, ctx, inst, recipient, offer)
			if err := dialog.Ack(ctx); err != nil {
				t.Fatalf("ACK: %v", err)
			}
			waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
			defer hangup(t, inst, inbound.ID)

			l, ok := inst.legMgr.Get(inbound.ID)
			if !ok {
				t.Fatalf("leg %s not found", inbound.ID)
			}
			var col opusCollector
			go func() {
				buf := make([]byte, 1500)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					var pkt rtp.Packet
					if pkt.Unmarshal(buf[:n]) == nil && pkt.PayloadType == 111 {
						col.add(pkt.Payload)
					}
				}
			}()
			col.begin()
			go feedRichTone(ctx, l.AudioWriter())
			time.Sleep(2500 * time.Millisecond)
			col.check(t, tc)
		})
	}
}

// TestOpusBitrate_WebRTC is TestOpusBitrate_SIP for a WebRTC client:
// WEBRTC_OPUS_BITRATE, capped by the fmtp in the client's offer.
func TestOpusBitrate_WebRTC(t *testing.T) {
	for _, tc := range opusBitrateCases(func(c *config.Config, bps int) { c.WebRTCOpusBitrate = bps }) {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstanceFull(t, "opus-webrtc-"+tc.name, tc.mutate, nil)

			clientPC, err := loopbackWebRTCAPI().NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatalf("client NewPeerConnection: %v", err)
			}
			defer clientPC.Close()
			if _, err := clientPC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
				webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
				t.Fatal(err)
			}

			var col opusCollector
			clientPC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				for {
					pkt, _, err := track.ReadRTP()
					if err != nil {
						return
					}
					col.add(pkt.Payload)
				}
			})
			candCh := make(chan webrtc.ICECandidateInit, 16)
			clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
				if c == nil {
					close(candCh)
					return
				}
				select {
				case candCh <- c.ToJSON():
				default:
				}
			})
			connected := make(chan struct{}, 1)
			clientPC.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
				if s == webrtc.PeerConnectionStateConnected {
					select {
					case connected <- struct{}{}:
					default:
					}
				}
			})

			offer, err := clientPC.CreateOffer(nil)
			if err != nil {
				t.Fatalf("create offer: %v", err)
			}
			if err := clientPC.SetLocalDescription(offer); err != nil {
				t.Fatalf("set local desc: %v", err)
			}
			sdp := clientPC.LocalDescription().SDP
			if tc.fmtp != "" {
				if !strings.Contains(sdp, "a=fmtp:111 ") {
					t.Fatalf("client offer has no Opus fmtp:\n%s", sdp)
				}
				sdp = strings.Replace(sdp, "a=fmtp:111 ", "a=fmtp:111 "+tc.fmtp+";", 1)
			}

			resp := httpPost(t, inst.baseURL()+"/v1/webrtc/offer", map[string]string{"sdp": sdp})
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Fatalf("webrtc_offer: status %d", resp.StatusCode)
			}
			var res struct {
				LegID string `json:"leg_id"`
				SDP   string `json:"sdp"`
			}
			decodeJSON(t, resp, &res)
			if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer, SDP: res.SDP,
			}); err != nil {
				t.Fatalf("client SetRemoteDescription: %v", err)
			}
			trickleICEUntilConnected(t, inst, clientPC, res.LegID, candCh, connected, 10*time.Second)
			inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
				return e.Data.GetLegID() == res.LegID
			}, 5*time.Second)
			defer httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), res.LegID))

			l, ok := inst.legMgr.Get(res.LegID)
			if !ok {
				t.Fatalf("leg %s not found", res.LegID)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			col.begin()
			go feedRichTone(ctx, l.AudioWriter())
			time.Sleep(2500 * time.Millisecond)
			col.check(t, tc)
		})
	}
}
