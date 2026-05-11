package sip

import (
	"context"
	"errors"
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// dialogServerCache and dialogClientCache replace sipgo.NewDialogServerCache /
// sipgo.NewDialogClientCache so we can configure DialogUA.RewriteContact —
// the upstream helpers hide the DialogUA behind an unexported field.

type dialogServerCache struct {
	ua      *sipgo.DialogUA
	dialogs sync.Map // dialogID -> *sipgo.DialogServerSession
}

func newDialogServerCache(ua *sipgo.DialogUA) *dialogServerCache {
	return &dialogServerCache{ua: ua}
}

func (c *dialogServerCache) ReadInvite(req *sip.Request, tx sip.ServerTransaction) (*sipgo.DialogServerSession, error) {
	dtx, err := c.ua.ReadInvite(req, tx)
	if err != nil {
		return nil, err
	}
	id := dtx.ID
	c.dialogs.Store(id, dtx)
	dtx.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			c.dialogs.Delete(id)
		}
	})
	return dtx, nil
}

func (c *dialogServerCache) MatchDialogRequest(req *sip.Request) (*sipgo.DialogServerSession, error) {
	id, err := sip.DialogIDFromRequestUAS(req)
	if err != nil {
		return nil, errors.Join(sipgo.ErrDialogOutsideDialog, err)
	}
	v, ok := c.dialogs.Load(id)
	if !ok {
		return nil, sipgo.ErrDialogDoesNotExists
	}
	return v.(*sipgo.DialogServerSession), nil
}

func (c *dialogServerCache) ReadAck(req *sip.Request, tx sip.ServerTransaction) error {
	dt, err := c.MatchDialogRequest(req)
	if err != nil {
		return err
	}
	return dt.ReadAck(req, tx)
}

func (c *dialogServerCache) ReadBye(req *sip.Request, tx sip.ServerTransaction) error {
	dt, err := c.MatchDialogRequest(req)
	if err != nil {
		return err
	}
	return dt.ReadBye(req, tx)
}

type dialogClientCache struct {
	ua      *sipgo.DialogUA
	dialogs sync.Map // dialogID -> *sipgo.DialogClientSession
}

func newDialogClientCache(ua *sipgo.DialogUA) *dialogClientCache {
	return &dialogClientCache{ua: ua}
}

func (c *dialogClientCache) MatchRequestDialog(req *sip.Request) (*sipgo.DialogClientSession, error) {
	id, err := sip.DialogIDFromRequestUAC(req)
	if err != nil {
		return nil, errors.Join(err, sipgo.ErrDialogOutsideDialog)
	}
	v, ok := c.dialogs.Load(id)
	if !ok {
		return nil, sipgo.ErrDialogDoesNotExists
	}
	return v.(*sipgo.DialogClientSession), nil
}

func (c *dialogClientCache) WriteInvite(ctx context.Context, inviteRequest *sip.Request) (*sipgo.DialogClientSession, error) {
	dt, err := c.ua.WriteInvite(ctx, inviteRequest)
	if err != nil {
		return nil, err
	}
	dt.OnState(func(s sip.DialogState) {
		switch s {
		case sip.DialogStateEstablished:
			if dt.ID != "" {
				c.dialogs.Store(dt.ID, dt)
			}
		case sip.DialogStateEnded:
			if dt.ID != "" {
				c.dialogs.Delete(dt.ID)
			}
		}
	})
	return dt, nil
}

func (c *dialogClientCache) ReadBye(req *sip.Request, tx sip.ServerTransaction) error {
	dt, err := c.MatchRequestDialog(req)
	if err != nil {
		return err
	}
	return dt.ReadBye(req, tx)
}
