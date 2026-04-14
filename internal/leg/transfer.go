package leg

import (
	"context"
	"fmt"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// DialogIdentity returns (callID, localTag, remoteTag) from this leg's view.
func (l *SIPLeg) DialogIdentity() (callID, localTag, remoteTag string, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.inbound != nil {
		req := l.inbound.Dialog.InviteRequest
		res := l.inbound.Dialog.InviteResponse
		if req == nil || res == nil {
			return "", "", "", false
		}
		callID = ""
		if cid := req.CallID(); cid != nil {
			callID = cid.Value()
		}
		if to := res.To(); to != nil {
			if t, ok2 := to.Params.Get("tag"); ok2 {
				localTag = t
			}
		}
		if from := req.From(); from != nil {
			if t, ok2 := from.Params.Get("tag"); ok2 {
				remoteTag = t
			}
		}
		return callID, localTag, remoteTag, callID != "" && localTag != "" && remoteTag != ""
	}
	if l.outbound != nil {
		req := l.outbound.Dialog.InviteRequest
		res := l.outbound.Dialog.InviteResponse
		if req == nil || res == nil {
			return "", "", "", false
		}
		if cid := req.CallID(); cid != nil {
			callID = cid.Value()
		}
		if from := req.From(); from != nil {
			if t, ok2 := from.Params.Get("tag"); ok2 {
				localTag = t
			}
		}
		if to := res.To(); to != nil {
			if t, ok2 := to.Params.Get("tag"); ok2 {
				remoteTag = t
			}
		}
		return callID, localTag, remoteTag, callID != "" && localTag != "" && remoteTag != ""
	}
	return "", "", "", false
}

// Transfer sends an in-dialog REFER. replaces non-nil = attended (RFC 3891).
func (l *SIPLeg) Transfer(ctx context.Context, target string, replaces *sipmod.ReplacesParams) error {
	l.mu.RLock()
	st := l.state
	l.mu.RUnlock()
	if st != StateConnected {
		return fmt.Errorf("leg is %s, must be connected to transfer", st)
	}

	var dialog interface{}
	if l.inbound != nil {
		dialog = l.inbound.Dialog
	} else if l.outbound != nil {
		dialog = l.outbound.Dialog
	} else {
		return fmt.Errorf("no dialog available")
	}
	return l.engine.SendRefer(ctx, dialog, target, replaces)
}

// SendNotifySipfrag emits a NOTIFY sipfrag for the implicit refer subscription.
func (l *SIPLeg) SendNotifySipfrag(ctx context.Context, statusCode int, reason string, terminated bool) error {
	var dialog interface{}
	if l.inbound != nil {
		dialog = l.inbound.Dialog
	} else if l.outbound != nil {
		dialog = l.outbound.Dialog
	} else {
		return fmt.Errorf("no dialog available")
	}
	return l.engine.SendNotifySipfrag(ctx, dialog, statusCode, reason, terminated)
}
