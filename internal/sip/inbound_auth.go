package sip

import (
	"crypto/subtle"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// ChallengeParams carries the digest-challenge inputs a VSI/REST client
// supplies when it decides to challenge an inbound INVITE or REGISTER. The
// credential (Password or HA1) is held only for the lifetime of the pending
// challenge and is never persisted. Exactly one of Password or HA1 must be set.
type ChallengeParams struct {
	Realm     string   // digest realm advertised in the challenge
	Username  string   // expected username; empty accepts whatever username the response carries
	Password  string   // plaintext secret (mutually exclusive with HA1)
	HA1       string   // precomputed H(username:realm:password) hex (mutually exclusive with Password)
	Algorithm string   // "MD5" (default), "SHA-256", "SHA-512-256"
	QOP       []string // advertised qop, e.g. ["auth"]; empty omits the qop directive
}

func (p ChallengeParams) withDefaults() ChallengeParams {
	if p.Algorithm == "" {
		p.Algorithm = "MD5"
	}
	return p
}

// AuthResult is the outcome of verifying an Authorization header against a
// previously issued challenge.
type AuthResult int

const (
	// AuthNone means no Authorization header was present, or no live pending
	// challenge matched it. The caller should surface the request as
	// unauthenticated and let the client decide whether to challenge.
	AuthNone AuthResult = iota
	// AuthValid means the digest response matched the expected credential.
	AuthValid
	// AuthInvalid means a matching challenge was found but the response (or
	// username) did not verify. The caller should reject with 403.
	AuthInvalid
)

type pendingChallenge struct {
	nonce      string
	opaque     string
	params     ChallengeParams
	maxExpires int // REGISTER TTL cap carried to the credentialed retry; 0 = none
	expiresAt  time.Time
}

// pendingAuthStore holds issued-but-unverified challenges keyed by Call-ID. A
// UAC re-sends a challenged INVITE/REGISTER with the same Call-ID (RFC 3261
// §22), so Call-ID correlates the challenge to its credentialed retry.
type pendingAuthStore struct {
	mu     sync.Mutex
	byCall map[string]pendingChallenge
	ttl    time.Duration
}

func newPendingAuthStore(ttl time.Duration) *pendingAuthStore {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &pendingAuthStore{byCall: make(map[string]pendingChallenge), ttl: ttl}
}

func (s *pendingAuthStore) put(callID string, pc pendingChallenge) {
	s.mu.Lock()
	s.byCall[callID] = pc
	s.mu.Unlock()
}

// take returns and removes the pending challenge for callID. A challenge is
// single-use: each issued nonce verifies (or fails) exactly one retry.
func (s *pendingAuthStore) take(callID string) (pendingChallenge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, ok := s.byCall[callID]
	if ok {
		delete(s.byCall, callID)
	}
	return pc, ok
}

func callIDOf(req *sip.Request) string {
	if c := req.CallID(); c != nil {
		return c.Value()
	}
	return ""
}

// recordChallenge generates a fresh nonce, stores the pending challenge keyed
// by callID, and returns the WWW-Authenticate header value (including the
// "Digest " prefix) the caller should attach to a 401 response. maxExpires
// (>0), relevant only to REGISTER, is carried on the pending challenge so the
// credentialed retry binds with the capped TTL; pass 0 for INVITE challenges.
func (e *Engine) recordChallenge(callID string, p ChallengeParams, maxExpires int) string {
	p = p.withDefaults()
	nonce := sip.GenerateTagN(16)
	opaque := sip.GenerateTagN(8)
	chal := &digest.Challenge{
		Realm:     p.Realm,
		Nonce:     nonce,
		Opaque:    opaque,
		Algorithm: p.Algorithm,
		QOP:       p.QOP,
	}
	e.pendingAuth.put(callID, pendingChallenge{
		nonce:      nonce,
		opaque:     opaque,
		params:     p,
		maxExpires: maxExpires,
		expiresAt:  time.Now().Add(e.pendingAuth.ttl),
	})
	return chal.String()
}

// VerifyInboundAuth validates the Authorization header of an inbound request
// against the challenge previously issued for its Call-ID. method is the SIP
// method ("INVITE" / "REGISTER") signed by the digest. On AuthValid the second
// return value is the authenticated username and the third is the TTL cap
// (seconds, 0 = none) recorded with the challenge — meaningful only for
// REGISTER. Non-valid results return "" and 0.
func (e *Engine) VerifyInboundAuth(req *sip.Request, method string) (AuthResult, string, int) {
	authHdr := req.GetHeader("Authorization")
	if authHdr == nil {
		return AuthNone, "", 0
	}
	pc, ok := e.pendingAuth.take(callIDOf(req))
	if !ok || time.Now().After(pc.expiresAt) {
		return AuthNone, "", 0
	}
	cred, err := digest.ParseCredentials(authHdr.Value())
	if err != nil || cred.Nonce != pc.nonce {
		return AuthNone, "", 0
	}

	chal := &digest.Challenge{
		Realm:     pc.params.Realm,
		Nonce:     pc.nonce,
		Opaque:    pc.opaque,
		Algorithm: pc.params.Algorithm,
		QOP:       pc.params.QOP,
	}
	opts := digest.Options{
		Method:   method,
		URI:      cred.URI,
		Username: cred.Username,
		Count:    cred.Nc,
		Cnonce:   cred.Cnonce,
	}
	if pc.params.HA1 != "" {
		opts.A1 = pc.params.HA1
	} else {
		opts.Password = pc.params.Password
	}
	expected, err := digest.Digest(chal, opts)
	if err != nil {
		return AuthInvalid, "", 0
	}
	if subtle.ConstantTimeCompare([]byte(expected.Response), []byte(cred.Response)) != 1 {
		return AuthInvalid, "", 0
	}
	if pc.params.Username != "" && !strings.EqualFold(pc.params.Username, cred.Username) {
		return AuthInvalid, "", 0
	}
	return AuthValid, cred.Username, pc.maxExpires
}
