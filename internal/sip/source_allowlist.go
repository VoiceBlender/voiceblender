package sip

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/VoiceBlender/voiceblender/internal/metrics"
	"github.com/emiago/sipgo/sip"
)

// ParseAllowedSources parses a comma-separated list of CIDR ranges and bare
// IPs naming the sources SIP requests are accepted from. It returns nil, which
// accepts every source, for an empty list or one holding a /0 prefix.
func ParseAllowedSources(s string) ([]netip.Prefix, error) {
	parsed, err := ParseLocalNets(s)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, 0, len(parsed))
	for _, p := range parsed {
		if p.Bits() == 0 {
			return nil, nil
		}
		out = append(out, unmapPrefix(p))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// sourceAllowlist drops SIP requests from sources outside prefixes. An empty
// list accepts every source.
type sourceAllowlist struct {
	prefixes []netip.Prefix
	rejected atomic.Uint64
	log      *slog.Logger
}

func (a *sourceAllowlist) enabled() bool { return len(a.prefixes) > 0 }

// allows takes a source as sipgo reports it: an IP literal with a port.
func (a *sourceAllowlist) allows(source string) bool {
	if !a.enabled() {
		return true
	}
	addr, ok := parsePeerAddr(source)
	if !ok {
		return false
	}
	addr = addr.WithZone("")
	for _, p := range a.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (a *sourceAllowlist) reject(source, transport, method string) {
	a.rejected.Add(1)
	metrics.RecordSIPRejected()
	if a.log != nil {
		a.log.Debug("SIP request dropped: source not in allowlist",
			"source", source, "transport", transport, "method", method)
	}
}

// filterUDP discards an unlisted source's datagram before it is parsed, which
// keeps sipgo from answering a malformed request with a stateless 400.
// Responses pass: they answer requests this side sent. Stream transports are
// left to the request handlers, a read there not being a whole message.
func (a *sourceAllowlist) filterUDP(info sip.TransportReadProps, data []byte) ([]byte, error) {
	if !strings.EqualFold(info.Transport, "udp") {
		return data, nil
	}
	if bytes.HasPrefix(bytes.TrimLeft(data, "\r\n"), []byte("SIP/2.0 ")) {
		return data, nil
	}
	source := ""
	if info.RemoteAddr != nil {
		source = info.RemoteAddr.String()
	}
	if a.allows(source) {
		return data, nil
	}
	a.reject(source, info.Transport, "")
	return nil, nil
}

// dropUnlisted abandons a request without answering it.
func (e *Engine) dropUnlisted(req *sip.Request, tx sip.ServerTransaction) {
	tx.Terminate()
	e.allowlist.reject(req.Source(), req.Transport(), string(req.Method))
}

// RequestsRejected returns how many SIP requests the source allowlist dropped.
func (e *Engine) RequestsRejected() uint64 {
	return e.allowlist.rejected.Load()
}

func (e *Engine) handleNoRoute(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, sip.StatusMethodNotAllowed, "Method Not Allowed", nil)
	res.AppendHeader(e.ServerHeader())
	res.AppendHeader(e.AllowHeader())
	if err := e.respondMaybeFromSource(tx, req, res); err != nil {
		e.log.Error("respond 405 failed", "error", err, "method", string(req.Method))
	}
}
