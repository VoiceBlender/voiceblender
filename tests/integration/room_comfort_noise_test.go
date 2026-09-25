//go:build integration

package integration

import (
	"net/http"
	"testing"
)

func roomComfortNoise(t *testing.T, inst *testInstance, id string) bool {
	t.Helper()
	r, ok := inst.roomMgr.Get(id)
	if !ok {
		t.Fatalf("room %s not found", id)
	}
	return r.Mixer().ComfortNoiseEnabled()
}

func TestRoomCreate_ComfortNoiseOverride(t *testing.T) {
	inst := newTestInstance(t, "cn")

	cases := []struct {
		id   string
		body map[string]interface{}
		want bool
	}{
		{"cn-default", map[string]interface{}{"id": "cn-default"}, true},
		{"cn-off", map[string]interface{}{"id": "cn-off", "comfort_noise": false}, false},
		{"cn-on", map[string]interface{}{"id": "cn-on", "comfort_noise": true}, true},
	}
	for _, tc := range cases {
		resp := httpPost(t, inst.baseURL()+"/v1/rooms", tc.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: status %d", tc.id, resp.StatusCode)
		}
		if got := roomComfortNoise(t, inst, tc.id); got != tc.want {
			t.Errorf("%s: comfort noise = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestRoomCreate_ComfortNoiseOverridesServerDefault(t *testing.T) {
	inst := newTestInstance(t, "cnd")
	inst.roomMgr.SetComfortNoiseEnabled(false)

	resp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{"id": "cn-inherit"})
	resp.Body.Close()
	resp = httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{"id": "cn-forced", "comfort_noise": true})
	resp.Body.Close()

	if roomComfortNoise(t, inst, "cn-inherit") {
		t.Error("cn-inherit: comfort noise on, want server default (off)")
	}
	if !roomComfortNoise(t, inst, "cn-forced") {
		t.Error("cn-forced: comfort noise off, want on")
	}
}

func TestVSI_CreateRoom_ComfortNoiseOff(t *testing.T) {
	inst := newTestInstance(t, "cnvsi")
	conn := dialVSI(t, inst)
	defer conn.Close()

	f := vsiSend(t, conn, "create_room", "r1", map[string]interface{}{"id": "cn-vsi", "comfort_noise": false})
	if f.Type != "create_room.result" {
		t.Fatalf("create_room: got frame %+v", f)
	}
	if roomComfortNoise(t, inst, "cn-vsi") {
		t.Error("comfort noise on after create_room with comfort_noise=false")
	}
}
