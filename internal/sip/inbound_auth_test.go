package sip

import (
	"crypto/md5"
	"encoding/hex"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// newAuthTestEngine builds a minimal Engine carrying only a pending-auth store,
// avoiding the network setup NewEngine performs.
func newAuthTestEngine(ttl time.Duration) *Engine {
	return &Engine{pendingAuth: newPendingAuthStore(ttl)}
}

// authRequest builds an inbound request carrying the Authorization header a UAC
// would compute from the challenge value, plus the matching Call-ID.
func authRequest(t *testing.T, method sip.RequestMethod, callID, challengeVal, user, pass, ha1, uri string) *sip.Request {
	t.Helper()
	chal, err := digest.ParseChallenge(challengeVal)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	opts := digest.Options{Method: method.String(), URI: uri, Username: user, Count: 1, Cnonce: "0a4f113b"}
	if ha1 != "" {
		opts.A1 = ha1
	} else {
		opts.Password = pass
	}
	cred, err := digest.Digest(chal, opts)
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}
	req := sip.NewRequest(method, sip.Uri{Scheme: "sip", Host: "vb.example"})
	cid := sip.CallIDHeader(callID)
	req.AppendHeader(&cid)
	req.AppendHeader(sip.NewHeader("Authorization", cred.String()))
	return req
}

func TestRecordChallenge_HeaderShape(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	val := e.recordChallenge("call-1", ChallengeParams{Realm: "vb.example", Password: "s3cret"}, 0)
	chal, err := digest.ParseChallenge(val)
	if err != nil {
		t.Fatalf("challenge not parseable: %v (%q)", err, val)
	}
	if chal.Realm != "vb.example" {
		t.Errorf("realm = %q, want vb.example", chal.Realm)
	}
	if chal.Nonce == "" {
		t.Error("nonce is empty")
	}
	if chal.Algorithm != "MD5" {
		t.Errorf("algorithm = %q, want MD5 (default)", chal.Algorithm)
	}
}

func TestRecordChallenge_NonceUnique(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		val := e.recordChallenge("call", ChallengeParams{Realm: "r"}, 0)
		chal, _ := digest.ParseChallenge(val)
		if seen[chal.Nonce] {
			t.Fatalf("duplicate nonce %q", chal.Nonce)
		}
		seen[chal.Nonce] = true
	}
}

func TestVerifyInboundAuth_ValidPassword(t *testing.T) {
	for _, alg := range []string{"MD5", "SHA-256"} {
		t.Run(alg, func(t *testing.T) {
			e := newAuthTestEngine(time.Minute)
			val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", Password: "pw", Algorithm: alg}, 0)
			req := authRequest(t, sip.INVITE, "c1", val, "alice", "pw", "", "sip:vb@vb.example")
			res, user, _ := e.VerifyInboundAuth(req, "INVITE")
			if res != AuthValid {
				t.Fatalf("result = %v, want AuthValid", res)
			}
			if user != "alice" {
				t.Errorf("user = %q, want alice", user)
			}
		})
	}
}

func TestVerifyInboundAuth_CarriesMaxExpires(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", Password: "pw"}, 30)
	req := authRequest(t, sip.REGISTER, "c1", val, "alice", "pw", "", "sip:vb.example")
	res, _, maxExpires := e.VerifyInboundAuth(req, "REGISTER")
	if res != AuthValid {
		t.Fatalf("result = %v, want AuthValid", res)
	}
	if maxExpires != 30 {
		t.Errorf("maxExpires = %d, want 30", maxExpires)
	}
}

func TestVerifyInboundAuth_ValidHA1(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	sum := md5.Sum([]byte("alice:vb:pw"))
	ha1 := hex.EncodeToString(sum[:])
	val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", HA1: ha1}, 0)
	req := authRequest(t, sip.REGISTER, "c1", val, "alice", "", ha1, "sip:vb.example")
	if res, _, _ := e.VerifyInboundAuth(req, "REGISTER"); res != AuthValid {
		t.Fatalf("result = %v, want AuthValid", res)
	}
}

func TestVerifyInboundAuth_WrongPassword(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", Password: "right"}, 0)
	// Client signs with the wrong password.
	req := authRequest(t, sip.INVITE, "c1", val, "alice", "wrong", "", "sip:vb@vb.example")
	if res, _, _ := e.VerifyInboundAuth(req, "INVITE"); res != AuthInvalid {
		t.Fatalf("result = %v, want AuthInvalid", res)
	}
}

func TestVerifyInboundAuth_WrongUsername(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", Password: "pw"}, 0)
	// A correctly-computed response, but for a different username than expected.
	req := authRequest(t, sip.INVITE, "c1", val, "mallory", "pw", "", "sip:vb@vb.example")
	if res, _, _ := e.VerifyInboundAuth(req, "INVITE"); res != AuthInvalid {
		t.Fatalf("result = %v, want AuthInvalid", res)
	}
}

func TestVerifyInboundAuth_NoChallengeRecorded(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	// Build a request whose Authorization references a nonce we never issued.
	other := newAuthTestEngine(time.Minute)
	val := other.recordChallenge("c1", ChallengeParams{Realm: "vb", Password: "pw"}, 0)
	req := authRequest(t, sip.INVITE, "c1", val, "alice", "pw", "", "sip:vb@vb.example")
	if res, _, _ := e.VerifyInboundAuth(req, "INVITE"); res != AuthNone {
		t.Fatalf("result = %v, want AuthNone", res)
	}
}

func TestVerifyInboundAuth_NoAuthHeader(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "vb.example"})
	if res, _, _ := e.VerifyInboundAuth(req, "INVITE"); res != AuthNone {
		t.Fatalf("result = %v, want AuthNone", res)
	}
}

func TestVerifyInboundAuth_ExpiredNonce(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", Password: "pw"}, 0)
	// Force the stored entry to be already expired.
	pc := e.pendingAuth.byCall["c1"]
	pc.expiresAt = time.Now().Add(-time.Second)
	e.pendingAuth.byCall["c1"] = pc
	req := authRequest(t, sip.INVITE, "c1", val, "alice", "pw", "", "sip:vb@vb.example")
	if res, _, _ := e.VerifyInboundAuth(req, "INVITE"); res != AuthNone {
		t.Fatalf("result = %v, want AuthNone (expired)", res)
	}
}

func TestVerifyInboundAuth_SingleUse(t *testing.T) {
	e := newAuthTestEngine(time.Minute)
	val := e.recordChallenge("c1", ChallengeParams{Realm: "vb", Username: "alice", Password: "pw"}, 0)
	req := authRequest(t, sip.INVITE, "c1", val, "alice", "pw", "", "sip:vb@vb.example")
	if res, _, _ := e.VerifyInboundAuth(req, "INVITE"); res != AuthValid {
		t.Fatalf("first verify = %v, want AuthValid", res)
	}
	// The challenge is consumed; a replay no longer matches.
	req2 := authRequest(t, sip.INVITE, "c1", val, "alice", "pw", "", "sip:vb@vb.example")
	if res, _, _ := e.VerifyInboundAuth(req2, "INVITE"); res != AuthNone {
		t.Fatalf("replay = %v, want AuthNone", res)
	}
}
