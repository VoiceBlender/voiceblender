package sip

import (
	"bytes"
	"strings"
	"testing"
)

const testSDPBody = "v=0\r\n" +
	"o=- 1 1 IN IP4 10.0.0.1\r\n" +
	"s=-\r\n" +
	"c=IN IP4 10.0.0.1\r\n" +
	"t=0 0\r\n" +
	"m=audio 40000 RTP/AVP 0\r\n" +
	"a=sendonly\r\n"

const testMetadataBody = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<recording xmlns="urn:ietf:params:xml:ns:recording:1"><datamode>complete</datamode></recording>`

func multipartSIPRECBody(boundary string) string {
	return "--" + boundary + "\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Content-Disposition: session;handling=required\r\n" +
		"\r\n" +
		testSDPBody +
		"\r\n--" + boundary + "\r\n" +
		"Content-Type: application/rs-metadata+xml\r\n" +
		"Content-Disposition: recording-session\r\n" +
		"\r\n" +
		testMetadataBody +
		"\r\n--" + boundary + "--\r\n"
}

func TestParseMessageBody_SinglePartIsVerbatim(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
	}{
		{"declared sdp", "application/sdp"},
		{"declared with charset", "application/sdp;charset=UTF-8"},
		{"absent content type", ""},
		{"misdeclared", "text/plain"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mb, err := ParseMessageBody(tc.contentType, []byte(testSDPBody))
			if err != nil {
				t.Fatalf("ParseMessageBody = %v, want nil", err)
			}
			if mb.Multipart {
				t.Fatalf("Multipart = true, want false")
			}
			if len(mb.Parts) != 1 {
				t.Fatalf("len(Parts) = %d, want 1", len(mb.Parts))
			}
			sdp, ok := mb.SDP()
			if !ok {
				t.Fatal("SDP() = (_, false), want true")
			}
			if !bytes.Equal(sdp, []byte(testSDPBody)) {
				t.Fatalf("SDP() returned %q, want the body verbatim", sdp)
			}
		})
	}
}

func TestParseMessageBody_EmptyBodyHasNoSDP(t *testing.T) {
	mb, err := ParseMessageBody("", nil)
	if err != nil {
		t.Fatalf("ParseMessageBody = %v, want nil", err)
	}
	if _, ok := mb.SDP(); ok {
		t.Fatal("SDP() = (_, true), want false for an empty body")
	}
}

func TestParseMessageBody_MultipartSIPREC(t *testing.T) {
	const boundary = "unique-boundary-1"
	mb, err := ParseMessageBody(`multipart/mixed;boundary="`+boundary+`"`, []byte(multipartSIPRECBody(boundary)))
	if err != nil {
		t.Fatalf("ParseMessageBody = %v, want nil", err)
	}
	if !mb.Multipart {
		t.Fatal("Multipart = false, want true")
	}
	if len(mb.Parts) != 2 {
		t.Fatalf("len(Parts) = %d, want 2", len(mb.Parts))
	}

	sdp, ok := mb.SDP()
	if !ok {
		t.Fatal("SDP() = (_, false), want true")
	}
	if string(sdp) != testSDPBody {
		t.Fatalf("SDP() = %q, want %q", sdp, testSDPBody)
	}

	meta, ok := mb.RSMetadata()
	if !ok {
		t.Fatal("RSMetadata() = (_, false), want true")
	}
	if string(meta) != testMetadataBody {
		t.Fatalf("RSMetadata() = %q, want %q", meta, testMetadataBody)
	}

	if got := mb.Parts[0].Disposition; got != "session" {
		t.Fatalf("Parts[0].Disposition = %q, want %q", got, "session")
	}
	if got := mb.Parts[1].Disposition; got != "recording-session" {
		t.Fatalf("Parts[1].Disposition = %q, want %q", got, "recording-session")
	}
}

func TestParseMessageBody_LegacyMetadataContentType(t *testing.T) {
	const boundary = "b1"
	body := "--" + boundary + "\r\n" +
		"Content-Type: application/sdp\r\n\r\n" + testSDPBody +
		"\r\n--" + boundary + "\r\n" +
		"Content-Type: application/rs-metadata\r\n\r\n" + testMetadataBody +
		"\r\n--" + boundary + "--\r\n"

	mb, err := ParseMessageBody("multipart/mixed;boundary="+boundary, []byte(body))
	if err != nil {
		t.Fatalf("ParseMessageBody = %v, want nil", err)
	}
	if _, ok := mb.RSMetadata(); !ok {
		t.Fatal("RSMetadata() = (_, false), want true for the legacy content type")
	}
}

func TestParseMessageBody_MetadataOnlyHasNoSDP(t *testing.T) {
	const boundary = "b1"
	body := "--" + boundary + "\r\n" +
		"Content-Type: application/rs-metadata+xml\r\n\r\n" + testMetadataBody +
		"\r\n--" + boundary + "--\r\n"

	mb, err := ParseMessageBody("multipart/mixed;boundary="+boundary, []byte(body))
	if err != nil {
		t.Fatalf("ParseMessageBody = %v, want nil", err)
	}
	if _, ok := mb.SDP(); ok {
		t.Fatal("SDP() = (_, true), want false")
	}
	if _, ok := mb.RSMetadata(); !ok {
		t.Fatal("RSMetadata() = (_, false), want true")
	}
}

func TestParseMessageBody_Errors(t *testing.T) {
	if _, err := ParseMessageBody("multipart/mixed", []byte("whatever")); err == nil {
		t.Fatal("ParseMessageBody with no boundary = nil error, want an error")
	}
	if _, err := ParseMessageBody("application/sdp;;", []byte(testSDPBody)); err == nil {
		t.Fatal("ParseMessageBody with a malformed content type = nil error, want an error")
	}
}

func TestBuildMultipartMixed_RoundTrip(t *testing.T) {
	boundary := MultipartBoundary("call-id-abc@10.0.0.1")

	ct, body, err := BuildMultipartMixed(boundary, []BodyPart{
		{ContentType: ContentTypeSDP, Disposition: "session;handling=required", Data: []byte(testSDPBody)},
		{ContentType: ContentTypeRSMetadata, Disposition: "recording-session", Data: []byte(testMetadataBody)},
	})
	if err != nil {
		t.Fatalf("BuildMultipartMixed = %v, want nil", err)
	}
	if !strings.HasPrefix(ct, "multipart/mixed;boundary=") {
		t.Fatalf("content type = %q, want a multipart/mixed type", ct)
	}

	mb, err := ParseMessageBody(ct, body)
	if err != nil {
		t.Fatalf("ParseMessageBody of built body = %v, want nil", err)
	}
	sdp, ok := mb.SDP()
	if !ok || string(sdp) != testSDPBody {
		t.Fatalf("round-tripped SDP = (%q, %v), want the original", sdp, ok)
	}
	meta, ok := mb.RSMetadata()
	if !ok || string(meta) != testMetadataBody {
		t.Fatalf("round-tripped metadata = (%q, %v), want the original", meta, ok)
	}
}

func TestBuildMultipartMixed_Deterministic(t *testing.T) {
	parts := []BodyPart{{ContentType: ContentTypeSDP, Data: []byte(testSDPBody)}}
	boundary := MultipartBoundary("seed")

	_, first, err := BuildMultipartMixed(boundary, parts)
	if err != nil {
		t.Fatalf("BuildMultipartMixed = %v, want nil", err)
	}
	_, second, err := BuildMultipartMixed(MultipartBoundary("seed"), parts)
	if err != nil {
		t.Fatalf("BuildMultipartMixed = %v, want nil", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the same seed and parts produced different bytes")
	}
	if MultipartBoundary("seed") == MultipartBoundary("other") {
		t.Fatal("different seeds produced the same boundary")
	}
}

func TestBuildMultipartMixed_NoParts(t *testing.T) {
	if _, _, err := BuildMultipartMixed("b", nil); err == nil {
		t.Fatal("BuildMultipartMixed with no parts = nil error, want an error")
	}
}
