package sip

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// ClientTLSConfig controls how the certificate of a remote SIP TLS peer
// (registrar, outbound proxy, carrier SBC) is trusted on outbound dials. The
// zero value means full verification against the host trust store.
type ClientTLSConfig struct {
	// CAFile is a PEM bundle of extra roots trusted in addition to the system
	// pool. A peer's own self-signed certificate can be pinned this way.
	CAFile string
	// InsecureSkipVerify accepts any certificate, from any peer. Per-trunk
	// exemptions (see peerTrust) are the narrower alternative.
	InsecureSkipVerify bool
}

// Enabled reports whether any non-default trust setting is in effect.
func (c ClientTLSConfig) Enabled() bool {
	return c.CAFile != "" || c.InsecureSkipVerify
}

// peerTrust is the set of peers whose certificate is accepted unverified,
// keyed by the hostname dialed. Trunks add and remove themselves as they are
// created and deleted, so entries are refcounted: two trunks may name the same
// upstream, and the second must not have its exemption revoked by the first.
//
// The key is what ends up in the TLS SNI extension, which is the only
// per-connection identity a verification callback receives — hence hostnames
// only. A peer dialed by IP literal sends no SNI and cannot be exempted here.
type peerTrust struct {
	mu    sync.RWMutex
	hosts map[string]int
}

func newPeerTrust() *peerTrust {
	return &peerTrust{hosts: make(map[string]int)}
}

func (p *peerTrust) add(host string) {
	host = normalizeTrustHost(host)
	if host == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts[host]++
}

func (p *peerTrust) remove(host string) {
	host = normalizeTrustHost(host)
	if host == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.hosts[host]; n > 1 {
		p.hosts[host] = n - 1
	} else {
		delete(p.hosts, host)
	}
}

func (p *peerTrust) trusted(host string) bool {
	host = normalizeTrustHost(host)
	if host == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.hosts[host] > 0
}

func normalizeTrustHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// AddInsecureTLSPeer exempts every outbound TLS connection dialed at host from
// certificate verification, until a matching RemoveInsecureTLSPeer. Used by
// trunks configured with tls_insecure_skip_verify; an IP-literal host cannot be
// exempted (it sends no SNI) and is ignored.
func (e *Engine) AddInsecureTLSPeer(host string) {
	if e != nil && e.peerTrust != nil {
		e.peerTrust.add(host)
	}
}

// RemoveInsecureTLSPeer drops one exemption added by AddInsecureTLSPeer.
func (e *Engine) RemoveInsecureTLSPeer(host string) {
	if e != nil && e.peerTrust != nil {
		e.peerTrust.remove(host)
	}
}

// buildClientTLSConfig produces the dial-side TLS config shared by every
// outbound SIP TLS connection. Selective per-peer trust is only reachable by
// taking verification over from crypto/tls, which is why InsecureSkipVerify is
// set here and the real check lives in VerifyConnection.
func buildClientTLSConfig(c ClientTLSConfig, trust *peerTrust) (*tls.Config, error) {
	var roots *x509.CertPool
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA file %q: %w", c.CAFile, err)
		}
		roots, err = x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TLS CA file %q contains no PEM certificate", c.CAFile)
		}
	}

	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            roots,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if c.InsecureSkipVerify {
				return nil
			}
			if trust != nil && trust.trusted(cs.ServerName) {
				return nil
			}
			return verifyPeerChain(cs, roots)
		},
	}, nil
}

// verifyPeerChain is the standard verification crypto/tls would have done.
// cs.ServerName carries the SNI, which is empty for a peer dialed by IP
// literal — such a connection is chain-verified but not name-verified.
func verifyPeerChain(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("peer sent no certificate")
	}
	opts := x509.VerifyOptions{
		DNSName:       cs.ServerName,
		Roots:         roots,
		Intermediates: x509.NewCertPool(),
	}
	for _, cert := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(cert)
	}
	if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
		return fmt.Errorf("verify peer certificate: %w", err)
	}
	return nil
}
