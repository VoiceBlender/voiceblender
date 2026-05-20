//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// httpPutJSON sends a PUT with a JSON body and returns the response. The
// caller must close resp.Body.
func httpPutJSON(t *testing.T, url string, body interface{}) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	return resp
}

// sortMatrix sorts every row of a matrix in place so reflect.DeepEqual is
// stable across map iteration orders.
func sortMatrix(m map[string][]string) map[string][]string {
	for k, v := range m {
		sort.Strings(v)
		m[k] = v
	}
	return m
}

// TestRoutingMatrix_HTTP_PutGetPatch exercises the REST endpoints for the
// per-room audio routing matrix and asserts that the corresponding events
// are published on the bus.
func TestRoutingMatrix_HTTP_PutGetPatch(t *testing.T) {
	inst := newTestInstance(t, "rt")
	createRoom(t, inst, "rm", 16000)

	// PUT — replace the matrix.
	putResp := httpPutJSON(t, inst.baseURL()+"/v1/rooms/rm/routing",
		map[string]interface{}{
			"matrix": map[string][]string{
				"customer":   {"agent"},
				"agent":      {"customer", "supervisor"},
				"supervisor": {"customer", "agent"},
			},
		})
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /routing: status %d", putResp.StatusCode)
	}
	var putView struct {
		Matrix map[string][]string `json:"matrix"`
	}
	decodeJSON(t, putResp, &putView)
	got := sortMatrix(putView.Matrix)
	want := sortMatrix(map[string][]string{
		"customer":   {"agent"},
		"agent":      {"customer", "supervisor"},
		"supervisor": {"customer", "agent"},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PUT response matrix = %v, want %v", got, want)
	}

	inst.collector.waitForMatch(t, events.RoomRoutingChanged, func(e events.Event) bool {
		d, ok := e.Data.(*events.RoomRoutingChangedData)
		return ok && d.RoomID == "rm" && d.Reason == "set"
	}, 2*time.Second)

	// GET — round-trip.
	getResp := httpGet(t, inst.baseURL()+"/v1/rooms/rm/routing")
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /routing: status %d", getResp.StatusCode)
	}
	var getView struct {
		Matrix map[string][]string `json:"matrix"`
	}
	decodeJSON(t, getResp, &getView)
	if !reflect.DeepEqual(sortMatrix(getView.Matrix), want) {
		t.Errorf("GET matrix = %v, want %v", sortMatrix(getView.Matrix), want)
	}

	// PATCH — clear the customer row (back to full mesh for customers).
	patchResp := httpPatchJSON(t, inst.baseURL()+"/v1/rooms/rm/routing",
		map[string]interface{}{
			"updates": []map[string]interface{}{
				{"listener_role": "customer", "sources": nil},
			},
		})
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /routing: status %d", patchResp.StatusCode)
	}
	var patchView struct {
		Matrix map[string][]string `json:"matrix"`
	}
	decodeJSON(t, patchResp, &patchView)
	if _, present := patchView.Matrix["customer"]; present {
		t.Errorf("after PATCH clearing customer row, key should be absent; got %v", patchView.Matrix)
	}

	inst.collector.waitForMatch(t, events.RoomRoutingChanged, func(e events.Event) bool {
		d, ok := e.Data.(*events.RoomRoutingChangedData)
		return ok && d.RoomID == "rm" && d.Reason == "update"
	}, 2*time.Second)
}

// TestRoutingMatrix_RoomMissing returns 404 on every routing endpoint when
// the room does not exist.
func TestRoutingMatrix_RoomMissing(t *testing.T) {
	inst := newTestInstance(t, "rt404")

	cases := []struct {
		name string
		do   func() *http.Response
	}{
		{"GET", func() *http.Response { return httpGet(t, inst.baseURL()+"/v1/rooms/ghost/routing") }},
		{"PUT", func() *http.Response {
			return httpPutJSON(t, inst.baseURL()+"/v1/rooms/ghost/routing",
				map[string]interface{}{"matrix": map[string][]string{}})
		}},
		{"PATCH", func() *http.Response {
			return httpPatchJSON(t, inst.baseURL()+"/v1/rooms/ghost/routing",
				map[string]interface{}{"updates": []map[string]interface{}{}})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := c.do()
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s missing-room: status %d, want 404", c.name, resp.StatusCode)
			}
		})
	}
}
