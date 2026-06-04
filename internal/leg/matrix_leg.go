package leg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/matrix"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MatrixLeg implements the Leg interface over MSC3401 1:1 VoIP calls. Matrix
// is the signaling channel; pion handles SDP/ICE/DTLS-SRTP/Opus locally
// through an embedded PCMedia (same one used by WebRTC and WhatsApp legs).
//
// The leg is direction-agnostic at the media level — the only difference
// between inbound and outbound is whether Answer() drives an m.call.answer
// (inbound) or is a no-op (outbound, which connects via ConnectOutbound after
// the API handler awaits the remote answer).
type MatrixLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	media  *PCMedia
	sender matrix.EventSender

	// Matrix room (DIFFERENT from VoiceBlender mixer room).
	matrixRoomID id.RoomID

	// MSC3401 identifiers.
	callID        string
	partyID       string // ours
	remotePartyID string // locked on first inbound answer / outbound invite
	remoteUserID  id.UserID

	roomID     string // VoiceBlender mixer room
	appID      string
	role       string
	muted      atomic.Bool
	deaf       atomic.Bool
	acceptDTMF atomic.Bool

	createdAt  time.Time
	answeredAt time.Time

	// Inbound only.
	answerCh  chan struct{}
	answerSDP string // local description SDP from inbound CreateAnswer + gather

	onDTMF func(digit rune)
	log    *slog.Logger

	disconnectDone atomic.Bool
	hangupOnce     sync.Once
	pumpCancel     atomic.Value // context.CancelFunc

	// onRemoteHangup is invoked once when the remote sends m.call.hangup.
	// Wired by the API handler after construction so it can drive
	// publishDisconnect/cleanupLeg without an import cycle.
	onRemoteHangup atomic.Value // func(reason string)
}

// MatrixLegConfig configures both inbound and outbound MatrixLegs.
type MatrixLegConfig struct {
	Media        *PCMedia
	Sender       matrix.EventSender
	MatrixRoomID id.RoomID
	CallID       string
	PartyID      string // ours; auto-generated if empty
	Log          *slog.Logger

	// Inbound only.
	RemoteUserID  id.UserID
	RemotePartyID string
	AnswerSDP     string // pre-built answer SDP after gather
}

// NewMatrixOutboundPendingLeg creates a ringing-state leg without a sent
// invite. The API handler drives invite + AwaitAnswer asynchronously and
// upgrades the leg via ConnectOutbound on the remote answer.
func NewMatrixOutboundPendingLeg(cfg MatrixLegConfig) *MatrixLeg {
	partyID := cfg.PartyID
	if partyID == "" {
		partyID = uuid.New().String()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	l := &MatrixLeg{
		id:           uuid.New().String(),
		legType:      TypeMatrixOutbound,
		state:        StateRinging,
		media:        cfg.Media,
		sender:       cfg.Sender,
		matrixRoomID: cfg.MatrixRoomID,
		callID:       cfg.CallID,
		partyID:      partyID,
		createdAt:    time.Now(),
		log:          cfg.Log,
	}
	l.acceptDTMF.Store(true)
	return l
}

// NewMatrixInboundLeg wraps an inbound m.call.invite whose SDP offer has
// already been applied to PCMedia and whose answer SDP has been gathered.
// Answer() sends the m.call.answer once the REST layer signals it.
func NewMatrixInboundLeg(cfg MatrixLegConfig) *MatrixLeg {
	partyID := cfg.PartyID
	if partyID == "" {
		partyID = uuid.New().String()
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	l := &MatrixLeg{
		id:            uuid.New().String(),
		legType:       TypeMatrixInbound,
		state:         StateRinging,
		media:         cfg.Media,
		sender:        cfg.Sender,
		matrixRoomID:  cfg.MatrixRoomID,
		callID:        cfg.CallID,
		partyID:       partyID,
		remotePartyID: cfg.RemotePartyID,
		remoteUserID:  cfg.RemoteUserID,
		createdAt:     time.Now(),
		answerCh:      make(chan struct{}),
		answerSDP:     cfg.AnswerSDP,
		log:           cfg.Log,
	}
	l.acceptDTMF.Store(true)
	return l
}

// ConnectOutbound transitions an outbound leg to connected and locks the
// remote party id. answerSDP is the m.call.answer body.
func (l *MatrixLeg) ConnectOutbound(remotePartyID string, remoteUser id.UserID, answerSDP string) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return fmt.Errorf("leg already hung up")
	}
	if l.state == StateConnected {
		l.mu.Unlock()
		return nil
	}
	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}
	if err := l.media.PC().SetRemoteDescription(answer); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("set remote description: %w", err)
	}
	l.remotePartyID = remotePartyID
	if remoteUser != "" {
		l.remoteUserID = remoteUser
	}
	l.state = StateConnected
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.media.Start()
	return nil
}

// StartCandidatePump runs the local-candidate drain + remote-event subscriber
// in the background. Idempotent. Stops on ctx cancellation, leg hangup, or
// remote m.call.hangup.
func (l *MatrixLeg) StartCandidatePump(ctx context.Context) {
	pumpCtx, cancel := context.WithCancel(ctx)
	if !l.pumpCancel.CompareAndSwap(nil, context.CancelFunc(cancel)) {
		cancel()
		return
	}
	sub := l.sender.Subscribe(l.matrixRoomID, l.callID)
	go l.localCandidateLoop(pumpCtx)
	go l.remoteEventLoop(pumpCtx, sub)
}

// SetOnRemoteHangup installs the callback fired once when the remote sends
// m.call.hangup. Safe to call concurrently with the pump.
func (l *MatrixLeg) SetOnRemoteHangup(f func(reason string)) {
	l.onRemoteHangup.Store(f)
}

func (l *MatrixLeg) localCandidateLoop(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var sentEnd bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cands, done := l.media.DrainLocalCandidates()
		if len(cands) > 0 {
			payload := &mevent.CallCandidatesEventContent{
				BaseCallEventContent: mevent.BaseCallEventContent{
					CallID:  l.callID,
					PartyID: l.partyID,
					Version: "1",
				},
				Candidates: make([]mevent.CallCandidate, 0, len(cands)),
			}
			for _, c := range cands {
				cand := mevent.CallCandidate{Candidate: c.Candidate}
				if c.SDPMid != nil {
					cand.SDPMID = *c.SDPMid
				}
				if c.SDPMLineIndex != nil {
					cand.SDPMLineIndex = int(*c.SDPMLineIndex)
				}
				payload.Candidates = append(payload.Candidates, cand)
			}
			if err := l.sender.SendCandidates(ctx, l.matrixRoomID, payload); err != nil {
				l.log.Warn("matrix leg: send candidates", "leg_id", l.id, "error", err)
			}
		}
		if done && !sentEnd {
			sentEnd = true
			eoc := &mevent.CallCandidatesEventContent{
				BaseCallEventContent: mevent.BaseCallEventContent{
					CallID:  l.callID,
					PartyID: l.partyID,
					Version: "1",
				},
				Candidates: []mevent.CallCandidate{{Candidate: ""}},
			}
			if err := l.sender.SendCandidates(ctx, l.matrixRoomID, eoc); err != nil {
				l.log.Debug("matrix leg: send end-of-candidates", "leg_id", l.id, "error", err)
			}
		}
	}
}

func (l *MatrixLeg) remoteEventLoop(ctx context.Context, sub <-chan matrix.CallEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			switch ev.Kind {
			case matrix.KindCandidates:
				if !l.matchesParty(ev.Candidates.PartyID) {
					continue
				}
				for _, c := range ev.Candidates.Candidates {
					if c.Candidate == "" {
						continue // end-of-candidates marker
					}
					mid := c.SDPMID
					mli := uint16(c.SDPMLineIndex)
					if err := l.media.AddICECandidate(webrtc.ICECandidateInit{
						Candidate:     c.Candidate,
						SDPMid:        &mid,
						SDPMLineIndex: &mli,
					}); err != nil {
						l.log.Debug("matrix leg: add ICE candidate", "leg_id", l.id, "error", err)
					}
				}
			case matrix.KindHangup:
				reason := string(ev.Hangup.Reason)
				if reason == "" {
					reason = "remote_hangup"
				}
				if f, ok := l.onRemoteHangup.Load().(func(string)); ok && f != nil {
					f(reason)
				}
				return
			case matrix.KindReject:
				if f, ok := l.onRemoteHangup.Load().(func(string)); ok && f != nil {
					f("rejected")
				}
				return
			case matrix.KindAnswer:
				// Late answer after we already established (e.g. fork).
				// Lock onto the first remote party; ignore subsequent ones.
				l.mu.Lock()
				if l.remotePartyID == "" {
					l.remotePartyID = ev.Answer.PartyID
				}
				l.mu.Unlock()
			case matrix.KindNegotiate:
				// MSC2746 mid-call renegotiation: not supported in v1.
				l.log.Debug("matrix leg: ignoring m.call.negotiate", "leg_id", l.id)
			}
		}
	}
}

// matchesParty returns true if the remote partyID matches the one we've
// locked onto (or if we haven't locked yet — first event wins).
func (l *MatrixLeg) matchesParty(partyID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.remotePartyID == "" {
		return true
	}
	return partyID == "" || partyID == l.remotePartyID
}

// CallID returns the MSC3401 call identifier.
func (l *MatrixLeg) CallID() string { return l.callID }

// PartyID returns our MSC2746 party identifier.
func (l *MatrixLeg) PartyID() string { return l.partyID }

// MatrixRoomID returns the Matrix room (distinct from RoomID, the VoiceBlender
// mixer room).
func (l *MatrixLeg) MatrixRoomID() id.RoomID { return l.matrixRoomID }

// AnswerCh signals (via close) that the REST layer has authorised the leg to
// send its m.call.answer. Inbound only.
func (l *MatrixLeg) AnswerCh() <-chan struct{} { return l.answerCh }

// RequestAnswer is the inbound-leg entry point invoked from
// POST /v1/legs/{id}/answer.
func (l *MatrixLeg) RequestAnswer() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.answerCh == nil {
		return fmt.Errorf("outbound leg: nothing to answer")
	}
	if l.state != StateRinging {
		return fmt.Errorf("leg is %s, expected ringing", l.state)
	}
	select {
	case <-l.answerCh:
		return fmt.Errorf("already answering")
	default:
		close(l.answerCh)
	}
	return nil
}

// ── Leg interface ───────────────────────────────────────────────────────

func (l *MatrixLeg) ID() string      { return l.id }
func (l *MatrixLeg) Type() LegType   { return l.legType }
func (l *MatrixLeg) SampleRate() int { return l.media.SampleRate() }

func (l *MatrixLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *MatrixLeg) Context() context.Context { return l.media.Context() }

func (l *MatrixLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *MatrixLeg) SetRoomID(id string) {
	l.mu.Lock()
	l.roomID = id
	l.mu.Unlock()
}

func (l *MatrixLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *MatrixLeg) SetAppID(id string) {
	l.mu.Lock()
	l.appID = id
	l.mu.Unlock()
}

func (l *MatrixLeg) Role() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.role
}

func (l *MatrixLeg) SetRole(r string) {
	l.mu.Lock()
	l.role = r
	l.mu.Unlock()
}

func (l *MatrixLeg) IsMuted() bool              { return l.muted.Load() }
func (l *MatrixLeg) SetMuted(m bool)            { l.muted.Store(m) }
func (l *MatrixLeg) IsDeaf() bool               { return l.deaf.Load() }
func (l *MatrixLeg) SetDeaf(d bool)             { l.deaf.Store(d) }
func (l *MatrixLeg) AcceptDTMF() bool           { return l.acceptDTMF.Load() }
func (l *MatrixLeg) SetAcceptDTMF(a bool)       { l.acceptDTMF.Store(a) }
func (l *MatrixLeg) SetSpeakingTap(w io.Writer) { l.media.SetSpeakingTap(w) }
func (l *MatrixLeg) ClearSpeakingTap()          { l.media.ClearSpeakingTap() }
func (l *MatrixLeg) IsHeld() bool               { return false }

func (l *MatrixLeg) CreatedAt() time.Time  { return l.createdAt }
func (l *MatrixLeg) AnsweredAt() time.Time { return l.answeredAt }

// SIPHeaders returns nil — Matrix has no SIP-style header concept.
func (l *MatrixLeg) SIPHeaders() map[string]string { return nil }

// Headers returns nil — Matrix has no SIP-style header concept.
func (l *MatrixLeg) Headers() map[string]string { return nil }

func (l *MatrixLeg) RTPStats() RTPStats { return RTPStats{} }

// ClaimDisconnect returns true on the first caller; false thereafter.
func (l *MatrixLeg) ClaimDisconnect() bool {
	return l.disconnectDone.CompareAndSwap(false, true)
}

func (l *MatrixLeg) Answer(ctx context.Context) error {
	l.mu.Lock()
	if l.answerCh == nil {
		l.mu.Unlock()
		return fmt.Errorf("outbound leg: Answer not applicable")
	}
	if l.state == StateConnected {
		l.mu.Unlock()
		return nil
	}
	sdp := l.answerSDP
	callID := l.callID
	partyID := l.partyID
	roomID := l.matrixRoomID
	l.mu.Unlock()

	if sdp == "" {
		return errors.New("missing answer SDP")
	}
	answer := &mevent.CallAnswerEventContent{
		BaseCallEventContent: mevent.BaseCallEventContent{
			CallID:  callID,
			PartyID: partyID,
			Version: "1",
		},
		Answer: mevent.CallData{
			Type: mevent.CallDataTypeAnswer,
			SDP:  sdp,
		},
	}
	if err := l.sender.SendAnswer(ctx, roomID, answer); err != nil {
		return fmt.Errorf("send m.call.answer: %w", err)
	}

	l.mu.Lock()
	l.state = StateConnected
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.media.Start()
	return nil
}

func (l *MatrixLeg) Hangup(ctx context.Context) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return nil
	}
	l.state = StateHungUp
	callID := l.callID
	partyID := l.partyID
	roomID := l.matrixRoomID
	l.mu.Unlock()

	l.hangupOnce.Do(func() {
		_ = l.sender.SendHangup(ctx, roomID, &mevent.CallHangupEventContent{
			BaseCallEventContent: mevent.BaseCallEventContent{
				CallID:  callID,
				PartyID: partyID,
				Version: "1",
			},
			Reason: mevent.CallHangupUserHangup,
		})
	})
	if v := l.pumpCancel.Load(); v != nil {
		if cf, ok := v.(context.CancelFunc); ok && cf != nil {
			cf()
		}
	}
	l.sender.Unsubscribe(l.matrixRoomID, l.callID)
	return l.media.Close()
}

func (l *MatrixLeg) OnDTMF(f func(digit rune)) {
	l.mu.Lock()
	l.onDTMF = f
	l.mu.Unlock()
	l.media.SetOnDTMF(f)
}

func (l *MatrixLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF send over Matrix not yet implemented")
}

func (l *MatrixLeg) OnTextReceived(_ func(text string, lossMarker bool)) {}

func (l *MatrixLeg) SendText(_ context.Context, _ string) error { return ErrRTTNotNegotiated }

func (l *MatrixLeg) AcceptText() bool     { return false }
func (l *MatrixLeg) SetAcceptText(_ bool) {}
func (l *MatrixLeg) RTTNegotiated() bool  { return false }

func (l *MatrixLeg) AudioReader() io.Reader { return l.media.AudioReader() }
func (l *MatrixLeg) AudioWriter() io.Writer { return l.media.AudioWriter() }

// Media exposes the underlying PCMedia for handler-side SDP work.
func (l *MatrixLeg) Media() *PCMedia { return l.media }
