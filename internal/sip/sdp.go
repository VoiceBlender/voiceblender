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
}

// SDPMedia holds parsed remote media parameters.
type SDPMedia struct {
	RemoteIP      string
	RemotePort    int
	AddressFamily string                    // "IP4" or "IP6" (from c= line); empty if not present
	Codecs        []codec.CodecType         // Codecs from m= line, in offer order
	CodecPTs      map[codec.CodecType]uint8 // Actual PT for each codec from remote SDP
	CodecRates    map[codec.CodecType]int   // Clock rate (Hz) for each codec, from a=rtpmap; falls back to codec default
	Ptime         int                       // ms, default 20
	Direction     string                    // "sendrecv", "sendonly", "recvonly", "inactive"; empty = sendrecv
}

// codecRtpmap returns the rtpmap value string for a codec (e.g. "opus/48000/2").
func codecRtpmap(c codec.CodecType) string {
	if c == codec.CodecOpus {
		return "opus/48000/2"
	}
	return fmt.Sprintf("%s/%d", c.String(), c.ClockRate())
}

// codecFmtp returns the fmtp parameters for a codec, or "" if none.
func codecFmtp(c codec.CodecType) string {
	if c == codec.CodecOpus {
		return "minptime=20; useinbandfec=1; stereo=0; sprop-stereo=0"
	}
	return ""
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
func addCodecAttributes(md *pionsdp.MediaDescription, pt uint8, c codec.CodecType) {
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d %s", pt, codecRtpmap(c))))
	if fmtp := codecFmtp(c); fmtp != "" {
		md.Attributes = append(md.Attributes,
			pionsdp.NewAttribute("fmtp", fmt.Sprintf("%d %s", pt, fmtp)))
	}
}

// addTelephoneEvent appends telephone-event rtpmap and fmtp for the given PT and clock rate.
func addTelephoneEvent(md *pionsdp.MediaDescription, pt uint8, clockRate int) {
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d telephone-event/%d", pt, clockRate)))
	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("fmtp", fmt.Sprintf("%d 0-16", pt)))
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
	formats = append(formats, "101") // telephone-event/8000

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
		addCodecAttributes(md, c.PayloadType(), c)
	}
	if hasOpus {
		addTelephoneEvent(md, 100, 48000)
	}
	addTelephoneEvent(md, 101, 8000)

	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("ptime", "20"),
		pionsdp.NewPropertyAttribute("sendrecv"),
		pionsdp.NewPropertyAttribute("rtcp-mux"),
	)

	sd.MediaDescriptions = append(sd.MediaDescriptions, md)

	b, _ := sd.Marshal()
	return b
}

// GenerateAnswer builds an SDP answer with a single selected codec.
// selectedPT is the payload type to use (echoed from the remote offer for dynamic PTs).
func GenerateAnswer(cfg SDPConfig, selected codec.CodecType, selectedPT uint8) []byte {
	sd := buildSessionDescription(cfg.LocalIP)

	formats := []string{strconv.Itoa(int(selectedPT))}
	if selected == codec.CodecOpus {
		formats = append(formats, "100") // telephone-event/48000
	}
	formats = append(formats, "101") // telephone-event/8000

	md := &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "audio",
			Port:    pionsdp.RangedPort{Value: cfg.RTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}

	addCodecAttributes(md, selectedPT, selected)
	if selected == codec.CodecOpus {
		addTelephoneEvent(md, 100, 48000)
	}
	addTelephoneEvent(md, 101, 8000)

	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("ptime", "20"),
		pionsdp.NewPropertyAttribute("sendrecv"),
		pionsdp.NewPropertyAttribute("rtcp-mux"),
	)

	sd.MediaDescriptions = append(sd.MediaDescriptions, md)

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
		Ptime:      20,
		CodecPTs:   make(map[codec.CodecType]uint8),
		CodecRates: make(map[codec.CodecType]int),
	}

	// Session-level c= line.
	if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		m.RemoteIP = sd.ConnectionInformation.Address.Address
		m.AddressFamily = sd.ConnectionInformation.AddressType
	}

	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Media != "audio" {
			continue
		}
		m.RemotePort = md.MediaName.Port.Value

		// Media-level c= overrides session-level.
		if md.ConnectionInformation != nil && md.ConnectionInformation.Address != nil {
			m.RemoteIP = md.ConnectionInformation.Address.Address
			m.AddressFamily = md.ConnectionInformation.AddressType
		}

		// Build rtpmap: PT → codec name + clock rate from attributes.
		rtpmap := make(map[uint8]string)
		rtpmapRate := make(map[uint8]int)
		for _, a := range md.Attributes {
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

		// Parse payload types from m= line formats.
		for _, ptStr := range md.MediaName.Formats {
			pt, err := strconv.Atoi(ptStr)
			if err != nil {
				continue
			}
			upt := uint8(pt)

			// Try static PT mapping first.
			ct := codec.CodecTypeFromPT(upt)
			if ct != codec.CodecUnknown {
				m.Codecs = append(m.Codecs, ct)
				m.CodecPTs[ct] = upt
				if rate, ok := rtpmapRate[upt]; ok {
					m.CodecRates[ct] = rate
				} else {
					m.CodecRates[ct] = ct.ClockRate()
				}
				continue
			}

			// Try rtpmap for dynamic PTs.
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
				}
			}
		}

		break // Only handle first audio m= line.
	}

	if m.RemoteIP == "" {
		return nil, fmt.Errorf("no connection address found in SDP")
	}
	if m.RemotePort == 0 {
		return nil, fmt.Errorf("no audio media line found in SDP")
	}

	return m, nil
}

// GenerateReInviteSDP builds an SDP body for a re-INVITE (hold/unhold).
// It is similar to GenerateAnswer but uses the specified direction attribute.
func GenerateReInviteSDP(cfg SDPConfig, selected codec.CodecType, selectedPT uint8, direction string) []byte {
	sd := buildSessionDescription(cfg.LocalIP)

	formats := []string{strconv.Itoa(int(selectedPT))}
	if selected == codec.CodecOpus {
		formats = append(formats, "100") // telephone-event/48000
	}
	formats = append(formats, "101") // telephone-event/8000

	md := &pionsdp.MediaDescription{
		MediaName: pionsdp.MediaName{
			Media:   "audio",
			Port:    pionsdp.RangedPort{Value: cfg.RTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}

	addCodecAttributes(md, selectedPT, selected)
	if selected == codec.CodecOpus {
		addTelephoneEvent(md, 100, 48000)
	}
	addTelephoneEvent(md, 101, 8000)

	md.Attributes = append(md.Attributes,
		pionsdp.NewAttribute("ptime", "20"),
		pionsdp.NewPropertyAttribute(direction),
		pionsdp.NewPropertyAttribute("rtcp-mux"),
	)

	sd.MediaDescriptions = append(sd.MediaDescriptions, md)

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
