//go:build integration && interop

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

type roomMemberSpec struct {
	dir   string // vb-dials or peer-dials
	codec string
	ptime int
	hz    int
}

type roomMember struct {
	roomMemberSpec
	legID string
	call  peers.Call
}

func (m roomMember) String() string {
	return fmt.Sprintf("%s %s@%dms (%d Hz)", m.dir, m.codec, m.ptime, m.hz)
}

// buildRoom connects one peer call per spec, puts them all in a new room and
// has each play its own tone.
func buildRoom(t *testing.T, inst *testInstance, p peers.Peer, specs []roomMemberSpec) (string, []roomMember) {
	t.Helper()
	resp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", resp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, resp, &rm)

	var members []roomMember
	for _, s := range specs {
		var legID string
		var call peers.Call
		if s.dir == "vb-dials" {
			legID, call = connectVBToPeer(t, inst, p, interopDial{Codecs: []string{s.codec}}, peers.AnswerOpts{Codec: s.codec, Ptime: s.ptime})
		} else {
			call, legID = connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{s.codec}, Ptime: s.ptime}, nil)
		}
		if s.ptime > 0 {
			if got := call.Ptime(); got != s.ptime {
				t.Errorf("%s: peer sends %d ms packets, want %d", s.dir, got, s.ptime)
			}
		}
		add := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", inst.baseURL(), rm.ID), map[string]interface{}{"leg_id": legID})
		add.Body.Close()
		if add.StatusCode != http.StatusOK {
			t.Fatalf("add %s to room: status %d", legID, add.StatusCode)
		}
		inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool { return e.Data.GetLegID() == legID }, 3*time.Second)
		if err := call.PlayTone(s.hz); err != nil {
			t.Fatal(err)
		}
		members = append(members, roomMember{roomMemberSpec: s, legID: legID, call: call})
	}
	return rm.ID, members
}

// assertMix checks every tone in want is present at a comparable level and
// none in absent is: a room member hears everyone but itself.
func assertMix(t *testing.T, label string, s []int16, rate int, want, absent []int) {
	t.Helper()
	win := s
	if skip := rate / 2; len(win) > skip+rate {
		win = win[skip : skip+rate]
	}
	if rms := rmsInt16(win); rms < 300 {
		t.Errorf("%s: silent (rms %.0f)", label, rms)
		return
	}
	var weakest float64 = -1
	levels := map[int]float64{}
	for _, hz := range want {
		levels[hz] = goertzelPower(win, rate, float64(hz))
		if weakest < 0 || levels[hz] < weakest {
			weakest = levels[hz]
		}
	}
	var strongest float64
	for _, hz := range want {
		if levels[hz] > strongest {
			strongest = levels[hz]
		}
	}
	for _, hz := range want {
		if levels[hz] < strongest/20 {
			t.Errorf("%s: %d Hz missing from the mix (%.2g vs strongest %.2g)", label, hz, levels[hz], strongest)
		}
	}
	for _, hz := range absent {
		if p := goertzelPower(win, rate, float64(hz)); p > weakest/50 {
			t.Errorf("%s: own %d Hz tone fed back (%.2g vs weakest wanted %.2g)", label, hz, p, weakest)
		}
	}
}

func othersHz(members []roomMember, self int) []int {
	var out []int
	for i, m := range members {
		if i != self {
			out = append(out, m.hz)
		}
	}
	return out
}

func assertEveryoneHearsEveryoneElse(t *testing.T, members []roomMember) {
	t.Helper()
	for i, m := range members {
		s, rate := recordPeer(t, m.call, 2*time.Second)
		assertMix(t, m.String(), s, rate, othersHz(members, i), []int{m.hz})
	}
}

func TestInterop_Room_MixMinus(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		_, members := buildRoom(t, inst, p, []roomMemberSpec{
			{dir: "vb-dials", codec: "PCMU", hz: 600},
			{dir: "vb-dials", codec: "PCMU", hz: 900},
			{dir: "peer-dials", codec: "PCMU", hz: 1300},
		})
		assertEveryoneHearsEveryoneElse(t, members)
	})
}

// Members on different codecs, and packetization where the peer can send it,
// share one mix; any rate or timing slip shows up as a missing tone or a
// dropout.
func TestInterop_Room_MixedCodecs(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		specs := []roomMemberSpec{
			{dir: "vb-dials", codec: "PCMU", ptime: 20, hz: 600},
			{dir: "peer-dials", codec: "G722", ptime: 30, hz: 900},
			{dir: "vb-dials", codec: "OPUS", ptime: 40, hz: 1300},
		}
		for i := range specs {
			requireCap(t, p.Caps().HasCodec(specs[i].codec), specs[i].codec)
			if !p.Caps().Ptime {
				specs[i].ptime = 0
			}
		}
		inst := newInteropVB(t, "vb", nil)
		_, members := buildRoom(t, inst, p, specs)
		assertEveryoneHearsEveryoneElse(t, members)
		if testing.Short() {
			return
		}
		for _, m := range members {
			s, rate := recordPeer(t, m.call, 10*time.Second)
			assertContinuous(t, m.String(), s, rate)
		}
	})
}

func TestInterop_Room_Recording(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		roomID, members := buildRoom(t, inst, p, []roomMemberSpec{
			{dir: "vb-dials", codec: "PCMU", hz: 600},
			{dir: "peer-dials", codec: "G722", hz: 900},
			{dir: "vb-dials", codec: "OPUS", hz: 1300},
		})
		url := fmt.Sprintf("%s/v1/rooms/%s/record", inst.baseURL(), roomID)
		resp := httpPost(t, url, nil)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("start room recording: status %d", resp.StatusCode)
		}
		time.Sleep(3 * time.Second)
		var rec recordingResponse
		decodeJSON(t, httpDelete(t, url), &rec)
		s, rate := readWAVChannel(t, rec.File, 0)
		assertMix(t, "room recording", s, rate, []int{members[0].hz, members[1].hz, members[2].hz}, nil)
	})
}

func TestInterop_Ptime(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Ptime, "non-20 ms packetization")
		inst := newInteropVB(t, "vb", nil)
		for _, ptime := range []int{30, 40} {
			for _, setup := range interopCallSetups {
				t.Run(fmt.Sprintf("%dms/%s", ptime, setup.name), func(t *testing.T) {
					legID, call := setup.connect(t, inst, p, []string{"PCMU"}, ptime)
					if got := call.Ptime(); got != ptime {
						t.Errorf("peer sends %d ms packets, want %d", got, ptime)
					}
					assertMediaBothWays(t, inst, legID, call)
					assertDTMFBothWaysInterop(t, inst, legID, call)
				})
			}
		}
	})
}
