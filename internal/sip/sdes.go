package sip

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/srtp/v3"
)

// SRTPMode is the server-wide SDES-SRTP policy for classic SIP legs.
type SRTPMode string

const (
	// SRTPDisabled answers and offers plain RTP only; SRTP sections are rejected.
	SRTPDisabled SRTPMode = "disabled"
	// SRTPOptional answers SDES-SRTP when offered and plain RTP otherwise.
	// Outbound calls offer SRTP only when asked to.
	SRTPOptional SRTPMode = "optional"
	// SRTPRequired rejects plain RTP audio and always offers SRTP.
	SRTPRequired SRTPMode = "required"
)

// ParseSRTPMode validates a SIP_SRTP value. Empty means SRTPOptional.
func ParseSRTPMode(s string) (SRTPMode, error) {
	switch m := SRTPMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return SRTPOptional, nil
	case SRTPDisabled, SRTPOptional, SRTPRequired:
		return m, nil
	default:
		return "", fmt.Errorf("invalid SRTP mode %q (want disabled, optional or required)", s)
	}
}

// SDES crypto suite names (RFC 4568, RFC 6188, RFC 7714).
const (
	SuiteAES128CMSHA1_80 = "AES_CM_128_HMAC_SHA1_80"
	SuiteAES128CMSHA1_32 = "AES_CM_128_HMAC_SHA1_32"
	SuiteAES256CMSHA1_80 = "AES_256_CM_HMAC_SHA1_80"
	SuiteAES256CMSHA1_32 = "AES_256_CM_HMAC_SHA1_32"
	SuiteAEADAES128GCM   = "AEAD_AES_128_GCM"
	SuiteAEADAES256GCM   = "AEAD_AES_256_GCM"
)

var sdesSuites = map[string]srtp.ProtectionProfile{
	SuiteAES128CMSHA1_80: srtp.ProtectionProfileAes128CmHmacSha1_80,
	SuiteAES128CMSHA1_32: srtp.ProtectionProfileAes128CmHmacSha1_32,
	SuiteAES256CMSHA1_80: srtp.ProtectionProfileAes256CmHmacSha1_80,
	SuiteAES256CMSHA1_32: srtp.ProtectionProfileAes256CmHmacSha1_32,
	SuiteAEADAES128GCM:   srtp.ProtectionProfileAeadAes128Gcm,
	SuiteAEADAES256GCM:   srtp.ProtectionProfileAeadAes256Gcm,
}

// offerSuites are the suites put in our own offers, most preferred first.
var offerSuites = []string{SuiteAES128CMSHA1_80, SuiteAES128CMSHA1_32}

// SDESCrypto is one a=crypto attribute (RFC 4568 §9.1) carrying a single key.
type SDESCrypto struct {
	Tag   int
	Suite string
	// Key is the master key followed by the master salt.
	Key []byte
	// MKI is the master key identifier at its signalled length; nil when absent.
	MKI []byte
}

// SuiteSupported reports whether suite is one we can encrypt and decrypt.
func SuiteSupported(suite string) bool {
	_, ok := sdesSuites[suite]
	return ok
}

func suiteKeyLens(suite string) (keyLen, saltLen int, err error) {
	p, ok := sdesSuites[suite]
	if !ok {
		return 0, 0, fmt.Errorf("unsupported crypto suite %q", suite)
	}
	if keyLen, err = p.KeyLen(); err != nil {
		return 0, 0, err
	}
	if saltLen, err = p.SaltLen(); err != nil {
		return 0, 0, err
	}
	return keyLen, saltLen, nil
}

// NewSDESCrypto returns a crypto attribute with a fresh random master key.
func NewSDESCrypto(tag int, suite string) (SDESCrypto, error) {
	keyLen, saltLen, err := suiteKeyLens(suite)
	if err != nil {
		return SDESCrypto{}, err
	}
	key := make([]byte, keyLen+saltLen)
	if _, err := rand.Read(key); err != nil {
		return SDESCrypto{}, fmt.Errorf("generate SRTP key: %w", err)
	}
	return SDESCrypto{Tag: tag, Suite: suite, Key: key}, nil
}

// NewOfferCrypto returns one freshly keyed attribute per offered suite, tagged
// from 1 in preference order.
func NewOfferCrypto() ([]SDESCrypto, error) {
	out := make([]SDESCrypto, 0, len(offerSuites))
	for i, suite := range offerSuites {
		c, err := NewSDESCrypto(i+1, suite)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// SameKey reports whether two attributes key SRTP identically. The tag is
// only an SDP handle, so it is not compared.
func (c *SDESCrypto) SameKey(o *SDESCrypto) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.Suite == o.Suite && bytes.Equal(c.Key, o.Key) && bytes.Equal(c.MKI, o.MKI)
}

// AttrValue renders the attribute value, without the "crypto:" key.
func (c *SDESCrypto) AttrValue() string {
	v := fmt.Sprintf("%d %s inline:%s", c.Tag, c.Suite, base64.StdEncoding.EncodeToString(c.Key))
	if len(c.MKI) > 0 {
		v += fmt.Sprintf("|%d:%d", mkiValue(c.MKI), len(c.MKI))
	}
	return v
}

func mkiValue(mki []byte) uint64 {
	var buf [8]byte
	if len(mki) > 8 {
		mki = mki[len(mki)-8:]
	}
	copy(buf[8-len(mki):], mki)
	return binary.BigEndian.Uint64(buf[:])
}

// ParseSDESCrypto parses an a=crypto value. Attributes this implementation
// cannot honour — an unknown suite, more than one key, or any session
// parameter — return an error so the caller can move on to the next one.
func ParseSDESCrypto(v string) (SDESCrypto, error) {
	fields := strings.Fields(v)
	if len(fields) < 3 {
		return SDESCrypto{}, fmt.Errorf("crypto: want tag, suite and key params")
	}
	if len(fields) > 3 {
		return SDESCrypto{}, fmt.Errorf("crypto: session parameters are not supported")
	}
	tag, err := strconv.Atoi(fields[0])
	if err != nil || tag < 0 || tag > 999999999 {
		return SDESCrypto{}, fmt.Errorf("crypto: invalid tag %q", fields[0])
	}
	suite := strings.ToUpper(fields[1])
	keyLen, saltLen, err := suiteKeyLens(suite)
	if err != nil {
		return SDESCrypto{}, err
	}
	if strings.Contains(fields[2], ";") {
		return SDESCrypto{}, fmt.Errorf("crypto: multiple keys are not supported")
	}
	inline, ok := cutPrefixFold(fields[2], "inline:")
	if !ok {
		return SDESCrypto{}, fmt.Errorf("crypto: key method is not inline")
	}

	parts := strings.Split(inline, "|")
	key, err := decodeKey(parts[0])
	if err != nil {
		return SDESCrypto{}, fmt.Errorf("crypto: key: %w", err)
	}
	if len(key) != keyLen+saltLen {
		return SDESCrypto{}, fmt.Errorf("crypto: %s key is %d bytes, want %d", suite, len(key), keyLen+saltLen)
	}

	c := SDESCrypto{Tag: tag, Suite: suite, Key: key}
	for _, p := range parts[1:] {
		if val, size, isMKI := strings.Cut(p, ":"); isMKI {
			if c.MKI, err = parseMKI(val, size); err != nil {
				return SDESCrypto{}, err
			}
		} else if err := checkLifetime(p); err != nil {
			return SDESCrypto{}, err
		}
	}
	return c, nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// decodeKey accepts padded and unpadded base64; both appear in the wild.
func decodeKey(s string) ([]byte, error) {
	if k, err := base64.StdEncoding.DecodeString(s); err == nil {
		return k, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func parseMKI(val, size string) ([]byte, error) {
	n, err := strconv.Atoi(size)
	if err != nil || n < 1 || n > 128 {
		return nil, fmt.Errorf("crypto: invalid MKI length %q", size)
	}
	v, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("crypto: invalid MKI value %q", val)
	}
	mki := make([]byte, n)
	for i := n - 1; i >= 0 && v > 0; i-- {
		mki[i] = byte(v)
		v >>= 8
	}
	if v != 0 {
		return nil, fmt.Errorf("crypto: MKI value %s does not fit in %d bytes", val, n)
	}
	return mki, nil
}

// checkLifetime validates a key lifetime ("2^31" or a decimal count). The
// value itself is not enforced: no call lives long enough to exhaust a key.
func checkLifetime(s string) error {
	if base, exp, ok := strings.Cut(s, "^"); ok {
		if base != "2" {
			return fmt.Errorf("crypto: invalid lifetime %q", s)
		}
		s = exp
	}
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return fmt.Errorf("crypto: invalid lifetime %q", s)
	}
	return nil
}

// ProtoIsSRTP reports whether an m= transport profile is an SRTP one:
// RTP/SAVP (SDES) or UDP/TLS/RTP/SAVPF (DTLS-SRTP).
func ProtoIsSRTP(proto []string) bool {
	for _, tok := range proto {
		switch strings.ToUpper(tok) {
		case "SAVP", "SAVPF":
			return true
		}
	}
	return false
}

// protoIsSDES reports whether proto is a profile SDES keys: RTP/SAVP or
// RTP/SAVPF. DTLS-SRTP profiles (UDP/TLS/RTP/SAVP*) are not.
func protoIsSDES(proto []string) bool {
	if len(proto) != 2 || !strings.EqualFold(proto[0], "RTP") {
		return false
	}
	return strings.EqualFold(proto[1], "SAVP") || strings.EqualFold(proto[1], "SAVPF")
}

// RejectProto returns the profile a port-0 rejection of a section offered on
// proto echoes: the SRTP profile itself, or nil for the plain RTP/AVP default.
func RejectProto(proto []string) []string {
	if ProtoIsSRTP(proto) {
		return proto
	}
	return nil
}

// SelectAnswerCrypto decides how an answer treats one offered audio section
// under mode. It returns the offered attribute to answer with (nil for plain
// RTP), or a non-empty rejection reason.
func SelectAnswerCrypto(proto []string, offered []SDESCrypto, mode SRTPMode) (*SDESCrypto, string) {
	if !ProtoIsSRTP(proto) {
		if mode == SRTPRequired {
			return nil, "srtp_required"
		}
		return nil, ""
	}
	if mode == SRTPDisabled {
		return nil, "srtp_disabled"
	}
	if !protoIsSDES(proto) {
		return nil, "unsupported_transport"
	}
	if len(offered) == 0 {
		return nil, "no_supported_crypto"
	}
	c := offered[0]
	return &c, ""
}

// AudioTransportAcceptable reports whether at least one active audio section
// of the offer has a transport mode allows. An offer with no active audio is
// reported acceptable so the codec checks downstream decide it.
func (m *SDPMedia) AudioTransportAcceptable(mode SRTPMode) bool {
	if m == nil {
		return true
	}
	active := 0
	for _, ml := range m.MLines {
		if ml.Media != "audio" || ml.Port == 0 || ml.AudioIdx < 0 {
			continue
		}
		active++
		if _, reason := SelectAnswerCrypto(ml.Proto, m.Audio[ml.AudioIdx].Crypto, mode); reason == "" {
			return true
		}
	}
	return active == 0
}

// AnswerCryptoFor finds the attribute in an answer that accepts one of our
// offered attributes: same tag and same suite (RFC 4568 §7.1.2).
func AnswerCryptoFor(offered []SDESCrypto, answer []SDESCrypto) (local, remote *SDESCrypto, ok bool) {
	for i := range answer {
		for j := range offered {
			if answer[i].Tag == offered[j].Tag && answer[i].Suite == offered[j].Suite {
				l, r := offered[j], answer[i]
				return &l, &r, true
			}
		}
	}
	return nil, nil, false
}
