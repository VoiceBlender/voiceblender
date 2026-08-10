package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/tts"
	"github.com/go-chi/chi/v5"
)

// Discard reasons published on tts.discarded.
const (
	discardApp     = "app"
	discardExpired = "expired"
	discardLegGone = "leg_gone"
)

type stagedState int

const (
	stagedSynthesizing stagedState = iota
	stagedReady
	stagedCommitted
	stagedDone // discarded or failed; terminal
)

// stagedTTS is an utterance synthesized ahead of the decision to play it.
// The audio is buffered rather than left streaming from the provider, so that
// committing costs no upstream round trip and an upstream failure surfaces
// while the app can still react to it.
type stagedTTS struct {
	ttsID  string
	legID  string
	appID  string
	volume int
	target *legTTSTarget

	state    stagedState // guarded by stagedTTSReg
	mimeType string
	audio    []byte
	err      error

	ready       chan struct{} // closed once synthesis settles
	done        chan struct{} // closed on a terminal state; stops the watcher
	doneOnce    sync.Once
	cancelSynth context.CancelFunc
}

// finish stops the watcher. Commit, discard and a failed synthesis all race to
// call it.
func (e *stagedTTS) finish() {
	e.doneOnce.Do(func() { close(e.done) })
}

// stagedTTSReg holds staged utterances that are not yet playing. A tts id is
// in exactly one of stagedTTSReg and legPlayers, never both.
var stagedTTSReg = struct {
	sync.Mutex
	m map[string]map[string]*stagedTTS // legID -> ttsID -> entry
}{m: make(map[string]map[string]*stagedTTS)}

// Fallbacks mirroring config.Load's defaults, so a Server built with a zero
// Config still stages sanely instead of refusing every request.
const (
	defaultPreflightTTL       = 30 * time.Second
	defaultPreflightMaxPerLeg = 3
	defaultPreflightMaxBytes  = 4 << 20
)

func (s *Server) preflightLimits() (ttl time.Duration, maxPerLeg, maxBytes int) {
	ttl, maxPerLeg, maxBytes = s.Config.TTSPreflightTTL, s.Config.TTSPreflightMaxPerLeg, s.Config.TTSPreflightMaxBytes
	if ttl <= 0 {
		ttl = defaultPreflightTTL
	}
	if maxPerLeg <= 0 {
		maxPerLeg = defaultPreflightMaxPerLeg
	}
	if maxBytes <= 0 {
		maxBytes = defaultPreflightMaxBytes
	}
	return ttl, maxPerLeg, maxBytes
}

// TTSDiscardResult is the success payload for discarding a staged utterance.
type TTSDiscardResult struct {
	Status string `json:"status"`
}

func (s *Server) doPreflightLegTTS(legID string, req TTSRequest) (*TTSStartResult, error) {
	t, err := s.validateLegTTS(legID, req)
	if err != nil {
		return nil, err
	}

	_, maxPerLeg, _ := s.preflightLimits()
	ttsID := newTTSID()

	entry := &stagedTTS{
		ttsID:  ttsID,
		legID:  legID,
		appID:  t.leg.AppID(),
		volume: req.Volume,
		target: t,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}

	stagedTTSReg.Lock()
	if len(stagedTTSReg.m[legID]) >= maxPerLeg {
		stagedTTSReg.Unlock()
		// Refuse rather than evict: evicting the oldest would silently
		// destroy an utterance the app is about to commit.
		return nil, newAPIError(http.StatusConflict, "too many staged TTS utterances on this leg (max %d)", maxPerLeg)
	}
	if stagedTTSReg.m[legID] == nil {
		stagedTTSReg.m[legID] = make(map[string]*stagedTTS)
	}
	stagedTTSReg.m[legID][ttsID] = entry
	stagedTTSReg.Unlock()

	synthCtx, cancelSynth := context.WithCancel(t.leg.Context())
	entry.cancelSynth = cancelSynth

	go s.synthesizeStaged(synthCtx, entry, req)
	go s.watchStaged(entry)

	return &TTSStartResult{TTSID: ttsID, Status: "staged"}, nil
}

func (s *Server) synthesizeStaged(ctx context.Context, entry *stagedTTS, req TTSRequest) {
	defer entry.cancelSynth()

	scope := events.LegRoomScope{LegID: entry.legID, AppID: entry.appID}
	_, _, maxBytes := s.preflightLimits()

	result, err := entry.target.provider.Synthesize(ctx, req.Text, ttsOptions(req, entry.target.apiKey))
	var audio []byte
	var mimeType string
	oversize := false
	if err == nil {
		mimeType = result.MimeType
		audio, err = io.ReadAll(io.LimitReader(result.Audio, int64(maxBytes)+1))
		result.Audio.Close()
		if err == nil && len(audio) > maxBytes {
			oversize = true
			audio = nil
			err = newAPIError(http.StatusBadRequest, "synthesized audio exceeds TTS_PREFLIGHT_MAX_BYTES (%d)", maxBytes)
		}
	}

	stagedTTSReg.Lock()
	discarded := entry.state == stagedDone
	// A commit that landed before synthesis finished is still waiting on
	// entry.ready, so the result must be stored for it — only a discard makes
	// it genuinely worthless.
	if !discarded {
		entry.err = err
		if err != nil {
			entry.state = stagedDone
			delete(stagedTTSReg.m[entry.legID], entry.ttsID)
			if len(stagedTTSReg.m[entry.legID]) == 0 {
				delete(stagedTTSReg.m, entry.legID)
			}
		} else {
			entry.audio = audio
			entry.mimeType = mimeType
			if entry.state == stagedSynthesizing {
				entry.state = stagedReady
			}
		}
	}
	committed := entry.state == stagedCommitted
	stagedTTSReg.Unlock()
	close(entry.ready)

	if discarded {
		return
	}

	if err != nil {
		entry.finish()
		// A leg ending mid-synthesis is a discard, not a provider failure.
		// Without this, whichever of watchStaged and this goroutine wins the
		// race would decide which event the app sees.
		if !committed && entry.target.leg.Context().Err() != nil {
			s.Bus.Publish(events.TTSDiscarded, &events.TTSDiscardedData{
				LegRoomScope: scope,
				TTSID:        entry.ttsID,
				Reason:       discardLegGone,
			})
			return
		}
		category := tts.Categorize(err)
		if oversize {
			category = tts.CategoryPermanentInput
		}
		s.Bus.Publish(events.TTSError, &events.TTSErrorData{
			LegRoomScope: scope,
			TTSID:        entry.ttsID,
			Error:        err.Error(),
			Category:     string(category),
		})
		return
	}

	if committed {
		// Already being played; "ready to commit" would only confuse the app.
		return
	}
	s.Bus.Publish(events.TTSStaged, &events.TTSStagedData{
		LegRoomScope: scope,
		TTSID:        entry.ttsID,
		Bytes:        len(audio),
		DurationMs:   pcmDurationMs(audio, mimeType),
	})
}

// watchStaged drops the entry if the leg ends or the TTL elapses first. Staged
// entries need their own sweep: legPlayers is only ever cleaned up when a
// player goroutine returns, and a staged utterance has no player yet.
func (s *Server) watchStaged(entry *stagedTTS) {
	ttl, _, _ := s.preflightLimits()
	timer := time.NewTimer(ttl)
	defer timer.Stop()

	select {
	case <-entry.done:
		return
	case <-entry.target.leg.Context().Done():
		s.discardStaged(entry, discardLegGone)
	case <-timer.C:
		s.discardStaged(entry, discardExpired)
	}
}

// discardStaged drops a staged utterance unless it already reached a terminal
// state. Returns whether this call was the one that discarded it.
func (s *Server) discardStaged(entry *stagedTTS, reason string) bool {
	stagedTTSReg.Lock()
	if entry.state == stagedCommitted || entry.state == stagedDone {
		stagedTTSReg.Unlock()
		return false
	}
	entry.state = stagedDone
	entry.audio = nil
	delete(stagedTTSReg.m[entry.legID], entry.ttsID)
	if len(stagedTTSReg.m[entry.legID]) == 0 {
		delete(stagedTTSReg.m, entry.legID)
	}
	stagedTTSReg.Unlock()

	if entry.cancelSynth != nil {
		entry.cancelSynth()
	}
	entry.finish()

	s.Bus.Publish(events.TTSDiscarded, &events.TTSDiscardedData{
		LegRoomScope: events.LegRoomScope{LegID: entry.legID, AppID: entry.appID},
		TTSID:        entry.ttsID,
		Reason:       reason,
	})
	return true
}

// doCommitLegTTS starts playing a staged utterance. It returns as soon as the
// player is registered, without waiting for synthesis, so that committing is a
// drop-in for leg_tts: both report failure asynchronously on tts.error.
func (s *Server) doCommitLegTTS(legID, ttsID string) (*TTSStartResult, error) {
	stagedTTSReg.Lock()
	entry, ok := stagedTTSReg.m[legID][ttsID]
	if !ok {
		stagedTTSReg.Unlock()
		return nil, newAPIError(http.StatusNotFound, "no staged TTS with that id")
	}
	if entry.state == stagedCommitted {
		stagedTTSReg.Unlock()
		return nil, newAPIError(http.StatusConflict, "staged TTS already committed")
	}
	entry.state = stagedCommitted
	delete(stagedTTSReg.m[legID], ttsID)
	if len(stagedTTSReg.m[legID]) == 0 {
		delete(stagedTTSReg.m, legID)
	}
	stagedTTSReg.Unlock()

	entry.finish()

	// Registered synchronously so a leg_play_stop issued right after commit
	// reliably finds the player.
	player := s.registerLegPlayerAs(legID, ttsID, entry.volume)

	go func() {
		<-entry.ready
		stagedTTSReg.Lock()
		audio, mimeType, synthErr := entry.audio, entry.mimeType, entry.err
		stagedTTSReg.Unlock()
		if synthErr != nil {
			deregisterLegPlayer(legID, ttsID)
			return // synthesizeStaged already published tts.error
		}
		s.playLegTTSAudio(entry.target, ttsID, player, bytes.NewReader(audio), mimeType)
	}()

	return &TTSStartResult{TTSID: ttsID, Status: "committed"}, nil
}

func (s *Server) doDiscardLegTTS(legID, ttsID string) (*TTSDiscardResult, error) {
	stagedTTSReg.Lock()
	entry, ok := stagedTTSReg.m[legID][ttsID]
	stagedTTSReg.Unlock()
	if !ok {
		// A committed utterance has already left the registry for legPlayers.
		if legPlayerExists(legID, ttsID) {
			return nil, newAPIError(http.StatusConflict, "staged TTS already committed; use leg_play_stop to stop playback")
		}
		return nil, newAPIError(http.StatusNotFound, "no staged TTS with that id")
	}
	if !s.discardStaged(entry, discardApp) {
		return nil, newAPIError(http.StatusConflict, "staged TTS already committed; use leg_play_stop to stop playback")
	}
	return &TTSDiscardResult{Status: "discarded"}, nil
}

func (s *Server) preflightTTSLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req TTSRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doPreflightLegTTS(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) commitTTSLeg(w http.ResponseWriter, r *http.Request) {
	res, err := s.doCommitLegTTS(chi.URLParam(r, "id"), chi.URLParam(r, "ttsID"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) discardTTSLeg(w http.ResponseWriter, r *http.Request) {
	res, err := s.doDiscardLegTTS(chi.URLParam(r, "id"), chi.URLParam(r, "ttsID"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// pcmDurationMs reports how long buffered raw PCM will play for. Returns 0 for
// container formats, whose duration is not derivable from the byte count.
func pcmDurationMs(audio []byte, mimeType string) int {
	mt := strings.ToLower(mimeType)
	if !strings.HasPrefix(mt, "audio/pcm") && !strings.HasPrefix(mt, "audio/l16") {
		return 0
	}
	rate := 16000
	if idx := strings.Index(mt, "rate="); idx != -1 {
		if v, err := strconv.Atoi(mt[idx+5:]); err == nil && v > 0 {
			rate = v
		}
	}
	return len(audio) / 2 * 1000 / rate
}
