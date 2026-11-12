package sip

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/pion/rtp"
)

// key30 is a 30-byte AES-128 master key plus salt.
var key30 = []byte("0123456789abcdefghijklmnopqrst")

func inline(key []byte) string { return "inline:" + base64.StdEncoding.EncodeToString(key) }

func TestParseSRTPMode(t *testing.T) {
	for in, want := range map[string]SRTPMode{
		"": SRTPOptional, "optional": SRTPOptional, "Disabled": SRTPDisabled, " required ": SRTPRequired,
	} {
		got, err := ParseSRTPMode(in)
		if err != nil || got != want {
			t.Errorf("ParseSRTPMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseSRTPMode("mandatory"); err == nil {
		t.Error("ParseSRTPMode(mandatory) should fail")
	}
}

func TestParseSDESCrypto(t *testing.T) {
	unpadded := base64.RawStdEncoding.EncodeToString(key30)
	cases := []struct {
		name  string
		value string
		want  SDESCrypto
	}{
		{"plain", "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30), SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}},
		{"32-bit tag", "2 AES_CM_128_HMAC_SHA1_32 " + inline(key30), SDESCrypto{Tag: 2, Suite: SuiteAES128CMSHA1_32, Key: key30}},
		{"lifetime", "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|2^31", SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}},
		{"decimal lifetime", "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|1048576", SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}},
		{"MKI", "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|1:4", SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30, MKI: []byte{0, 0, 0, 1}}},
		{"lifetime and MKI", "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|2^20|258:2", SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30, MKI: []byte{1, 2}}},
		{"unpadded base64", "1 AES_CM_128_HMAC_SHA1_80 inline:" + unpadded, SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}},
		{"case-insensitive method and suite", "1 aes_cm_128_hmac_sha1_80 INLINE:" + base64.StdEncoding.EncodeToString(key30), SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSDESCrypto(tc.value)
			if err != nil {
				t.Fatalf("ParseSDESCrypto: %v", err)
			}
			if got.Tag != tc.want.Tag || got.Suite != tc.want.Suite || !bytes.Equal(got.Key, tc.want.Key) || !bytes.Equal(got.MKI, tc.want.MKI) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseSDESCrypto_Rejects(t *testing.T) {
	cases := map[string]string{
		"unknown suite":      "1 F8_128_HMAC_SHA1_80 " + inline(key30),
		"short key":          "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30[:20]),
		"wrong length 256":   "1 AES_256_CM_HMAC_SHA1_80 " + inline(key30),
		"session parameter":  "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + " UNENCRYPTED_SRTP",
		"multiple keys":      "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|1:1;" + inline(key30) + "|2:1",
		"not inline":         "1 AES_CM_128_HMAC_SHA1_80 uri:http://example",
		"bad tag":            "x AES_CM_128_HMAC_SHA1_80 " + inline(key30),
		"bad MKI length":     "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|1:0",
		"MKI value too wide": "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|256:1",
		"bad lifetime":       "1 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "|3^20",
		"missing key params": "1 AES_CM_128_HMAC_SHA1_80",
		"invalid base64":     "1 AES_CM_128_HMAC_SHA1_80 inline:!!!!",
	}
	for name, v := range cases {
		if _, err := ParseSDESCrypto(v); err == nil {
			t.Errorf("%s: ParseSDESCrypto(%q) should fail", name, v)
		}
	}
}

func TestSDESCrypto_AttrValueRoundTrip(t *testing.T) {
	for _, c := range []SDESCrypto{
		{Tag: 3, Suite: SuiteAES128CMSHA1_80, Key: key30},
		{Tag: 1, Suite: SuiteAES128CMSHA1_32, Key: key30, MKI: []byte{0, 7}},
	} {
		got, err := ParseSDESCrypto(c.AttrValue())
		if err != nil {
			t.Fatalf("ParseSDESCrypto(%q): %v", c.AttrValue(), err)
		}
		if got.Tag != c.Tag || !got.SameKey(&c) {
			t.Errorf("round trip of %q gave %+v", c.AttrValue(), got)
		}
	}
}

func TestNewOfferCrypto(t *testing.T) {
	offer, err := NewOfferCrypto()
	if err != nil {
		t.Fatal(err)
	}
	if len(offer) != 2 || offer[0].Suite != SuiteAES128CMSHA1_80 || offer[1].Suite != SuiteAES128CMSHA1_32 {
		t.Fatalf("offer suites = %+v", offer)
	}
	for i, c := range offer {
		if c.Tag != i+1 || len(c.Key) != 30 {
			t.Errorf("offer[%d] = tag %d, %d-byte key", i, c.Tag, len(c.Key))
		}
	}
	if bytes.Equal(offer[0].Key, offer[1].Key) {
		t.Error("each offered suite needs its own key")
	}
	for _, suite := range []string{SuiteAES256CMSHA1_80, SuiteAEADAES128GCM, SuiteAEADAES256GCM} {
		c, err := NewSDESCrypto(1, suite)
		if err != nil {
			t.Fatalf("NewSDESCrypto(%s): %v", suite, err)
		}
		if _, err := ParseSDESCrypto(c.AttrValue()); err != nil {
			t.Errorf("%s: generated key does not parse back: %v", suite, err)
		}
	}
}

func TestSDESCrypto_SameKeyIgnoresTag(t *testing.T) {
	a := SDESCrypto{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}
	b := a
	b.Tag = 9
	if !a.SameKey(&b) {
		t.Error("tag must not affect SameKey")
	}
	b.Suite = SuiteAES128CMSHA1_32
	if a.SameKey(&b) {
		t.Error("suite must affect SameKey")
	}
	if a.SameKey(nil) || !(*SDESCrypto)(nil).SameKey(nil) {
		t.Error("nil handling")
	}
}

func TestSelectAnswerCrypto(t *testing.T) {
	avp := []string{"RTP", "AVP"}
	savp := []string{"RTP", "SAVP"}
	savpf := []string{"RTP", "SAVPF"}
	dtls := []string{"UDP", "TLS", "RTP", "SAVPF"}
	offered := []SDESCrypto{{Tag: 2, Suite: SuiteAES128CMSHA1_32, Key: key30}, {Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}}

	cases := []struct {
		name       string
		proto      []string
		crypto     []SDESCrypto
		mode       SRTPMode
		wantTag    int // 0 = plain
		wantReason string
	}{
		{"plain optional", avp, nil, SRTPOptional, 0, ""},
		{"plain with stray crypto stays plain", avp, offered, SRTPOptional, 0, ""},
		{"plain required", avp, nil, SRTPRequired, 0, "srtp_required"},
		{"SAVP picks offerer's first", savp, offered, SRTPOptional, 2, ""},
		{"SAVPF accepted", savpf, offered, SRTPRequired, 2, ""},
		{"SAVP disabled", savp, offered, SRTPDisabled, 0, "srtp_disabled"},
		{"SAVP no crypto", savp, nil, SRTPOptional, 0, "no_supported_crypto"},
		{"DTLS", dtls, offered, SRTPOptional, 0, "unsupported_transport"},
		{"empty mode is optional", savp, offered, "", 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := SelectAnswerCrypto(tc.proto, tc.crypto, tc.mode)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			switch {
			case tc.wantTag == 0 && got != nil:
				t.Errorf("got crypto %+v, want plain", got)
			case tc.wantTag != 0 && (got == nil || got.Tag != tc.wantTag):
				t.Errorf("got %+v, want tag %d", got, tc.wantTag)
			}
		})
	}
}

func TestAnswerCryptoFor(t *testing.T) {
	offered := []SDESCrypto{{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}, {Tag: 2, Suite: SuiteAES128CMSHA1_32, Key: key30}}
	peerKey := []byte("tsrqponmlkjihgfedcba9876543210")

	local, remote, ok := AnswerCryptoFor(offered, []SDESCrypto{{Tag: 2, Suite: SuiteAES128CMSHA1_32, Key: peerKey}})
	if !ok || local.Tag != 2 || local.Suite != SuiteAES128CMSHA1_32 || !bytes.Equal(remote.Key, peerKey) {
		t.Fatalf("got local=%+v remote=%+v ok=%v", local, remote, ok)
	}
	if _, _, ok := AnswerCryptoFor(offered, []SDESCrypto{{Tag: 2, Suite: SuiteAES128CMSHA1_80, Key: peerKey}}); ok {
		t.Error("a tag answered with a different suite must not match")
	}
	if _, _, ok := AnswerCryptoFor(offered, nil); ok {
		t.Error("an answer without crypto must not match")
	}
}

// srtpPair returns two sessions keyed against each other, each sending to the
// other over loopback.
func srtpPair(t *testing.T, suite string, mki []byte) (a, b *RTPSession) {
	t.Helper()
	a, b = mustSession(t), mustSession(t)
	ka, err := NewSDESCrypto(1, suite)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := NewSDESCrypto(1, suite)
	ka.MKI, kb.MKI = mki, mki
	if err := a.SetSRTP(&ka, &kb); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSRTP(&kb, &ka); err != nil {
		t.Fatal(err)
	}
	if err := a.SetRemote("127.0.0.1", b.LocalPort()); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRemote("127.0.0.1", a.LocalPort()); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func mustSession(t *testing.T) *RTPSession {
	t.Helper()
	s, err := NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetReadDeadline(time.Now().Add(2 * time.Second))
	return s
}

func testPacket(seq uint16, payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, Timestamp: uint32(seq) * 160, SSRC: 0xdecafbad},
		Payload: payload,
	}
}

func TestRTPSession_SRTPRoundTripEverySuite(t *testing.T) {
	for suite := range sdesSuites {
		for _, mki := range [][]byte{nil, {0, 0, 0, 5}} {
			name := suite
			if mki != nil {
				name += "/MKI"
			}
			t.Run(name, func(t *testing.T) {
				a, b := srtpPair(t, suite, mki)
				payload := bytes.Repeat([]byte{0xAB}, 160)

				if err := a.WriteRTP(testPacket(1, payload)); err != nil {
					t.Fatal(err)
				}
				var buf []byte
				var err error
				if buf, err = a.WriteRTPBuf(testPacket(2, payload), buf); err != nil {
					t.Fatal(err)
				}
				// WriteRTPBuf's returned buffer must stay the plaintext packet the
				// caller reuses, not the ciphertext.
				if !bytes.Equal(buf[len(buf)-160:], payload) {
					t.Error("WriteRTPBuf returned an encrypted buffer")
				}

				var pkt rtp.Packet
				for seq := uint16(1); seq <= 2; seq++ {
					if err := b.ReadRTPInto(&pkt); err != nil {
						t.Fatalf("read seq %d: %v", seq, err)
					}
					if pkt.SequenceNumber != seq || !bytes.Equal(pkt.Payload, payload) {
						t.Fatalf("seq %d: got seq=%d payload len=%d", seq, pkt.SequenceNumber, len(pkt.Payload))
					}
				}
			})
		}
	}
}

func TestRTPSession_SRTPOnTheWire(t *testing.T) {
	sess, peer := newLoopbackPair(t)
	local, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	remote, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	if err := sess.SetSRTP(&local, &remote); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetRemote("127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x55}, 160)
	if err := sess.WriteRTP(testPacket(1, payload)); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw := make([]byte, 1500)
	n, _, err := peer.ReadFromUDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	if n != 12+160+10 {
		t.Errorf("wire packet is %d bytes, want header+payload+80-bit tag = %d", n, 12+160+10)
	}
	if bytes.Contains(raw[:n], payload[:32]) {
		t.Error("payload went out in the clear")
	}
}

func TestRTPSession_SRTPDropsUnauthenticated(t *testing.T) {
	sess, peer := newLoopbackPair(t)
	local, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	remote, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	if err := sess.SetSRTP(&local, &remote); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetRemote("127.0.0.1", 9); err != nil {
		t.Fatal(err)
	}

	// A plain RTP packet carries no valid tag.
	sendRTP(t, peer, sess, 1, bytes.Repeat([]byte{1}, 40))
	var pkt rtp.Packet
	if err := sess.ReadRTPInto(&pkt); !errors.Is(err, ErrNotRTP) || !errors.Is(err, ErrSRTPAuth) {
		t.Fatalf("plain packet into SRTP session: err = %v, want ErrNotRTP and ErrSRTPAuth", err)
	}
	if got := sess.RemoteAddr().Port; got != 9 {
		t.Errorf("an unauthenticated packet latched the remote to port %d", got)
	}
}

func TestRTPSession_SetSRTPKeepsContextForSameKey(t *testing.T) {
	sess := mustSession(t)
	local, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	remote, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	if err := sess.SetSRTP(&local, &remote); err != nil {
		t.Fatal(err)
	}
	tx, rx := sess.tx.Load(), sess.rx.Load()

	retagged := local
	retagged.Tag = 4
	if err := sess.SetSRTP(&retagged, &remote); err != nil {
		t.Fatal(err)
	}
	if sess.tx.Load() != tx || sess.rx.Load() != rx {
		t.Error("an unchanged key must keep its context so the rollover counter survives")
	}
	if got := sess.LocalCrypto(); got.Tag != 4 {
		t.Errorf("LocalCrypto().Tag = %d, want the re-answered tag 4", got.Tag)
	}

	rekey, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	if err := sess.SetSRTP(&retagged, &rekey); err != nil {
		t.Fatal(err)
	}
	if sess.tx.Load() != tx || sess.rx.Load() == rx {
		t.Error("only the direction whose key changed should be rebuilt")
	}

	if err := sess.SetSRTP(&local, nil); err == nil {
		t.Error("SetSRTP with one side nil should fail")
	}
	if err := sess.SetSRTP(nil, nil); err != nil || sess.SRTPEnabled() || sess.LocalCrypto() != nil {
		t.Errorf("SetSRTP(nil, nil) should return to plain RTP (err=%v)", err)
	}
}

func TestGenerateOffer_SRTP(t *testing.T) {
	offer, err := NewOfferCrypto()
	if err != nil {
		t.Fatal(err)
	}
	sdp := string(GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.10",
		RTPPort: 40000,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
		Crypto:  offer,
	}))
	if !strings.Contains(sdp, "m=audio 40000 RTP/SAVP 0 101\r\n") {
		t.Errorf("offer is not RTP/SAVP:\n%s", sdp)
	}
	for _, c := range offer {
		if !strings.Contains(sdp, "a=crypto:"+c.AttrValue()+"\r\n") {
			t.Errorf("offer lacks a=crypto:%s", c.AttrValue())
		}
	}

	m, err := ParseSDP([]byte(sdp))
	if err != nil {
		t.Fatal(err)
	}
	p := m.PrimaryStream()
	if strings.Join(p.Proto, "/") != "RTP/SAVP" || len(p.Crypto) != 2 || !p.Crypto[0].SameKey(&offer[0]) {
		t.Errorf("parsed proto=%v crypto=%+v", p.Proto, p.Crypto)
	}

	plain := string(GenerateOffer(SDPConfig{LocalIP: "192.0.2.10", RTPPort: 40000, Codecs: []codec.CodecType{codec.CodecPCMU}}))
	if !strings.Contains(plain, "RTP/AVP") || strings.Contains(plain, "a=crypto") {
		t.Errorf("a plain offer must stay RTP/AVP without crypto:\n%s", plain)
	}
}

func TestGenerateAnswer_SRTPEchoesProfile(t *testing.T) {
	local := SDESCrypto{Tag: 7, Suite: SuiteAES128CMSHA1_80, Key: key30}
	sdp := string(GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.10",
		Codecs:  []codec.CodecType{codec.CodecPCMU},
		Streams: []AudioStream{
			{Port: 40000, Codecs: []codec.CodecType{codec.CodecPCMU}, Crypto: []SDESCrypto{local}, Proto: []string{"RTP", "SAVPF"}},
			{Port: 0, Proto: RejectProto([]string{"RTP", "SAVP"})},
			{Port: 0, Proto: RejectProto([]string{"RTP", "AVP"})},
		},
	}, codec.CodecPCMU, 0, false))
	for _, want := range []string{
		"m=audio 40000 RTP/SAVPF 0 101\r\n",
		"a=crypto:7 AES_CM_128_HMAC_SHA1_80 " + inline(key30) + "\r\n",
		"m=audio 0 RTP/SAVP 0\r\n",
		"m=audio 0 RTP/AVP 0\r\n",
	} {
		if !strings.Contains(sdp, want) {
			t.Errorf("answer lacks %q:\n%s", want, sdp)
		}
	}
}

func TestPlanAnswer_SRTP(t *testing.T) {
	offer := mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/SAVP 0",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 "+inline(key30),
		"m=audio 40002 RTP/AVP 0",
		"m=audio 40004 RTP/SAVP 0",
	)
	opts := baseOpts()

	opts.SRTP = SRTPOptional
	plans := PlanAnswer(offer, opts)
	if plans[0].Action != SlotAccept || plans[0].Crypto == nil || plans[0].Crypto.Tag != 1 {
		t.Errorf("SDES section: %+v", plans[0])
	}
	if plans[1].Action != SlotAccept || plans[1].Crypto != nil {
		t.Errorf("plain section under optional: %+v", plans[1])
	}
	if plans[2].Action != SlotReject || plans[2].Reason != "no_supported_crypto" {
		t.Errorf("SAVP without crypto: %+v", plans[2])
	}

	opts.SRTP = SRTPRequired
	if p := PlanAnswer(offer, opts)[1]; p.Action != SlotReject || p.Reason != "srtp_required" {
		t.Errorf("plain section under required: %+v", p)
	}
	opts.SRTP = SRTPDisabled
	if p := PlanAnswer(offer, opts)[0]; p.Action != SlotReject || p.Reason != "srtp_disabled" {
		t.Errorf("SDES section under disabled: %+v", p)
	}
}

func TestApplyAnswerSRTP(t *testing.T) {
	offered := []SDESCrypto{{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}}
	answer := func(lines ...string) *SDPMedia {
		return mustParse(t, append([]string{"v=0", "o=- 1 0 IN IP4 192.0.2.1", "s=-", "c=IN IP4 192.0.2.1", "t=0 0"}, lines...)...)
	}
	peer := "a=crypto:1 AES_CM_128_HMAC_SHA1_80 " + inline([]byte("tsrqponmlkjihgfedcba9876543210"))

	sess := mustSession(t)
	if err := applyAnswerSRTP([]*RTPSession{sess}, [][]SDESCrypto{offered}, answer("m=audio 40000 RTP/SAVP 0", peer)); err != nil {
		t.Fatal(err)
	}
	if c := sess.LocalCrypto(); c == nil || !c.SameKey(&offered[0]) {
		t.Errorf("session not keyed with the accepted offer: %+v", c)
	}

	for name, a := range map[string]*SDPMedia{
		"plain answer":     answer("m=audio 40000 RTP/AVP 0"),
		"SAVP no crypto":   answer("m=audio 40000 RTP/SAVP 0"),
		"unknown tag":      answer("m=audio 40000 RTP/SAVP 0", strings.Replace(peer, "crypto:1", "crypto:5", 1)),
		"AVP with crypto":  answer("m=audio 40000 RTP/AVP 0", peer),
		"DTLS with crypto": answer("m=audio 40000 UDP/TLS/RTP/SAVP 0", peer),
	} {
		if err := applyAnswerSRTP([]*RTPSession{mustSession(t)}, [][]SDESCrypto{offered}, a); err == nil {
			t.Errorf("%s: should fail the call", name)
		}
	}

	// A section the peer rejected is left for the leg to tear down.
	if err := applyAnswerSRTP([]*RTPSession{mustSession(t)}, [][]SDESCrypto{offered}, answer("m=audio 0 RTP/SAVP 0", "m=audio 40000 RTP/AVP 0")); err != nil {
		t.Errorf("rejected section: %v", err)
	}
	// Nothing offered with SRTP, nothing to check.
	if err := applyAnswerSRTP([]*RTPSession{mustSession(t)}, [][]SDESCrypto{nil}, answer("m=audio 40000 RTP/AVP 0")); err != nil {
		t.Errorf("plain call: %v", err)
	}
}

func TestLocalStreams_TombstoneEchoesSRTPProfile(t *testing.T) {
	var tbl MLineTable
	tbl.Append(MLineSlot{Media: "audio", Proto: []string{"RTP", "SAVP"}, State: SlotTombstone})
	tbl.Append(MLineSlot{Media: "audio", Proto: []string{"RTP", "AVP"}, State: SlotTombstone})
	got := tbl.LocalStreams(false, nil)
	if p := got[0].TransportProto(); strings.Join(p, "/") != "RTP/SAVP" {
		t.Errorf("SRTP tombstone renders %v", p)
	}
	if p := got[1].TransportProto(); strings.Join(p, "/") != "RTP/AVP" {
		t.Errorf("plain tombstone renders %v", p)
	}
}
