//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

func createActiveTrunk(t *testing.T, baseURL string, regPort int, aor, appID string) string {
	t.Helper()
	resp, body := createTrunkRequest(t, baseURL, map[string]interface{}{
		"type":   "sip_register",
		"app_id": appID,
		"sip_register": map[string]interface{}{
			"registrar_uri": fmt.Sprintf("sip:127.0.0.1:%d", regPort),
			"aor":           aor,
			"password":      "secret",
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST trunk status = %d, body=%s", resp.StatusCode, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)
	waitForTrunkStatus(t, baseURL, id, "active", 3*time.Second)
	return id
}

// sendTrunkInvite sends an INVITE addressed to the given registered Contact
// user from a 127.0.0.1 socket, which matches the fake registrar's host.
func sendTrunkInvite(t *testing.T, cli *rawSIPClient, sipPort int, user string, extra ...sip.Header) {
	t.Helper()
	offerSDP := []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 1 IN IP4 %s", cli.host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", cli.host),
		"t=0 0",
		"m=audio 40030 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"",
	}, "\r\n"))

	target := sip.Uri{Scheme: "sip", User: user, Host: "127.0.0.1", Port: sipPort}
	req := sip.NewRequest(sip.INVITE, target)
	fromHdr := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "carrier", Host: cli.host, Port: cli.port}, Params: sip.NewParams()}
	fromHdr.Params.Add("tag", sip.GenerateTagN(8))
	req.AppendHeader(fromHdr)
	req.AppendHeader(&sip.ToHeader{Address: target, Params: sip.NewParams()})
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: cli.host, Port: cli.port}})
	cid := sip.CallIDHeader(sip.GenerateTagN(16))
	req.AppendHeader(&cid)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.INVITE})
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	for _, h := range extra {
		req.AppendHeader(h)
	}
	req.SetBody(offerSDP)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	go func() { _, _ = cli.client.Do(ctx, req) }()
}

// TestTrunk_SIPRegister_InboundInheritsAppID pins that an inbound call arriving
// over a registered trunk carries the trunk's app_id on leg.ringing, that the
// trunk's app_id beats a caller-supplied X-App-ID, and that trunks sharing a
// registrar are told apart by the Request-URI user.
func TestTrunk_SIPRegister_InboundInheritsAppID(t *testing.T) {
	inst := newTestInstance(t, "trunk-inbound-app")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{})

	aliceID := createActiveTrunk(t, inst.baseURL(), reg.port, "sip:alice@vb.test", "app-alice")
	bobID := createActiveTrunk(t, inst.baseURL(), reg.port, "sip:bob@vb.test", "app-bob")

	cli := newRawSIPClient(t, "trunk-inbound-ua")

	cases := []struct {
		user      string
		wantTrunk string
		wantApp   string
	}{
		{"alice", aliceID, "app-alice"},
		{"bob", bobID, "app-bob"},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			sendTrunkInvite(t, cli, inst.sipPort, tc.user, sip.NewHeader("X-App-ID", "spoofed"))

			ev := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
				d, ok := e.Data.(*events.LegRingingData)
				return ok && strings.Contains(d.To, tc.user)
			}, 5*time.Second)
			d := ev.Data.(*events.LegRingingData)
			if d.TrunkID != tc.wantTrunk {
				t.Errorf("trunk_id = %q, want %q", d.TrunkID, tc.wantTrunk)
			}
			if d.AppID != tc.wantApp {
				t.Errorf("app_id = %q, want %q", d.AppID, tc.wantApp)
			}

			resp := httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID)
			resp.Body.Close()
		})
	}
}
