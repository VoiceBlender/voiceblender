package sip

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func newInviteWithHeaders(t *testing.T, body []byte, contentType string, headers ...sip.Header) *InboundCall {
	t.Helper()
	uri := sip.Uri{User: "rec", Host: "10.0.0.1"}
	req := sip.NewRequest(sip.INVITE, uri)
	req.SetBody(body)
	if contentType != "" {
		req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	}
	for _, h := range headers {
		req.AppendHeader(h)
	}

	mb, err := BodyOf(req)
	if err != nil {
		t.Fatalf("BodyOf = %v, want nil", err)
	}
	return &InboundCall{Request: req, Body: mb}
}

func TestDetectSIPREC_RequireHeader(t *testing.T) {
	call := newInviteWithHeaders(t, []byte(testSDPBody), ContentTypeSDP,
		sip.NewHeader("Require", "siprec"))

	got := DetectSIPREC(call)
	if !got.Required {
		t.Error("Required = false, want true")
	}
	if got.HasMetadata {
		t.Error("HasMetadata = true, want false for a plain SDP body")
	}
	if !IsSIPRECInvite(call) {
		t.Error("IsSIPRECInvite = false, want true")
	}
}

func TestDetectSIPREC_ProxyRequireAndCasing(t *testing.T) {
	call := newInviteWithHeaders(t, []byte(testSDPBody), ContentTypeSDP,
		sip.NewHeader("Proxy-Require", "timer, SIPREC"))

	if !DetectSIPREC(call).Required {
		t.Error("Required = false; Proxy-Require must count and matching must be case-insensitive")
	}
}

func TestDetectSIPREC_ContactFeatureTag(t *testing.T) {
	call := newInviteWithHeaders(t, []byte(testSDPBody), ContentTypeSDP,
		sip.NewHeader("Contact", "<sip:sbc@10.0.0.9:5060>;+sip.src"))

	got := DetectSIPREC(call)
	if !got.FeatureTag {
		t.Error("FeatureTag = false, want true")
	}
	if !got.Claimed() {
		t.Error("Claimed() = false, want true")
	}
}

func TestDetectSIPREC_MetadataPart(t *testing.T) {
	const boundary = "b1"
	call := newInviteWithHeaders(t,
		[]byte(multipartSIPRECBody(boundary)),
		"multipart/mixed;boundary="+boundary)

	got := DetectSIPREC(call)
	if !got.HasMetadata {
		t.Error("HasMetadata = false, want true")
	}
	if got.Required || got.FeatureTag {
		t.Errorf("signals = %+v, want only HasMetadata", got)
	}
	if !IsSIPRECInvite(call) {
		t.Error("IsSIPRECInvite = false, want true")
	}
}

func TestDetectSIPREC_OrdinaryInviteIsNot(t *testing.T) {
	call := newInviteWithHeaders(t, []byte(testSDPBody), ContentTypeSDP,
		sip.NewHeader("Contact", "<sip:alice@10.0.0.9:5060>"),
		sip.NewHeader("Require", "timer"),
		sip.NewHeader("Supported", "siprec"))

	got := DetectSIPREC(call)
	if got.Claimed() {
		t.Errorf("an ordinary INVITE was classified as SIPREC: %+v", got)
	}
	if IsSIPRECInvite(call) {
		t.Error("IsSIPRECInvite = true, want false")
	}
}

func TestDetectSIPREC_NilSafe(t *testing.T) {
	if IsSIPRECInvite(nil) {
		t.Error("IsSIPRECInvite(nil) = true, want false")
	}
	if DetectSIPREC(&InboundCall{}).Claimed() {
		t.Error("an empty call was classified as SIPREC")
	}
}

func TestHasOptionTag(t *testing.T) {
	call := newInviteWithHeaders(t, nil, "",
		sip.NewHeader("Require", "  100rel , timer  "))

	if !HasOptionTag(call.Request, "timer") {
		t.Error(`HasOptionTag("timer") = false; tokens must be split and trimmed`)
	}
	if !HasOptionTag(call.Request, "100rel") {
		t.Error(`HasOptionTag("100rel") = false`)
	}
	if HasOptionTag(call.Request, "siprec") {
		t.Error(`HasOptionTag("siprec") = true, want false`)
	}
}

func TestUnsupportedHeader(t *testing.T) {
	h := UnsupportedHeader(OptionTagSIPREC)
	if h.Name() != "Unsupported" {
		t.Errorf("header name = %q, want Unsupported", h.Name())
	}
	if h.Value() != "siprec" {
		t.Errorf("header value = %q, want siprec", h.Value())
	}
}

func TestSetRequestBody_PlainSDPUnchanged(t *testing.T) {
	uri := sip.Uri{User: "bob", Host: "10.0.0.1"}
	req := sip.NewRequest(sip.INVITE, uri)

	if err := setRequestBody(req, []byte(testSDPBody), nil); err != nil {
		t.Fatalf("setRequestBody = %v, want nil", err)
	}
	if string(req.Body()) != testSDPBody {
		t.Errorf("body = %q, want the SDP verbatim", req.Body())
	}
	if h := req.GetHeader("Content-Type"); h == nil || h.Value() != ContentTypeSDP {
		t.Errorf("Content-Type = %v, want application/sdp", h)
	}
}

func TestSetRequestBody_MultipartWithMetadata(t *testing.T) {
	uri := sip.Uri{User: "srs", Host: "10.0.0.1"}
	req := sip.NewRequest(sip.INVITE, uri)

	err := setRequestBody(req, []byte(testSDPBody), []BodyPart{{
		ContentType: ContentTypeRSMetadata,
		Disposition: "recording-session",
		Data:        []byte(testMetadataBody),
	}})
	if err != nil {
		t.Fatalf("setRequestBody = %v, want nil", err)
	}

	mb, err := BodyOf(req)
	if err != nil {
		t.Fatalf("BodyOf = %v, want nil", err)
	}
	if !mb.Multipart {
		t.Fatal("body is not multipart")
	}
	sdp, ok := mb.SDP()
	if !ok || string(sdp) != testSDPBody {
		t.Errorf("SDP part = (%q, %v), want the offer", sdp, ok)
	}
	md, ok := mb.RSMetadata()
	if !ok || string(md) != testMetadataBody {
		t.Errorf("metadata part = (%q, %v), want the document", md, ok)
	}
}
