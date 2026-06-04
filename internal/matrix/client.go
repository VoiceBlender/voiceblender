package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Client wraps mautrix.Client for outbound MSC3401 use: one Client per
// outbound MatrixLeg, authenticated with REST-supplied credentials, running a
// narrow /sync filtered to the target Matrix room only. The Client both sends
// m.call.* events and dispatches the inbound side (answer / candidates /
// hangup) to per-call subscribers.
type Client struct {
	mx     *mautrix.Client
	roomID id.RoomID
	log    *slog.Logger

	disp *dispatcher

	// crypto is non-nil only when the matrix package was built with the
	// `goolm` tag. In default builds it is nil and SendOrEncrypt falls
	// through to plaintext SendMessageEvent. Method calls are nil-safe.
	crypto *cryptoHandle

	cancel atomic.Value // context.CancelFunc

	startOnce sync.Once
	startErr  error
}

// ClientConfig configures a per-leg outbound Matrix Client.
type ClientConfig struct {
	HomeserverURL string    // e.g. "https://matrix.example.org"
	UserID        id.UserID // e.g. "@bot:example.org"
	DeviceID      id.DeviceID
	AccessToken   string
	RoomID        id.RoomID // narrow /sync target
	Log           *slog.Logger
}

// NewClient constructs a Client without starting the sync loop. Call Start
// once the leg is registered so the dispatcher can deliver inbound events.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.HomeserverURL == "" {
		return nil, errors.New("homeserver URL is required")
	}
	if cfg.UserID == "" {
		return nil, errors.New("user ID is required")
	}
	if cfg.AccessToken == "" {
		return nil, errors.New("access token is required")
	}
	if cfg.RoomID == "" {
		return nil, errors.New("room ID is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	mx, err := mautrix.NewClient(cfg.HomeserverURL, cfg.UserID, cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("mautrix.NewClient: %w", err)
	}
	mx.DeviceID = cfg.DeviceID
	mx.Store = mautrix.NewMemorySyncStore()
	c := &Client{
		mx:     mx,
		roomID: cfg.RoomID,
		log:    log,
		disp:   newDispatcher(),
	}
	c.installSyncer()
	// Crypto helper construction is best-effort. On failure (e.g. SQLite
	// driver missing in a default build that somehow reached here) we log
	// and proceed with plaintext-only behaviour.
	if h, err := newCryptoHandle(mx, log); err != nil {
		log.Warn("matrix client: crypto helper init failed; sending plaintext",
			"error", err)
	} else {
		c.crypto = h
	}
	return c, nil
}

// MautrixClient exposes the underlying mautrix.Client for advanced use cases
// (e.g. tests). Callers must not modify Store, Syncer, or run their own Sync.
func (c *Client) MautrixClient() *mautrix.Client { return c.mx }

func (c *Client) installSyncer() {
	syncer := mautrix.NewDefaultSyncer()
	syncer.ParseEventContent = true
	syncer.FilterJSON = &mautrix.Filter{
		Room: &mautrix.RoomFilter{
			Rooms: []id.RoomID{c.roomID},
			Timeline: &mautrix.FilterPart{
				Types: SubscribedEventTypes,
				Limit: 50,
			},
			// State events are required for crypto: m.room.encryption
			// tells the cryptohelper which rooms to encrypt for, and
			// m.room.member is needed for device tracking / key sharing.
			// Filter is dropped (no NotTypes) so all state passes through.
			Ephemeral:   &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
			AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		},
		Presence:    &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
	}
	for _, t := range CallEventTypes {
		syncer.OnEventType(t, c.onCallEvent)
	}
	syncer.OnEventType(event.EventEncrypted, c.onEncryptedEvent)
	c.mx.Syncer = syncer
}

// Start runs the /sync loop in a goroutine. Subsequent calls are no-ops.
// Stop() (or context cancellation) terminates the loop.
func (c *Client) Start(ctx context.Context) error {
	c.startOnce.Do(func() {
		// cryptohelper.Init wraps the syncer to add auto-decryption; it
		// must run after installSyncer (done in NewClient) and before
		// sync begins. Nil-safe in default builds.
		if err := c.crypto.Init(ctx); err != nil {
			c.log.Warn("matrix client: crypto init failed; sending plaintext",
				"error", err)
			c.crypto = nil
		}
		syncCtx, cancel := context.WithCancel(ctx)
		c.cancel.Store(cancel)
		go func() {
			defer cancel()
			if err := c.mx.SyncWithContext(syncCtx); err != nil && !errors.Is(err, context.Canceled) {
				c.log.Warn("matrix client: sync exited", "user_id", c.mx.UserID, "room_id", c.roomID, "error", err)
				c.startErr = err
			}
			c.disp.closeAll()
		}()
	})
	return c.startErr
}

// Close stops the sync loop and tears down subscriptions. Idempotent.
func (c *Client) Close() error {
	if v := c.cancel.Load(); v != nil {
		if cf, ok := v.(context.CancelFunc); ok && cf != nil {
			cf()
		}
	}
	if err := c.crypto.Close(); err != nil {
		c.log.Debug("matrix client: crypto close error", "error", err)
	}
	return nil
}

func (c *Client) onCallEvent(ctx context.Context, evt *event.Event) {
	if evt.Sender == c.mx.UserID {
		c.log.Debug("matrix client: ignoring own event",
			"type", evt.Type.Type, "event_id", evt.ID, "room_id", evt.RoomID)
		return
	}
	ev := decodeCallEvent(evt)
	if ev == nil {
		c.log.Warn("matrix client: failed to decode call event",
			"type", evt.Type.Type, "event_id", evt.ID, "sender", evt.Sender, "room_id", evt.RoomID)
		return
	}
	delivered := c.disp.dispatch(*ev)
	if delivered {
		c.log.Info("matrix client: received call event",
			"kind", ev.Kind, "call_id", ev.CallID, "sender", ev.Sender, "room_id", ev.RoomID)
	} else {
		c.log.Info("matrix client: received call event but no subscriber",
			"kind", ev.Kind, "call_id", ev.CallID, "sender", ev.Sender, "room_id", ev.RoomID,
			"hint", "the call may have already ended, or the await/pump goroutine has not subscribed yet")
	}
}

// onEncryptedEvent fires when the homeserver delivers an m.room.encrypted
// event into a room we are syncing. With the `goolm` build tag the
// cryptohelper transparently decrypts these and re-dispatches them through
// the normal call-event handlers, so this only fires when decryption failed
// or crypto is not compiled in.
func (c *Client) onEncryptedEvent(ctx context.Context, evt *event.Event) {
	if evt.Sender == c.mx.UserID {
		return
	}
	if cryptoCompiledIn() {
		c.log.Debug("matrix client: m.room.encrypted event reached handler — likely undecryptable",
			"room_id", evt.RoomID, "sender", evt.Sender, "event_id", evt.ID)
		return
	}
	c.log.Warn("matrix client: received m.room.encrypted event — room is E2EE; m.call.* events cannot be read",
		"room_id", evt.RoomID, "sender", evt.Sender, "event_id", evt.ID,
		"hint", "rebuild with `-tags goolm` to enable end-to-end encryption, or use an unencrypted room")
}

// decodeCallEvent extracts the canonical (RoomID, CallID, kind-specific
// payload) tuple from a parsed mautrix Event. Returns nil if the event is not
// a recognised m.call.* type or is missing a call_id.
func decodeCallEvent(evt *event.Event) *CallEvent {
	if evt == nil {
		return nil
	}
	out := &CallEvent{
		RoomID: evt.RoomID,
		Sender: evt.Sender,
	}
	switch evt.Type {
	case event.CallInvite:
		c, ok := evt.Content.Parsed.(*event.CallInviteEventContent)
		if !ok {
			return nil
		}
		out.Kind = KindInvite
		out.CallID = c.CallID
		out.Invite = c
	case event.CallAnswer:
		c, ok := evt.Content.Parsed.(*event.CallAnswerEventContent)
		if !ok {
			return nil
		}
		out.Kind = KindAnswer
		out.CallID = c.CallID
		out.Answer = c
	case event.CallCandidates:
		c, ok := evt.Content.Parsed.(*event.CallCandidatesEventContent)
		if !ok {
			return nil
		}
		out.Kind = KindCandidates
		out.CallID = c.CallID
		out.Candidates = c
	case event.CallHangup:
		c, ok := evt.Content.Parsed.(*event.CallHangupEventContent)
		if !ok {
			return nil
		}
		out.Kind = KindHangup
		out.CallID = c.CallID
		out.Hangup = c
	case event.CallReject:
		c, ok := evt.Content.Parsed.(*event.CallRejectEventContent)
		if !ok {
			return nil
		}
		out.Kind = KindReject
		out.CallID = c.CallID
		out.Reject = c
	case event.CallNegotiate:
		c, ok := evt.Content.Parsed.(*event.CallNegotiateEventContent)
		if !ok {
			return nil
		}
		out.Kind = KindNegotiate
		out.CallID = c.CallID
		out.Negotiate = c
	default:
		return nil
	}
	if out.CallID == "" {
		return nil
	}
	return out
}

// SendInvite is the outbound-only counterpart to the EventSender methods.
// Caller must populate offer SDP, lifetime, call_id, party_id, version.
func (c *Client) SendInvite(ctx context.Context, content *event.CallInviteEventContent) error {
	return c.crypto.SendOrEncrypt(ctx, c.mx, c.roomID, event.CallInvite, content)
}

// SendAnswer satisfies EventSender. roomID must equal the configured room.
func (c *Client) SendAnswer(ctx context.Context, roomID id.RoomID, content *event.CallAnswerEventContent) error {
	if roomID == "" {
		roomID = c.roomID
	}
	return c.crypto.SendOrEncrypt(ctx, c.mx, roomID, event.CallAnswer, content)
}

func (c *Client) SendCandidates(ctx context.Context, roomID id.RoomID, content *event.CallCandidatesEventContent) error {
	if roomID == "" {
		roomID = c.roomID
	}
	return c.crypto.SendOrEncrypt(ctx, c.mx, roomID, event.CallCandidates, content)
}

func (c *Client) SendHangup(ctx context.Context, roomID id.RoomID, content *event.CallHangupEventContent) error {
	if roomID == "" {
		roomID = c.roomID
	}
	return c.crypto.SendOrEncrypt(ctx, c.mx, roomID, event.CallHangup, content)
}

func (c *Client) Subscribe(roomID id.RoomID, callID string) <-chan CallEvent {
	return c.disp.subscribe(roomID, callID)
}

func (c *Client) Unsubscribe(roomID id.RoomID, callID string) {
	c.disp.unsubscribe(roomID, callID)
}

// AwaitAnswer blocks until an m.call.answer arrives for callID or ctx ends.
// The subscription is registered eagerly so callers can compose AwaitAnswer
// with SendInvite without missing a fast answer.
func (c *Client) AwaitAnswer(ctx context.Context, callID string) (*event.CallAnswerEventContent, *CallEvent, error) {
	ch := c.disp.subscribe(c.roomID, callID)
	defer c.disp.unsubscribe(c.roomID, callID)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil, nil, errors.New("subscription closed")
			}
			switch ev.Kind {
			case KindAnswer:
				return ev.Answer, &ev, nil
			case KindHangup:
				return nil, &ev, fmt.Errorf("remote hung up before answer: %s", ev.Hangup.Reason)
			case KindReject:
				return nil, &ev, errors.New("remote rejected invite")
			default:
				// Candidates can arrive before answer (trickle); ignore here.
				continue
			}
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// RoomID returns the Matrix room this Client is bound to.
func (c *Client) RoomID() id.RoomID { return c.roomID }

// UserID returns the Matrix user ID this Client authenticates as.
func (c *Client) UserID() id.UserID { return c.mx.UserID }
