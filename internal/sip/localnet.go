package sip

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ParseLocalNets parses a comma-separated list of CIDR ranges and bare IPs.
func ParseLocalNets(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "/") {
			p, err := netip.ParsePrefix(tok)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", tok, err)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(tok)
		if err != nil {
			return nil, fmt.Errorf("invalid IP %q: %w", tok, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// localAdvertisedIP returns the IPv4 address to advertise to peer when it is
// on one of the configured local networks, and "" when the default advertised
// address applies. peer is an IP literal, with or without a port.
func (e *Engine) localAdvertisedIP(peer string) string {
	if len(e.localNets) == 0 || e.localIP == "" {
		return ""
	}
	addr, ok := parsePeerAddr(peer)
	if !ok || !addr.Is4() {
		return ""
	}
	for _, n := range e.localNets {
		if n.Contains(addr) {
			return e.localIP
		}
	}
	return ""
}

func parsePeerAddr(peer string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(peer); err == nil {
		return ap.Addr().Unmap(), true
	}
	peer = strings.TrimPrefix(strings.TrimSuffix(peer, "]"), "[")
	if a, err := netip.ParseAddr(peer); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

func (e *Engine) contactWithHost(host string) *sip.ContactHeader {
	return &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: host, Port: e.bindPort}}
}

// localContact returns the Contact to advertise to peer when it is on a local
// network, and nil when the default Contact applies.
func (e *Engine) localContact(peer string) *sip.ContactHeader {
	ip := e.localAdvertisedIP(peer)
	if ip == "" {
		return nil
	}
	return e.contactWithHost(ip)
}

// AdvertisedIPForPeer is AdvertisedIPForFamily narrowed by the peer's address:
// a peer on a local network gets the local address rather than the external one.
func (e *Engine) AdvertisedIPForPeer(family, peer string) string {
	if family != "IP6" {
		if ip := e.localAdvertisedIP(peer); ip != "" {
			return ip
		}
	}
	return e.AdvertisedIPForFamily(family)
}

// appendDialogContact gives an in-dialog request the Contact this side
// advertised at dialog setup. Any other Contact would re-target the peer.
func (e *Engine) appendDialogContact(req *sip.Request, dialog interface{}) {
	switch d := dialog.(type) {
	case *sipgo.DialogServerSession:
		if c := e.localContact(d.InviteRequest.Source()); c != nil {
			req.AppendHeader(c)
		}
	case *sipgo.DialogClientSession:
		if c := d.InviteRequest.Contact(); c != nil {
			req.AppendHeader(sip.HeaderClone(c))
		}
	}
}

// nextHopHost is the host the INVITE is actually sent to, which is what
// decides the address family and whether the peer is local.
func (o InviteOptions) nextHopHost(recipient sip.Uri) string {
	if len(o.ForkTargets) == 1 && o.ForkTargets[0].Socket != "" {
		if host, _ := splitHostPort(o.ForkTargets[0].Socket); host != "" {
			return host
		}
	}
	if o.ProxyURI != nil && len(o.ForkTargets) == 0 {
		return o.ProxyURI.Host
	}
	if o.RouteURI != nil {
		return o.RouteURI.Host
	}
	return recipient.Host
}
