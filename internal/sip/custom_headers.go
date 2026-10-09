package sip

import (
	"strings"

	"github.com/emiago/sipgo/sip"
)

// CustomHeaders returns the X- and P- headers of req, matched in any case and
// keyed by name as received. It returns nil when there are none.
func CustomHeaders(req *sip.Request) map[string]string {
	if req == nil {
		return nil
	}
	var out map[string]string
	for _, h := range req.Headers() {
		name := h.Name()
		if !isCustomHeaderName(name) {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[name] = h.Value()
	}
	return out
}

func isCustomHeaderName(name string) bool {
	if len(name) < 3 || name[1] != '-' {
		return false
	}
	switch name[0] {
	case 'x', 'X', 'p', 'P':
		return true
	}
	return false
}

// LookupHeader finds name in hdrs ignoring case. An exact-case key wins.
func LookupHeader(hdrs map[string]string, name string) (string, bool) {
	if v, ok := hdrs[name]; ok {
		return v, true
	}
	for k, v := range hdrs {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}
