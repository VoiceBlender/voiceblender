package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

type statsLeg struct {
	apiMockLeg
	stats leg.RTPStats
}

func (l *statsLeg) RTPStats() leg.RTPStats { return l.stats }

func TestDisconnectData_OutboundQuality(t *testing.T) {
	inbound := leg.RTPStats{PacketsReceived: 100, PacketsLost: 1, JitterMs: 2, MOSScore: 4.3}

	l := &statsLeg{apiMockLeg: apiMockLeg{id: "a", createdAt: time.Now()}, stats: inbound}
	b, _ := json.Marshal(disconnectData(l, "remote_bye"))
	if strings.Contains(string(b), "outbound") {
		t.Errorf("outbound present without an RTCP report: %s", b)
	}

	inbound.Outbound = &leg.OutboundRTPStats{PacketsSent: 250, PacketsLost: 4, JitterMs: 1.5, RTTMs: 42.1}
	l.stats = inbound
	d := disconnectData(l, "remote_bye")
	o := d.Quality.Outbound
	if o == nil || o.PacketsSent != 250 || o.PacketsLost != 4 || o.JitterMs != 1.5 || o.RTTMs != 42.1 {
		t.Fatalf("outbound = %+v", o)
	}
	b, _ = json.Marshal(d)
	if !strings.Contains(string(b), `"outbound":{"rtp_packets_sent":250,"rtp_packets_lost":4,"rtp_jitter_ms":1.5,"rtt_ms":42.1}`) {
		t.Errorf("json = %s", b)
	}
}
