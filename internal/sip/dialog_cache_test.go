package sip

import (
	"errors"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestDialogServerCache_MatchEmpty(t *testing.T) {
	c := newDialogServerCache(&sipgo.DialogUA{})
	req := sip.NewRequest(sip.BYE, sip.Uri{Scheme: "sip", Host: "example.com"})
	if _, err := c.MatchDialogRequest(req); err == nil {
		t.Fatal("expected error for missing dialog headers")
	}
}

func TestDialogClientCache_MatchEmpty(t *testing.T) {
	c := newDialogClientCache(&sipgo.DialogUA{})
	req := sip.NewRequest(sip.BYE, sip.Uri{Scheme: "sip", Host: "example.com"})
	_, err := c.MatchRequestDialog(req)
	if err == nil {
		t.Fatal("expected error for missing dialog headers")
	}
	// Errors should join sipgo's sentinel so callers can branch on it.
	if !errors.Is(err, sipgo.ErrDialogOutsideDialog) {
		t.Errorf("err = %v, want chain containing ErrDialogOutsideDialog", err)
	}
}
