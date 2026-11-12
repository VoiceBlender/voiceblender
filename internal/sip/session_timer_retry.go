package sip

import (
	"errors"
	"strconv"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// raiseSessionExpires turns an INVITE that failed with 422 Session Interval
// Too Small into the headers to retry it with (RFC 4028 §6): Session-Expires
// and Min-SE raised to the Min-SE the peer demanded, other headers unchanged.
// ok is false when err is not such a 422 or the INVITE asked for no timer.
func raiseSessionExpires(err error, headers []sip.Header) (out []sip.Header, minSE uint32, ok bool) {
	res := dialogErrorResponse(err)
	if res == nil || res.StatusCode != 422 {
		return nil, 0, false
	}
	h := res.GetHeader("Min-SE")
	if h == nil {
		return nil, 0, false
	}
	if minSE = ParseMinSE(h.Value()); minSE == 0 {
		return nil, 0, false
	}

	found := false
	out = make([]sip.Header, 0, len(headers)+1)
	for _, hdr := range headers {
		switch name := strings.ToLower(hdr.Name()); name {
		case "session-expires", "x":
			interval, _ := ParseSessionExpires(hdr.Value())
			if interval >= minSE {
				return nil, 0, false
			}
			value := strconv.FormatUint(uint64(minSE), 10)
			if _, params, has := strings.Cut(hdr.Value(), ";"); has {
				value += ";" + params
			}
			out = append(out, sip.NewHeader(hdr.Name(), value))
			found = true
		case "min-se":
		default:
			out = append(out, hdr)
		}
	}
	if !found {
		return nil, 0, false
	}
	out = append(out, sip.NewHeader("Min-SE", strconv.FormatUint(uint64(minSE), 10)))
	return out, minSE, true
}

func dialogErrorResponse(err error) *sip.Response {
	var ptr *sipgo.ErrDialogResponse
	if errors.As(err, &ptr) && ptr != nil {
		return ptr.Res
	}
	var val sipgo.ErrDialogResponse
	if errors.As(err, &val) {
		return val.Res
	}
	return nil
}
