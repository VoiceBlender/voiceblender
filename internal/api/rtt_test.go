package api

import (
	"net/http"
	"testing"
	"time"
)

func TestSendRTT_LegNotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/legs/nope/rtt", `{"text":"hello"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d want %d", w.Code, http.StatusNotFound)
	}
}

func TestSendRTT_EmptyText(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", createdAt: time.Now()}
	s.LegMgr.Add(l)
	w := doRequest(s, http.MethodPost, "/v1/legs/leg-1/rtt", `{"text":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d want %d", w.Code, http.StatusBadRequest)
	}
}

func TestSendRTT_NotNegotiated(t *testing.T) {
	// apiMockLeg always returns RTTNegotiated()=false.
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", createdAt: time.Now()}
	s.LegMgr.Add(l)
	w := doRequest(s, http.MethodPost, "/v1/legs/leg-1/rtt", `{"text":"hi"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d want %d", w.Code, http.StatusConflict)
	}
}

func TestAcceptRejectRTT_LegNotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/legs/nope/rtt/accept", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("accept: got %d want %d", w.Code, http.StatusNotFound)
	}
	w = doRequest(s, http.MethodPost, "/v1/legs/nope/rtt/reject", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("reject: got %d want %d", w.Code, http.StatusNotFound)
	}
}
