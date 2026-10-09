package sip

import (
	"reflect"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestCustomHeaders(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "a", Host: "example.com"})
	req.AppendHeader(sip.NewHeader("X-Upper", "1"))
	req.AppendHeader(sip.NewHeader("x-lower", "2"))
	req.AppendHeader(sip.NewHeader("P-Asserted-Identity", "<sip:alice@example.com>"))
	req.AppendHeader(sip.NewHeader("p-charging-vector", "icid-value=abc"))
	req.AppendHeader(sip.NewHeader("X-Dup", "first"))
	req.AppendHeader(sip.NewHeader("X-Dup", "last"))
	req.AppendHeader(sip.NewHeader("Subject", "ignored"))
	req.AppendHeader(sip.NewHeader("Priority", "ignored"))
	req.AppendHeader(sip.NewHeader("Xavier", "ignored"))

	want := map[string]string{
		"X-Upper":             "1",
		"x-lower":             "2",
		"P-Asserted-Identity": "<sip:alice@example.com>",
		"p-charging-vector":   "icid-value=abc",
		"X-Dup":               "last",
	}
	if got := CustomHeaders(req); !reflect.DeepEqual(got, want) {
		t.Errorf("CustomHeaders = %v, want %v", got, want)
	}
}

func TestCustomHeaders_None(t *testing.T) {
	if got := CustomHeaders(nil); got != nil {
		t.Errorf("CustomHeaders(nil) = %v, want nil", got)
	}
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "a", Host: "example.com"})
	req.AppendHeader(sip.NewHeader("Subject", "hello"))
	if got := CustomHeaders(req); got != nil {
		t.Errorf("CustomHeaders = %v, want nil", got)
	}
}

func TestIsCustomHeaderName(t *testing.T) {
	cases := map[string]bool{
		"X-Foo": true, "x-foo": true, "P-Foo": true, "p-foo": true,
		"X-": false, "x": false, "": false, "XFoo": false,
		"Y-Foo": false, "Proxy-Authorization": false, "Priority": false,
	}
	for name, want := range cases {
		if got := isCustomHeaderName(name); got != want {
			t.Errorf("isCustomHeaderName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLookupHeader(t *testing.T) {
	hdrs := map[string]string{"x-app-id": "lower", "P-Called-Party-ID": "sip:bob@example.com"}
	if v, ok := LookupHeader(hdrs, "X-App-ID"); !ok || v != "lower" {
		t.Errorf("LookupHeader(X-App-ID) = %q, %v", v, ok)
	}
	if v, ok := LookupHeader(hdrs, "p-called-party-id"); !ok || v != "sip:bob@example.com" {
		t.Errorf("LookupHeader(p-called-party-id) = %q, %v", v, ok)
	}
	if _, ok := LookupHeader(hdrs, "X-Missing"); ok {
		t.Error("LookupHeader(X-Missing) found a value")
	}
	if _, ok := LookupHeader(nil, "X-App-ID"); ok {
		t.Error("LookupHeader on nil map found a value")
	}

	both := map[string]string{"X-App-ID": "exact", "x-app-id": "folded"}
	if v, _ := LookupHeader(both, "X-App-ID"); v != "exact" {
		t.Errorf("exact-case key should win, got %q", v)
	}
}
