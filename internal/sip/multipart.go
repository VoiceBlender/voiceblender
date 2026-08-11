package sip

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// SIP body content types we recognize.
//
// Both SIPREC metadata types are accepted because the two RFCs contradicted
// each other: RFC 7865 specified application/rs-metadata+xml, RFC 7866
// specified application/rs-metadata, and neither registered the type.
// RFC 9806 resolves that erratum (Err7987) in favour of +xml and registers it,
// but it also records the interoperability consequence -- an SRC written to
// RFC 7866 sends the unsuffixed name, which was normative for nine years.
const (
	ContentTypeSDP              = "application/sdp"
	ContentTypeRSMetadata       = "application/rs-metadata+xml"
	ContentTypeRSMetadataLegacy = "application/rs-metadata"
)

// BodyPart is one part of a SIP message body. A non-multipart body is
// represented as a single part holding the whole body.
type BodyPart struct {
	// ContentType is lowercased with parameters stripped, e.g. "application/sdp".
	ContentType string
	Params      map[string]string
	// Disposition is the Content-Disposition type ("session",
	// "recording-session"), lowercased and without parameters.
	Disposition string
	Headers     map[string]string
	Data        []byte
}

// MessageBody is a parsed SIP message body.
type MessageBody struct {
	Raw       []byte
	Multipart bool
	Parts     []BodyPart
}

// BodyCarrier is the subset of a SIP message that carries a body. Both
// *sip.Request and *sip.Response satisfy it.
type BodyCarrier interface {
	Body() []byte
	GetHeaders(name string) []sip.Header
}

// ParseMessageBody splits body according to contentType. A body that is not
// multipart is returned verbatim as a single part, so every existing
// single-part call site keeps identical bytes.
func ParseMessageBody(contentType string, body []byte) (*MessageBody, error) {
	mb := &MessageBody{Raw: body}

	mediaType, params, err := parseMediaType(contentType)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		mb.Parts = []BodyPart{{ContentType: mediaType, Params: params, Data: body}}
		return mb, nil
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("multipart body without boundary parameter")
	}
	mb.Multipart = true

	r := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		p, err := r.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart part %d: %w", len(mb.Parts), err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, fmt.Errorf("read multipart part %d body: %w", len(mb.Parts), err)
		}
		mb.Parts = append(mb.Parts, bodyPartFromMIME(p.Header, data))
	}

	return mb, nil
}

func bodyPartFromMIME(hdr textproto.MIMEHeader, data []byte) BodyPart {
	partType, partParams, _ := parseMediaType(hdr.Get("Content-Type"))
	disposition, _, _ := parseMediaType(hdr.Get("Content-Disposition"))

	headers := make(map[string]string, len(hdr))
	for k, v := range hdr {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	return BodyPart{
		ContentType: partType,
		Params:      partParams,
		Disposition: disposition,
		Headers:     headers,
		Data:        data,
	}
}

// parseMediaType normalizes a Content-Type value. An empty value yields an
// empty media type rather than an error: SIP peers omit Content-Type on empty
// bodies, and callers treat that as "whatever the body is".
func parseMediaType(v string) (string, map[string]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil, nil
	}
	mediaType, params, err := mime.ParseMediaType(v)
	if err != nil {
		return "", nil, fmt.Errorf("parse content type %q: %w", v, err)
	}
	return strings.ToLower(mediaType), params, nil
}

// Part returns the body of the first part with the given content type.
func (b *MessageBody) Part(contentType string) ([]byte, bool) {
	if b == nil {
		return nil, false
	}
	want := strings.ToLower(contentType)
	for i := range b.Parts {
		if b.Parts[i].ContentType == want {
			return b.Parts[i].Data, true
		}
	}
	return nil, false
}

// SDP returns the session description. A single-part body is returned whatever
// its declared type, preserving the behaviour of peers that omit or misdeclare
// Content-Type on a plain SDP offer — except when it declares itself a type we
// know is not SDP.
//
// RFC 7866 §9.1 lets an SRC send metadata with no SDP in an INVITE, an UPDATE,
// or a 200 to an offerless INVITE, and is explicit that "when a SIP message
// contains only an SDP offer or metadata, the multipart/mixed container is
// optional". So a bare rs-metadata body is a correct request, and taking it for
// an offer answers it 400 Bad SDP.
func (b *MessageBody) SDP() ([]byte, bool) {
	if b == nil || len(b.Parts) == 0 {
		return nil, false
	}
	if !b.Multipart {
		switch b.Parts[0].ContentType {
		case ContentTypeRSMetadata, ContentTypeRSMetadataLegacy:
			return nil, false
		}
		return b.Parts[0].Data, len(b.Parts[0].Data) > 0
	}
	data, ok := b.Part(ContentTypeSDP)
	return data, ok && len(data) > 0
}

// RSMetadata returns the SIPREC metadata document (RFC 7865), accepting both
// the registered and the legacy content type.
func (b *MessageBody) RSMetadata() ([]byte, bool) {
	if data, ok := b.Part(ContentTypeRSMetadata); ok {
		return data, true
	}
	return b.Part(ContentTypeRSMetadataLegacy)
}

// BodyOf parses the body of a SIP message using its Content-Type header.
func BodyOf(msg BodyCarrier) (*MessageBody, error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}
	ct := ""
	if hs := msg.GetHeaders("Content-Type"); len(hs) > 0 {
		ct = hs[0].Value()
	}
	return ParseMessageBody(ct, msg.Body())
}

// SDPOf extracts the session description from a SIP message, transparently
// unwrapping a multipart body.
func SDPOf(msg BodyCarrier) ([]byte, error) {
	mb, err := BodyOf(msg)
	if err != nil {
		return nil, err
	}
	sdp, ok := mb.SDP()
	if !ok {
		return nil, fmt.Errorf("message body carries no SDP")
	}
	return sdp, nil
}

// ParseSDPMessage parses the session description carried by a SIP message,
// transparently unwrapping a multipart body.
func ParseSDPMessage(msg BodyCarrier) (*SDPMedia, error) {
	sdp, err := SDPOf(msg)
	if err != nil {
		return nil, err
	}
	return ParseSDP(sdp)
}

// BuildMultipartMixed renders parts as a multipart/mixed body, returning the
// Content-Type header value to send with it. The boundary is supplied by the
// caller so the output is deterministic.
func BuildMultipartMixed(boundary string, parts []BodyPart) (string, []byte, error) {
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("multipart body needs at least one part")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(boundary); err != nil {
		return "", nil, fmt.Errorf("set boundary: %w", err)
	}

	for i, p := range parts {
		hdr := make(textproto.MIMEHeader, len(p.Headers)+2)
		for k, v := range p.Headers {
			hdr.Set(k, v)
		}
		if p.ContentType != "" {
			hdr.Set("Content-Type", p.ContentType)
		}
		if p.Disposition != "" {
			hdr.Set("Content-Disposition", p.Disposition)
		}
		pw, err := w.CreatePart(hdr)
		if err != nil {
			return "", nil, fmt.Errorf("create multipart part %d: %w", i, err)
		}
		if _, err := pw.Write(p.Data); err != nil {
			return "", nil, fmt.Errorf("write multipart part %d: %w", i, err)
		}
	}
	if err := w.Close(); err != nil {
		return "", nil, fmt.Errorf("close multipart writer: %w", err)
	}

	return "multipart/mixed;boundary=" + boundary, buf.Bytes(), nil
}

// bodySetter is the slice of a SIP request setRequestBody writes to.
type bodySetter interface {
	SetBody([]byte)
	AppendHeader(sip.Header)
}

// setRequestBody attaches an SDP offer to a request, wrapping it in a
// multipart/mixed container when extra parts are supplied. With no extra parts
// it emits exactly the plain application/sdp body every call has always sent.
func setRequestBody(req bodySetter, sdp []byte, extra []BodyPart) error {
	if len(extra) == 0 {
		req.SetBody(sdp)
		req.AppendHeader(sip.NewHeader("Content-Type", ContentTypeSDP))
		return nil
	}

	parts := make([]BodyPart, 0, len(extra)+1)
	parts = append(parts, BodyPart{
		ContentType: ContentTypeSDP,
		Disposition: "session;handling=required",
		Data:        sdp,
	})
	parts = append(parts, extra...)

	contentType, body, err := BuildMultipartMixed(MultipartBoundary(string(sdp)), parts)
	if err != nil {
		return fmt.Errorf("build multipart request body: %w", err)
	}
	req.SetBody(body)
	req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	return nil
}

// MultipartBoundary derives a stable boundary token from seed (typically the
// Call-ID), so retransmissions and golden tests reproduce the same bytes.
func MultipartBoundary(seed string) string {
	h := fnv.New64a()
	_, _ = io.WriteString(h, seed)
	return fmt.Sprintf("vb-boundary-%016x", h.Sum64())
}
