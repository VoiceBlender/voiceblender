package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Listener owns the single process-wide /sync loop authenticated with the
// service-account credentials configured via env vars. On every fresh
// m.call.invite delivered through /sync it dispatches the InboundHandler;
// other m.call.* events are routed to per-call subscribers (the inbound legs).
type Listener struct {
	mx      *mautrix.Client
	handler InboundHandler
	log     *slog.Logger

	disp *dispatcher

	// crypto is non-nil only when built with the `goolm` tag; nil-safe.
	crypto *cryptoHandle

	tokenExpired atomic.Bool
}

// ListenerConfig configures the global Matrix Listener.
type ListenerConfig struct {
	HomeserverURL string
	AccessToken   string
	UserID        id.UserID
	DeviceID      id.DeviceID
	SyncTimeoutMs int // mautrix default is 30s
}

// NewListener constructs a Listener without starting it. The InboundHandler is
// invoked exactly once per fresh m.call.invite delivered through /sync; ev
// holds the parsed invite payload, and sender lets the handler send
// answer/candidates/hangup for the new call without taking a direct dependency
// on the Listener type.
func NewListener(cfg ListenerConfig, handler InboundHandler, log *slog.Logger) (*Listener, error) {
	if cfg.HomeserverURL == "" {
		return nil, errors.New("homeserver URL is required")
	}
	if cfg.UserID == "" {
		return nil, errors.New("user ID is required")
	}
	if cfg.AccessToken == "" {
		return nil, errors.New("access token is required")
	}
	if handler == nil {
		return nil, errors.New("inbound handler is required")
	}
	if log == nil {
		log = slog.Default()
	}
	mx, err := mautrix.NewClient(cfg.HomeserverURL, cfg.UserID, cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("mautrix.NewClient: %w", err)
	}
	mx.DeviceID = cfg.DeviceID
	mx.Store = mautrix.NewMemorySyncStore()

	l := &Listener{
		mx:      mx,
		handler: handler,
		log:     log,
		disp:    newDispatcher(),
	}
	syncer := mautrix.NewDefaultSyncer()
	syncer.ParseEventContent = true
	syncer.FilterJSON = &mautrix.Filter{
		Room: &mautrix.RoomFilter{
			Timeline: &mautrix.FilterPart{
				Types: SubscribedEventTypes,
				Limit: 50,
			},
			// State events are required for crypto (m.room.encryption,
			// m.room.member). Filter dropped; state passes through.
			Ephemeral:   &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
			AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		},
		Presence:    &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
	}
	for _, t := range CallEventTypes {
		syncer.OnEventType(t, l.onCallEvent)
	}
	syncer.OnEventType(event.EventEncrypted, l.onEncryptedEvent)
	mx.Syncer = syncer
	// Crypto helper is best-effort; on failure log and proceed plaintext.
	if h, err := newCryptoHandle(mx, log); err != nil {
		log.Warn("matrix listener: crypto helper init failed; sending plaintext",
			"error", err)
	} else {
		l.crypto = h
	}
	return l, nil
}

// MautrixClient exposes the underlying mautrix.Client for tests.
func (l *Listener) MautrixClient() *mautrix.Client { return l.mx }

// Run starts the sync loop and blocks until ctx is cancelled or a fatal,
// non-token-expired error from mautrix bubbles up. On M_UNKNOWN_TOKEN the loop
// backs off 30s and retries; operators are expected to rotate the env var and
// restart for permanent revocation.
func (l *Listener) Run(ctx context.Context) error {
	const tokenBackoff = 30 * time.Second
	// Initialise the cryptohelper before the first /sync so the syncer is
	// wrapped to auto-decrypt incoming m.room.encrypted events. Nil-safe.
	if err := l.crypto.Init(ctx); err != nil {
		l.log.Warn("matrix listener: crypto init failed; sending plaintext",
			"error", err)
		l.crypto = nil
	}
	defer func() {
		if err := l.crypto.Close(); err != nil {
			l.log.Debug("matrix listener: crypto close error", "error", err)
		}
	}()
	for {
		if ctx.Err() != nil {
			l.disp.closeAll()
			return ctx.Err()
		}
		err := l.mx.SyncWithContext(ctx)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			l.disp.closeAll()
			return nil
		}
		// Token revoked / expired: pause, retry, keep ringing legs from
		// silently leaking.
		if errors.Is(err, mautrix.MUnknownToken) {
			l.tokenExpired.Store(true)
			l.log.Error("matrix listener: access token rejected; backing off", "backoff", tokenBackoff)
			select {
			case <-ctx.Done():
				l.disp.closeAll()
				return ctx.Err()
			case <-time.After(tokenBackoff):
				continue
			}
		}
		l.log.Warn("matrix listener: sync error; retrying", "error", err)
		select {
		case <-ctx.Done():
			l.disp.closeAll()
			return ctx.Err()
		case <-time.After(5 * time.Second):
			continue
		}
	}
}

// TokenExpired reports whether the most recent /sync failed with
// M_UNKNOWN_TOKEN. Cleared once a subsequent /sync succeeds.
func (l *Listener) TokenExpired() bool { return l.tokenExpired.Load() }

func (l *Listener) onCallEvent(ctx context.Context, evt *event.Event) {
	l.tokenExpired.Store(false)
	if evt.Sender == l.mx.UserID {
		l.log.Debug("matrix listener: ignoring own event",
			"type", evt.Type.Type, "event_id", evt.ID, "room_id", evt.RoomID)
		return
	}
	ev := decodeCallEvent(evt)
	if ev == nil {
		l.log.Warn("matrix listener: failed to decode call event",
			"type", evt.Type.Type, "event_id", evt.ID, "sender", evt.Sender, "room_id", evt.RoomID)
		return
	}

	if ev.Kind == KindInvite {
		// Treat already-expired invites as stale (per MSC2746 the caller
		// would have timed out client-side anyway).
		if ev.Invite.Lifetime > 0 {
			age := time.Since(time.UnixMilli(evt.Timestamp))
			if age > time.Duration(ev.Invite.Lifetime)*time.Millisecond {
				l.log.Info("matrix listener: dropping stale invite",
					"call_id", ev.CallID, "age_ms", age.Milliseconds(), "lifetime_ms", ev.Invite.Lifetime)
				return
			}
		}
		l.log.Info("matrix listener: dispatching inbound invite",
			"call_id", ev.CallID, "sender", ev.Sender, "room_id", ev.RoomID)
		go l.handler(ctx, ev, l)
		return
	}

	// Route in-call events to the leg subscribed for (room, call_id).
	delivered := l.disp.dispatch(*ev)
	if delivered {
		l.log.Info("matrix listener: received call event",
			"kind", ev.Kind, "call_id", ev.CallID, "sender", ev.Sender, "room_id", ev.RoomID)
	} else {
		l.log.Info("matrix listener: received call event but no subscriber",
			"kind", ev.Kind, "call_id", ev.CallID, "sender", ev.Sender, "room_id", ev.RoomID,
			"hint", "the call may have already ended, or the inbound handler/leg has not subscribed yet")
	}
}

// onEncryptedEvent fires when the homeserver delivers an m.room.encrypted
// event into a room we are syncing. With the `goolm` build tag the
// cryptohelper transparently decrypts these and re-dispatches them to the
// normal call-event handlers, so this only fires when decryption failed or
// crypto is not compiled in.
func (l *Listener) onEncryptedEvent(ctx context.Context, evt *event.Event) {
	if evt.Sender == l.mx.UserID {
		return
	}
	if cryptoCompiledIn() {
		l.log.Debug("matrix listener: m.room.encrypted event reached handler — likely undecryptable",
			"room_id", evt.RoomID, "sender", evt.Sender, "event_id", evt.ID)
		return
	}
	l.log.Warn("matrix listener: received m.room.encrypted event — room is E2EE; m.call.* events cannot be read",
		"room_id", evt.RoomID, "sender", evt.Sender, "event_id", evt.ID,
		"hint", "rebuild with `-tags goolm` to enable end-to-end encryption, or use an unencrypted room")
}

// EventSender implementation. All Send* methods authenticate as the
// service account, encrypting via megolm if the room is encrypted and the
// goolm build tag is set.

func (l *Listener) SendAnswer(ctx context.Context, roomID id.RoomID, content *event.CallAnswerEventContent) error {
	return l.crypto.SendOrEncrypt(ctx, l.mx, roomID, event.CallAnswer, content)
}

func (l *Listener) SendCandidates(ctx context.Context, roomID id.RoomID, content *event.CallCandidatesEventContent) error {
	return l.crypto.SendOrEncrypt(ctx, l.mx, roomID, event.CallCandidates, content)
}

func (l *Listener) SendHangup(ctx context.Context, roomID id.RoomID, content *event.CallHangupEventContent) error {
	return l.crypto.SendOrEncrypt(ctx, l.mx, roomID, event.CallHangup, content)
}

func (l *Listener) Subscribe(roomID id.RoomID, callID string) <-chan CallEvent {
	return l.disp.subscribe(roomID, callID)
}

func (l *Listener) Unsubscribe(roomID id.RoomID, callID string) {
	l.disp.unsubscribe(roomID, callID)
}

// UserID returns the Matrix user ID this Listener authenticates as.
func (l *Listener) UserID() id.UserID { return l.mx.UserID }
