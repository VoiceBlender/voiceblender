package matrix

import (
	"encoding/json"
	"testing"

	mevent "maunium.net/go/mautrix/event"
)

// canonical m.call.invite content as emitted by Element-Web. Lifetime is an
// integer (ms) and version is the string "1" (mautrix parses both string and
// numeric forms via CallVersion).
const elementWebInvite = `{
  "call_id": "1700000000000",
  "party_id": "abcdef0123456789",
  "version": "1",
  "lifetime": 60000,
  "offer": {
    "type": "offer",
    "sdp": "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
  }
}`

func TestDecodeCallInvite(t *testing.T) {
	var c mevent.CallInviteEventContent
	if err := json.Unmarshal([]byte(elementWebInvite), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.CallID != "1700000000000" {
		t.Errorf("call_id = %q", c.CallID)
	}
	if c.PartyID != "abcdef0123456789" {
		t.Errorf("party_id = %q", c.PartyID)
	}
	if c.Lifetime != 60000 {
		t.Errorf("lifetime = %d", c.Lifetime)
	}
	if c.Offer.Type != mevent.CallDataTypeOffer {
		t.Errorf("offer.type = %q", c.Offer.Type)
	}
	if c.Offer.SDP == "" {
		t.Errorf("offer.sdp empty")
	}
}

func TestDecodeCallHangup(t *testing.T) {
	const body = `{"call_id":"x","party_id":"p","version":"1","reason":"user_hangup"}`
	var c mevent.CallHangupEventContent
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Reason != mevent.CallHangupUserHangup {
		t.Errorf("reason = %q", c.Reason)
	}
}
