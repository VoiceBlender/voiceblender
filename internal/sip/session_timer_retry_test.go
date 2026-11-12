package sip

import (
	"errors"
	"fmt"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func dialogError(code int, hdrs ...sip.Header) error {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Host: "127.0.0.1"})
	res := sip.NewResponseFromRequest(req, code, "x", nil)
	for _, h := range hdrs {
		res.AppendHeader(h)
	}
	return fmt.Errorf("wait answer: %w", &sipgo.ErrDialogResponse{Res: res})
}

func headerMap(hs []sip.Header) map[string]string {
	m := map[string]string{}
	for _, h := range hs {
		m[h.Name()] = h.Value()
	}
	return m
}

func TestRaiseSessionExpires(t *testing.T) {
	in := []sip.Header{
		sip.NewHeader("Session-Expires", "90;refresher=uac"),
		sip.NewHeader("Min-SE", "90"),
		sip.NewHeader("X-Keep", "1"),
	}
	out, minSE, ok := raiseSessionExpires(dialogError(422, sip.NewHeader("Min-SE", "120")), in)
	if !ok || minSE != 120 {
		t.Fatalf("ok=%v minSE=%d, want a retry at 120", ok, minSE)
	}
	got := headerMap(out)
	if got["Session-Expires"] != "120;refresher=uac" || got["Min-SE"] != "120" || got["X-Keep"] != "1" || len(out) != 3 {
		t.Errorf("retry headers = %v", got)
	}
	if headerMap(in)["Session-Expires"] != "90;refresher=uac" {
		t.Error("input headers were modified")
	}
}

func TestRaiseSessionExpires_NoRetry(t *testing.T) {
	se := []sip.Header{sip.NewHeader("Session-Expires", "90")}
	cases := []struct {
		name    string
		err     error
		headers []sip.Header
	}{
		{"not a dialog error", errors.New("boom"), se},
		{"other status", dialogError(486, sip.NewHeader("Min-SE", "120")), se},
		{"422 without Min-SE", dialogError(422), se},
		{"no Session-Expires sent", dialogError(422, sip.NewHeader("Min-SE", "120")), []sip.Header{sip.NewHeader("X-A", "1")}},
		{"already at Min-SE", dialogError(422, sip.NewHeader("Min-SE", "90")), se},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := raiseSessionExpires(tc.err, tc.headers); ok {
				t.Error("unexpected retry")
			}
		})
	}
}

func TestRaiseSessionExpires_CompactForm(t *testing.T) {
	out, _, ok := raiseSessionExpires(dialogError(422, sip.NewHeader("Min-SE", "300")), []sip.Header{sip.NewHeader("x", "90")})
	if !ok || headerMap(out)["x"] != "300" {
		t.Errorf("ok=%v headers=%v", ok, headerMap(out))
	}
}
