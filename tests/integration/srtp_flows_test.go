//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

// assertStreamsSRTP checks the srtp flag of each of a leg's audio streams, in
// m-line order.
func assertStreamsSRTP(t *testing.T, inst *testInstance, legID string, want ...bool) {
	t.Helper()
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", inst.baseURL(), legID))
	var streams []legStreamView
	decodeJSON(t, resp, &streams)
	if len(streams) != len(want) {
		t.Fatalf("[%s] leg %s has %d streams, want %d", inst.name, legID, len(streams), len(want))
	}
	for i, s := range streams {
		if s.SRTP != want[i] {
			t.Errorf("[%s] leg %s stream %s: srtp = %v, want %v", inst.name, legID, s.ID, s.SRTP, want[i])
		}
	}
}

// TestSRTP_StreamFlag checks that GET /v1/legs/{id}/streams tells an encrypted
// stream from a plain one, on both ends of a call and for a stream added to it.
func TestSRTP_StreamFlag(t *testing.T) {
	t.Run("SDES call", func(t *testing.T) {
		instA := newTestInstance(t, "srtp-flag-a")
		instB := newTestInstance(t, "srtp-flag-b")
		out, in := dialSRTP(t, instA, instB, map[string]interface{}{"srtp": true})
		assertStreamsSRTP(t, instA, out, true)
		assertStreamsSRTP(t, instB, in, true)

		add := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), out), map[string]interface{}{"direction": "sendonly"})
		if add.StatusCode != http.StatusCreated {
			msg, _ := io.ReadAll(add.Body)
			t.Fatalf("add stream: status %d: %s", add.StatusCode, msg)
		}
		var added legStreamView
		decodeJSON(t, add, &added)
		if !added.SRTP {
			t.Error("the added stream's own view does not report srtp")
		}
		assertStreamsSRTP(t, instA, out, true, true)
		assertStreamsSRTP(t, instB, in, true, true)
		hangup(t, instA, out)
	})

	t.Run("plain call", func(t *testing.T) {
		instA := newTestInstance(t, "plain-flag-a")
		instB := newTestInstance(t, "plain-flag-b")
		out, in := dialSRTP(t, instA, instB, nil)
		assertStreamsSRTP(t, instA, out, false)
		assertStreamsSRTP(t, instB, in, false)
		hangup(t, instA, out)
	})
}
