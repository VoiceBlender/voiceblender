//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws/wsutil"
)

type instanceStatus struct {
	Status          string `json:"status"`
	DrainSource     string `json:"drain_source"`
	ExitWhenDrained bool   `json:"exit_when_drained"`
	DrainDeadline   string `json:"drain_deadline"`
	Legs            int    `json:"legs"`
	Rooms           int    `json:"rooms"`
}

func getStatus(t *testing.T, inst *testInstance, path string) (int, instanceStatus) {
	t.Helper()
	resp := httpGet(t, inst.baseURL()+path)
	defer resp.Body.Close()
	var st instanceStatus
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("GET %s: decode %q: %v", path, body, err)
	}
	return resp.StatusCode, st
}

func waitStatus(t *testing.T, inst *testInstance, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		_, st := getStatus(t, inst, "/health/live")
		if st.Status == want {
			return
		}
		last = st.Status
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: status %q, want %q within %v", inst.name, last, want, timeout)
}

func startDrain(t *testing.T, inst *testInstance, body interface{}) instanceStatus {
	t.Helper()
	resp := httpPost(t, inst.baseURL()+"/v1/drain", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/drain: status %d", resp.StatusCode)
	}
	var st instanceStatus
	decodeJSON(t, resp, &st)
	return st
}

func waitDisconnectReason(t *testing.T, inst *testInstance, legID string, timeout time.Duration) string {
	t.Helper()
	e := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, timeout)
	return e.Data.(*events.LegDisconnectedData).CDR.Reason
}

func dialLeg(t *testing.T, from, to *testInstance) string {
	t.Helper()
	resp := httpPost(t, from.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", to.sipPort),
		"codecs": []string{"PCMU"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg on %s: status %d", from.name, resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)
	return lv.ID
}

func withDrainConfig(grace, retryAfter int) func(*config.Config) {
	return func(c *config.Config) {
		c.DrainEmptyGraceSeconds = grace
		c.DrainRetryAfterSeconds = retryAfter
	}
}

// A drain refuses new calls at SIP level, keeps the live call and the API
// working, turns drained once the last leg has gone, and can be cancelled.
func TestDrain_RefusesNewCallsKeepsExisting(t *testing.T) {
	instA := newTestInstance(t, "drain-a")
	instB := newTestInstanceWithOpts(t, "drain-b", withDrainConfig(1, 30))
	waitStatus(t, instB, "ready", 3*time.Second)

	outID, inID := establishCall(t, instA, instB)

	st := startDrain(t, instB, nil)
	if st.Status != "draining" || st.DrainSource != "api" || st.ExitWhenDrained || st.Legs != 1 {
		t.Fatalf("drain status = %+v", st)
	}
	if code, _ := getStatus(t, instB, "/health/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("ready while draining = %d, want 503", code)
	}
	if code, _ := getStatus(t, instB, "/health/live"); code != http.StatusOK {
		t.Fatalf("live while draining = %d, want 200", code)
	}

	refusedID := dialLeg(t, instA, instB)
	if reason := waitDisconnectReason(t, instA, refusedID, 5*time.Second); reason != "service_unavailable" {
		t.Fatalf("new call while draining ended with %q, want service_unavailable", reason)
	}

	waitForLegState(t, instA.baseURL(), outID, "connected", time.Second)
	waitForLegState(t, instB.baseURL(), inID, "connected", time.Second)

	roomResp := httpPost(t, instB.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room while draining with a live leg = %d, want 201", roomResp.StatusCode)
	}
	roomResp.Body.Close()

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID)).Body.Close()
	waitDisconnectReason(t, instB, inID, 5*time.Second)
	waitStatus(t, instB, "drained", 3*time.Second)

	refused := httpPost(t, instB.baseURL()+"/v1/rooms", map[string]interface{}{})
	if refused.StatusCode != http.StatusServiceUnavailable || refused.Header.Get("Retry-After") != "30" {
		t.Fatalf("create room when drained = %d Retry-After %q, want 503 / 30", refused.StatusCode, refused.Header.Get("Retry-After"))
	}
	refused.Body.Close()

	cancel := httpDelete(t, instB.baseURL()+"/v1/drain")
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /v1/drain = %d", cancel.StatusCode)
	}
	cancel.Body.Close()
	waitStatus(t, instB, "ready", time.Second)
	establishCall(t, instA, instB)

	var seen []string
	for _, e := range instB.collector.matchAll(events.InstanceStatusChanged, func(events.Event) bool { return true }) {
		d := e.Data.(*events.InstanceStatusChangedData)
		seen = append(seen, d.From+">"+d.To)
	}
	want := "ready>draining draining>drained drained>ready"
	if strings.Join(seen, " ") != want {
		t.Fatalf("instance.status_changed = %v, want %s", seen, want)
	}
}

// The application hangs up one leg and dials the next within the grace
// period; the drain must not count as drained in between.
func TestDrain_GraceCoversRedial(t *testing.T) {
	instA := newTestInstance(t, "grace-a")
	instB := newTestInstanceWithOpts(t, "grace-b", withDrainConfig(3, 0))
	waitStatus(t, instB, "ready", 3*time.Second)

	_, inID := establishCall(t, instA, instB)
	startDrain(t, instB, nil)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inID)).Body.Close()
	waitDisconnectReason(t, instB, inID, 5*time.Second)

	nextID := dialLeg(t, instB, instA)
	time.Sleep(4 * time.Second)
	if _, st := getStatus(t, instB, "/health/live"); st.Status != "draining" {
		t.Fatalf("status after redial = %q, want draining", st.Status)
	}
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), nextID)).Body.Close()
	waitStatus(t, instB, "drained", 6*time.Second)
}

// The ordered shutdown ends calls through the normal teardown: the far end
// gets a BYE, leg.disconnected says server_shutdown, and the room recording
// is finished instead of left as a temp file.
func TestShutdown_EndsCallsAndFinishesRecordings(t *testing.T) {
	instA := newTestInstance(t, "shutdown-a")
	instB := newTestInstance(t, "shutdown-b")

	roomID, legID, file := startRoomRecordingWithCall(t, instA, instB, "shutdown-app")
	var inID string
	for _, l := range instB.legMgr.List() {
		inID = l.ID()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := instA.apiSrv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if reason := waitDisconnectReason(t, instA, legID, time.Second); reason != "server_shutdown" {
		t.Fatalf("leg.disconnected reason = %q, want server_shutdown", reason)
	}
	if reason := waitDisconnectReason(t, instB, inID, 5*time.Second); reason != "remote_bye" {
		t.Fatalf("far end reason = %q, want remote_bye", reason)
	}
	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == roomID
	}, time.Second)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("recording %s missing: %v", file, err)
	}
	tmp, _ := filepath.Glob(filepath.Join(instA.cfg.RecordingDir, "*.rec-*.tmp"))
	tmp2, _ := filepath.Glob(filepath.Join(instA.cfg.RecordingDir, ".rec-*.tmp"))
	if len(tmp)+len(tmp2) != 0 {
		t.Fatalf("temp recording files left: %v %v", tmp, tmp2)
	}

	if code, st := getStatus(t, instA, "/health/live"); code != http.StatusServiceUnavailable || st.Status != "stopping" {
		t.Fatalf("live after shutdown = %d %q, want 503 stopping", code, st.Status)
	}
	refusedID := dialLeg(t, instB, instA)
	if reason := waitDisconnectReason(t, instB, refusedID, 5*time.Second); reason != "service_unavailable" {
		t.Fatalf("call to a stopping instance ended with %q, want service_unavailable", reason)
	}
}

// A drain deadline that passes with a call still up requests the ordered
// shutdown, which then ends the call.
func TestDrain_TimeoutRequestsShutdown(t *testing.T) {
	instA := newTestInstance(t, "timeout-a")
	instB := newTestInstance(t, "timeout-b")
	outID, inID := establishCall(t, instA, instB)

	st := startDrain(t, instB, map[string]interface{}{"timeout_seconds": 1})
	if st.DrainDeadline == "" {
		t.Fatal("drain_deadline not set")
	}
	select {
	case <-instB.apiSrv.Lifecycle.ShutdownRequested():
	case <-time.After(5 * time.Second):
		t.Fatal("drain timeout did not request shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := instB.apiSrv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if reason := waitDisconnectReason(t, instB, inID, time.Second); reason != "server_shutdown" {
		t.Fatalf("reason = %q, want server_shutdown", reason)
	}
	if reason := waitDisconnectReason(t, instA, outID, 5*time.Second); reason != "remote_bye" {
		t.Fatalf("far end reason = %q, want remote_bye", reason)
	}
}

// An app_id-filtered VSI client still gets instance.status_changed, and the
// drain commands work over VSI.
func TestDrain_VSI(t *testing.T) {
	inst := newTestInstance(t, "drain-vsi")
	waitStatus(t, inst, "ready", 3*time.Second)
	conn := dialVSIFiltered(t, inst, "^acme$")
	defer conn.Close()

	send := func(typ, reqID string) {
		msg, _ := json.Marshal(map[string]interface{}{"type": typ, "request_id": reqID})
		if err := wsutil.WriteClientText(conn, msg); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
	}
	// await reads frames until both the result for reqID and an
	// instance.status_changed event to wantTo have arrived, in any order.
	await := func(reqID, wantTo string) map[string]interface{} {
		var result map[string]interface{}
		sawEvent := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && (result == nil || !sawEvent) {
			conn.SetReadDeadline(deadline)
			data, err := wsutil.ReadServerText(conn)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var f map[string]interface{}
			if json.Unmarshal(data, &f) != nil {
				continue
			}
			if f["request_id"] == reqID {
				result = f
			}
			if f["type"] == "instance.status_changed" && f["to"] == wantTo {
				sawEvent = true
			}
		}
		if result == nil || !sawEvent {
			t.Fatalf("%s: result=%v sawEvent=%v", reqID, result, sawEvent)
		}
		return result
	}

	send("start_drain", "r1")
	res := await("r1", "drained")
	if d, _ := res["data"].(map[string]interface{}); d["status"] != "drained" {
		t.Fatalf("start_drain result = %v", res)
	}
	send("cancel_drain", "r2")
	res = await("r2", "ready")
	if d, _ := res["data"].(map[string]interface{}); d["status"] != "ready" {
		t.Fatalf("cancel_drain result = %v", res)
	}
}

// A sip_register trunk unregisters on drain and registers again when the
// drain is cancelled; a trunk with unregister_on_drain=false stays.
func TestDrain_TrunkUnregisterOnDrain(t *testing.T) {
	registrar := newTestInstance(t, "drain-registrar")
	inst := newTestInstance(t, "drain-trunks")
	waitStatus(t, inst, "ready", 3*time.Second)

	create := func(user string, unregister *bool) {
		spec := map[string]interface{}{
			"registrar_uri": fmt.Sprintf("sip:127.0.0.1:%d", registrar.sipPort),
			"aor":           fmt.Sprintf("sip:%s@127.0.0.1", user),
			"password":      "secret",
		}
		if unregister != nil {
			spec["unregister_on_drain"] = *unregister
		}
		resp := httpPost(t, inst.baseURL()+"/v1/sip/trunks", map[string]interface{}{"type": "sip_register", "sip_register": spec})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create trunk %s: %d", user, resp.StatusCode)
		}
		resp.Body.Close()
	}
	no := false
	create("alice", nil)
	create("bob", &no)

	active := func(user string, after time.Time) {
		registrar.collector.waitForMatch(t, events.SIPRegistrationActive, func(e events.Event) bool {
			d := e.Data.(*events.SIPRegistrationActiveData)
			return strings.Contains(d.AOR, user+"@") && e.Timestamp.After(after)
		}, 5*time.Second)
	}
	start := time.Now()
	active("alice", start)
	active("bob", start)

	drainAt := time.Now()
	startDrain(t, inst, nil)
	registrar.collector.waitForMatch(t, events.SIPRegistrationExpired, func(e events.Event) bool {
		return strings.Contains(e.Data.(*events.SIPRegistrationExpiredData).AOR, "alice@")
	}, 5*time.Second)
	inst.collector.waitForMatch(t, events.SIPOutboundRegistrationExpired, func(e events.Event) bool {
		d := e.Data.(*events.SIPOutboundRegistrationExpiredData)
		return strings.Contains(d.AOR, "alice@") && d.Reason == "drain"
	}, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	if registrar.collector.hasEvent(events.SIPRegistrationExpired, func(e events.Event) bool {
		return strings.Contains(e.Data.(*events.SIPRegistrationExpiredData).AOR, "bob@")
	}) {
		t.Fatal("trunk with unregister_on_drain=false was unregistered")
	}

	httpDelete(t, inst.baseURL()+"/v1/drain").Body.Close()
	active("alice", drainAt)
}

func TestDrain_Metrics(t *testing.T) {
	inst := newTestInstanceWithMetrics(t, "drain-metrics")
	waitStatus(t, inst, "ready", 3*time.Second)
	if v := parseGaugeValue(t, metricsBody(t, inst.baseURL()), "voiceblender_draining"); v != 0 {
		t.Fatalf("voiceblender_draining = %v before drain", v)
	}
	startDrain(t, inst, nil)
	body := metricsBody(t, inst.baseURL())
	if v := parseGaugeValue(t, body, "voiceblender_draining"); v != 1 {
		t.Fatalf("voiceblender_draining = %v while drained", v)
	}
	if !strings.Contains(body, `voiceblender_instance_status{status="drained"} 1`) {
		t.Fatalf("instance status gauge missing:\n%s", body)
	}
}
