package sip

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	pionsdp "github.com/pion/sdp/v3"
)

// SDPConfig holds local media parameters for SDP generation.
type SDPConfig struct {
	LocalIP string
	RTPPort int
	Codecs  []codec.CodecType // Offered/supported codecs in preference order

	// Optional RTT (T.140 / RFC 4103) section. Set TextRTPPort != 0 to emit
	// an m=text line in offers/answers/re-INVITEs. TextT140PT and TextREDPT
	// are dynamic payload types; TextREDPT == 0 disables RFC 2198 redundancy.
	// RTTRedundancy is the number of t140/t140/.../t140 generations declared
	// in the RED fmtp.
	TextRTPPort   int
	TextT140PT    uint8
	TextREDPT     uint8
	RTTRedundancy int

	// AMRWBOctetAligned controls the AMR-WB fmtp emitted for an offer/answer:
	// true emits "octet-align=1", false emits no octet-align (RFC 4867 default,
	// bandwidth-efficient). On answers it must echo the peer's negotiated format.
	AMRWBOctetAligned bool

	// AMRWBModeSet, when non-empty (e.g. "0,1,2"), adds a "mode-set=..." AMR-WB
	// fmtp param. Used on answers to echo the peer's negotiated mode-set per
	// RFC 4867; left empty on offers (we accept all modes on receive).
	AMRWBModeSet string

	// AMRNBOctetAligned controls the AMR-NB fmtp emitted for an offer/answer:
	// true emits "octet-align=1", false emits no octet-align (RFC 4867 default,
	// bandwidth-efficient). On answers it must echo the peer's negotiated format.
	AMRNBOctetAligned bool

	// AMRNBModeSet, when non-empty (e.g. "0,4,7"), adds a "mode-set=..." AMR-NB
	// fmtp param. Used on answers to echo the peer's negotiated mode-set per
	// RFC 4867; left empty on offers (we accept all modes on receive).
	AMRNBModeSet string

	// DTMFPT, when non-zero, is the telephone-event (RFC 4733) PT to advertise
	// in the generated SDP. Zero defaults to 101. In answers callers should set
	// this from the remote offer to mirror the offerer's choice.
	DTMFPT uint8

	// DTMFClockRate, when non-zero, is the telephone-event clock rate (Hz) to
	// advertise. Zero defaults to TelephoneEventClockRate(selected codec). In
	// answers callers must set this from the remote offer so RFC 3264
	// offer/answer semantics hold — phones like Fanvil pin telephone-event at
	// 8 kHz regardless of audio codec, and unilaterally upgrading to 16 kHz
	// breaks their DTMF.
	DTMFClockRate int
}

// SDPMedia holds parsed remote media parameters.
type SDPMedia struct {
	RemoteIP      string
	RemotePort    int
	AddressFamily string                     // "IP4" or "IP6" (from c= line); empty if not present
	Codecs        []codec.CodecType          // Codecs from m= line, in offer order
	CodecPTs      map[codec.CodecType]uint8  // Actual PT for each codec from remote SDP
	CodecRates    map[codec.CodecType]int    // Clock rate (Hz) for each codec, from a=rtpmap; falls back to codec default
	CodecFmtp     map[codec.CodecType]string // Raw a=fmtp params for each codec (e.g. AMR-WB "octet-align=1; mode-set=...")
	Ptime         int                        // ms, default 20
	Direction     string                     // "sendrecv", "sendonly", "recvonly", "inactive"; empty = sendrecv
	DTMFEventPTs  map[uint8]int              // telephone-event (RFC 4733) PT -> clock rate, as advertised by the remote

	// Text (RTT, T.140 / RFC 4103). Non-nil when the remote SDP carried an
	// m=text line with a non-zero port. A port of zero (peer rejecting the
	// text section per RFC 3264) leaves this field nil.
	Text *SDPTextMedia
}

// SDPTextMedia holds parsed remote RTT parameters.
type SDPTextMedia struct {
	RemoteIP   string
	RemotePort int
	T140PT     uint8 // 0 if no t140/1000 advertised
	REDPT      uint8 // 0 if no red/1000 advertised
	Direction  string
}

// codecRtpmap returns the rtpmap value string for a codec (e.g. "opus/48000/2").
// AMR-NB uses the RFC 4867 §8.1 encoding name "AMR" (AMR-WB uses "AMR-WB").
func codecRtpmap(c codec.CodecType) string {
	switch c {
	case codec.CodecOpus:
		return "opus/48000/2"
	case codec.CodecAMRWB:
		return "AMR-WB/16000/1"
	case codec.CodecAMRNB:
		return "AMR/8000/1"
	default:
		return fmt.Sprintf("%s/%d", c.String(), c.ClockRate())
	}
}

// codecFmtp returns the fmtp parameters for a codec, or "" if none.
// The amrwb* / amrnb* args control each AMR variant's framing format and
// (optional) mode-set; they're ignored for unrelated codecs.
func codecFmtp(c codec.CodecType, amrwbOctetAligned bool, amrwbModeSet string, amrnbOctetAligned bool, amrnbModeSet string) string {
	switch c {
	case codec.CodecOpus:
		return "minptime=20; useinbandfec=1; stereo=0; sprop-stereo=0"
	case codec.CodecAMRWB:
		var parts []string
		if amrwbOctetAligned {
			parts = append(parts, "octet-align=1") // else bandwidth-efficient (RFC 4867 default)
		}
		if amrwbModeSet != "" {
			parts = append(parts, "mode-set="+amrwbModeSet)
		}
		return strings.Join(parts, "; ")
	case codec.CodecAMRNB:
		var parts []string
		if amrnbOctetAligned {
			parts = append(parts, "octet-align=1") // else bandwidth-efficient (RFC 4867 default)
		}
		if amrnbModeSet != "" {
			parts = append(parts, "mode-set="+amrnbModeSet)
		}
		return strings.Join(parts, "; ")
	default:
		return ""
	}
}

// AMRWBOctetAligned reports whether the AMR-WB fmtp params select octet-aligned
// framing. Per RFC 4867 the default (no octet-align) is bandwidth-efficient, so
// an absent or "octet-align=0" parameter means bandwidth-efficient.
func AMRWBOctetAligned(fmtp string) bool {
	for _, p := range strings.Split(fmtp, ";") {
		p = strings.TrimSpace(p)
		if strings.EqualFold(p, "octet-align=1") {
			return true
		}
	}
	return false
}

// AMRWBModeSet parses an AMR-WB "mode-set=a,b,c" fmtp parameter into the set of
// allowed speech modes (0..8). Returns nil when no valid mode-set is present
// (RFC 4867: absence means all modes are permitted). Modes outside 0..8 are
// dropped.
func AMRWBModeSet(fmtp string) []int {
	for _, p := range strings.Split(fmtp, ";") {
		p = strings.TrimSpace(p)
		v, ok := strings.CutPrefix(strings.ToLower(p), "mode-set=")
		if !ok {
			continue
		}
		var modes []int
		for _, tok := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(tok))
			if err == nil && n >= 0 && n <= 8 {
				modes = append(modes, n)
			}
		}
		return modes
	}
	return nil
}

// FormatAMRWBModeSet renders modes as a "0,1,2" mode-set value (the inverse of
// AMRWBModeSet); returns "" for an empty set.
func FormatAMRWBModeSet(modes []int) string {
	parts := make([]string, len(modes))
	for i, m := range modes {
		parts[i] = strconv.Itoa(m)
	}
	return strings.Join(parts, ",")
}

// ClampAMRWBMode constrains a desired ceiling mode to the peer's negotiated
// mode-set: it returns the highest set member <= ceiling, or — when the ceiling
// is below every member — the lowest member (so the result always stays inside
// the set). A nil/empty set means no restriction, so the ceiling is returned.
func ClampAMRWBMode(ceiling int, modeSet []int) int {
	if len(modeSet) == 0 {
		return ceiling
	}
	best, min := -1, modeSet[0]
	for _, m := range modeSet {
		if m < min {
			min = m
		}
		if m <= ceiling && m > best {
			best = m
		}
	}
	if best < 0 {
		return min
	}
	return best
}

// AMRNBOctetAligned reports whether the AMR-NB fmtp params select octet-aligned
// framing. Per RFC 4867 the default (no octet-align) is bandwidth-efficient, so
// an absent or "octet-align=0" parameter means bandwidth-efficient.
func AMRNBOctetAligned(fmtp string) bool {
	for _, p := range strings.Split(fmtp, ";") {
		p = strings.TrimSpace(p)
		if strings.EqualFold(p, "octet-align=1") {
			return true
		}
	}
	return false
}

// AMRNBModeSet parses an AMR-NB "mode-set=a,b,c" fmtp parameter into the set of
// allowed speech modes (0..7). Returns nil when no valid mode-set is present
// (RFC 4867: absence means all modes are permitted). Modes outside 0..7 are
// dropped.
func AMRNBModeSet(fmtp string) []int {
	for _, p := range strings.Split(fmtp, ";") {
		p = strings.TrimSpace(p)
		v, ok := strings.CutPrefix(strings.ToLower(p), "mode-set=")
		if !ok {
			continue
		}
		var modes []int
		for _, tok := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(tok))
			if err == nil && n >= 0 && n <= 7 {
				modes = append(modes, n)
			}
		}
		return modes
	}
	return nil
}

// FormatAMRNBModeSet renders modes as a "0,4,7" mode-set value (the inverse of
// AMRNBModeSet); returns "" for an empty set.
func FormatAMRNBModeSet(modes []int) string {
	parts := make([]string, len(modes))
	for i, m := range modes {
		parts[i] = strconv.Itoa(m)
	}
	return strings.Join(parts, ",")
}

// ClampAMRNBMode constrains a desired ceiling mode to the peer's negotiated
// mode-set. Behaviour mirrors ClampAMRWBMode but ranges over AMR-NB modes 0..7.
func ClampAMRNBMode(ceiling int, modeSet []int) int {
	if len(modeSet) == 0 {
		return ceiling
	}
	best, min := -1, modeSet[0]
	for _, m := range modeSet {
		if m < min {
			min = m
		}
		if m <= ceiling && m > best {
			best = m
		}
	}
	if best < 0 {
		return min
	}
	return best
}

// buildSessionDescription creates the common session-level SDP fields. The
// SDP address-type token (IP4/IP6) is derived from localIP — empty or
// non-literal input falls back to IP4 for backward compatibility.
func buildSessionDescription(localIP string) *pionsdp.SessionDescription {
	sessID := uint64(rand.Int64N(1<<31 - 1))
	addrType := AddressFamily(localIP)
	if addrType == "" {
		addrType = "IP4"
	}
	return &pionsdp.SessionDescription{
		Version: 0,
		Origin: pionsdp.Origin{
			Username:       "-",
			SessionID:      sessID,
			SessionVersion: 0,
			NetworkType:    "IN",
			AddressType:    addrType,
			UnicastAddress: localIP,
		},
		SessionName: "-",
		ConnectionInformation: &pionsdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: addrType,
			Address:     &pionsdp.Address{Address: localIP},
		},
		TimeDescriptions: []pionsdp.TimeDescription{
			{Timing: pionsdp.Timing{StartTime: 0, StopTime: 0}},
		},
	}
}

// addCodecAttributes appends rtpmap and fmtp attributes for a codec.
func addCodecAttributes(md *pionsdp.MediaDescription, pt uint8, c codec.CodecType, cfg SDPConfig) {
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d %s", pt, codecRtpmap(c))))
	if fmtp := codecFmtp(c, cfg.AMRWBOctetAligned, cfg.AMRWBModeSet, cfg.AMRNBOctetAligned, cfg.AMRNBModeSet); fmtp != "" {
		md.Attributes = append(md.Attributes,
			pionsdp.NewAttribute("fmtp", fmt.Sprintf("%d %s", pt, fmtp)))
	}
}

// TelephoneEventClockRate returns the RTP clock rate to pair with the
// telephone-event (RFC 4733) format for codec c. RFC 4733 requires the
// telephone-event clock rate to match the audio codec's RTP clock rate, so
// AMR-WB uses 16 kHz; all other codecs use the conventional 8 kHz.
func TelephoneEventClockRate(c codec.CodecType) int {
	if c == codec.CodecAMRWB {
		return 16000
	}
	return 8000
}

// DTMFPTForRate returns the remote telephone-event payload type advertised at
// the given clock rate, if any.
func (m *SDPMedia) DTMFPTForRate(rate int) (uint8, bool) {
	for pt, r := range m.DTMFEventPTs {
		if r == rate {
			return pt, true
		}
	}
	return 0, false
}

// PreferredDTMFEvent returns the telephone-event PT and clock rate to mirror
// in an answer (RFC 3264 offer/answer). When the remote advertised multiple
// telephone-event lines, the lowest PT wins for determinism. Returns
// (0, 0, false) when no telephone-event was offered.
func (m *SDPMedia) PreferredDTMFEvent() (pt uint8, rate int, ok bool) {
	var best uint8
	var bestRate int
	found := false
	for p, r := range m.DTMFEventPTs {
		if !found || p < best {
			best = p
			bestRate = r
			found = true
		}
	}
	return best, bestRate, found
}

// resolveDTMF returns the telephone-event PT and clock rate for a generated
// SDP. Explicit values in cfg win; otherwise we fall back to PT 101 and the
// codec-conventional rate.
func resolveDTMF(cfg SDPConfig, selected codec.CodecType) (uint8, int) {
	pt := cfg.DTMFPT
	if pt == 0 {
		pt = 101
	}
	rate := cfg.DTMFClockRate
	if rate == 0 {
		rate = TelephoneEventClockRate(selected)
	}
	return pt, rate
}

// addTelephoneEvent appends telephone-event rtpmap and fmtp for the given PT and clock rate.
func addTelephoneEvent(md *pionsdp.MediaDescription, pt uint8, clockRate int) {
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d telephone-event/%d", pt, clockRate)))
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("fmtp", fmt.Sprintf("%d 0-16", pt)))
}

// buildTextMediaDescription returns an m=text MediaDescription for RTT
// (RFC 4103 + RFC 2198 redundancy) using the parameters in cfg, or nil if
// cfg.TextRTPPort is zero. direction must be one of the standard SDP
// direction attribute names ("sendrecv", "sendonly", "recvonly", "inactive").
func buildTextMediaDescription(cfg SDPConfig, direction string) *pionsdp.MediaDescription {
	if cfg.TextRTPPort == 0 {
		return nil
	}
	t140PT := cfg.TextT140PT
	if t140PT == 0 {
		t140PT = 99
	}
	redPT := cfg.TextREDPT

	formats := []string{}
	if redPT != 0 {
		formats = append(formats, strconv.Itoa(int(redPT)))
	}
	formats = append(formats, strconv.Itoa(int(t140PT)))

	md := &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "text",
			Port:    pionsdp.RangedPort{Value: cfg.TextRTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}
	if redPT != 0 {
		md.Attributes = append(md.Attributes,
			pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d red/1000", redPT)))
	}
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d t140/1000", t140PT)))
	if redPT != 0 {
		// fmtp lists redundancy generations: e.g. "98 99/99/99" for 2-gen RED.
		repeats := cfg.RTTRedundancy + 1
		if repeats < 2 {
			repeats = 2
		}
		parts := make([]string, repeats)
		for i := range parts {
			parts[i] = strconv.Itoa(int(t140PT))
		}
		md.Attributes = append(md.Attributes,
			pionsdp.NewAttribute("fmtp", fmt.Sprintf("%d %s", redPT, strings.Join(parts, "/"))))
	}
	if direction != "" {
		md.Attributes = append(md.Attributes, pionsdp.NewPropertyAttribute(direction))
	}
	return md
}

// rejectedTextSection returns an m=text section with port=0 — the RFC 3264
// way to reject a text section offered by the peer. Generated when the peer
// offers RTT and we have it disabled; preserves m-line ordering for the
// answer.
func rejectedTextSection() *pionsdp.MediaDescription {
	return &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "text",
			Port:    pionsdp.RangedPort{Value: 0},
			Protos:  []string{"RTP", "AVP"},
			Formats: []string{"0"},
		},
	}
}

// GenerateOffer builds an SDP offer with all configured codecs.
func GenerateOffer(cfg SDPConfig) []byte {
	sd := buildSessionDescription(cfg.LocalIP)

	// Check if Opus is offered — if so we also offer telephone-event at 48kHz.
	hasOpus := false
	for _, c := range cfg.Codecs {
		if c == codec.CodecOpus {
			hasOpus = true
			break
		}
	}

	// Build format list for m= line.
	formats := make([]string, 0, len(cfg.Codecs)+2)
	for _, c := range cfg.Codecs {
		formats = append(formats, strconv.Itoa(int(c.PayloadType())))
	}
	if hasOpus {
		formats = append(formats, "100") // telephone-event/48000
	}
	formats = append(formats, "101") // telephone-event

	// PT 101 telephone-event clock rate follows the preferred codec so it
	// matches the codec the peer is most likely to select (AMR-WB needs 16kHz).
	teRate := 8000
	if len(cfg.Codecs) > 0 {
		teRate = TelephoneEventClockRate(cfg.Codecs[0])
	}

	md := &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "audio",
			Port:    pionsdp.RangedPort{Value: cfg.RTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}

	// Add codec attributes.
	for _, c := range cfg.Codecs {
		addCodecAttributes(md, c.PayloadType(), c, cfg)
	}
	if hasOpus {
		addTelephoneEvent(md, 100, 48000)
	}
	addTelephoneEvent(md, 101, teRate)

	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("ptime", "20"),
		pionsdp.NewPropertyAttribute("sendrecv"),
		pionsdp.NewPropertyAttribute("rtcp-mux"),
	)

	sd.MediaDescriptions = append(sd.MediaDescriptions, md)

	if textMD := buildTextMediaDescription(cfg, "sendrecv"); textMD != nil {
		sd.MediaDescriptions = append(sd.MediaDescriptions, textMD)
	}

	b, _ := sd.Marshal()
	return b
}

// GenerateAnswer builds an SDP answer with a single selected codec.
// selectedPT echoes the remote offer's PT for dynamic codecs. When
// cfg.TextRTPPort != 0 the answer accepts RTT; when textRejected is true
// the answer includes a port=0 m=text section per RFC 3264.
func GenerateAnswer(cfg SDPConfig, selected codec.CodecType, selectedPT uint8, textRejected bool) []byte {
	sd := buildSessionDescription(cfg.LocalIP)

	dtmfPT, dtmfRate := resolveDTMF(cfg, selected)

	formats := []string{strconv.Itoa(int(selectedPT))}
	if selected == codec.CodecOpus {
		formats = append(formats, "100") // telephone-event/48000
	}
	formats = append(formats, strconv.Itoa(int(dtmfPT)))

	md := &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "audio",
			Port:    pionsdp.RangedPort{Value: cfg.RTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}

	addCodecAttributes(md, selectedPT, selected, cfg)
	if selected == codec.CodecOpus {
		addTelephoneEvent(md, 100, 48000)
	}
	addTelephoneEvent(md, dtmfPT, dtmfRate)

	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("ptime", "20"),
		pionsdp.NewPropertyAttribute("sendrecv"),
		pionsdp.NewPropertyAttribute("rtcp-mux"),
	)

	sd.MediaDescriptions = append(sd.MediaDescriptions, md)

	if cfg.TextRTPPort != 0 {
		if tmd := buildTextMediaDescription(cfg, "sendrecv"); tmd != nil {
			sd.MediaDescriptions = append(sd.MediaDescriptions, tmd)
		}
	} else if textRejected {
		sd.MediaDescriptions = append(sd.MediaDescriptions, rejectedTextSection())
	}

	b, _ := sd.Marshal()
	return b
}

// ParseSDP parses a remote SDP body and extracts media parameters.
func ParseSDP(raw []byte) (*SDPMedia, error) {
	var sd pionsdp.SessionDescription
	if err := sd.Unmarshal(raw); err != nil {
		return nil, fmt.Errorf("unmarshal SDP: %w", err)
	}

	m := &SDPMedia{
		Ptime:        20,
		CodecPTs:     make(map[codec.CodecType]uint8),
		CodecRates:   make(map[codec.CodecType]int),
		CodecFmtp:    make(map[codec.CodecType]string),
		DTMFEventPTs: make(map[uint8]int),
	}

	// Session-level c= line.
	if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		m.RemoteIP = sd.ConnectionInformation.Address.Address
		m.AddressFamily = sd.ConnectionInformation.AddressType
	}

	audioParsed := false
	for _, md := range sd.MediaDescriptions {
		switch md.MediaName.Media {
		case "audio":
			if audioParsed {
				continue
			}
			parseAudioMedia(md, m)
			audioParsed = true
		case "text":
			parseTextMedia(md, m, &sd)
		}
	}

	if m.RemoteIP == "" {
		return nil, fmt.Errorf("no connection address found in SDP")
	}
	if m.RemotePort == 0 {
		return nil, fmt.Errorf("no audio media line found in SDP")
	}

	return m, nil
}

// parseAudioMedia populates the audio-related fields of m from a single
// audio MediaDescription.
func parseAudioMedia(md *pionsdp.MediaDescription, m *SDPMedia) {
	m.RemotePort = md.MediaName.Port.Value
	if md.ConnectionInformation != nil && md.ConnectionInformation.Address != nil {
		m.RemoteIP = md.ConnectionInformation.Address.Address
		m.AddressFamily = md.ConnectionInformation.AddressType
	}

	rtpmap := make(map[uint8]string)
	rtpmapRate := make(map[uint8]int)
	fmtpByPT := make(map[uint8]string)
	for _, a := range md.Attributes {
		if a.Key == "fmtp" {
			parts := strings.SplitN(a.Value, " ", 2)
			if len(parts) == 2 {
				if pt, err := strconv.Atoi(parts[0]); err == nil {
					fmtpByPT[uint8(pt)] = parts[1]
				}
			}
		}
		if a.Key == "rtpmap" {
			parts := strings.SplitN(a.Value, " ", 2)
			if len(parts) != 2 {
				continue
			}
			pt, err := strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			name := parts[1]
			if idx := strings.Index(name, "/"); idx > 0 {
				rest := name[idx+1:]
				name = name[:idx]
				rateStr := rest
				if i := strings.Index(rest, "/"); i > 0 {
					rateStr = rest[:i]
				}
				if rate, err := strconv.Atoi(rateStr); err == nil {
					rtpmapRate[uint8(pt)] = rate
				}
			}
			rtpmap[uint8(pt)] = name
			if strings.EqualFold(name, "telephone-event") {
				rate := rtpmapRate[uint8(pt)]
				if rate == 0 {
					rate = 8000
				}
				m.DTMFEventPTs[uint8(pt)] = rate
			}
		}
		if a.Key == "ptime" {
			if v, err := strconv.Atoi(a.Value); err == nil {
				m.Ptime = v
			}
		}
		switch a.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			m.Direction = a.Key
		}
	}

	for _, ptStr := range md.MediaName.Formats {
		pt, err := strconv.Atoi(ptStr)
		if err != nil {
			continue
		}
		upt := uint8(pt)

		ct := codec.CodecTypeFromPT(upt)
		if ct != codec.CodecUnknown {
			m.Codecs = append(m.Codecs, ct)
			m.CodecPTs[ct] = upt
			if rate, ok := rtpmapRate[upt]; ok {
				m.CodecRates[ct] = rate
			} else {
				m.CodecRates[ct] = ct.ClockRate()
			}
			if fmtp, ok := fmtpByPT[upt]; ok {
				m.CodecFmtp[ct] = fmtp
			}
			continue
		}
		if name, ok := rtpmap[upt]; ok {
			ct = codec.CodecTypeFromName(name)
			if ct != codec.CodecUnknown {
				m.Codecs = append(m.Codecs, ct)
				m.CodecPTs[ct] = upt
				if rate, ok := rtpmapRate[upt]; ok {
					m.CodecRates[ct] = rate
				} else {
					m.CodecRates[ct] = ct.ClockRate()
				}
				if fmtp, ok := fmtpByPT[upt]; ok {
					m.CodecFmtp[ct] = fmtp
				}
			}
		}
	}
}

// parseTextMedia parses an m=text section (RFC 4103) and, when port != 0,
// stores the negotiated parameters in m.Text.
func parseTextMedia(md *pionsdp.MediaDescription, m *SDPMedia, sd *pionsdp.SessionDescription) {
	port := md.MediaName.Port.Value
	if port == 0 {
		// Peer rejecting the text section — leave m.Text nil.
		return
	}
	tx := &SDPTextMedia{RemotePort: port}
	if md.ConnectionInformation != nil && md.ConnectionInformation.Address != nil {
		tx.RemoteIP = md.ConnectionInformation.Address.Address
	} else if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		tx.RemoteIP = sd.ConnectionInformation.Address.Address
	}
	for _, a := range md.Attributes {
		switch a.Key {
		case "rtpmap":
			parts := strings.SplitN(a.Value, " ", 2)
			if len(parts) != 2 {
				continue
			}
			pt, err := strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			name := parts[1]
			if idx := strings.Index(name, "/"); idx > 0 {
				name = name[:idx]
			}
			switch strings.ToLower(name) {
			case "t140":
				tx.T140PT = uint8(pt)
			case "red":
				tx.REDPT = uint8(pt)
			}
		case "sendrecv", "sendonly", "recvonly", "inactive":
			tx.Direction = a.Key
		}
	}
	if tx.T140PT == 0 && tx.REDPT == 0 {
		// No usable PT advertised; treat as not negotiated.
		return
	}
	m.Text = tx
}

// GenerateReInviteSDP builds an SDP body for a re-INVITE (hold/unhold).
// It is similar to GenerateAnswer but uses the specified direction attribute.
func GenerateReInviteSDP(cfg SDPConfig, selected codec.CodecType, selectedPT uint8, direction string) []byte {
	sd := buildSessionDescription(cfg.LocalIP)

	dtmfPT, dtmfRate := resolveDTMF(cfg, selected)

	formats := []string{strconv.Itoa(int(selectedPT))}
	if selected == codec.CodecOpus {
		formats = append(formats, "100") // telephone-event/48000
	}
	formats = append(formats, strconv.Itoa(int(dtmfPT)))

	md := &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "audio",
			Port:    pionsdp.RangedPort{Value: cfg.RTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}

	addCodecAttributes(md, selectedPT, selected, cfg)
	if selected == codec.CodecOpus {
		addTelephoneEvent(md, 100, 48000)
	}
	addTelephoneEvent(md, dtmfPT, dtmfRate)

	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("ptime", "20"),
		pionsdp.NewPropertyAttribute(direction),
		pionsdp.NewPropertyAttribute("rtcp-mux"),
	)

	sd.MediaDescriptions = append(sd.MediaDescriptions, md)

	if textMD := buildTextMediaDescription(cfg, direction); textMD != nil {
		sd.MediaDescriptions = append(sd.MediaDescriptions, textMD)
	}

	b, _ := sd.Marshal()
	return b
}

// NegotiateCodec finds the first codec in the remote SDP that is also in the supported list.
// Returns the codec type, the payload type from the remote SDP, and whether negotiation succeeded.
func NegotiateCodec(remote *SDPMedia, supported []codec.CodecType) (codec.CodecType, uint8, bool) {
	return NegotiateCodecPreferred(remote, supported, codec.CodecUnknown)
}

// NegotiateCodecPreferred is like NegotiateCodec but biases the choice toward
// preferred when it is non-zero. The preferred codec must appear in both the
// remote offer and the supported list; otherwise selection falls back to the
// regular preference order.
func NegotiateCodecPreferred(remote *SDPMedia, supported []codec.CodecType, preferred codec.CodecType) (codec.CodecType, uint8, bool) {
	if preferred != codec.CodecUnknown {
		offered := false
		for _, o := range remote.Codecs {
			if o == preferred {
				offered = true
				break
			}
		}
		ours := false
		for _, s := range supported {
			if s == preferred {
				ours = true
				break
			}
		}
		if offered && ours {
			pt := preferred.PayloadType()
			if remote.CodecPTs != nil {
				if remotePT, ok := remote.CodecPTs[preferred]; ok {
					pt = remotePT
				}
			}
			return preferred, pt, true
		}
	}
	for _, o := range remote.Codecs {
		for _, s := range supported {
			if o == s {
				pt := o.PayloadType() // default (static) PT
				if remote.CodecPTs != nil {
					if remotePT, ok := remote.CodecPTs[o]; ok {
						pt = remotePT
					}
				}
				return o, pt, true
			}
		}
	}
	return codec.CodecUnknown, 0, false
}
